//go:build integration_luaengine

package client_test

// The known divergences of the parity and interop harnesses. Where the native
// backend keeps go-luarocks' own semantics by decision and the lua backend
// runs upstream LuaRocks, the two backends end differently by design; tt, the
// main consumer, drives the native backend as a deterministic executor (exact
// pins, DepsNone, a topological order, its own pruning) and relies on them.
// Each divergence is named here with its reason, and the scenarios it shows in
// say so, checking what each backend does where that is cheap, instead of
// comparing the trees.

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

const (
	// divWrapperTrees: a native command wrapper lists Config.Tree alone; one
	// that upstream install writes lists every rock tree its --deps-mode
	// selects, which for each DepsPolicy (all, none, order) is Config.Tree
	// and then Config.Tarantool.Prefix. Make and Build pass no --deps-mode,
	// and upstream's default, "one", lists Config.Tree alone too.
	divWrapperTrees = "native keeps go-luarocks' own semantics: a command wrapper lists Config.Tree alone, " +
		"where upstream install lists the Tarantool prefix's tree after it"
	// divOtherVersions: native Install, Build and Make keep the other
	// installed versions of a rock; upstream removes them unless --keep,
	// which only Build can pass (BuildOpts.Keep).
	divOtherVersions = "native keeps go-luarocks' own semantics: Install, Build and Make keep the other " +
		"installed versions of a rock, where upstream removes them unless --keep"
	// divResolvedDeps: native records a rock's resolved dependencies once, at
	// its deploy, from its direct dependencies; upstream recomputes every
	// rock's, transitively, on each write of the tree manifest.
	divResolvedDeps = "native keeps go-luarocks' own semantics: a rock's resolved dependencies are recorded " +
		"at its deploy, from its direct dependencies, where upstream recomputes every rock's transitively"
	// divRemoveDependents: native Remove removes a rock another depends on;
	// upstream refuses unless forced.
	divRemoveDependents = "native keeps go-luarocks' own semantics: Remove does not check the dependents " +
		"of a rock, where upstream refuses to break one unless forced"
)

// prefixTree returns the root of the real Tarantool installation the
// harnesses build against, the rock tree upstream install lists after
// Config.Tree (divWrapperTrees), or "" when there is none.
func prefixTree() string {
	bin, err := exec.LookPath("tarantool")
	if err != nil {
		return ""
	}

	return filepath.Dir(filepath.Dir(bin))
}

// dropPrefixTree removes the module directories of the Tarantool prefix's
// tree from the package.path and package.cpath of a command wrapper — what
// divWrapperTrees adds to the ones upstream install writes — so a wrapper
// either backend wrote compares with the other's. Text that is no wrapper
// comes back unchanged.
func dropPrefixTree(text string) string {
	root := prefixTree()
	if root == "" || !strings.Contains(text, "package.path=") {
		return text
	}

	p := tree.Paths{Tree: root}
	lua := filepath.ToSlash(p.DeployLuaDir())

	for _, entry := range []string{lua + "/?.lua;", lua + "/?/init.lua;", filepath.ToSlash(p.DeployLibDir()) + "/?.so;"} {
		text = strings.ReplaceAll(text, entry, "")
	}

	return text
}

// checkWrapperTrees checks the trees every command wrapper of the two trees
// lists, as divWrapperTrees says each backend writes them: the native ones its
// own tree alone, the lua ones its own tree and, after an install, the
// Tarantool prefix's.
func checkWrapperTrees(t *testing.T, op parityOp, treeNative, treeLua string) {
	t.Helper()

	for _, side := range []struct {
		root string
		want []string
	}{
		{treeNative, []string{treeNative}},
		{treeLua, []string{treeLua}},
	} {
		if side.root == treeLua && op.name == "install" {
			side.want = append(side.want, prefixTree())
		}

		bins, _ := os.ReadDir(filepath.Join(side.root, "bin"))
		for _, b := range bins {
			p := filepath.Join(side.root, "bin", b.Name())

			body, err := os.ReadFile(p) //nolint:gosec // test temp path
			if err != nil || !strings.Contains(string(body), "package.path=") {
				continue
			}

			assert.Equal(t, side.want, wrapperTrees(t, p), "the trees %s lists (%s)", p, divWrapperTrees)
		}
	}
}

// versionDivergence is how the two trees of a parity version scenario differ
// by design.
type versionDivergence struct {
	reason string
	// ops are the operations it shows in; nil for every one.
	ops map[string]bool
	// lua is what the lua backend leaves installed, for divOtherVersions; the
	// native backend leaves every version a step deployed.
	lua [][2]string
}

// installsMakeOrInstall are the operations that cannot pass --keep.
var installsMakeOrInstall = map[string]bool{"make": true, "install": true}

// knownVersionDivergences are the parity version scenarios the backends end
// differently by design, by name.
var knownVersionDivergences = map[string]versionDivergence{
	"upgrade":                   {divOtherVersions, nil, [][2]string{{"vrock", "2.0-1"}}},
	"downgrade":                 {divOtherVersions, nil, [][2]string{{"vrock", "1.0-1"}}},
	"three-versions":            {divOtherVersions, nil, [][2]string{{"vrock", "3.0-1"}}},
	"downgrade-c-to-lua":        {divOtherVersions, nil, [][2]string{{"pair", "1.0-1"}}},
	"dependent-follows-upgrade": {divOtherVersions, nil, [][2]string{{"user", "1.0-1"}, {"vrock", "2.0-1"}}},
	// Only Build passes Keep on as --keep; Make and Install remove the
	// other versions on the lua backend whatever the scenario asks.
	"upgrade-keep":        {divOtherVersions, installsMakeOrInstall, [][2]string{{"vrock", "2.0-1"}}},
	"downgrade-keep":      {divOtherVersions, installsMakeOrInstall, [][2]string{{"vrock", "1.0-1"}}},
	"three-versions-keep": {divOtherVersions, installsMakeOrInstall, [][2]string{{"vrock", "3.0-1"}}},
	"lua-to-c-keep":       {divOtherVersions, installsMakeOrInstall, [][2]string{{"pair", "2.0-1"}}},
	"c-to-lua-keep":       {divOtherVersions, installsMakeOrInstall, [][2]string{{"pair", "2.0-1"}}},
	// The trees agree but for the resolved dependencies of top.
	"dependency-chain": {reason: divResolvedDeps},
}

// versionDivergenceOf returns the known divergence of sc under op, if any.
func versionDivergenceOf(op parityOp, sc parityVersionScenario) (versionDivergence, bool) {
	d, ok := knownVersionDivergences[sc.name]
	if !ok || (d.ops != nil && !d.ops[op.name]) {
		return versionDivergence{}, false
	}

	return d, true
}

// stepVersions returns every name/version the steps of sc deploy, sorted: what
// the native backend leaves installed, since it removes none of them.
func stepVersions(sc parityVersionScenario) [][2]string {
	seen := map[[2]string]bool{}

	var out [][2]string

	for _, st := range sc.steps {
		nv := [2]string{st.fx.pkg, st.fx.version}
		if !seen[nv] {
			seen[nv] = true
			out = append(out, nv)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}

		return out[i][1] < out[j][1]
	})

	return out
}

// compareResolvedDeps is compareTreeManifests under divResolvedDeps: the two
// tree manifests must agree in everything but each rock's resolved
// dependencies, of which the native ones — the direct dependencies — must be
// among the lua ones, the transitive closure.
func compareResolvedDeps(tb testing.TB, label, treeNative, treeLua string) {
	tb.Helper()

	mN := readTreeManifestOf(tb, treeNative)
	mL := readTreeManifestOf(tb, treeLua)

	for name, versions := range mL.Repository {
		for v, entryL := range versions {
			entryN, ok := mN.Repository[name][v]
			if !ok {
				continue
			}

			for dep, ver := range entryN.Dependencies {
				assert.Equal(tb, entryL.Dependencies[dep], ver,
					"[%s] %s %s: the native resolved dependency %s must be among the lua ones (%s)",
					label, name, v, dep, divResolvedDeps)
			}

			entryN.Dependencies, entryL.Dependencies = nil, nil
			mN.Repository[name][v], mL.Repository[name][v] = entryN, entryL
		}
	}

	assert.Equal(tb, mL.Repository, mN.Repository, "[%s] tree manifest repository, but resolved dependencies", label)
	assert.Equal(tb, mL.Modules, mN.Modules, "[%s] tree manifest modules", label)
	assert.Equal(tb, mL.Commands, mN.Commands, "[%s] tree manifest commands", label)
	assert.Equal(tb, mL.Dependencies, mN.Dependencies, "[%s] tree manifest dependencies", label)
}

// followB holds a direction of a divergent interop scenario to the reference
// of the backend role B runs; followA to role A's.
var (
	followB = map[string]client.Backend{nativeWrites: client.BackendLua, luaWrites: client.BackendNative}
	followA = map[string]client.Backend{nativeWrites: client.BackendNative, luaWrites: client.BackendLua}
)
