package build_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// The tests below hand the builtin backend a rockspec whose paths leave the
// sources or the destination dir, and check that it fails and touches
// nothing outside. Each lays out one temp dir holding the sources, the
// destination and an "outside" sibling the escaping path points at, so a
// regression lands in the test's own temp dir.

type buildLayout struct {
	src, dest, outside string
}

func newBuildLayout(t *testing.T) buildLayout {
	t.Helper()

	parent := t.TempDir()
	l := buildLayout{
		src:     filepath.Join(parent, "src"),
		dest:    filepath.Join(parent, "dest"),
		outside: filepath.Join(parent, "outside"),
	}

	require.NoError(t, build.WriteFile(filepath.Join(l.src, "demo.lua"), "return 1"))
	require.NoError(t, build.WriteFile(filepath.Join(l.outside, "victim"), "keep"))
	require.NoError(t, build.WriteFile(filepath.Join(l.outside, "victim.lua"), "return 'secret'"))

	return l
}

// assertOutsideIntact checks that the outside dir still holds its two files,
// unchanged.
func (l buildLayout) assertOutsideIntact(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(l.outside)
	require.NoError(t, err)
	assert.Len(t, entries, 2, "nothing may be added to or removed from %s", l.outside)

	for name, want := range map[string]string{"victim": "keep", "victim.lua": "return 'secret'"} {
		got, err := build.ReadFile(filepath.Join(l.outside, name))
		require.NoError(t, err)
		assert.Equal(t, want, string(got), name)
	}
}

// assertDestEmpty checks that nothing was copied into the destination.
func (l buildLayout) assertDestEmpty(t *testing.T) {
	t.Helper()

	_ = filepath.Walk(l.dest, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			t.Errorf("unexpected file in dest: %s", p)
		}

		return nil
	})
}

func (l buildLayout) run(b rocks.Build) error {
	b.Type = "builtin"
	spec := &rocks.Rockspec{Package: "demo", Version: "1.0-1", Build: b}

	return build.RunBackend(context.Background(), spec, l.src, l.dest, rocks.Config{})
}

// TestBuiltin_InstallKeyStaysInDest: a build.install.bin or conf key is a
// path under dest/bin or dest/conf, so "../../outside/..." wrote, and
// chmodded, a file beside the destination.
func TestBuiltin_InstallKeyStaysInDest(t *testing.T) {
	t.Parallel()

	for _, bi := range []rocks.BuildInstall{
		{Bin: map[string]string{"../../outside/pwn": "demo.lua"}},
		{Bin: map[string]string{"../../outside/victim": "demo.lua"}},
		{Conf: map[string]string{"../../outside/victim": "demo.lua"}},
		{Bin: map[string]string{".": "demo.lua"}},
	} {
		l := newBuildLayout(t)

		require.ErrorIs(t, l.run(rocks.Build{Install: bi}), rocks.ErrUnsafePath, "%+v", bi)
		l.assertOutsideIntact(t)
	}
}

// TestBuiltin_InstallValueStaysInSource: the value of a build.install entry
// is a path in the sources; "../outside/victim" copied a file from beside
// them.
func TestBuiltin_InstallValueStaysInSource(t *testing.T) {
	t.Parallel()

	for _, bi := range []rocks.BuildInstall{
		{Lua: map[string]string{"stolen": "../outside/victim.lua"}},
		{Lib: map[string]string{"stolen": "../outside/victim"}},
		{Bin: map[string]string{"stolen": "../outside/victim"}},
		{Conf: map[string]string{"stolen": "../outside/victim"}},
	} {
		l := newBuildLayout(t)

		require.ErrorIs(t, l.run(rocks.Build{Install: bi}), rocks.ErrUnsafePath, "%+v", bi)
		l.assertDestEmpty(t)
	}
}

// TestBuiltin_ModulePathStaysInSource: a Lua build.modules path is copied
// from the sources, so "../outside/victim.lua" copied a file from beside
// them.
func TestBuiltin_ModulePathStaysInSource(t *testing.T) {
	t.Parallel()

	l := newBuildLayout(t)

	err := l.run(rocks.Build{Modules: map[string]rocks.Module{"stolen": {Path: "../outside/victim.lua"}}})
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	l.assertDestEmpty(t)
}

// TestBuiltin_ModuleSourcesStayInSource: the C sources of a module are
// handed to the compiler as paths in the sources; one outside them must be
// refused before the compiler runs.
func TestBuiltin_ModuleSourcesStayInSource(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cc-ran")
	stub := filepath.Join(dir, "cc")
	require.NoError(t, build.WriteFile(stub, "#!/bin/sh\n: > '"+marker+"'\n"))
	require.NoError(t, build.ChmodX(stub))
	t.Setenv("CC", stub)

	cfg := rocks.Config{Tarantool: rocks.TarantoolConfig{IncludeDir: dir}}

	for _, m := range []rocks.Module{
		{Sources: []string{"../outside/x.c"}},
		{Sources: []string{"/etc/hostname"}},
		{Path: "../outside/x.c"},
	} {
		l := newBuildLayout(t)
		spec := &rocks.Rockspec{
			Package: "demo", Version: "1.0-1",
			Build: rocks.Build{Type: "builtin", Modules: map[string]rocks.Module{"c": m}},
		}

		err := build.RunBackend(context.Background(), spec, l.src, l.dest, cfg)
		require.ErrorIs(t, err, rocks.ErrUnsafePath, "%+v", m)
		assert.NoFileExists(t, marker, "%+v: the compiler must not run", m)
	}
}

// TestBuiltin_CopyDirectoriesStayInDest: a copy_directories entry is joined
// under both the sources and the destination, which are siblings here, so
// "../outside" copied the outside dir onto itself and truncated every file
// in it.
func TestBuiltin_CopyDirectoriesStayInDest(t *testing.T) {
	t.Parallel()

	l := newBuildLayout(t)

	require.ErrorIs(t, l.run(rocks.Build{CopyDirectories: []string{"../outside"}}), rocks.ErrUnsafePath)
	l.assertOutsideIntact(t)
}

// TestBuiltin_CopyDirectoriesSymlinkOut: a copy_directories entry holding a
// symlink to a file outside the sources copied that file. A link that stays
// inside the sources is still followed.
func TestBuiltin_CopyDirectoriesSymlinkOut(t *testing.T) {
	t.Parallel()

	l := newBuildLayout(t)

	require.NoError(t, build.WriteFile(filepath.Join(l.src, "doc", "README"), "readme"))
	require.NoError(t, os.Symlink("README", filepath.Join(l.src, "doc", "inside")))
	require.NoError(t, os.Symlink(filepath.Join(l.outside, "victim"), filepath.Join(l.src, "doc", "leak")))

	require.ErrorIs(t, l.run(rocks.Build{CopyDirectories: []string{"doc"}}), rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.dest, "doc", "leak"))

	require.NoError(t, os.Remove(filepath.Join(l.src, "doc", "leak")))
	require.NoError(t, l.run(rocks.Build{CopyDirectories: []string{"doc"}}))

	got, err := build.ReadFile(filepath.Join(l.dest, "doc", "inside"))
	require.NoError(t, err)
	assert.Equal(t, "readme", string(got))
}
