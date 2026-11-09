package tree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// The tests below give Deploy and DeleteVersion a name that leaves the tree,
// as a rockspec or a rock_manifest from a server can, and check that the
// operation fails and touches nothing outside. Each lays out one temp dir
// holding the tree, the sources and an "outside" sibling the escaping name
// points at, so a regression lands in the test's own temp dir.

const containVer = "1.0-1"

// containLayout is the temp dir of one containment test: the tree, a source
// dir and the outside dir beside them, which holds a victim file.
type containLayout struct {
	parent, src, outside string
	tree                 *tree.Tree
}

func newContainLayout(t *testing.T) containLayout {
	t.Helper()

	parent := t.TempDir()
	l := containLayout{
		parent:  parent,
		src:     filepath.Join(parent, "src"),
		outside: filepath.Join(parent, "outside"),
	}

	writeFile(t, filepath.Join(l.outside, "victim"), "keep")
	writeFile(t, filepath.Join(l.src, "demo.lua"), "return 1")

	tr, err := tree.Open(rocks.Config{Tree: filepath.Join(parent, "tree")})
	require.NoError(t, err)

	// The tree has the layout an earlier install leaves.
	for _, d := range []string{tr.RocksDir(), tr.DeployLuaDir(), tr.DeployLibDir(), tr.BinDir()} {
		require.NoError(t, os.MkdirAll(d, 0o750))
	}

	l.tree = tr

	return l
}

// assertOutsideIntact checks that the outside dir still holds just its victim
// file, unchanged.
func (l containLayout) assertOutsideIntact(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(l.outside)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	assert.Equal(t, []string{"victim"}, names, "nothing may be added to or removed from %s", l.outside)

	body, err := os.ReadFile(filepath.Join(l.outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(body))
}

func (l containLayout) spec(b rocks.Build) *rocks.Rockspec {
	return &rocks.Rockspec{Package: demoPkg, Version: containVer, Build: b}
}

// TestDeploy_InstallBinKeyStaysInTree: a build.install.bin key is the path of
// the command under <tree>/bin, so "../../outside/pwn" named a file beside the
// tree, which Deploy wrote even for a "none" rock that runs no build code.
func TestDeploy_InstallBinKeyStaysInTree(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	spec := l.spec(rocks.Build{
		Type:    "none",
		Install: rocks.BuildInstall{Bin: map[string]string{"../../outside/pwn": "demo.lua"}},
	})

	_, err := l.tree.Deploy(spec, l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.outside, "pwn"))
	l.assertOutsideIntact(t)
}

// TestDeploy_InstallBinKeyDotKeepsBinDir: a key of "." names <tree>/bin
// itself, which Deploy moved aside to bin~ to write the command in its place.
func TestDeploy_InstallBinKeyDotKeepsBinDir(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	spec := l.spec(rocks.Build{
		Type:    "none",
		Install: rocks.BuildInstall{Bin: map[string]string{".": "demo.lua"}},
	})

	_, err := l.tree.Deploy(spec, l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.DirExists(t, l.tree.BinDir())
	assert.NoFileExists(t, l.tree.BinDir()+"~")
}

// TestDeploy_InstallConfKeyStaysInTree: a build.install.conf key is joined
// under the per-rock conf dir, six levels below the tree.
func TestDeploy_InstallConfKeyStaysInTree(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	spec := l.spec(rocks.Build{
		Type:    "none",
		Install: rocks.BuildInstall{Conf: map[string]string{"../../../../../../../outside/victim": "demo.lua"}},
	})

	_, err := l.tree.Deploy(spec, l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	l.assertOutsideIntact(t)
}

// TestDeploy_InstallValueStaysInSource: the value of a build.install entry is
// a path in the sources; "../outside/victim" copied a file from beside them
// into the tree.
func TestDeploy_InstallValueStaysInSource(t *testing.T) {
	t.Parallel()

	for _, b := range []rocks.BuildInstall{
		{Lua: map[string]string{"stolen": "../outside/victim"}},
		{Lib: map[string]string{"stolen": "../outside/victim"}},
		{Bin: map[string]string{"stolen": "../outside/victim"}},
		{Conf: map[string]string{"stolen": "../outside/victim"}},
	} {
		l := newContainLayout(t)

		_, err := l.tree.Deploy(l.spec(rocks.Build{Type: "none", Install: b}), l.src, l.src)
		require.ErrorIs(t, err, rocks.ErrUnsafePath, "%+v", b)
		assert.Empty(t, deployedFiles(t, l.tree.Tree), "%+v: nothing may be copied into the tree", b)
	}
}

// TestDeploy_ModulePathStaysInSource: a build.modules path is read from the
// sources, so "../outside/victim" copied a file from beside them.
func TestDeploy_ModulePathStaysInSource(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	spec := l.spec(rocks.Build{Modules: map[string]rocks.Module{"stolen": {Path: "../outside/victim.lua"}}})
	writeFile(t, filepath.Join(l.outside, "victim.lua"), "return 'secret'")

	_, err := l.tree.Deploy(spec, l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.tree.DeployLuaDir(), "stolen.lua"))
}

// TestDeploy_CopyDirectoriesStayInTree: a copy_directories entry is joined
// under both the sources and the install dir, so a run of ".." read a
// directory beside the sources and wrote it beside the tree.
func TestDeploy_CopyDirectoriesStayInTree(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	// The sources sit seven levels below the temp dir, the install dir six, so
	// six ".." read <parent>/s/loot and write <parent>/loot.
	src := filepath.Join(l.parent, "s", "s", "s", "s", "s", "s", "src")
	writeFile(t, filepath.Join(src, "demo.lua"), "return 1")
	writeFile(t, filepath.Join(l.parent, "s", "loot", "secret"), "secret")

	spec := l.spec(rocks.Build{Type: "none", CopyDirectories: []string{"../../../../../../loot"}})

	_, err := l.tree.Deploy(spec, src, src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoDirExists(t, filepath.Join(l.parent, "loot"))
}

// TestDeploy_CopyDirectoriesSymlinkOut: a copy_directories entry inside the
// sources holding a symlink to a file outside them copied that file into the
// tree. A link that stays inside the sources is still followed.
func TestDeploy_CopyDirectoriesSymlinkOut(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	writeFile(t, filepath.Join(l.src, "doc", "README"), "readme")
	require.NoError(t, os.Symlink("README", filepath.Join(l.src, "doc", "inside")))
	require.NoError(t, os.Symlink(filepath.Join(l.outside, "victim"), filepath.Join(l.src, "doc", "leak")))

	_, err := l.tree.Deploy(l.spec(rocks.Build{Type: "none", CopyDirectories: []string{"doc"}}), l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.tree.InstallDir(demoPkg, containVer), "doc", "leak"))

	require.NoError(t, os.Remove(filepath.Join(l.src, "doc", "leak")))

	_, err = l.tree.Deploy(l.spec(rocks.Build{Type: "none", CopyDirectories: []string{"doc"}}), l.src, l.src)
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(l.tree.InstallDir(demoPkg, containVer), "doc", "inside"))
}

// TestDeploy_DefaultDocsFollowLinksInside: vshard's sources hold GEMINI.md as
// a symlink to AGENTS.md, and the default docs copied regular files only, so
// the rock lacked GEMINI.md where upstream copies it. A link that stays
// inside the sources is copied as the file it points to; one to a directory
// or to nothing is skipped; one leading out of the sources fails.
func TestDeploy_DefaultDocsFollowLinksInside(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)

	writeFile(t, filepath.Join(l.src, "AGENTS.md"), "agents")
	writeFile(t, filepath.Join(l.src, "docs.md", "x"), "a directory named like a doc")
	require.NoError(t, os.Symlink("AGENTS.md", filepath.Join(l.src, "GEMINI.md")))
	require.NoError(t, os.Symlink(filepath.Join(l.src, "AGENTS.md"), filepath.Join(l.src, "CLAUDE.md")))
	require.NoError(t, os.Symlink("docs.md", filepath.Join(l.src, "dir.md")))
	require.NoError(t, os.Symlink("no-such-file", filepath.Join(l.src, "dangling.md")))

	spec := l.spec(rocks.Build{Type: "none"})

	_, err := l.tree.Deploy(spec, l.src, l.src)
	require.NoError(t, err)

	docDir := l.tree.DocDir(demoPkg, containVer)
	for _, name := range []string{"AGENTS.md", "GEMINI.md", "CLAUDE.md"} {
		got, err := os.ReadFile(filepath.Join(docDir, name)) //nolint:gosec // test temp path
		require.NoError(t, err, "%s must be copied", name)
		assert.Equal(t, "agents", string(got), name)

		fi, err := os.Lstat(filepath.Join(docDir, name))
		require.NoError(t, err)
		assert.True(t, fi.Mode().IsRegular(), "%s is copied as a file", name)
	}

	assert.NoFileExists(t, filepath.Join(docDir, "dir.md"))
	assert.NoFileExists(t, filepath.Join(docDir, "dangling.md"))

	l = newContainLayout(t)
	require.NoError(t, os.Symlink(filepath.Join(l.outside, "victim"), filepath.Join(l.src, "README")))

	_, err = l.tree.Deploy(l.spec(rocks.Build{Type: "none"}), l.src, l.src)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.tree.DocDir(demoPkg, containVer), "README"))
	l.assertOutsideIntact(t)
}

// TestDeploy_RejectsNameEscapingRocksDir: Deploy builds the install dir from
// the rock's name and version, so a hand-built spec named ".." wrote above
// the rocks dir: its default docs went to <install-dir>/doc.
func TestDeploy_RejectsNameEscapingRocksDir(t *testing.T) {
	t.Parallel()

	for _, nv := range [][2]string{{"..", containVer}, {demoPkg, ".."}, {"a/b", containVer}, {demoPkg, "../../../../../outside"}} {
		l := newContainLayout(t)
		writeFile(t, filepath.Join(l.src, "README"), "readme")

		spec := &rocks.Rockspec{Package: nv[0], Version: nv[1], Build: rocks.Build{Type: "none"}}

		_, err := l.tree.Deploy(spec, l.src, l.src)
		require.ErrorIs(t, err, rocks.ErrUnsafePath, "%v", nv)
		l.assertOutsideIntact(t)
	}
}

// writeRockManifest plants a rock_manifest for demo/1.0-1 that lists file in
// section, as a tree written by another tool, or tampered with, can hold.
func writeRockManifest(t *testing.T, tr *tree.Tree, section, file string) {
	t.Helper()

	body := "rock_manifest = {\n  " + section + " = {\n    [\"" + file + "\"] = \"0123\"\n  }\n}\n"
	writeFile(t, filepath.Join(tr.InstallDir(demoPkg, containVer), "rock_manifest"), body)
}

// TestDeleteVersion_PlantedRockManifestStaysInTree: DeleteVersion removes the
// files the rock_manifest lists, joined under their deploy dir, so a planted
// entry deleted any file the user can delete.
func TestDeleteVersion_PlantedRockManifestStaysInTree(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ section, file string }{
		{"lua", "../../../outside/victim"}, // <tree>/share/tarantool
		{"lib", "../../../outside/victim"}, // <tree>/lib/tarantool
		{"bin", "../../outside/victim"},    // <tree>/bin
	} {
		l := newContainLayout(t)
		writeRockManifest(t, l.tree, tc.section, tc.file)

		err := l.tree.DeleteVersion(demoPkg, containVer)
		require.ErrorIs(t, err, rocks.ErrUnsafePath, "%s %s", tc.section, tc.file)
		l.assertOutsideIntact(t)
	}
}

// TestDeleteVersion_PlantedNextProviderStaysInTree: promoting the next
// provider renames its versioned file onto the plain path of the removed
// one, both built from the rock_manifest entry.
func TestDeleteVersion_PlantedNextProviderStaysInTree(t *testing.T) {
	t.Parallel()

	l := newContainLayout(t)
	writeRockManifest(t, l.tree, "bin", "../../outside/victim")

	l.tree.Providers = func(tree.ItemKind, string) []string {
		return []string{demoPkg + "/" + containVer, "other/2.0-1"}
	}

	err := l.tree.DeleteVersion(demoPkg, containVer)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	l.assertOutsideIntact(t)
}

// TestDeleteVersion_RejectsNameEscapingRocksDir: the install dir DeleteVersion
// removes whole is <rocks-dir>/<name>/<version>, and both come from the tree
// manifest, so "../../../.." and "outside" removed the outside dir.
func TestDeleteVersion_RejectsNameEscapingRocksDir(t *testing.T) {
	t.Parallel()

	for _, nv := range [][2]string{{"../../../..", "outside"}, {"..", ".."}, {demoPkg, "."}, {"", containVer}} {
		l := newContainLayout(t)

		err := l.tree.DeleteVersion(nv[0], nv[1])
		require.ErrorIs(t, err, rocks.ErrUnsafePath, "%v", nv)
		l.assertOutsideIntact(t)
		assert.DirExists(t, l.tree.RocksDir())
	}
}
