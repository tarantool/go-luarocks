package tree_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
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
	"github.com/tarantool/go-luarocks/tree"
)

// The tests here build their trees with the native client's Make, from
// pure-Lua builtin rocks, so nothing is compiled or fetched and the
// Tarantool prefix need not exist.

// viewTarantool is the Tarantool configuration the fixture rocks build with.
var viewTarantool = rocks.TarantoolConfig{Prefix: "/opt/test", IncludeDir: "/opt/test/include", Version: "2.11.0"}

// rockDef is a pure-Lua rock: a module per entry of modules (module name →
// source) and a command per entry of bins (command name → Lua source).
type rockDef struct {
	name, version string
	modules       map[string]string
	bins          map[string]string
	deps          []string
}

// The fixture: two versions of alpha and a beta, all three providing the
// module shared and the command tool, beta depending on alpha.
var (
	alpha1 = rockDef{
		name: "alpha", version: "1.0-1",
		modules: map[string]string{"shared": "return 'alpha 1'\n", "alpha": "return 'alpha 1 own'\n"},
		bins:    map[string]string{"tool": "print('alpha 1')\n"},
	}
	alpha2 = rockDef{
		name: "alpha", version: "2.0-1",
		modules: map[string]string{"shared": "return 'alpha 2'\n", "alpha": "return 'alpha 2 own'\n"},
		bins:    map[string]string{"tool": "print('alpha 2')\n"},
	}
	beta1 = rockDef{
		name: "beta", version: "1.0-1",
		modules: map[string]string{"shared": "return 'beta 1'\n", "beta": "return 'beta 1 own'\n"},
		bins:    map[string]string{"tool": "print('beta 1')\n"},
		deps:    []string{"alpha >= 1.0"},
	}
)

// ref is the RockRef of d.
func (d rockDef) ref() tree.RockRef {
	return tree.RockRef{Name: d.name, Version: d.version}
}

// makeRock builds and deploys d into the tree at root with the native
// client's Make.
func makeRock(t *testing.T, root string, d rockDef) {
	t.Helper()

	src := t.TempDir()

	mods := make([]string, 0, len(d.modules))
	bins := make([]string, 0, len(d.bins))

	for _, mod := range slices.Sorted(maps.Keys(d.modules)) {
		writeFixture(t, filepath.Join(src, mod+".lua"), d.modules[mod])
		mods = append(mods, fmt.Sprintf("[%q] = %q", mod, mod+".lua"))
	}

	for _, bin := range slices.Sorted(maps.Keys(d.bins)) {
		writeFixture(t, filepath.Join(src, "bin", bin), d.bins[bin])
		bins = append(bins, fmt.Sprintf("[%q] = %q", bin, "bin/"+bin))
	}

	depList := make([]string, 0, len(d.deps))
	for _, dep := range d.deps {
		depList = append(depList, fmt.Sprintf("%q", dep))
	}

	spec := fmt.Sprintf(`package = %q
version = %q
source = { url = "file://localhost/%s.tar.gz" }
dependencies = { %s }
build = {
   type = "builtin",
   modules = { %s },
   install = { bin = { %s } },
}
`, d.name, d.version, d.name, strings.Join(depList, ", "), strings.Join(mods, ", "), strings.Join(bins, ", "))

	specPath := filepath.Join(src, d.name+"-"+d.version+".rockspec")
	writeFixture(t, specPath, spec)

	r, err := client.New(rocks.Config{Tree: root, WorkingDir: src, Tarantool: viewTarantool})
	require.NoError(t, err, "New")
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}), "Make %s", d.ref())
}

// writeFixture writes body to p, creating its directory.
func writeFixture(t *testing.T, p, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
}

// buildTree returns a fresh tree with the rocks made into it in order.
func buildTree(t *testing.T, defs ...rockDef) string {
	t.Helper()

	root := t.TempDir()
	for _, d := range defs {
		makeRock(t, root, d)
	}

	return root
}

// inspect returns the View of the tree at root.
func inspect(t *testing.T, root string) *tree.View {
	t.Helper()

	v, err := tree.Inspect(context.Background(), root)
	require.NoError(t, err, "Inspect %s", root)

	return v
}

// nativeRemove removes r from the tree at root with the native client's
// Remove, which removes what it is asked to whatever depends on it, as
// upstream does when forced.
func nativeRemove(t *testing.T, root string, r tree.RockRef) {
	t.Helper()

	c, err := client.New(rocks.Config{Tree: root, Tarantool: viewTarantool})
	require.NoError(t, err, "New")
	require.NoError(t, c.Remove(context.Background(), r.Name, client.RemoveOpts{Version: r.Version, ForceFast: true}),
		"Remove %s", r)
}

// update runs fn on the tree at root through Tree.Update.
func update(t *testing.T, root string, fn func(tx *tree.Tx) error) error {
	t.Helper()

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err, "Open")

	return tr.Update(context.Background(), fn)
}

// readFile returns the contents of p.
func readFile(t *testing.T, p string) string {
	t.Helper()

	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(t, err, "read %s", p)

	return string(b)
}

// requireConsistent checks every rock of the View of root against its tree:
// Verify finds nothing, every deployed file is owned by its rock where Files
// says it is, and the file there is the rock's own.
func requireConsistent(t *testing.T, root string, defs ...rockDef) {
	t.Helper()

	v := inspect(t, root)

	byRef := map[tree.RockRef]rockDef{}
	for _, d := range defs {
		byRef[d.ref()] = d
	}

	for _, ref := range v.Rocks() {
		problems, err := v.Verify(ref)
		require.NoError(t, err, "Verify %s", ref)
		require.Empty(t, problems, "Verify %s", ref)

		files, err := v.Files(ref)
		require.NoError(t, err, "Files %s", ref)

		d, known := byRef[ref]

		for _, f := range files {
			switch f.Section {
			case tree.SectionLua, tree.SectionLib, tree.SectionBin:
			case tree.SectionRockspec, tree.SectionConf, tree.SectionDoc, tree.SectionOther:
				continue
			}

			owner, kind, ok := v.Owner(f.Path)
			require.True(t, ok, "%s has an owner", f.Path)
			assert.Equal(t, ref, owner, "owner of %s", f.Path)

			wantKind := tree.ModuleItem
			if f.Section == tree.SectionBin {
				wantKind = tree.CommandItem
			}

			assert.Equal(t, wantKind, kind, "kind of %s", f.Path)

			if !known {
				continue
			}

			if f.Section == tree.SectionBin {
				assert.Equal(t, d.bins[f.Entry], readFile(t, f.Script), "the real script of %s %s", ref, f.Entry)
			} else {
				assert.Equal(t, d.modules[strings.TrimSuffix(f.Entry, ".lua")], readFile(t, f.Path), "%s %s at %s", ref, f.Entry, f.Path)
			}
		}
	}
}

// fileOf returns the file of ref's Files with the section and entry.
func fileOf(t *testing.T, v *tree.View, ref tree.RockRef, section tree.Section, entry string) tree.File {
	t.Helper()

	files, err := v.Files(ref)
	require.NoError(t, err, "Files %s", ref)

	for _, f := range files {
		if f.Section == section && f.Entry == entry {
			return f
		}
	}

	require.Failf(t, "no such file", "%s has no %s %s in %v", ref, section, entry, files)

	return tree.File{}
}

// TestView_SharedItems follows the module and the command that alpha and
// beta both provide through an upgrade, a downgrade and removals: which rock
// holds the plain path, where each one's files are, who owns each file and
// which script the command runs, each time as the files on disk say.
func TestView_SharedItems(t *testing.T) {
	t.Parallel()

	root := buildTree(t, beta1, alpha1, alpha2)
	paths := tree.Paths{Tree: root}
	plainShared := filepath.Join(paths.DeployLuaDir(), "shared.lua")
	plainTool := filepath.Join(paths.BinDir(), "tool")

	v := inspect(t, root)
	assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref(), beta1.ref()}, v.Rocks(), "Rocks")
	assert.Equal(t, []string{"2.0-1", "1.0-1"}, v.Versions("alpha"), "Versions")
	assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref(), beta1.ref()}, v.Providers(tree.ModuleItem, "shared"), "Providers of shared")
	assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref(), beta1.ref()}, v.Providers(tree.CommandItem, "tool"), "Providers of tool")
	assert.Empty(t, v.Providers(tree.CommandItem, "shared"), "a module is no command")

	upgraded := fileOf(t, v, alpha2.ref(), tree.SectionLua, "shared.lua")
	assert.True(t, upgraded.Plain, "the upgrade holds the plain path")
	assert.Equal(t, plainShared, upgraded.Path)

	for _, d := range []rockDef{alpha1, beta1} {
		f := fileOf(t, v, d.ref(), tree.SectionLua, "shared.lua")
		assert.False(t, f.Plain, "%s holds a versioned path", d.ref())
		assert.Equal(t, tree.MungedPath(paths.DeployLuaDir(), plainShared, d.name, d.version), f.Path, "%s's shared.lua", d.ref())

		tool := fileOf(t, v, d.ref(), tree.SectionBin, "tool")
		assert.Equal(t, tree.MungedPath(paths.BinDir(), plainTool, d.name, d.version), tool.Path, "%s's tool", d.ref())
		assert.Equal(t, filepath.Join(paths.InstallDir(d.name, d.version), "bin", "tool"), tool.Script, "%s's tool script", d.ref())
	}

	script, entry, ok := v.Command("tool")
	require.True(t, ok, "Command tool")
	assert.Equal(t, filepath.Join(paths.InstallDir("alpha", "2.0-1"), "bin", "tool"), script, "the script tool runs")
	assert.Equal(t, plainTool, entry, "the entry of tool")

	_, _, ok = v.Command("absent")
	assert.False(t, ok, "Command of nothing")

	owner, kind, ok := v.Owner("share/tarantool/shared.lua")
	require.True(t, ok, "a tree-relative path has an owner")
	assert.Equal(t, alpha2.ref(), owner)
	assert.Equal(t, tree.ModuleItem, kind)

	_, _, ok = v.Owner("share/tarantool/nothing.lua")
	assert.False(t, ok, "a file no rock lists")

	_, _, ok = v.Owner("../outside")
	assert.False(t, ok, "a path outside the tree")

	requireConsistent(t, root, alpha1, alpha2, beta1)

	// Downgrade: the older alpha takes the plain paths back.
	nativeRemove(t, root, alpha2.ref())

	v = inspect(t, root)
	assert.True(t, fileOf(t, v, alpha1.ref(), tree.SectionLua, "shared.lua").Plain, "after the downgrade")
	script, _, ok = v.Command("tool")
	require.True(t, ok)
	assert.Equal(t, filepath.Join(paths.InstallDir("alpha", "1.0-1"), "bin", "tool"), script, "after the downgrade")
	requireConsistent(t, root, alpha1, beta1)

	// The last alpha goes: beta takes them over.
	nativeRemove(t, root, alpha1.ref())

	v = inspect(t, root)
	beta := fileOf(t, v, beta1.ref(), tree.SectionLua, "shared.lua")
	assert.True(t, beta.Plain, "beta holds shared once alpha is gone")
	assert.Equal(t, plainShared, beta.Path)
	assert.Equal(t, []tree.RockRef{beta1.ref()}, v.Providers(tree.ModuleItem, "shared"))
	requireConsistent(t, root, beta1)

	// Upgrade over beta: alpha sorts first and takes the plain paths.
	makeRock(t, root, alpha2)

	v = inspect(t, root)
	assert.True(t, fileOf(t, v, alpha2.ref(), tree.SectionLua, "shared.lua").Plain, "alpha over beta")
	assert.False(t, fileOf(t, v, beta1.ref(), tree.SectionLua, "shared.lua").Plain, "beta under alpha")
	requireConsistent(t, root, alpha2, beta1)
}

// TestView_Installed: Installed takes any spelling of a version that compares
// equal, answers with the manifest's, and wants the install dir there.
func TestView_Installed(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, alpha2)
	v := inspect(t, root)

	got, ok := v.Installed("alpha", "2.0-1")
	require.True(t, ok)
	assert.Equal(t, alpha2.ref(), got)

	got, ok = v.Installed("alpha", "2.0.0-1")
	require.True(t, ok, "a version that compares equal")
	assert.Equal(t, alpha2.ref(), got, "the manifest's spelling")

	_, ok = v.Installed("alpha", "3.0-1")
	assert.False(t, ok, "a version not installed")

	_, ok = v.Installed("beta", "1.0-1")
	assert.False(t, ok, "a rock not installed")

	require.NoError(t, os.RemoveAll(tree.Paths{Tree: root}.InstallDir("alpha", "1.0-1")))

	_, ok = v.Installed("alpha", "1.0-1")
	assert.False(t, ok, "the manifest lists it, but its install dir is gone")
}

// TestView_Dependencies: the declared dependencies, the resolved ones the
// manifest records at deploy time, and the rocks that depend on a version.
func TestView_Dependencies(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, alpha2, beta1)
	v := inspect(t, root)

	declared := v.Dependencies(beta1.ref())
	require.Len(t, declared, 1)
	assert.Equal(t, "alpha", declared[0].Name)

	assert.Equal(t, map[string]string{"alpha": "2.0-1"}, v.Requires(beta1.ref()), "the newest alpha beta accepts")
	assert.Empty(t, v.Requires(alpha1.ref()), "alpha requires nothing")
	assert.Nil(t, v.Requires(tree.RockRef{Name: "nothing", Version: "1.0-1"}), "a rock not installed")

	assert.Equal(t, []tree.RockRef{beta1.ref()}, v.Dependents(alpha1.ref()), "beta accepts alpha 1")
	assert.Equal(t, []tree.RockRef{beta1.ref()}, v.Dependents(alpha2.ref()), "and alpha 2")
	assert.Empty(t, v.Dependents(beta1.ref()), "nothing depends on beta")

	// Deployed before any alpha, beta resolved none.
	early := inspect(t, buildTree(t, beta1, alpha1))
	assert.Empty(t, early.Requires(beta1.ref()), "resolved when beta was deployed, before alpha")
}

// TestView_CheckPlain: a rock whose files another rock holds the plain paths
// of is not plain, and the error names the holder.
func TestView_CheckPlain(t *testing.T) {
	t.Parallel()

	v := inspect(t, buildTree(t, alpha2, beta1))

	require.NoError(t, v.CheckPlain(alpha2.ref()), "alpha holds every plain path")

	err := v.CheckPlain(beta1.ref())
	require.ErrorIs(t, err, tree.ErrNotPlain)
	assert.Contains(t, err.Error(), "lua/shared.lua (held by alpha/2.0-1)")
	assert.Contains(t, err.Error(), "bin/tool (held by alpha/2.0-1)")
	assert.NotContains(t, err.Error(), "beta.lua", "beta's own module is plain")

	require.ErrorIs(t, v.CheckPlain(tree.RockRef{Name: "nothing", Version: "1"}), tree.ErrNotInstalled)
}

// TestView_Verify: Verify finds a file gone from its spot, a file changed, a
// command whose real script changed, and a spot holding a symlink.
func TestView_Verify(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha2, beta1)
	v := inspect(t, root)

	shared := fileOf(t, v, alpha2.ref(), tree.SectionLua, "shared.lua")
	own := fileOf(t, v, alpha2.ref(), tree.SectionLua, "alpha.lua")
	tool := fileOf(t, v, alpha2.ref(), tree.SectionBin, "tool")
	versioned := fileOf(t, v, beta1.ref(), tree.SectionLua, "shared.lua")

	require.NoError(t, os.Remove(shared.Path))
	require.NoError(t, os.WriteFile(versioned.Path, []byte("return 'changed'\n"), 0o600))
	require.NoError(t, os.WriteFile(tool.Script, []byte("print('changed')\n"), 0o600))
	require.NoError(t, os.Remove(own.Path))
	require.NoError(t, os.Symlink(versioned.Path, own.Path))

	problems, err := v.Verify(alpha2.ref())
	require.ErrorIs(t, err, tree.ErrIntegrity)

	got := map[string]tree.ProblemKind{}
	for _, p := range problems {
		got[p.Path] = p.Kind
	}

	assert.Equal(t, map[string]tree.ProblemKind{
		own.Path:    tree.ProblemNotRegular,
		shared.Path: tree.ProblemMissing,
		tool.Script: tree.ProblemModified,
	}, got, "alpha's problems")

	problems, err = v.Verify(beta1.ref())
	require.ErrorIs(t, err, tree.ErrIntegrity)
	require.Len(t, problems, 1, "beta's problems: %v", problems)
	assert.Equal(t, tree.ProblemModified, problems[0].Kind)
	assert.Equal(t, versioned.Path, problems[0].Path)

	sum := md5.Sum([]byte("return 'changed'\n"))
	assert.Equal(t, hex.EncodeToString(sum[:]), problems[0].MD5, "the digest found")
	assert.Equal(t, versioned.MD5, problems[0].File.MD5, "the digest recorded")

	_, err = v.Verify(tree.RockRef{Name: "nothing", Version: "1"})
	require.ErrorIs(t, err, tree.ErrNotInstalled)
}

// TestInspect_EmptyAndBrokenTrees: Inspect reads a tree that is not there,
// or has no manifest, as empty and creates nothing; a manifest that does not
// parse is ErrCorrupt, and a rock_manifest that is gone fails the rock only.
func TestInspect_EmptyAndBrokenTrees(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing")
	v := inspect(t, missing)
	assert.Empty(t, v.Rocks(), "a missing tree")
	assert.NoDirExists(t, missing, "Inspect created nothing")

	empty := t.TempDir()
	assert.Empty(t, inspect(t, empty).Rocks(), "an empty tree")

	entries, err := os.ReadDir(empty)
	require.NoError(t, err)
	assert.Empty(t, entries, "Inspect created nothing")

	noManifest := t.TempDir()
	rocksDir := tree.Paths{Tree: noManifest}.RocksDir()
	require.NoError(t, os.MkdirAll(rocksDir, 0o750))
	assert.Empty(t, inspect(t, noManifest).Rocks(), "a tree without a manifest")

	entries, err = os.ReadDir(rocksDir)
	require.NoError(t, err)
	assert.Empty(t, entries, "Inspect created no lock file")

	corrupt := t.TempDir()
	writeFixture(t, filepath.Join(tree.Paths{Tree: corrupt}.RocksDir(), "manifest"), "repository = {\n")
	_, err = tree.Inspect(context.Background(), corrupt)
	require.ErrorIs(t, err, tree.ErrCorrupt, "a manifest that does not parse")

	tr, err := tree.Open(rocks.Config{Tree: corrupt})
	require.NoError(t, err)
	_, err = tr.View(context.Background())
	require.ErrorIs(t, err, tree.ErrCorrupt, "Tree.View of it")
	require.ErrorIs(t, tr.Update(context.Background(), func(*tree.Tx) error { return nil }), tree.ErrCorrupt,
		"Update of it")

	broken := buildTree(t, alpha1, beta1)
	require.NoError(t, os.Remove(filepath.Join(tree.Paths{Tree: broken}.InstallDir("beta", "1.0-1"), "rock_manifest")))

	v = inspect(t, broken)
	_, err = v.Files(beta1.ref())
	require.ErrorIs(t, err, tree.ErrCorrupt, "the rock without its rock_manifest")

	_, err = v.Files(alpha1.ref())
	require.NoError(t, err, "the other rock")

	_, err = v.Files(tree.RockRef{Name: "nothing", Version: "1"})
	require.ErrorIs(t, err, tree.ErrNotInstalled)
}

// treeState returns every regular file under root, by its slash-separated
// path relative to root, with its contents, root itself replaced by <TREE>
// so that the command launchers of two trees compare.
func treeState(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = strings.ReplaceAll(readFile(t, p), root, "<TREE>")

		return nil
	}))

	return out
}

// TestTx_RemoveAsTheClientDoes: Tx.Remove leaves the tree a native Remove of
// the same versions leaves, files and manifests alike.
func TestTx_RemoveAsTheClientDoes(t *testing.T) {
	t.Parallel()

	defs := []rockDef{beta1, alpha1, alpha2}
	byClient := buildTree(t, defs...)
	byTx := buildTree(t, defs...)

	for _, r := range []tree.RockRef{alpha2.ref(), beta1.ref()} {
		nativeRemove(t, byClient, r)
	}

	require.NoError(t, update(t, byTx, func(tx *tree.Tx) error {
		if err := tx.Remove(alpha2.ref()); err != nil {
			return err
		}

		assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, tx.View().Rocks(), "the Tx's view follows it")

		return tx.Remove(beta1.ref())
	}))

	assert.Equal(t, treeState(t, byClient), treeState(t, byTx), "the trees")
	requireConsistent(t, byTx, alpha1)
}

// TestTx_Prune: Prune removes what keep does not keep in one pass, leaving
// the tree the native Removes of the same versions leave, in either order;
// and the outcome does not depend on the order the rocks were installed in.
func TestTx_Prune(t *testing.T) {
	t.Parallel()

	keep := tree.KeepVersions(map[string]string{"alpha": "1.0.0-1"})
	want := []tree.RockRef{alpha2.ref(), beta1.ref()}

	prune := func(root string) {
		t.Helper()

		require.NoError(t, update(t, root, func(tx *tree.Tx) error {
			removed, err := tx.Prune(keep, tree.PruneOpts{})
			assert.Equal(t, want, removed, "what Prune removed")

			return err
		}))
	}

	pruned := buildTree(t, beta1, alpha1, alpha2)
	prune(pruned)
	requireConsistent(t, pruned, alpha1)

	state := treeState(t, pruned)

	for _, order := range [][]tree.RockRef{{alpha2.ref(), beta1.ref()}, {beta1.ref(), alpha2.ref()}} {
		removed := buildTree(t, beta1, alpha1, alpha2)
		for _, r := range order {
			nativeRemove(t, removed, r)
		}

		assert.Equal(t, state, treeState(t, removed), "Prune against the Removes %v", order)
	}

	for _, installs := range [][]rockDef{{alpha2, beta1, alpha1}, {alpha1, alpha2, beta1}} {
		other := buildTree(t, installs...)
		prune(other)
		assert.Equal(t, state, treeState(t, other), "Prune of the tree installed as %v", installs)
	}
}

// TestTx_PruneOrphans: with Orphans, Prune also removes the install dirs the
// manifest does not list, and the rock dirs that leaves empty, whatever keep
// says; without, it leaves them.
func TestTx_PruneOrphans(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1)
	paths := tree.Paths{Tree: root}

	ghost := paths.InstallDir("ghost", "1.0-1")
	writeFixture(t, filepath.Join(ghost, "rock_manifest"), "rock_manifest = {}\n")

	stray := paths.InstallDir("alpha", "0.9-1")
	writeFixture(t, filepath.Join(stray, "leftover"), "x")
	require.NoError(t, os.MkdirAll(filepath.Join(paths.RocksDir(), "husk"), 0o750))

	require.NoError(t, update(t, root, func(tx *tree.Tx) error {
		removed, err := tx.Prune(nil, tree.PruneOpts{})
		assert.Empty(t, removed, "a nil keep keeps everything, orphans aside")

		return err
	}))
	assert.DirExists(t, ghost, "an orphan without Orphans")

	require.NoError(t, update(t, root, func(tx *tree.Tx) error {
		removed, err := tx.Prune(func(tree.RockRef) bool { return true }, tree.PruneOpts{Orphans: true})
		assert.Equal(t, []tree.RockRef{{Name: "alpha", Version: "0.9-1"}, {Name: "ghost", Version: "1.0-1"}}, removed,
			"the orphans")

		return err
	}))

	assert.NoDirExists(t, ghost)
	assert.NoDirExists(t, filepath.Dir(ghost), "the rock dir an orphan leaves empty")
	assert.NoDirExists(t, stray)
	assert.NoDirExists(t, filepath.Join(paths.RocksDir(), "husk"), "an empty rock dir")
	assert.DirExists(t, paths.InstallDir("alpha", "1.0-1"), "the installed rock")
	requireConsistent(t, root, alpha1)
}

// TestUpdate_Semantics: a Tx refuses a version it does not have and any use
// after its Update; the manifest is written when fn fails after a change and
// not at all when nothing changed.
func TestUpdate_Semantics(t *testing.T) {
	t.Parallel()

	fresh := t.TempDir()
	require.NoError(t, update(t, fresh, func(*tree.Tx) error { return nil }))
	assert.NoFileExists(t, filepath.Join(tree.Paths{Tree: fresh}.RocksDir(), "manifest"), "nothing changed, nothing written")

	root := buildTree(t, alpha1, beta1)

	var kept *tree.Tx

	errFn := errors.New("fn failed")
	err := update(t, root, func(tx *tree.Tx) error {
		kept = tx

		require.ErrorIs(t, tx.Remove(tree.RockRef{Name: "alpha", Version: "3.0-1"}), tree.ErrNotInstalled)
		require.NoError(t, tx.Remove(beta1.ref()))

		return errFn
	})
	require.ErrorIs(t, err, errFn, "Update fails as fn does")

	v := inspect(t, root)
	assert.Equal(t, []tree.RockRef{alpha1.ref()}, v.Rocks(), "the removal fn made before failing is in the manifest")
	requireConsistent(t, root, alpha1)

	require.Error(t, kept.Remove(alpha1.ref()), "a Tx after its Update")
	_, err = kept.Prune(nil, tree.PruneOpts{})
	require.Error(t, err, "a Tx after its Update")
}
