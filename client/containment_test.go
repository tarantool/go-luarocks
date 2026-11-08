package client_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// The tests below give the native engine's source resolution, Unpack and
// Pack names that leave their directory and check that they are refused.
// Each lays out one temp dir holding the working dirs and an "outside"
// sibling, so a regression lands in the test's own temp dir.

// containDirs returns a fresh temp dir, its "work" subdir and its "outside"
// subdir, which holds a victim file.
func containDirs(t *testing.T) (work, outside string) {
	t.Helper()

	parent := t.TempDir()
	work = filepath.Join(parent, "work")
	outside = filepath.Join(parent, "outside")

	require.NoError(t, os.MkdirAll(work, 0o750))
	require.NoError(t, os.MkdirAll(outside, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0o600))

	return work, outside
}

func requireVictimIntact(t *testing.T, outside string) {
	t.Helper()

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Len(t, entries, 1, "nothing may be added to %s", outside)

	body, err := os.ReadFile(filepath.Join(outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(body))
}

// TestFindSourceBaseDir_SourceDirStaysInSources: source.dir picks the build
// root inside the fetched sources, so "../outside" built from a directory
// beside them.
func TestFindSourceBaseDir_SourceDirStaysInSources(t *testing.T) {
	t.Parallel()

	work, _ := containDirs(t)

	for _, dir := range []string{"../outside", "/etc"} {
		_, err := client.FindSourceBaseDir(work, &rocks.Rockspec{Source: rocks.Source{Dir: dir}})
		require.ErrorIs(t, err, rocks.ErrUnsafePath, dir)
	}

	require.NoError(t, os.MkdirAll(filepath.Join(work, "src"), 0o750))

	got, err := client.FindSourceBaseDir(work, &rocks.Rockspec{Source: rocks.Source{Dir: "src"}})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(work, "src"), got)
}

// TestFindSourceBaseDir_DeducedParentIgnored: the directory guessed from a
// URL whose last element is ".." is the parent of the sources, not a guess.
func TestFindSourceBaseDir_DeducedParentIgnored(t *testing.T) {
	t.Parallel()

	work, _ := containDirs(t)
	require.NoError(t, os.WriteFile(filepath.Join(work, "a.lua"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(work, "b.lua"), nil, 0o600))

	got, err := client.FindSourceBaseDir(work, &rocks.Rockspec{Source: rocks.Source{URL: "https://example.com/x/.."}})
	require.NoError(t, err)
	assert.Equal(t, work, got)
}

// TestBundledSource_SourceFileStaysInRockDir: a .src.rock names its bundled
// archive by source.file, which was joined onto the unpacked rock as it came.
func TestBundledSource_SourceFileStaysInRockDir(t *testing.T) {
	t.Parallel()

	work, outside := containDirs(t)

	spec := &rocks.Rockspec{Source: rocks.Source{URL: "https://example.com/x.tar", File: "../outside/victim"}}

	_, err := client.BundledSource(context.Background(), spec, work)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	requireVictimIntact(t, outside)
}

// TestCopyBackFiles_StaysInDirs: Pack copies each lua and lib file of a
// rock_manifest from the deploy dir into the pack dir, so an entry with ".."
// copied a file from beside the deploy dir to beside the pack dir.
func TestCopyBackFiles_StaysInDirs(t *testing.T) {
	t.Parallel()

	work, outside := containDirs(t)
	deployDir := filepath.Join(work, "deploy", "lua")
	packDir := filepath.Join(work, "pack", "lua")

	require.NoError(t, os.MkdirAll(deployDir, 0o750))

	for _, rel := range []string{"../../../outside/victim", "/etc/hostname"} {
		err := client.CopyBackFiles(map[string]string{rel: "0123"}, deployDir, packDir, "demo", "1.0-1", 0o644)
		require.ErrorIs(t, err, rocks.ErrUnsafePath, rel)
	}

	requireVictimIntact(t, outside)
}

// TestNativeUnpack_StaysInDestDir: Unpack writes every entry inside destDir,
// also when destDir already holds a symlink pointing out, and keeps only the
// permission bits of an entry.
func TestNativeUnpack_StaysInDestDir(t *testing.T) {
	t.Parallel()

	work, outside := containDirs(t)
	r := newNativeClient(t, t.TempDir(), work)

	archive := filepath.Join(work, "x.rock")
	writeZip(t, archive, map[string]string{"../outside/new": "x"})

	dest := filepath.Join(work, "dest")
	require.ErrorIs(t, r.Unpack(context.Background(), archive, dest), rocks.ErrUnsafePath)
	requireVictimIntact(t, outside)

	writeZip(t, archive, map[string]string{"link/new": "x"})
	require.NoError(t, os.Symlink(outside, filepath.Join(dest, "link")))
	require.ErrorIs(t, r.Unpack(context.Background(), archive, dest), rocks.ErrUnsafePath)
	requireVictimIntact(t, outside)

	writeZip(t, archive, map[string]string{"bin/tool": "x"})
	require.NoError(t, r.Unpack(context.Background(), archive, dest))

	st, err := os.Stat(filepath.Join(dest, "bin", "tool"))
	require.NoError(t, err)
	assert.Zero(t, st.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
}
