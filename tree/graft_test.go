package tree_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/tree"
)

// viewCfg is the Config of the native client on the tree root, as the
// fixtures are built with.
func viewCfg(root string) rocks.Config {
	return rocks.Config{Tree: root, Tarantool: viewTarantool}
}

// openWrapped opens the tree at root with the command wrapper inputs the
// native client deploys with.
func openWrapped(t *testing.T, root string) *tree.Tree {
	t.Helper()

	tr, err := tree.Open(viewCfg(root))
	require.NoError(t, err, "Open")

	tr.BinWrap = client.BinWrapFor(viewCfg(root))

	return tr
}

// packRock returns the binary rock of d the native client packs from a tree
// holding d alone.
func packRock(t *testing.T, d rockDef) string {
	t.Helper()

	root := buildTree(t, d)
	work := t.TempDir()

	cfg := viewCfg(root)
	cfg.WorkingDir = work

	c, err := client.New(cfg)
	require.NoError(t, err, "New")

	archive, err := c.Pack(context.Background(), d.name, client.PackOpts{})
	require.NoError(t, err, "Pack %s", d.ref())

	return archive
}

// serveRocks returns a directory rock server holding the archives.
func serveRocks(t *testing.T, archives ...string) string {
	t.Helper()

	repo := t.TempDir()
	for _, a := range archives {
		require.NoError(t, os.Rename(a, filepath.Join(repo, filepath.Base(a))))
	}

	require.NoError(t, remote.MakeManifest(context.Background(), repo, remote.MakeManifestOpts{}), "MakeManifest")

	return repo
}

// installRocks installs the rocks from the server repo into the tree at root
// with the native client, each alone and in the order given.
func installRocks(t *testing.T, root, repo string, refs ...tree.RockRef) {
	t.Helper()

	cfg := viewCfg(root)
	cfg.WorkingDir = t.TempDir()
	cfg.Servers = []string{repo}

	c, err := client.New(cfg)
	require.NoError(t, err, "New")

	for _, r := range refs {
		require.NoError(t, c.Install(context.Background(), r.Name, client.InstallOpts{Version: r.Version, Deps: client.DepsNone}),
			"Install %s", r)
	}
}

// graft grafts src into the tree at root and returns the report.
func graft(t *testing.T, root, src string, opts tree.GraftOpts) (*tree.GraftReport, error) {
	t.Helper()

	var report *tree.GraftReport

	err := openWrapped(t, root).Update(context.Background(), func(tx *tree.Tx) error {
		var err error

		report, err = tx.Graft(src, opts)

		return err
	})

	return report, err
}

// TestGraft_AsNativeInstall: grafting a tree whose rocks share a module and
// a command, so that some of its files are at their versioned paths, into an
// empty tree leaves the tree the native client leaves when it installs the
// binary rocks of the same rocks, file for file and manifest for manifest.
func TestGraft_AsNativeInstall(t *testing.T) {
	t.Parallel()

	defs := []rockDef{alpha1, alpha2, beta1}
	src := buildTree(t, defs...)

	archives := make([]string, 0, len(defs))
	refs := make([]tree.RockRef, 0, len(defs))

	for _, d := range defs {
		archives = append(archives, packRock(t, d))
		refs = append(refs, d.ref())
	}

	installed := t.TempDir()
	installRocks(t, installed, serveRocks(t, archives...), refs...)

	grafted := t.TempDir()
	report, err := graft(t, grafted, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft")

	assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref(), beta1.ref()}, report.Grafted, "grafted")
	assert.Empty(t, report.Skipped, "skipped")
	assert.Empty(t, report.Untracked, "untracked")

	assert.Equal(t, treeState(t, installed), treeState(t, grafted), "the grafted tree vs the installed one")
	requireConsistent(t, grafted, defs...)

	// The source keeps its files where its own manifest put them, which the
	// grafted tree has too: alpha 1 at its versioned paths.
	f := fileOf(t, inspect(t, src), alpha1.ref(), tree.SectionLua, "shared.lua")
	assert.False(t, f.Plain, "alpha 1's shared in the source")
}

// TestGraft_Unsafe: a source whose deployed file is a symlink leading out of
// it, whose rock dir is one, or whose rock_manifest names a path above its
// directory fails Graft with ErrUnsafePath and leaves the destination as it
// was.
func TestGraft_Unsafe(t *testing.T) {
	t.Parallel()

	cases := map[string]func(t *testing.T, src, outside string){
		"symlink out": func(t *testing.T, src, outside string) {
			t.Helper()

			p := filepath.Join(tree.Paths{Tree: src}.DeployLuaDir(), "alpha.lua")
			writeFixture(t, filepath.Join(outside, "alpha.lua"), readFile(t, p))
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(filepath.Join(outside, "alpha.lua"), p))
		},
		"symlinked rock dir": func(t *testing.T, src, outside string) {
			t.Helper()

			dir := tree.Paths{Tree: src}.InstallDir("alpha", "1.0-1")
			moved := filepath.Join(outside, "alpha-1.0-1")
			require.NoError(t, os.Rename(dir, moved))
			require.NoError(t, os.Symlink(moved, dir))
		},
		"entry above its dir": func(t *testing.T, src, _ string) {
			t.Helper()

			p := filepath.Join(tree.Paths{Tree: src}.InstallDir("alpha", "1.0-1"), "rock_manifest")
			writeFixture(t, p, "rock_manifest = {\n   lua = {\n      [\"../../../../../x.lua\"] = \"0\",\n   },\n}\n")
		},
	}

	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			outside := t.TempDir()
			src := buildTree(t, alpha1)
			damage(t, src, outside)

			before := treeState(t, outside)

			dst := buildTree(t, beta1)
			state := treeState(t, dst)

			_, err := graft(t, dst, src, tree.GraftOpts{})
			require.ErrorIs(t, err, rocks.ErrUnsafePath, "Graft")
			assert.Equal(t, state, treeState(t, dst), "the destination")
			assert.Equal(t, before, treeState(t, outside), "outside")
		})
	}
}

// TestGraft_Integrity: a source file that is not what its rock_manifest
// records, at either of its paths, fails Graft with ErrIntegrity and leaves
// the destination as it was; one that is at the other path than the source's
// manifest says is found there.
func TestGraft_Integrity(t *testing.T) {
	t.Parallel()

	for name, damage := range map[string]func(t *testing.T, src string){
		"modified module": func(t *testing.T, src string) {
			t.Helper()
			writeFixture(t, filepath.Join(tree.Paths{Tree: src}.DeployLuaDir(), "alpha.lua"), "return 'evil'\n")
		},
		"missing script": func(t *testing.T, src string) {
			t.Helper()
			require.NoError(t, os.Remove(filepath.Join(tree.Paths{Tree: src}.InstallDir("alpha", "1.0-1"), "bin", "tool")))
		},
		"modified rockspec": func(t *testing.T, src string) {
			t.Helper()

			p := filepath.Join(tree.Paths{Tree: src}.InstallDir("alpha", "1.0-1"), "alpha-1.0-1.rockspec")
			writeFixture(t, p, readFile(t, p)+"-- edited\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			src := buildTree(t, alpha1)
			damage(t, src)

			dst := buildTree(t, beta1)
			before := treeState(t, dst)

			_, err := graft(t, dst, src, tree.GraftOpts{})
			require.ErrorIs(t, err, tree.ErrIntegrity, "Graft")
			assert.Equal(t, before, treeState(t, dst), "the destination")
		})
	}

	// alpha 2's module moved to the versioned path, alpha 1's to the plain:
	// as if the source's manifest said alpha 1 holds it.
	src := buildTree(t, alpha1, alpha2)
	lua := tree.Paths{Tree: src}.DeployLuaDir()
	plain := filepath.Join(lua, "shared.lua")
	v1 := tree.MungedPath(lua, plain, "alpha", "1.0-1")
	v2 := tree.MungedPath(lua, plain, "alpha", "2.0-1")
	require.NoError(t, os.Rename(plain, v2))
	require.NoError(t, os.Rename(v1, plain))

	dst := t.TempDir()
	_, err := graft(t, dst, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft of swapped spots")
	requireConsistent(t, dst, alpha1, alpha2)
	assert.Equal(t, alpha2.modules["shared"], readFile(t, filepath.Join(tree.Paths{Tree: dst}.DeployLuaDir(), "shared.lua")),
		"the newest version holds the plain path here")
}

// TestGraft_Installed: a version installed with the same rock_manifest is
// skipped; one with another fails Graft with ErrConflict, the destination as
// it was, unless Replace, which installs the source's.
func TestGraft_Installed(t *testing.T) {
	t.Parallel()

	src := buildTree(t, alpha1, beta1)

	same := buildTree(t, alpha1)
	report, err := graft(t, same, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft")
	assert.Equal(t, []tree.RockRef{alpha1.ref()}, report.Skipped, "skipped")
	assert.Equal(t, []tree.RockRef{beta1.ref()}, report.Grafted, "grafted")
	requireConsistent(t, same, alpha1, beta1)

	report, err = graft(t, same, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft again")
	assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, report.Skipped, "skipped the second time")
	assert.Empty(t, report.Grafted)

	other := alpha1
	other.modules = map[string]string{"shared": "return 'another alpha 1'\n", "alpha": "return 'alpha 1 own'\n"}

	differ := buildTree(t, other)
	before := treeState(t, differ)

	_, err = graft(t, differ, src, tree.GraftOpts{})
	require.ErrorIs(t, err, tree.ErrConflict, "Graft over another alpha 1")
	require.ErrorContains(t, err, "alpha/1.0-1")
	assert.Equal(t, before, treeState(t, differ), "the destination")

	report, err = graft(t, differ, src, tree.GraftOpts{Replace: true})
	require.NoError(t, err, "Graft with Replace")
	assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, report.Grafted, "grafted")
	requireConsistent(t, differ, alpha1, beta1)

	// The alpha 1 made into same keeps the staging dir of its build, which a
	// graft, as a pack, leaves out.
	want := treeState(t, same)
	maps.DeleteFunc(want, func(rel, _ string) bool { return strings.Contains(rel, "/build/") })
	assert.Equal(t, want, treeState(t, differ), "the replaced tree vs the one grafted into")
}

// TestGraft_SelectAndVersions: Select leaves rocks out, a rock dir without a
// rock_manifest among them; RemoveOtherVersions removes the versions of a
// grafted rock the source does not have.
func TestGraft_SelectAndVersions(t *testing.T) {
	t.Parallel()

	src := buildTree(t, alpha2, beta1)
	writeFixture(t, filepath.Join(tree.Paths{Tree: src}.InstallDir("ghost", "1.0-1"), "leftover"), "x")

	dst := buildTree(t, alpha1)

	_, err := graft(t, dst, src, tree.GraftOpts{})
	require.ErrorIs(t, err, tree.ErrCorrupt, "a rock dir without a rock_manifest")

	alphaOnly := func(r tree.RockRef) bool { return r.Name == "alpha" }

	report, err := graft(t, dst, src, tree.GraftOpts{Select: alphaOnly, RemoveOtherVersions: true})
	require.NoError(t, err, "Graft")
	assert.Equal(t, []tree.RockRef{alpha2.ref()}, report.Grafted)
	assert.Equal(t, []tree.RockRef{alpha2.ref()}, inspect(t, dst).Rocks(), "alpha 1 removed, beta left out")
	requireConsistent(t, dst, alpha2)

	kept := buildTree(t, alpha1)
	_, err = graft(t, kept, src, tree.GraftOpts{Select: alphaOnly})
	require.NoError(t, err, "Graft")
	assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref()}, inspect(t, kept).Rocks(), "alpha 1 kept")
	requireConsistent(t, kept, alpha1, alpha2)
}

// TestGraft_Untracked: the files of the source's deploy dirs no rock of it
// lists are reported and not copied; its own tree manifest is not read.
func TestGraft_Untracked(t *testing.T) {
	t.Parallel()

	src := buildTree(t, alpha1)
	p := tree.Paths{Tree: src}

	stray := []string{
		filepath.Join(p.DeployLuaDir(), "app", "init.lua"),
		filepath.Join(p.DeployLibDir(), "app", "native.so"),
		filepath.Join(p.BinDir(), "app-tool"),
		filepath.Join(p.DeployLuaDir(), "alpha.lua~"),
	}
	for _, s := range stray {
		writeFixture(t, s, "stray\n")
	}

	writeFixture(t, filepath.Join(src, ".meta", "notes"), "a tool's own\n")
	writeFixture(t, filepath.Join(p.RocksDir(), "manifest"), "this does not parse")

	dst := t.TempDir()
	report, err := graft(t, dst, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft")

	slices.Sort(stray)
	assert.Equal(t, stray, report.Untracked, "untracked")
	assert.Equal(t, []tree.RockRef{alpha1.ref()}, report.Grafted)
	requireConsistent(t, dst, alpha1)

	for _, s := range stray {
		rel, err := filepath.Rel(src, s)
		require.NoError(t, err)
		assert.NoFileExists(t, filepath.Join(dst, rel), "%s is not copied", rel)
	}

	_, err = graft(t, dst, dst, tree.GraftOpts{})
	require.Error(t, err, "Graft of a tree into itself")
}
