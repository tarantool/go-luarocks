//go:build integration_luaengine

package client_test

// The tree package's Graft, Rebuild and Check on trees the lua backend built
// (see interop_treeapi_test.go for the scenarios): a guest tree grafted into
// a project tree must end as the project tree into which upstream installed
// the guest's binary rocks, and upstream must keep working on it; the
// manifest Rebuild makes of a tree must be the one upstream's make_manifest
// makes, but for the resolved dependencies; and Check must find nothing on
// any of them.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/tree"
)

// requireHealthy requires Check to find nothing on the tree root. The
// wrappers are not compared: upstream's list the prefix's tree too.
func requireHealthy(t *testing.T, label, root string) {
	t.Helper()

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	report, err := tr.Check(context.Background(), tree.CheckOpts{})
	require.NoError(t, err, "[%s] Check", label)
	assert.Empty(t, report.Issues, "[%s] Check", label)
}

// withoutResolved returns the tree manifest of root without the resolved
// dependencies of each rock, which the native backend records direct and
// upstream transitive, and those resolved dependencies.
func withoutResolved(t *testing.T, root string) (*rocks.Manifest, map[string]map[string]string) {
	t.Helper()

	m := readTreeManifestOf(t, root)
	resolved := map[string]map[string]string{}

	for name, versions := range m.Repository {
		for v, e := range versions {
			resolved[name+"/"+v] = e.Dependencies
			e.Dependencies = nil
			versions[v] = e
		}
	}

	return m, resolved
}

// compareManifestsButResolved requires the tree manifests of got, which the
// native backend wrote, and want, which upstream wrote, to say the same but
// for the resolved dependencies, and each rock's resolved dependencies in got
// to be among those upstream resolved.
func compareManifestsButResolved(t *testing.T, label, got, want string) {
	t.Helper()

	mGot, rGot := withoutResolved(t, got)
	mWant, rWant := withoutResolved(t, want)
	assert.Equal(t, mWant, mGot, "[%s] the tree manifest, resolved dependencies aside", label)

	for rock, deps := range rGot {
		for name, version := range deps {
			assert.Equal(t, version, rWant[rock][name], "[%s] %s resolves %s as upstream does", label, rock, name)
		}
	}
}

// TestBackendInterop_TreeGraft grafts a guest tree the lua backend built
// into a project tree it built, and holds the result to the project tree
// into which the lua backend installed the guest's rocks from their binary
// rocks; then upstream lists, shows, resolves, removes and installs on it as
// on the reference.
func TestBackendInterop_TreeGraft(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	release := interopSlot()
	defer release()

	project := treeAPIScenario{"project", []parityFixture{sharer("bbb"), dependent("app", "bbb >= 1.0")}}
	guestFixtures := []parityFixture{sharer("aaa"), vrock("1.0"), dependent("user", "vrock >= 1.0")}
	guest := buildLuaTree(t, treeAPIScenario{"guest", guestFixtures}, prefix, incDir)

	requireHealthy(t, "guest", guest)

	// The reference: the guest's rocks, packed, installed by upstream.
	x := interopCtx{prefix: prefix, incDir: incDir}
	repo := t.TempDir()

	for _, fx := range guestFixtures {
		archive := packInstalled(t, x, client.BackendNative, guest, fx.pkg)
		require.NoError(t, os.Rename(archive, filepath.Join(repo, filepath.Base(archive))))
	}

	require.NoError(t, remote.MakeManifest(context.Background(), repo, remote.MakeManifestOpts{}))

	installGuest := func(root string) {
		t.Helper()

		markNativePacked(root) // divPackModes

		cfg := luaTreeCfg(t, root, prefix, incDir)
		cfg.Servers = []string{repo}

		for _, fx := range guestFixtures {
			require.NoError(t, newClient(t, cfg, client.WithBackend(client.BackendLua)).
				Install(context.Background(), fx.pkg, client.InstallOpts{Version: fx.version, Deps: client.DepsNone}),
				"lua Install %s", fx.pkg)
		}
	}

	want := buildLuaTree(t, project, prefix, incDir)
	installGuest(want)

	got := buildLuaTree(t, project, prefix, incDir)
	requireHealthy(t, "project", got)

	tr, err := tree.Open(rocks.Config{Tree: got})
	require.NoError(t, err)

	tr.BinWrap = client.BinWrapFor(luaTreeCfg(t, got, prefix, incDir))

	var report *tree.GraftReport

	require.NoError(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		report, err = tx.Graft(guest, tree.GraftOpts{})

		return err
	}), "Graft")

	assert.Equal(t, []tree.RockRef{{Name: "aaa", Version: "1.0-1"}, {Name: "user", Version: "1.0-1"}, {Name: "vrock", Version: "1.0-1"}},
		report.Grafted)
	assert.Empty(t, report.Skipped)
	assert.Empty(t, report.Untracked)

	label := "graft"
	compareDeployedRoots(t, label, []string{got}, []string{want}, 0)
	compareBookkeeping(t, label, got, want)

	installed := installedVersionsOrNone(t, want)
	require.Equal(t, installed, installedVersionsOrNone(t, got), "[%s] installed rocks", label)

	for _, nv := range installed {
		compareRockManifestsOf(t, label, manifestSide{"graft", got}, manifestSide{"lua", want}, nv[0], nv[1])
	}

	compareManifestsButResolved(t, label, got, want)
	compareViewWithLua(t, label, got, prefix, incDir)
	requireHealthy(t, label, got)
	requireHealthy(t, "installed by upstream", want)

	// A second Graft finds every rock there already.
	require.NoError(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		report, err = tx.Graft(guest, tree.GraftOpts{})

		return err
	}), "Graft again")
	assert.Len(t, report.Skipped, len(guestFixtures))
	assert.Empty(t, report.Grafted)

	// Upstream keeps working on the tree: it removes the guest's rocks and
	// installs them again, as it does on the reference.
	for _, fx := range slices.Backward(guestFixtures) {
		r := tree.RockRef{Name: fx.pkg, Version: fx.version}
		luaRemove(t, got, prefix, incDir, r)
		luaRemove(t, want, prefix, incDir, r)
	}

	compareTrees(t, label+", the guest removed by upstream", got, want)

	installGuest(got)
	installGuest(want)
	compareTrees(t, label+", the guest installed again by upstream", got, want)
	compareViewWithLua(t, label+", installed again", got, prefix, incDir)
}

// TestBackendInterop_TreeRebuild rebuilds the manifest of trees the lua
// backend built with Rebuild and with upstream's make_manifest --local-tree:
// the two must say the same, but for the resolved dependencies, upstream
// resolving them transitively and Rebuild the direct ones alone, and Check
// must find nothing on the tree before and after.
func TestBackendInterop_TreeRebuild(t *testing.T) {
	prefix, incDir := parityTarantool(t)

	for _, sc := range treeAPIScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()

			release := interopSlot()
			defer release()

			got := buildLuaTree(t, sc, prefix, incDir)
			requireHealthy(t, sc.name, got)

			want := t.TempDir()
			require.NoError(t, os.CopyFS(want, os.DirFS(got)), "copy the tree")

			manifest := filepath.Join(tree.Paths{Tree: got}.RocksDir(), "manifest")
			require.NoError(t, os.Remove(manifest))

			tr, err := tree.Open(rocks.Config{Tree: got})
			require.NoError(t, err)

			report, err := tr.Rebuild(context.Background(), tree.RebuildOpts{Strict: true})
			require.NoError(t, err, "[%s] Rebuild", sc.name)
			assert.Empty(t, report.Skipped)
			assert.Empty(t, report.NoRockspec)

			luaAdmin(t, want, prefix, incDir, "make_manifest", tree.Paths{Tree: want}.RocksDir(), "--local-tree")

			assert.Equal(t, slices.Sorted(maps.Keys(readTreeManifestOf(t, want).Repository)),
				slices.Sorted(maps.Keys(readTreeManifestOf(t, got).Repository)), "[%s] rocks", sc.name)
			compareManifestsButResolved(t, sc.name, got, want)
			requireHealthy(t, sc.name+", rebuilt", got)
			compareViewWithLua(t, sc.name+", rebuilt", got, prefix, incDir)
		})
	}
}
