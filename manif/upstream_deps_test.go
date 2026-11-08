package manif_test

// The dependency entries of a tree manifest against upstream's own code: a
// manifest upstream wrote must come back from ReadTree and WriteTree byte for
// byte, and upstream's next write of a manifest WriteTree wrote, the one
// writer.add_to_manifest and writer.remove_from_manifest make after an
// install or a removal, must accept it and leave it as it is.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/luaref"
	"github.com/tarantool/go-luarocks/manif"
)

// depsRefChunk drives upstream over one tree, the stub cfg's root_dir, with
// tarantool provided as tt provides it. It takes writer's update_dependencies
// and manif's postprocess_dependencies, both module-local, and returns:
//
//   - build(), the manifest of a tree of four rocks as upstream writes it
//     after installing them: app depends on tarantool and vrock without
//     constraints, on the namespaced user/nsrock and on checks with two
//     constraints, one with a revision;
//   - rewrite(file), what upstream writes back after loading the manifest
//     file as manif.load_manifest does (load, postprocess, type check) and
//     recomputing its dependencies as add_to_manifest does;
//   - namespace_file(name, version), the rock_namespace file of a rock.
const depsRefChunk = `
local update_dependencies, postprocess_dependencies = ...
local cfg = require("luarocks.core.cfg")
local core = require("luarocks.core.manif")
local manif = require("luarocks.manif")
local path = require("luarocks.path")
local persist = require("luarocks.persist")
local queries = require("luarocks.queries")
local type_manifest = require("luarocks.type.manifest")

cfg.cache.rocks_provided = { lua = "5.1-1", tarantool = "2.11.0-1" }
local rocks_dir = path.rocks_dir(cfg.root_dir)

local function dep_list(...)
   local out = {}
   for _, s in ipairs({...}) do
      table.insert(out, assert(queries.from_dep_string(s)))
   end
   return out
end

local function installed(modules, commands)
   return { { arch = "installed", modules = modules, commands = commands or {} } }
end

local function build()
   local manifest = {
      repository = {
         app = { ["1.0-1"] = installed({ app = "app.lua" }, { ["app-cli"] = "app-cli" }) },
         vrock = { ["2.0-1"] = installed({ vrock = "vrock.lua" }) },
         nsrock = { ["1.0-1"] = installed({ nsrock = "nsrock.lua" }) },
         checks = { ["3.1.0-2"] = installed({ checks = "checks.lua" }) },
      },
      modules = {
         app = { "app/1.0-1" },
         vrock = { "vrock/2.0-1" },
         nsrock = { "nsrock/1.0-1" },
         checks = { "checks/3.1.0-2" },
      },
      commands = { ["app-cli"] = { "app/1.0-1" } },
      dependencies = {
         app = { ["1.0-1"] = dep_list("tarantool", "vrock", "user/nsrock", "checks >= 3.1.0-2, < 4") },
         vrock = { ["2.0-1"] = dep_list() },
         nsrock = { ["1.0-1"] = dep_list("tarantool") },
         checks = { ["3.1.0-2"] = dep_list() },
      },
   }
   core.cache_manifest(rocks_dir, nil, manifest)
   update_dependencies(manifest, "one")
   return assert(persist.save_from_table_to_string(manifest))
end

local function rewrite(file)
   local manifest, globals = assert(core.manifest_loader(file, rocks_dir))
   postprocess_dependencies(manifest)
   assert(type_manifest.check(manifest, globals))
   update_dependencies(manifest, "one")
   return assert(persist.save_from_table_to_string(manifest))
end

local function namespace_file(name, version)
   return path.rock_namespace_file(name, version)
end

return build, rewrite, namespace_file
`

// depsRef is a reference VM running depsRefChunk.
type depsRef struct {
	vm                            *luaref.VM
	build, rewrite, namespaceFile lua.LValue
}

func newDepsRef(t *testing.T) *depsRef {
	t.Helper()

	vm, err := luaref.New()
	require.NoError(t, err, "boot reference")
	t.Cleanup(vm.Close)

	addToManifest, err := vm.Function("luarocks.manif.writer", "add_to_manifest")
	require.NoError(t, err)
	update, err := luaref.Upvalue(addToManifest, "update_dependencies")
	require.NoError(t, err)

	loadManifest, err := vm.Function("luarocks.manif", "load_manifest")
	require.NoError(t, err)
	postprocess, err := luaref.Upvalue(loadManifest, "postprocess_dependencies")
	require.NoError(t, err)

	chunk, err := vm.L.LoadString(depsRefChunk)
	require.NoError(t, err)

	res, err := vm.CallValue(chunk, update, postprocess)
	require.NoError(t, err)
	require.Len(t, res, 3)

	ref := &depsRef{vm: vm, build: res[0], rewrite: res[1], namespaceFile: res[2]}

	// nsrock is installed from the user namespace, as get_versions checks.
	ns, err := ref.call(ref.namespaceFile, lua.LString("nsrock"), lua.LString("1.0-1"))
	require.NoError(t, err)
	vm.AddFile(ns, "user")

	return ref
}

func (r *depsRef) call(fn lua.LValue, args ...lua.LValue) (string, error) {
	res, err := r.vm.CallValue(fn, args...)
	if err != nil {
		return "", err
	}

	return lua.LVAsString(res[0]), nil
}

// upstreamRewrite returns what upstream writes back over the manifest text.
func (r *depsRef) upstreamRewrite(t *testing.T, text string) string {
	t.Helper()

	const file = "/luaref-input/manifest"

	r.vm.AddFile(file, text)

	out, err := r.call(r.rewrite, lua.LString(file))
	require.NoError(t, err, "upstream rewrites the manifest:\n%s", text)

	return out
}

// writeTree writes m with FileStore and returns the text.
func writeTree(t *testing.T, m *rocks.Manifest) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, manif.FileStore{}.WriteTree(dir, m))

	b, err := os.ReadFile(filepath.Join(dir, "manifest")) //nolint:gosec // test temp path
	require.NoError(t, err)

	return string(b)
}

// TestFileStoreTree_UpstreamDependenciesRoundTrip reads a manifest upstream
// wrote and writes it back: the dependency entries keep their empty
// constraints tables and their namespaces, the text comes out byte for byte,
// and upstream's next write accepts it.
func TestFileStoreTree_UpstreamDependenciesRoundTrip(t *testing.T) {
	t.Parallel()

	ref := newDepsRef(t)

	upstream, err := ref.call(ref.build)
	require.NoError(t, err, "upstream builds the manifest")
	require.Contains(t, upstream, `namespace = "user"`, "the reference wrote no namespace")

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest"), []byte(upstream), 0o600))

	m, err := manif.FileStore{}.ReadTree(dir)
	require.NoError(t, err)
	assert.Equal(t, []rocks.Dep{
		{Name: "tarantool"},
		{Name: "vrock"},
		{Name: "nsrock", Namespace: "user"},
		{Name: "checks", Constraints: []rocks.VersionConstraint{
			{Op: ">=", Version: rocks.Version{Raw: "3.1.0-2", Components: []float64{3, 1, 0}, Revision: 2, HasRevision: true}},
			{Op: "<", Version: rocks.Version{Raw: "4", Components: []float64{4}}},
		}},
	}, m.Dependencies["app"]["1.0-1"])

	written := writeTree(t, m)
	assert.Equal(t, upstream, written, "the manifest upstream wrote does not round-trip")
	assert.Equal(t, written, ref.upstreamRewrite(t, written), "upstream's next write changes the manifest")
}

// TestFileStoreTree_UnconstrainedDependenciesUpstreamRewrite writes the
// manifest the native backend makes for the same tree, with dependencies
// that have no constraints, one of them on the provided tarantool, and a
// namespaced one: upstream's next write, which used to fail the assertion of
// vers.match_constraints on the missing constraints table, accepts it, and
// it is the manifest upstream writes for that tree.
func TestFileStoreTree_UnconstrainedDependenciesUpstreamRewrite(t *testing.T) {
	t.Parallel()

	ref := newDepsRef(t)

	entry := func(module string, deps map[string]string) rocks.RepoEntry {
		return rocks.RepoEntry{Arch: "installed", Modules: map[string]string{module: module + ".lua"}, Dependencies: deps}
	}
	app := entry("app", map[string]string{"vrock": "2.0-1", "nsrock": "1.0-1", "checks": "3.1.0-2"})
	app.Commands = map[string]string{"app-cli": "app-cli"}

	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"app":    {"1.0-1": app},
			"vrock":  {"2.0-1": entry("vrock", nil)},
			"nsrock": {"1.0-1": entry("nsrock", nil)},
			"checks": {"3.1.0-2": entry("checks", nil)},
		},
		Modules: map[string][]string{
			"app": {"app/1.0-1"}, "vrock": {"vrock/2.0-1"}, "nsrock": {"nsrock/1.0-1"}, "checks": {"checks/3.1.0-2"},
		},
		Commands: map[string][]string{"app-cli": {"app/1.0-1"}},
		Dependencies: map[string]map[string][]rocks.Dep{
			"app": {"1.0-1": {
				{Name: "tarantool"},
				{Name: "vrock", Constraints: []rocks.VersionConstraint{}},
				{Name: "nsrock", Namespace: "user"},
				{Name: "checks", Constraints: []rocks.VersionConstraint{
					{Op: ">=", Version: rocks.Version{Raw: "3.1.0-2", Components: []float64{3, 1, 0}, Revision: 2, HasRevision: true}},
					{Op: "<", Version: rocks.Version{Raw: "4", Components: []float64{4}}},
				}},
			}},
			"vrock":  {"2.0-1": nil},
			"nsrock": {"1.0-1": {{Name: "tarantool"}}},
			"checks": {"3.1.0-2": {}},
		},
	}

	written := writeTree(t, m)
	assert.Equal(t, written, ref.upstreamRewrite(t, written), "upstream's next write changes the manifest")

	upstream, err := ref.call(ref.build)
	require.NoError(t, err, "upstream builds the manifest")
	assert.Equal(t, upstream, written, "the native manifest is not the one upstream writes")
}
