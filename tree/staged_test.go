package tree_test

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// stagedSpec returns the rockspec of a staged rock name/version, evaluated
// from its source.
func stagedSpec(t *testing.T, name, version string) *rocks.Rockspec {
	t.Helper()

	src := fmt.Sprintf(`package = %q
version = %q
source = { url = "file://localhost/%s.tar.gz" }
dependencies = { "alpha >= 1.0" }
build = { type = "none" }
`, name, version, name)

	spec, err := rockspec.EvalSource(name+".rockspec", []byte(src), rocks.RockspecConfig{})
	require.NoError(t, err)

	return spec
}

// stageRockDir lays a binary rock of name/version out in a fresh directory:
// Lua modules, a C module, a Lua and a shell command, doc and conf
// directories, a file at the top, and the rockspec and a rock_manifest at the
// top, which an install does not take from there.
func stageRockDir(t *testing.T, spec *rocks.Rockspec) string {
	t.Helper()

	dir := t.TempDir()
	for rel, body := range map[string]string{
		"lua/" + spec.Package + "/init.lua":             "return 'staged'\n",
		"lua/shared.lua":                                "return 'staged shared'\n",
		"lib/" + spec.Package + "/core.so":              "\x7fELF not really\n",
		"bin/" + spec.Package + "-tool":                 "print('staged tool')\n",
		"bin/" + spec.Package + "-sh":                   "#!/bin/sh\necho staged\n",
		"doc/README.md":                                 "# staged\n",
		"conf/" + spec.Package + ".conf":                "key = value\n",
		"NOTES":                                         "notes\n",
		"rock_manifest":                                 "rock_manifest = {}\n",
		spec.Package + "-" + spec.Version + ".rockspec": string(spec.RawSource),
	} {
		writeFixture(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}

	return dir
}

// zipRock packs the directory dir into the binary rock of spec, named as a
// server names one, in a fresh directory, and returns its path.
func zipRock(t *testing.T, spec *rocks.Rockspec, dir string) string {
	t.Helper()

	out := filepath.Join(t.TempDir(), spec.Package+"-"+spec.Version+".all.rock")

	f, err := os.Create(out) //nolint:gosec // test temp path
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	require.NoError(t, filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		h, err := zip.FileInfoHeader(fi)
		if err != nil {
			return err
		}

		h.Name = filepath.ToSlash(rel)

		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}

		in, err := os.Open(p) //nolint:gosec // test temp path
		if err != nil {
			return err
		}

		defer func() { _ = in.Close() }()

		_, err = io.Copy(w, in)

		return err
	}))
	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	return out
}

// installStaged runs Tx.InstallStaged on the tree at root.
func installStaged(t *testing.T, root string, spec *rocks.Rockspec, dir string, opts tree.StagedOpts) (*rocks.RockManifest, error) {
	t.Helper()

	var rm *rocks.RockManifest

	err := openWrapped(t, root).Update(context.Background(), func(tx *tree.Tx) error {
		var err error

		rm, err = tx.InstallStaged(spec, dir, opts)

		return err
	})

	return rm, err
}

// TestInstallStaged_AsNativeInstall: a staged rock installed through
// InstallStaged leaves the tree the native client leaves when it installs
// the binary rock of the same files, alongside rocks it shares a module
// with.
func TestInstallStaged_AsNativeInstall(t *testing.T) {
	t.Parallel()

	spec := stagedSpec(t, "staged", "1.0-1")
	dir := stageRockDir(t, spec)

	installed := buildTree(t, alpha1, alpha2)
	installRocks(t, installed, serveRocks(t, zipRock(t, spec, dir)), tree.RockRef{Name: "staged", Version: "1.0-1"})

	staged := buildTree(t, alpha1, alpha2)
	rm, err := installStaged(t, staged, spec, dir, tree.StagedOpts{})
	require.NoError(t, err, "InstallStaged")

	assert.Equal(t, treeState(t, installed), treeState(t, staged), "the staged tree vs the installed one")
	assert.Contains(t, rm.Other, "NOTES", "a file at the top of the rock")
	assert.NotContains(t, rm.Other, "rock_manifest")
	assert.Contains(t, rm.Doc, "README.md")
	assert.Contains(t, rm.Conf, "staged.conf")
	assert.Len(t, rm.Bin, 2)

	ref := tree.RockRef{Name: "staged", Version: "1.0-1"}
	v := inspect(t, staged)
	assert.Equal(t, map[string]string{"alpha": "2.0-1"}, v.Requires(ref), "its dependency, resolved")
	require.ErrorIs(t, v.CheckPlain(ref), tree.ErrNotPlain, "alpha holds shared")
	assert.Empty(t, check(t, staged).Issues)

	_, err = installStaged(t, staged, spec, dir, tree.StagedOpts{})
	require.NoError(t, err, "InstallStaged again")
	assert.Equal(t, treeState(t, installed), treeState(t, staged), "the reinstalled tree")
}

// TestInstallStaged_Options: RequirePlain refuses, with the tree as it was, a
// rock that would not hold its plain paths, and takes one that would;
// RemoveOtherVersions removes the rock's other versions first, so its own
// files get their plain paths back.
func TestInstallStaged_Options(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1)
	before := treeState(t, root)

	zeta := stagedSpec(t, "zeta", "1.0-1")
	_, err := installStaged(t, root, zeta, stageRockDir(t, zeta), tree.StagedOpts{RequirePlain: true})
	require.ErrorIs(t, err, tree.ErrNotPlain, "zeta's shared, held by alpha")
	require.ErrorContains(t, err, "lua/shared.lua (held by alpha/1.0-1)")
	assert.Equal(t, before, treeState(t, root), "the tree")

	aaa := stagedSpec(t, "aaa", "1.0-1")
	_, err = installStaged(t, root, aaa, stageRockDir(t, aaa), tree.StagedOpts{RequirePlain: true})
	require.NoError(t, err, "aaa sorts before alpha")
	require.NoError(t, inspect(t, root).CheckPlain(tree.RockRef{Name: "aaa", Version: "1.0-1"}))

	// alpha 0.5 loses its files to alpha 1, unless alpha 1 goes first.
	old := stagedSpec(t, "alpha", "0.5-1")
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "lua", "alpha.lua"), "return 'alpha 0.5'\n")

	_, err = installStaged(t, root, old, dir, tree.StagedOpts{RequirePlain: true})
	require.ErrorIs(t, err, tree.ErrNotPlain, "alpha 0.5 below alpha 1")

	_, err = installStaged(t, root, old, dir, tree.StagedOpts{RequirePlain: true, RemoveOtherVersions: true})
	require.NoError(t, err, "alpha 0.5 replacing alpha 1")

	v := inspect(t, root)
	assert.Equal(t, []string{"0.5-1"}, v.Versions("alpha"))
	assert.Equal(t, "return 'alpha 0.5'\n", readFile(t, filepath.Join(tree.Paths{Tree: root}.DeployLuaDir(), "alpha.lua")))
	assert.Empty(t, check(t, root).Issues)
}

// TestInstallStaged_Spec: the spec needs a RawSource of its own package and
// version, and a version that parses.
func TestInstallStaged_Spec(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "lua", "x.lua"), "return 1\n")

	good := stagedSpec(t, "x", "1.0-1")

	noSource := *good
	noSource.RawSource = nil

	otherSource := *good
	otherSource.Version = "2.0-1"

	badVersion := stagedSpec(t, "x", "1.0-1")
	badVersion.Version = "not a version"

	unsafe := *good
	unsafe.Package = "../x"

	for name, spec := range map[string]*rocks.Rockspec{
		"no RawSource":           &noSource,
		"RawSource of another":   &otherSource,
		"a version not parsing":  badVersion,
		"a name leaving its dir": &unsafe,
		"nil":                    nil,
	} {
		_, err := installStaged(t, root, spec, dir, tree.StagedOpts{})
		require.Error(t, err, name)
	}

	assert.Empty(t, inspect(t, root).Rocks())

	_, err := installStaged(t, root, good, dir, tree.StagedOpts{})
	require.NoError(t, err)
}

// TestInstallStaged_NoFiles: a rock with no files — a stage dir that is
// empty, or does not exist — installs as its rockspec alone, with every
// option, and the tree holds nothing else of it.
func TestInstallStaged_NoFiles(t *testing.T) {
	t.Parallel()

	opts := tree.StagedOpts{RequirePlain: true, NoDisplace: true, RemoveOtherVersions: true}

	for name, dir := range map[string]string{
		"an empty stage": t.TempDir(),
		"no stage":       filepath.Join(t.TempDir(), "absent"),
	} {
		root := t.TempDir()
		spec := stagedSpec(t, "meta", "1.0-1")

		rm, err := installStaged(t, root, spec, dir, opts)
		require.NoError(t, err, name)
		assert.Equal(t, "meta-1.0-1.rockspec", rm.RockspecFile, name)
		assert.Empty(t, rm.Lua, name)
		assert.Empty(t, rm.Lib, name)
		assert.Empty(t, rm.Bin, name)
		assert.Empty(t, rm.Other, name)

		ref := tree.RockRef{Name: "meta", Version: "1.0-1"}
		v := inspect(t, root)
		assert.Equal(t, []tree.RockRef{ref}, v.Rocks(), name)

		if deps := v.Dependencies(ref); assert.Len(t, deps, 1, name) {
			assert.Equal(t, "alpha", deps[0].Name, name)
		}

		assert.Empty(t, check(t, root).Issues, name)
	}
}
