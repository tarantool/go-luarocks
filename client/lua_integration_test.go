//go:build integration_luaengine

package client_test

// End-to-end gate for the gopher-lua backend. Unlike the dispatch tests
// (which inject a recorder seam), this test boots the real embedded LuaRocks
// VM, runs `make` against a pure-Lua rockspec staged on disk, and asserts the
// rock actually landed in cfg.Tree via the facade's read path (List/Which).
//
// Build-tagged so it does not run in the default `go test ./client/` — it is
// the real backend gate, invoked with `-tags integration_luaengine`.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// TestLuaEngine_Make_PureLuaFixture_EndToEnd stages a small pure-Lua rockspec
// + module under a temp WorkingDir, runs r.Make through the lua backend, and
// asserts the deployed module is discoverable in the tree. Mirrors the fixture
// pattern of client_test.go's TestMake_BuiltinPureLua_EndToEnd.
func TestLuaEngine_Make_PureLuaFixture_EndToEnd(t *testing.T) {
	// The lua backend runs real LuaRocks, which shells out to the configured
	// Lua interpreter (tarantool). hardcoded.lua resolves the interpreter under
	// LUA_BINDIR, which the engine derives from the directory holding
	// cfg.Tarantool.Executable (cfg.Tarantool.Prefix/bin when unset). Locate
	// a real tarantool and derive its prefix; skip when none is installed.
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH; lua-backend end-to-end test needs a real interpreter: %v", err)
	}
	prefix := filepath.Dir(filepath.Dir(ttBin)) // <prefix>/bin/tarantool -> <prefix>

	// Even a pure-Lua builtin make runs deps.check_lua_incdir, which requires a
	// lua.h matching the configured Lua version. Tarantool ships its headers
	// under <prefix>/include/tarantool; skip if they are not present (the build
	// genuinely cannot proceed without them — we must not fake a path).
	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		t.Skipf("tarantool lua.h not found at %s; lua-backend make needs Lua headers: %v", incDir, err)
	}

	src := t.TempDir()
	tree := t.TempDir()

	must := func(p, body string) {
		t.Helper()
		full := filepath.Join(src, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
	must("src/foo.lua", "return {}\n")
	must("src/foo/bar.lua", "return {}\n")

	rockspec := `
package = "foo"
version = "1.0-1"
source = { url = "file://localhost/foo.tar.gz" }
build = {
   type = "builtin",
   modules = {
      ["foo"] = "src/foo.lua",
      ["foo.bar"] = "src/foo/bar.lua",
   },
}
`
	specPath := filepath.Join(src, "foo-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(rockspec), 0o644))

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: src,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
		},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}), "Make (lua backend)")

	// The real gate: the lua backend must have deployed the module into the
	// SAME tree layout the native backend uses (tree/paths.go). r.Which scans
	// the on-disk tree (tree.Open + tree.Which), independent of the manifest,
	// so it directly confirms the rock landed in cfg.Tree.
	p, ok, err := r.Which(context.Background(), "foo")
	require.NoError(t, err, "Which")
	require.True(t, ok, "Which(foo) returned not-found after lua-backend Make")
	_, err = os.Stat(p)
	require.NoError(t, err, "Which path %q does not exist", p)

	// Belt-and-suspenders: the module must be at the native DeployLuaDir path
	// and the per-rock rockspec must be installed under the rocks subdir.
	_, err = os.Stat(filepath.Join(tree, "share", "tarantool", "foo.lua"))
	assert.NoError(t, err, "deployed module missing at share/tarantool/foo.lua")
	_, err = os.Stat(filepath.Join(tree, "share", "tarantool", "rocks", "foo", "1.0-1", "foo-1.0-1.rockspec"))
	assert.NoError(t, err, "installed rockspec missing under rocks/foo/1.0-1")

	// NOTE: this test asserts via r.Which + filesystem checks because those are
	// the most direct on-disk gate, independent of the manifest. r.List also
	// works on lua-written trees now that manif.loadDepList accepts the empty
	// dependency table ({}) upstream emits for a no-dependency rock (see
	// manif/store.go and TestLoadDepList_EmptyUpstreamTable); the backend-parity
	// test (parity_test.go) exercises r.List across both backends.
}

// TestLuaEngine_Make_TarantoolDependency_Resolves is the regression gate for the
// tarantool/luarocks fork: a rockspec that declares `dependencies = {"tarantool
// >= ..."}` must resolve without trying to fetch a "tarantool" rock. The fork's
// util.lua registers `tarantool` as a provided dependency from
// TT_CLI_TARANTOOL_VERSION (there is no _TARANTOOL global in the gopher-lua VM),
// which newLuaEngine wires from cfg.Tarantool.Version. Stock upstream LuaRocks
// does NOT do this, so this would fail against the wrong vendored source.
func TestLuaEngine_Make_TarantoolDependency_Resolves(t *testing.T) {
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH: %v", err)
	}
	prefix := filepath.Dir(filepath.Dir(ttBin))
	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		t.Skipf("tarantool lua.h not found at %s: %v", incDir, err)
	}

	src := t.TempDir()
	tree := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "dep.lua"), []byte("return {}\n"), 0o644))
	// The tarantool dependency must be satisfied by rocks_provided, NOT fetched.
	rockspec := `
package = "dep"
version = "1.0-1"
source = { url = "file://localhost/dep.tar.gz" }
dependencies = { "tarantool >= 1.0" }
build = { type = "builtin", modules = { ["dep"] = "dep.lua" } }
`
	specPath := filepath.Join(src, "dep-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(rockspec), 0o644))

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: src,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
			Version:    "2.11.0-1", // value carries a dash, as the fork's matcher expects
		},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	// Without the provided-tarantool registration this Make fails trying to
	// locate a "tarantool" rock. With it, the dependency resolves and the rock
	// deploys.
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}), "Make with tarantool dependency")
	_, ok, err := r.Which(context.Background(), "dep")
	require.NoError(t, err, "Which(dep) after tarantool-dep make")
	require.True(t, ok, "Which(dep) after tarantool-dep make")
}

// TestLuaEngine_ReusedLState_SequentialMakes runs two Makes of different rocks
// on ONE engine and asserts both deploy. The name predates the current design:
// the engine once reused one LState for the *Rocks lifetime, where cfg cached
// after first init (cfg.initialized) could leak across calls. Each dispatch now
// gets its own VM, and this stays as the cross-call check the
// MutexSerializes test (two identical `help` calls) never exercised.
func TestLuaEngine_ReusedLState_SequentialMakes(t *testing.T) {
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH: %v", err)
	}
	prefix := filepath.Dir(filepath.Dir(ttBin))
	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		t.Skipf("tarantool lua.h not found at %s: %v", incDir, err)
	}

	work := t.TempDir()
	tree := t.TempDir()
	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: "2.11.0-1"},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	stage := func(name string) string {
		require.NoError(t, os.WriteFile(filepath.Join(work, name+".lua"), []byte("return {}\n"), 0o644))
		spec := "package = \"" + name + "\"\nversion = \"1.0-1\"\n" +
			"source = { url = \"file://localhost/x\" }\n" +
			"build = { type = \"builtin\", modules = { [\"" + name + "\"] = \"" + name + ".lua\" } }\n"
		p := filepath.Join(work, name+"-1.0-1.rockspec")
		require.NoError(t, os.WriteFile(p, []byte(spec), 0o644))
		return p
	}

	for _, name := range []string{"aaa", "bbb"} {
		require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: stage(name)}), "Make %s on reused LState", name)
		_, ok, err := r.Which(context.Background(), name)
		require.NoError(t, err, "Which(%s) after make on reused LState", name)
		require.True(t, ok, "Which(%s) after make on reused LState", name)
	}
}

// TestLuaEngine_Make_CustomBuildTypeFromTree installs a luarocks-build-<type>
// backend into the tree and then makes a rock of that build type. The engine
// starts every VM with an empty package.path, so the backend module is found
// only because luarocks.loader appends the rock trees to it
// (cfg.init_package_paths) — this is the gate on that. The tree is the user
// tree, <WorkingDir>/.rocks, the layout tt uses: cfg.rocks_trees holds it from
// cfg.init on. A tree named only by --tree joins that list after the loader has
// set package.path, so a backend installed there is not found; that does not
// depend on package.path starting empty.
func TestLuaEngine_Make_CustomBuildTypeFromTree(t *testing.T) {
	t.Parallel()

	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin))

	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		t.Skipf("tarantool lua.h not found at %s: %v", incDir, err)
	}

	work := t.TempDir()
	tree := filepath.Join(work, ".rocks")

	must := func(name, body string) string {
		t.Helper()

		p := filepath.Join(work, name)
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))

		return p
	}

	// The backend writes one module into the rock's lua dir, which build.lua
	// creates before running the driver and deploys after it.
	must("glrtest.lua", `
local dir = require("luarocks.dir")
local path = require("luarocks.path")

local glrtest = {}

function glrtest.run(rockspec)
   local out = dir.path(path.lua_dir(rockspec.name, rockspec.version), "glrtest_built.lua")
   local f = assert(io.open(out, "w"))
   f:write("return 'built by luarocks.build.glrtest'\n")
   f:close()
   return true
end

return glrtest
`)
	backendSpec := must("luarocks-build-glrtest-1.0-1.rockspec", `
package = "luarocks-build-glrtest"
version = "1.0-1"
source = { url = "file://localhost/x" }
build = { type = "builtin", modules = { ["luarocks.build.glrtest"] = "glrtest.lua" } }
`)
	appSpec := must("glrapp-1.0-1.rockspec", `
rockspec_format = "3.0"
package = "glrapp"
version = "1.0-1"
source = { url = "file://localhost/x" }
build_dependencies = { "luarocks-build-glrtest" }
build = { type = "glrtest" }
`)

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: "2.11.0-1"},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	ctx := context.Background()
	require.NoError(t, r.Make(ctx, client.MakeOpts{RockspecPath: backendSpec}), "Make the backend")
	require.NoError(t, r.Make(ctx, client.MakeOpts{RockspecPath: appSpec}), "Make a rock of the custom build type")

	_, err = os.Stat(filepath.Join(tree, "share", "tarantool", "glrtest_built.lua"))
	require.NoError(t, err, "the custom backend did not build the rock")
}
