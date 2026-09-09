package client_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// discardLogger silences the engine's stdout/stderr drain. A nil logger means
// slog.Default, which floods the test output with the multi-KB help text these
// probes dispatch to force cfg.init.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// luaProbe evaluates a Lua expression in the engine's VM and returns it as a
// string. Every assertion below reads in-VM cfg state, since that — not the Go
// side — is what the embedded LuaRocks actually acts on.
func luaProbe(t *testing.T, e *client.LuaEngine, expr string) string {
	t.Helper()

	require.NoError(t, e.LState().DoString("return "+expr), "probe %s", expr)

	v := e.LState().Get(-1)
	e.LState().Pop(1)

	return v.String()
}

// bootedEngine returns an engine whose cfg.init has run. cfg.init is lazy —
// it fires inside the first dispatch, not in boot — so probing cfg without a
// dispatch first reads an uninitialized table.
func bootedEngine(t *testing.T, cfg rocks.Config) *client.LuaEngine {
	t.Helper()

	e := client.NewLuaEngine(cfg, manif.FileStore{}, discardLogger())
	require.NoError(t, e.Call([]string{"help"}), "dispatch to force cfg.init")

	return e
}

// TestLuaEngine_TreeLayoutComesFromHardcoded pins the tree layout the native
// backend also uses (tree/paths.go). The engine used to install these three
// via a generated LUAROCKS_CONFIG file on the strength of a comment claiming
// the vendored cfg.lua ignores the hardcoded keys; it does not (core/cfg.lua
// make_defaults reads all three). This is the gate on that.
func TestLuaEngine_TreeLayoutComesFromHardcoded(t *testing.T) {
	t.Parallel()

	e := bootedEngine(t, luaTestCfg(t))

	require.Equal(t, "/share/tarantool/rocks", luaProbe(t, e, "require('luarocks.core.cfg').rocks_subdir"))
	require.Equal(t, "/share/tarantool", luaProbe(t, e, "require('luarocks.core.cfg').lua_modules_path"))
	require.Equal(t, "/lib/tarantool", luaProbe(t, e, "require('luarocks.core.cfg').lib_modules_path"))
}

// TestLuaEngine_WritesNoConfigFile asserts the engine owns no on-disk state:
// no generated config file, and nothing pointing LuaRocks at one.
func TestLuaEngine_WritesNoConfigFile(t *testing.T) {
	t.Parallel()

	before := configTempDirs(t)

	e := bootedEngine(t, luaTestCfg(t))

	require.Equal(t, before, configTempDirs(t),
		"engine created a generated-config temp dir")

	// The VM must see exactly what the host env holds — an override of its own
	// would be a config file the engine wrote. An unset host var reads as the
	// Lua nil, hence the tostring.
	want := os.Getenv("LUAROCKS_CONFIG")
	if want == "" {
		want = "nil"
	}

	require.Equal(t, want, luaProbe(t, e, "tostring(os.getenv('LUAROCKS_CONFIG'))"),
		"engine pointed the VM at a config file of its own")
}

// configTempDirs counts the temp dirs the removed writeConfigFile used to
// create. It is a count rather than a presence check because the tests run in
// parallel and other engines may hold their own.
func configTempDirs(t *testing.T) int {
	t.Helper()

	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "go-luarocks-cfg-*"))
	require.NoError(t, err, "glob temp dirs")

	return len(matches)
}

// TestLuaEngine_ConfigServersReachCfg asserts Config.Servers reaches the
// embedded LuaRocks as cfg.rocks_servers. Before this the lua backend only ever
// saw hardcoded.lua's default plus per-call --server overrides, so it queried
// different servers than the native backend given the same Config.
func TestLuaEngine_ConfigServersReachCfg(t *testing.T) {
	t.Parallel()

	cfg := luaTestCfg(t)
	cfg.Servers = []string{"http://first.example/", "http://second.example/"}

	e := bootedEngine(t, cfg)

	require.Equal(t, "2", luaProbe(t, e, "tostring(#require('luarocks.core.cfg').rocks_servers)"),
		"configured servers should REPLACE the default, not stack onto it")
	require.Equal(t, "http://first.example/", luaProbe(t, e, "require('luarocks.core.cfg').rocks_servers[1]"))
	require.Equal(t, "http://second.example/", luaProbe(t, e, "require('luarocks.core.cfg').rocks_servers[2]"))
}

// TestLuaEngine_NoConfigServersKeepsDefault pins the other half: an unset
// Config.Servers leaves the Tarantool default in place.
func TestLuaEngine_NoConfigServersKeepsDefault(t *testing.T) {
	t.Parallel()

	e := bootedEngine(t, luaTestCfg(t))

	require.Equal(t, "http://rocks.tarantool.org/",
		luaProbe(t, e, "require('luarocks.core.cfg').rocks_servers[1]"))
}

// TestLuaEngine_PwdAnchoredToWorkingDir covers the second job the generated
// file used to do. The shell-out fs backend runs cfg.variables.PWD to learn its
// base directory, so this asserts the command's OUTPUT, not its text — a path
// holding a single quote is the case that separates correct escaping from a
// command that silently reports the wrong directory.
func TestLuaEngine_PwdAnchoredToWorkingDir(t *testing.T) {
	t.Parallel()

	work := filepath.Join(t.TempDir(), "it's here")
	require.NoError(t, os.MkdirAll(work, 0o750), "create quoted work dir")

	cfg := luaTestCfg(t)
	cfg.WorkingDir = work

	e := bootedEngine(t, cfg)

	require.Equal(t, work, luaProbe(t, e, "require('luarocks.fs').current_dir()"),
		"fs.current_dir did not resolve to WorkingDir")
}
