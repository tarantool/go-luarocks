//go:build integration_luaengine

package client_test

// Backend interoperability: the two backends working on the SAME tree. The
// parity harness (parity_*_test.go) has each backend produce a tree of its
// own and diffs the two; this one has one backend operate on a tree the other
// produced, which is what tt does when it switches backends on an existing
// project, and what the package documentation promises when it says a rock
// installed by this library is queryable by upstream LuaRocks and vice versa.
//
// A scenario is a sequence of steps over one or more rock trees, each step
// run by one of two roles, A and B. Every scenario runs four times: with A
// native and B lua, with A lua and B native (the two directions), and with a
// single backend doing every step (the two references). Nothing about the
// outcome is hard-coded: each direction must end exactly where both
// references end — the same steps failing, the same deployed artifacts, the
// same rock bookkeeping, the same tree manifest — and every reader must report
// the same about its trees: the native List, Show and Which, and the lua
// engine's own `list`, `show`, `which` and `path` commands, which run
// upstream's code over the tree rather than the native reader the typed reads
// share. The two readers are also held to each other on every tree.
//
// Where the native backend keeps go-luarocks' own semantics by decision
// (parity_divergence_test.go), the two references end differently by design:
// such a scenario names the divergence (interopScenario.diverges), and each
// direction is held to the reference of the backend whose semantics decide
// its outcome (interopScenario.follow), or, where neither does, skipped by
// name through interopScenario.known with the divergence as the reason. The
// rock bookkeeping is compared without the build/ staging directory the
// native backend keeps in each install dir, the one documented difference of
// the parity harness under share/tarantool/rocks/, and a command wrapper
// without the Tarantool prefix's tree (divWrapperTrees).
//
// The scenarios are in interop_scenarios_test.go (reads, versions),
// interop_trees_test.go (removals, the manifest round trip, deps modes) and
// interop_pack_test.go (packing).

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/tree"
)

// interopSlots bounds how many scenarios run at once. They run in parallel to
// keep the suite's wall time down, but each one builds, compiles and boots lua
// VMs in four runs, so letting every CPU take one at a time, next to the other
// packages' tests, exhausted the memory of a small sandbox.
var interopSlots = make(chan struct{}, 3)

// interopSlot waits for a free slot and returns the function freeing it.
func interopSlot() func() {
	interopSlots <- struct{}{}

	return func() { <-interopSlots }
}

// interopRole is who runs a step of a scenario.
type interopRole int

const (
	roleA interopRole = iota
	roleB
)

// interopDirection assigns a backend to each role.
type interopDirection struct {
	name string
	a, b client.Backend
}

// interopDirections are the two directions every scenario runs in.
func interopDirections() []interopDirection {
	return []interopDirection{
		{nativeWrites, client.BackendNative, client.BackendLua},
		{luaWrites, client.BackendLua, client.BackendNative},
	}
}

// interopCtx is what a step runs with: the tarantool it builds against and
// the trees of the run.
type interopCtx struct {
	prefix, incDir string
	trees          []string
}

// cfg returns the Config of an operation on tree i of the run.
func (x interopCtx) cfg(t *testing.T, i int, servers ...string) rocks.Config {
	t.Helper()

	return rocks.Config{
		Tree:       x.trees[i],
		WorkingDir: t.TempDir(),
		Servers:    servers,
		Tarantool:  rocks.TarantoolConfig{Prefix: x.prefix, IncludeDir: x.incDir, Version: parityTarantoolVersion},
	}
}

// interopStep is one operation of a scenario, run by role with backend.
type interopStep struct {
	name string
	role interopRole
	run  func(t *testing.T, x interopCtx, backend client.Backend) error
}

// interopScenario is a sequence of steps over trees rock trees (one when
// zero), the first one seeded with seed (tree-relative path → contents).
type interopScenario struct {
	name  string
	trees int
	seed  map[string]string
	steps []interopStep
	// show and which name rocks and modules the readers are asked about on
	// top of the ones the tree manifest lists, such as removed ones.
	show, which []string
	// known names the directions, nativeWrites or luaWrites, that a known
	// divergence keeps from running, each with the divergence.
	known map[string]string
	// diverges names the known divergence that makes the two references end
	// differently; follow then names, per direction, the backend whose
	// reference that direction must end at (followA, followB).
	diverges string
	follow   map[string]client.Backend
}

// interopRun is the outcome of one run of a scenario.
type interopRun struct {
	trees []string
	// failed tells, per step, whether it returned an error.
	failed []bool
	// views holds, per tree, what the readers report about it.
	views []map[string]string
}

// deployStep deploys fx into the first tree through op, with keep as the Keep
// option.
func deployStep(role interopRole, op parityOp, fx parityFixture, keep bool) interopStep {
	return interopStep{
		name: op.name + " " + fx.name,
		role: role,
		run: func(t *testing.T, x interopCtx, backend client.Backend) error {
			t.Helper()
			parityStep(t, op, fx, keep, backend, x.trees[0], x.prefix, x.incDir)

			return nil
		},
	}
}

// removeStep removes the rock name from the first tree.
func removeStep(role interopRole, name string, opts client.RemoveOpts) interopStep {
	return interopStep{
		name: fmt.Sprintf("remove %s %s (%+v)", name, opts.Version, opts),
		role: role,
		run: func(t *testing.T, x interopCtx, backend client.Backend) error {
			t.Helper()

			return newClient(t, x.cfg(t, 0), client.WithBackend(backend)).Remove(context.Background(), name, opts)
		},
	}
}

// installStep installs the rock name from the directory server repo into tree
// i.
func installStep(role interopRole, i int, repo, name string) interopStep {
	return interopStep{
		name: fmt.Sprintf("install %s (tree %d)", name, i),
		role: role,
		run: func(t *testing.T, x interopCtx, backend client.Backend) error {
			t.Helper()

			return newClient(t, x.cfg(t, i, repo), client.WithBackend(backend)).
				Install(context.Background(), name, client.InstallOpts{})
		},
	}
}

// runInterop runs every step of sc, each through the backend backendOf gives
// its role, and returns the outcome, the readers' views included.
func runInterop(t *testing.T, prefix, incDir string, sc interopScenario, backendOf func(interopRole) client.Backend) interopRun {
	t.Helper()

	x := interopCtx{prefix: prefix, incDir: incDir, trees: make([]string, max(sc.trees, 1))}
	for i := range x.trees {
		x.trees[i] = t.TempDir()
	}

	for rel, body := range sc.seed {
		p := filepath.Join(x.trees[0], filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}

	run := interopRun{trees: x.trees}

	for _, st := range sc.steps {
		backend := backendOf(st.role)

		err := st.run(t, x, backend)
		if err != nil {
			t.Logf("[%s] step %q (%v) failed: %v", sc.name, st.name, backend, err)
		}

		run.failed = append(run.failed, err != nil)
	}

	for i := range x.trees {
		run.views = append(run.views, interopViews(t, x, i, sc.show, sc.which))
	}

	return run
}

// testInterop runs sc in both directions and against both references.
func testInterop(t *testing.T, prefix, incDir string, sc interopScenario) {
	t.Helper()

	release := interopSlot()
	defer release()

	all := func(b client.Backend) func(interopRole) client.Backend {
		return func(interopRole) client.Backend { return b }
	}

	refs := map[client.Backend]interopRun{
		client.BackendNative: runInterop(t, prefix, incDir, sc, all(client.BackendNative)),
		client.BackendLua:    runInterop(t, prefix, incDir, sc, all(client.BackendLua)),
	}

	for _, ref := range refs {
		compareReaders(t, sc.name, ref)
	}

	for _, dir := range interopDirections() {
		t.Run(dir.name, func(t *testing.T) {
			if why, ok := sc.known[dir.name]; ok {
				t.Skip("known divergence: " + why)
			}

			mixed := runInterop(t, prefix, incDir, sc, func(r interopRole) client.Backend {
				if r == roleA {
					return dir.a
				}

				return dir.b
			})

			compareReaders(t, sc.name+" "+dir.name, mixed)

			against := []client.Backend{client.BackendNative, client.BackendLua}

			if sc.diverges != "" {
				backend, ok := sc.follow[dir.name]
				require.True(t, ok, "[%s] a divergent scenario names the reference of every direction it runs", sc.name)
				t.Logf("[%s] known divergence: %s; %s ends where all-%v does", sc.name, sc.diverges, dir.name, backend)

				against = []client.Backend{backend}
			}

			for _, refBackend := range against {
				label := fmt.Sprintf("%s %s vs all-%v", sc.name, dir.name, refBackend)
				compareInteropRuns(t, label, mixed, refs[refBackend])
			}
		})
	}
}

// Directions, by name, for interopScenario.known.
const (
	nativeWrites = "native-writes/lua-reads"
	luaWrites    = "lua-writes/native-reads"
)

// compareInteropRuns requires got to end where want ends.
func compareInteropRuns(t *testing.T, label string, got, want interopRun) {
	t.Helper()

	assert.Equal(t, want.failed, got.failed, "[%s] which steps failed", label)

	for i := range want.trees {
		tl := fmt.Sprintf("%s, tree %d", label, i)
		compareDeployedRoots(t, tl, got.trees, want.trees, i)
		compareBookkeeping(t, tl, got.trees[i], want.trees[i])

		installedGot := installedVersionsOrNone(t, got.trees[i])
		installedWant := installedVersionsOrNone(t, want.trees[i])
		require.Equal(t, installedWant, installedGot, "[%s] installed rocks", tl)

		for _, nv := range installedWant {
			compareRockManifestsOf(t, tl, manifestSide{"interop", got.trees[i]}, manifestSide{"reference", want.trees[i]},
				nv[0], nv[1])
		}

		if installedWant != nil {
			compareTreeManifestsOf(t, tl, got.trees[i], want.trees[i])
		}

		assert.Equal(t, slices.Sorted(maps.Keys(want.views[i])), slices.Sorted(maps.Keys(got.views[i])),
			"[%s] what the readers were asked", tl)

		for key, w := range want.views[i] {
			if g, ok := got.views[i][key]; ok {
				assert.Equal(t, w, g, "[%s] %s", tl, key)
			}
		}
	}
}

// installedVersionsOrNone is installedVersions, but nil for a tree that has
// no tree manifest at all.
func installedVersionsOrNone(t *testing.T, root string) [][2]string {
	t.Helper()

	if _, err := os.Stat(filepath.Join(tree.Paths{Tree: root}.RocksDir(), "manifest")); os.IsNotExist(err) {
		return nil
	}

	out := installedVersions(t, root)
	if out == nil {
		out = [][2]string{}
	}

	return out
}

// compareTreeManifestsOf requires the tree manifests of got and want to say
// the same thing, through the native reader (compareTreeManifests) and as raw
// Lua tables, so nothing the native model leaves out can differ unnoticed.
func compareTreeManifestsOf(t *testing.T, label, got, want string) {
	t.Helper()

	compareTreeManifests(t, label, got, want)
	assert.Equal(t, rawTreeManifest(t, want), rawTreeManifest(t, got), "[%s] tree manifest, as Lua tables", label)
}

// rawTreeManifest parses the tree manifest of root with manif.Parse, with
// every empty table dropped, since the two writers differ in whether they
// emit one.
func rawTreeManifest(t *testing.T, root string) any {
	t.Helper()

	p := filepath.Join(tree.Paths{Tree: root}.RocksDir(), "manifest")
	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(t, err, "read %s", p)

	v, err := manif.Parse(b)
	require.NoError(t, err, "parse %s", p)

	return dropEmptyTables(v)
}

func dropEmptyTables(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}

		for k, e := range x {
			if e = dropEmptyTables(e); e != nil {
				out[k] = e
			}
		}

		if len(out) == 0 {
			return nil
		}

		return out
	case []any:
		if len(x) == 0 {
			return nil
		}

		out := make([]any, 0, len(x))
		for _, e := range x {
			out = append(out, dropEmptyTables(e))
		}

		return out
	default:
		return v
	}
}

// rootReplacer replaces each of roots with <TREE<i>>, i its index, the longest
// first, so a root that is a prefix of another does not break it up.
func rootReplacer(roots []string) *strings.Replacer {
	idx := make([]int, len(roots))
	for i := range idx {
		idx[i] = i
	}

	sort.SliceStable(idx, func(a, b int) bool { return len(roots[idx[a]]) > len(roots[idx[b]]) })

	pairs := make([]string, 0, 2*len(idx))
	for _, i := range idx {
		pairs = append(pairs, roots[i], fmt.Sprintf("<TREE%d>", i))
	}

	return strings.NewReplacer(pairs...)
}

// compareDeployedRoots requires tree i of got and want to hold the same
// deployed artifacts, byte for byte once the roots of each run's trees are
// normalized (a command wrapper names every tree it puts on package.path); a
// compiled .so by presence only.
func compareDeployedRoots(t *testing.T, label string, got, want []string, i int) {
	t.Helper()

	depGot := deployedArtifacts(t, got[i])
	depWant := deployedArtifacts(t, want[i])
	normGot, normWant := rootReplacer(got), rootReplacer(want)

	for _, rel := range slices.Sorted(maps.Keys(unionKeys(depGot, depWant))) {
		dGot, inGot := depGot[rel]
		dWant, inWant := depWant[rel]

		switch {
		case !inGot:
			t.Errorf("[%s] deployed artifact %q missing from the interop tree", label, rel)
		case !inWant:
			t.Errorf("[%s] deployed artifact %q only in the interop tree", label, rel)
		case dGot == dWant:
		case strings.HasSuffix(rel, ".so"):
			t.Logf("[%s] note: compiled %q differs byte-wise (non-reproducible build)", label, rel)
		default:
			bGot, err := os.ReadFile(filepath.Join(got[i], filepath.FromSlash(rel))) //nolint:gosec // test temp path
			require.NoError(t, err)
			bWant, err := os.ReadFile(filepath.Join(want[i], filepath.FromSlash(rel))) //nolint:gosec // test temp path
			require.NoError(t, err)
			assert.Equal(t, normWant.Replace(dropPrefixTree(string(bWant))), normGot.Replace(dropPrefixTree(string(bGot))),
				"[%s] deployed artifact %q", label, rel)
		}
	}
}

func unionKeys(a, b map[string]string) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}

	for k := range b {
		out[k] = struct{}{}
	}

	return out
}

// compareBookkeeping requires the two trees to keep the same files under
// share/tarantool/rocks/ — every installed version's directory and what is in
// it — save the build/ staging directory the native backend keeps in each
// install dir (a documented difference). Their contents are compared through
// the rock_manifest and the tree manifest.
func compareBookkeeping(t *testing.T, label, got, want string) {
	t.Helper()

	files := func(root string) map[string]struct{} {
		out := map[string]struct{}{}

		for rel := range rockPathSet(treeDigest(t, root)) {
			parts := strings.Split(strings.TrimPrefix(rel, "share/tarantool/rocks/"), "/")
			if len(parts) > 3 && parts[2] == "build" {
				continue
			}

			out[rel] = struct{}{}
		}

		return out
	}

	assert.Equal(t, sortedKeys(files(want)), sortedKeys(files(got)), "[%s] files under share/tarantool/rocks/", label)
}

// interopViews returns what the readers report about tree i of a run: the
// native List, Show and Which, and the lua engine's `list`, `show`, `which`
// and `path`, for every rock and module its tree manifest lists plus the extra ones given. The roots of the run's trees
// are normalized.
func interopViews(t *testing.T, x interopCtx, i int, extraRocks, extraModules []string) map[string]string {
	t.Helper()

	ctx := context.Background()
	cfg := x.cfg(t, i)
	// The lua engine's own package.cpath names the working directory's
	// .rocks tree, which `path` prints.
	norm := rootReplacer(append(slices.Clone(x.trees), cfg.WorkingDir))
	native := newClient(t, cfg)
	lua, ok := newClient(t, cfg, client.WithBackend(client.BackendLua)).Engine().(*client.LuaEngine)
	require.True(t, ok, "the lua backend's engine")

	rockNames := map[string]struct{}{}
	moduleNames := map[string]struct{}{}

	for _, n := range extraRocks {
		rockNames[n] = struct{}{}
	}

	for _, n := range extraModules {
		moduleNames[n] = struct{}{}
	}

	if m, err := (manif.FileStore{}).ReadTree(tree.Paths{Tree: x.trees[i]}.RocksDir()); err == nil {
		for n := range m.Repository {
			rockNames[n] = struct{}{}
		}

		for n := range m.Modules {
			moduleNames[n] = struct{}{}
		}
	}

	view := map[string]string{}
	set := func(key, out string, err error) {
		if err != nil {
			out += "error: " + err.Error()
		}

		view[key] = norm.Replace(out)
	}

	luaRun := func(argv ...string) (string, error) {
		return lua.Output(append([]string{"--tree", x.trees[i]}, argv...))
	}

	listed, err := native.List(ctx)
	if err == nil {
		lines := make([]string, 0, len(listed))
		for _, r := range listed {
			lines = append(lines, r.Name+" "+r.Version)
		}

		sort.Strings(lines)
		set("native list", strings.Join(lines, "\n"), nil)
	} else {
		set("native list", "", err)
	}

	out, err := luaRun("list", "--porcelain")
	set("lua list", out, err)
	out, err = luaRun("path")
	set("lua path", out, err)

	for name := range rockNames {
		info, err := native.Show(ctx, name)
		if err == nil {
			mods := slices.Clone(info.Modules)
			sort.Strings(mods)

			var deps []string
			for _, d := range info.Dependencies {
				deps = append(deps, d.Name)
			}

			set("native show "+name, fmt.Sprintf("package=%s version=%s summary=%q license=%q homepage=%q modules=%v deps=%v",
				info.Package, info.Version, info.Summary, info.License, info.Homepage, mods, deps), nil)
		} else {
			set("native show "+name, "", err)
		}

		out, err := luaRun("show", "--porcelain", name)
		set("lua show "+name, out, err)
	}

	for name := range moduleNames {
		p, found, err := native.Which(ctx, name)
		set("native which "+name, fmt.Sprintf("%s found=%v", p, found), err)

		out, err := luaRun("which", name)
		set("lua which "+name, out, err)
	}

	return view
}

// compareReaders requires the native readers and the lua engine's commands to
// agree on every tree of run: the rocks List and `list` report, the version,
// modules and description fields of Show and `show`, and the file Which and
// `which` resolve a module to.
func compareReaders(t *testing.T, label string, run interopRun) {
	t.Helper()

	for i, view := range run.views {
		tl := fmt.Sprintf("%s, tree %d", label, i)

		var luaList []string

		for line := range strings.SplitSeq(strings.TrimSpace(view["lua list"]), "\n") {
			if f := strings.Split(line, "\t"); len(f) >= 2 {
				luaList = append(luaList, f[0]+" "+f[1])
			}
		}

		sort.Strings(luaList)
		assert.Equal(t, strings.Join(luaList, "\n"), view["native list"], "[%s] List vs `list`", tl)

		for key, out := range view {
			if name, ok := strings.CutPrefix(key, "lua show "); ok {
				native := view["native show "+name]
				if strings.HasPrefix(native, "error: ") {
					native = "error"
				}

				assert.Equal(t, showFromPorcelain(out), native, "[%s] Show vs `show` of %s", tl, name)
			}

			if name, ok := strings.CutPrefix(key, "lua which "); ok {
				want := " found=false"
				if first, _, _ := strings.Cut(out, "\n"); !strings.Contains(out, "error: ") {
					want = first + " found=true"
				}

				assert.Equal(t, want, view["native which "+name], "[%s] Which vs `which` of %s", tl, name)
			}
		}
	}
}

// showFromPorcelain projects what `show --porcelain` printed onto the fields
// of the native Show view, so the two can be compared; a failed `show` maps to
// the error mark alone.
func showFromPorcelain(out string) string {
	if strings.Contains(out, "error: ") {
		return "error"
	}

	fields := map[string]string{}

	var mods, deps []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "\t")
		switch {
		case len(f) >= 2 && f[0] == "module":
			mods = append(mods, f[1])
		case len(f) >= 2 && f[0] == "dependency":
			deps = append(deps, strings.Fields(f[1])[0])
		case len(f) == 2:
			fields[f[0]] = f[1]
		}
	}

	sort.Strings(mods)

	return fmt.Sprintf("package=%s version=%s summary=%q license=%q homepage=%q modules=%v deps=%v",
		fields["package"], fields["version"], fields["summary"], fields["license"], fields["homepage"], mods, deps)
}
