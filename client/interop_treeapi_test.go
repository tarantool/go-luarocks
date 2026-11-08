//go:build integration_luaengine

package client_test

// The tree package's view and transactions on trees the lua backend built:
// what tree.Inspect reports must agree with upstream's own `list`, `show` and
// `which` over the same tree, every file of every rock must be where Files
// says and as its rock_manifest records, and after Tx.Remove or Tx.Prune the
// tree must be the one the lua backend's own removals leave, a tree upstream
// keeps working on and whose manifest upstream's make_manifest rebuilds as
// written.

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/tree"
)

// treeAPIScenario is a tree the lua backend builds, every fixture through
// Build with Keep, so that every version stays installed.
type treeAPIScenario struct {
	name  string
	steps []parityFixture
}

// treeAPIScenarios are the trees: two rocks sharing a module and a command,
// made in either order; three versions of a rock, made upwards and downwards;
// a dependent of a rock of two versions; and the Lua and C forms of one
// module from two rocks.
func treeAPIScenarios() []treeAPIScenario {
	return []treeAPIScenario{
		{"two-rocks-ab", []parityFixture{sharer("aaa"), sharer("bbb")}},
		{"two-rocks-ba", []parityFixture{sharer("bbb"), sharer("aaa")}},
		{"versions-up", []parityFixture{vrock("1.0"), vrock("2.0"), vrock("3.0")}},
		{"versions-down", []parityFixture{vrock("3.0"), vrock("1.0"), vrock("2.0")}},
		{"dependent", []parityFixture{vrock("1.0"), vrock("2.0"), dependent("user", "vrock >= 1.0")}},
		{"lua-then-c", []parityFixture{formRock("bbb", true), formRock("aaa", false)}},
	}
}

// treeAPIOp is the operation the scenarios build with: Build, whose Keep
// keeps the other versions of the rock on the lua backend.
func treeAPIOp(t *testing.T) parityOp {
	t.Helper()

	for _, op := range parityOps() {
		if op.name == "build" {
			return op
		}
	}

	require.FailNow(t, "no build operation")

	return parityOp{}
}

// buildLuaTree builds the scenario into a fresh tree with the lua backend.
func buildLuaTree(t *testing.T, sc treeAPIScenario, prefix, incDir string) string {
	t.Helper()

	root := t.TempDir()
	for _, fx := range sc.steps {
		parityStep(t, treeAPIOp(t), fx, true, client.BackendLua, root, prefix, incDir)
	}

	return root
}

// luaTreeCfg returns the Config of a lua-backend operation on the tree root.
func luaTreeCfg(t *testing.T, root, prefix, incDir string) rocks.Config {
	t.Helper()

	return rocks.Config{
		Tree:       root,
		WorkingDir: t.TempDir(),
		Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: parityTarantoolVersion},
	}
}

// luaOutput runs a luarocks command line over the tree root in the lua
// backend and returns what it printed.
func luaOutput(t *testing.T, root, prefix, incDir string, argv ...string) string {
	t.Helper()

	lua, ok := newClient(t, luaTreeCfg(t, root, prefix, incDir), client.WithBackend(client.BackendLua)).
		Engine().(*client.LuaEngine)
	require.True(t, ok, "the lua backend's engine")

	out, err := lua.Output(append([]string{"--tree", root}, argv...))
	require.NoError(t, err, "luarocks %v", argv)

	return out
}

// luaAdmin runs a luarocks-admin command line over the tree root in the lua
// backend, the tree given as the global --tree, which Admin does not pass.
func luaAdmin(t *testing.T, root, prefix, incDir string, argv ...string) {
	t.Helper()

	lua, ok := newClient(t, luaTreeCfg(t, root, prefix, incDir), client.WithBackend(client.BackendLua)).
		Engine().(*client.LuaEngine)
	require.True(t, ok, "the lua backend's engine")

	_, err := lua.Output(append([]string{"admin", "--tree", root}, argv...))
	require.NoError(t, err, "luarocks-admin %v", argv)
}

// compareViewWithLua requires tree.Inspect of root to agree with upstream's
// readers over it, and every rock's files to be where Files says they are.
func compareViewWithLua(t *testing.T, label, root, prefix, incDir string) {
	t.Helper()

	v, err := tree.Inspect(context.Background(), root)
	require.NoError(t, err, "[%s] Inspect", label)

	// `list --porcelain`: name, version, status, tree.
	var listed []string

	viewed := make([]string, 0, len(v.Rocks()))

	for line := range strings.SplitSeq(strings.TrimSpace(luaOutput(t, root, prefix, incDir, "list", "--porcelain")), "\n") {
		if f := strings.Split(line, "\t"); len(f) >= 2 {
			listed = append(listed, f[0]+" "+f[1])
		}
	}

	for _, ref := range v.Rocks() {
		viewed = append(viewed, ref.Name+" "+ref.Version)
	}

	slices.Sort(listed)
	slices.Sort(viewed)
	assert.Equal(t, listed, viewed, "[%s] Rocks vs `list`", label)

	names := map[string]bool{}
	for _, ref := range v.Rocks() {
		names[ref.Name] = true
	}

	// `show --porcelain`: the newest version and its declared dependencies.
	for name := range names {
		var version string

		var depNames []string

		for line := range strings.SplitSeq(luaOutput(t, root, prefix, incDir, "show", "--porcelain", name), "\n") {
			f := strings.Split(line, "\t")

			switch {
			case len(f) >= 2 && f[0] == "version":
				version = f[1]
			case len(f) >= 2 && f[0] == "dependency":
				depNames = append(depNames, strings.Fields(f[1])[0])
			}
		}

		newest := tree.RockRef{Name: name, Version: v.Versions(name)[0]}
		assert.Equal(t, version, newest.Version, "[%s] the newest version of %s vs `show`", label, name)

		declared := make([]string, 0, len(depNames))
		for _, d := range v.Dependencies(newest) {
			declared = append(declared, d.Name)
		}

		assert.ElementsMatch(t, depNames, declared, "[%s] the dependencies of %s vs `show`", label, newest)
	}

	// `which`: the file the loader takes a module from is the one the first
	// provider holds at its plain path.
	m, err := manif.FileStore{}.ReadTree(tree.Paths{Tree: root}.RocksDir())
	require.NoError(t, err, "[%s] read the tree manifest", label)

	for module := range m.Modules {
		out := luaOutput(t, root, prefix, incDir, "which", module)
		file, _, _ := strings.Cut(strings.TrimSpace(out), "\n")

		owner, kind, ok := v.Owner(file)
		if assert.True(t, ok, "[%s] `which %s` names %q, which has an owner", label, module, file) {
			assert.Equal(t, v.Providers(tree.ModuleItem, module)[0], owner, "[%s] the owner of %s's file", label, module)
			assert.Equal(t, tree.ModuleItem, kind, "[%s] %s", label, module)
		}
	}

	for cmd := range m.Commands {
		script, entry, ok := v.Command(cmd)
		require.True(t, ok, "[%s] Command %s", label, cmd)
		assert.FileExists(t, entry, "[%s] the entry of %s", label, cmd)

		owner, kind, ok := v.Owner(entry)
		require.True(t, ok, "[%s] the entry of %s has an owner", label, cmd)
		assert.Equal(t, tree.CommandItem, kind)
		assert.Equal(t, filepath.Join(tree.Paths{Tree: root}.InstallDir(owner.Name, owner.Version), "bin", cmd), script,
			"[%s] the script of %s is its plain holder's", label, cmd)
	}

	for _, ref := range v.Rocks() {
		problems, err := v.Verify(ref)
		require.NoError(t, err, "[%s] Verify %s", label, ref)
		assert.Empty(t, problems, "[%s] Verify %s", label, ref)
	}
}

// TestBackendInterop_TreeView reads trees the lua backend built through
// tree.Inspect and holds the View to upstream's readers.
func TestBackendInterop_TreeView(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, sc := range treeAPIScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			release := interopSlot()
			defer release()

			compareViewWithLua(t, sc.name, buildLuaTree(t, sc, prefix, incDir), prefix, incDir)
		})
	}
}

// treeAPIRemoval removes versions from a scenario's tree: through a Tx, with
// tx, and on the reference tree, through the lua backend's Remove of each of
// refs in order.
type treeAPIRemoval struct {
	name string
	sc   string
	tx   func(tx *tree.Tx) error
	refs []tree.RockRef
}

// treeAPIRemovals: the plain holder of shared items and the one under it, a
// middle version, a prune to one pinned version, a dependency its dependent
// keeps needing, and the C form of a module.
func treeAPIRemovals() []treeAPIRemoval {
	remove := func(refs ...tree.RockRef) func(tx *tree.Tx) error {
		return func(tx *tree.Tx) error {
			for _, r := range refs {
				if err := tx.Remove(r); err != nil {
					return err
				}
			}

			return nil
		}
	}
	prune := func(pins map[string]string) func(tx *tree.Tx) error {
		return func(tx *tree.Tx) error {
			_, err := tx.Prune(tree.KeepVersions(pins), tree.PruneOpts{Orphans: true})

			return err
		}
	}
	ref := func(name, version string) tree.RockRef { return tree.RockRef{Name: name, Version: version} }

	return []treeAPIRemoval{
		{"remove-plain", "two-rocks-ab", remove(ref("aaa", "1.0-1")), []tree.RockRef{ref("aaa", "1.0-1")}},
		{"remove-versioned", "two-rocks-ba", remove(ref("bbb", "1.0-1")), []tree.RockRef{ref("bbb", "1.0-1")}},
		{"remove-middle", "versions-up", remove(ref("vrock", "2.0-1")), []tree.RockRef{ref("vrock", "2.0-1")}},
		{
			"prune-to-old", "versions-down", prune(map[string]string{"vrock": "1.0-1"}),
			[]tree.RockRef{ref("vrock", "2.0-1"), ref("vrock", "3.0-1")},
		},
		{
			"prune-dependency", "dependent", prune(map[string]string{"vrock": "2.0-1", "user": "1.0-1"}),
			[]tree.RockRef{ref("vrock", "1.0-1")},
		},
		{"remove-c", "lua-then-c", remove(ref("aaa", "1.0-1")), []tree.RockRef{ref("aaa", "1.0-1")}},
	}
}

// luaRemove removes r from the tree root with the lua backend, forced, as
// the native Remove removes whatever depends on it.
func luaRemove(t *testing.T, root, prefix, incDir string, r tree.RockRef) {
	t.Helper()

	require.NoError(t,
		newClient(t, luaTreeCfg(t, root, prefix, incDir), client.WithBackend(client.BackendLua)).
			Remove(context.Background(), r.Name, client.RemoveOpts{Version: r.Version, ForceFast: true}),
		"lua Remove %s", r)
}

// compareTrees requires got to hold what want holds: the deployed artifacts,
// the bookkeeping under share/tarantool/rocks/, every rock_manifest and the
// tree manifest.
func compareTrees(t *testing.T, label, got, want string) {
	t.Helper()

	compareDeployedRoots(t, label, []string{got}, []string{want}, 0)
	compareBookkeeping(t, label, got, want)

	installed := installedVersionsOrNone(t, want)
	require.Equal(t, installed, installedVersionsOrNone(t, got), "[%s] installed rocks", label)

	for _, nv := range installed {
		compareRockManifestsOf(t, label, manifestSide{"tx", got}, manifestSide{"lua", want}, nv[0], nv[1])
	}

	if installed != nil {
		compareTreeManifestsOf(t, label, got, want)
	}
}

// TestBackendInterop_TreeTx removes rocks from trees the lua backend built
// through tree.Tx and holds the result to the lua backend's own removals: the
// same tree, which upstream keeps reading, installing into and removing from
// as it does the reference, and whose tree manifest upstream's make_manifest
// rebuilds unchanged from the rocks on disk.
func TestBackendInterop_TreeTx(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	scenarios := map[string]treeAPIScenario{}
	for _, sc := range treeAPIScenarios() {
		scenarios[sc.name] = sc
	}

	for _, rm := range treeAPIRemovals() {
		t.Run(rm.sc+"/"+rm.name, func(t *testing.T) {
			t.Parallel()

			release := interopSlot()
			defer release()

			label := rm.sc + "/" + rm.name
			sc := scenarios[rm.sc]
			got := buildLuaTree(t, sc, prefix, incDir)
			want := buildLuaTree(t, sc, prefix, incDir)

			tr, err := tree.Open(rocks.Config{Tree: got})
			require.NoError(t, err)
			require.NoError(t, tr.Update(context.Background(), rm.tx), "[%s] Update", label)

			for _, r := range rm.refs {
				luaRemove(t, want, prefix, incDir, r)
			}

			compareTrees(t, label, got, want)
			compareViewWithLua(t, label, got, prefix, incDir)

			// Upstream's make_manifest rebuilds the manifest Tx wrote from the
			// rocks on disk. The resolved dependencies it recomputes for every
			// rock, which the Tx leaves as they were, are not compared.
			written := readTreeManifestOf(t, got)
			rocksDir := tree.Paths{Tree: got}.RocksDir()
			luaAdmin(t, got, prefix, incDir, "make_manifest", rocksDir, "--local-tree")

			rebuilt := readTreeManifestOf(t, got)
			for _, m := range []*rocks.Manifest{written, rebuilt} {
				for _, versions := range m.Repository {
					for v, e := range versions {
						e.Dependencies = nil
						versions[v] = e
					}
				}
			}

			assert.Equal(t, rebuilt, written, "[%s] the manifest make_manifest rebuilds", label)

			// Upstream keeps working on the tree: it removes the rest and
			// installs again, as it does on the reference.
			left, err := tree.Inspect(context.Background(), want)
			require.NoError(t, err)

			for _, r := range slices.Backward(left.Rocks()) {
				luaRemove(t, got, prefix, incDir, r)
				luaRemove(t, want, prefix, incDir, r)
			}

			for _, fx := range sc.steps {
				parityStep(t, treeAPIOp(t), fx, true, client.BackendLua, got, prefix, incDir)
				parityStep(t, treeAPIOp(t), fx, true, client.BackendLua, want, prefix, incDir)
			}

			compareTrees(t, label+", rebuilt by upstream", got, want)
		})
	}
}
