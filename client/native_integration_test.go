//go:build integration

package client_test

// End-to-end gate for the NATIVE (pure-Go) backend's real-rock install path.
//
// The other end-to-end gate (lua_integration_test.go, -tags
// integration_luaengine) boots the embedded LuaRocks VM; the dispatch tests
// inject a fake engine; and client_test.go only exercises Make against a
// local fixture. None of them fetch+build a real rock from luarocks.org
// through the native backend — which is exactly the path that regressed
// (Install built against the rockspec download dir instead of fetching
// source.url, so standard rocks failed at build). This test closes that gap.
//
// Tagged `integration`. Skips gracefully when tarantool/headers are absent
// or luarocks.org is unreachable, so it never fails an offline CI run.

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// reachable reports whether a TCP connection to host:port succeeds within
// the timeout. Used to skip (not fail) when the network or luarocks.org is
// unavailable.
func reachable(ctx context.Context, hostPort string, timeout time.Duration) bool {
	d := net.Dialer{Timeout: timeout}

	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return false
	}

	_ = conn.Close()

	return true
}

// TestNative_Install_RealRock_EndToEnd installs a real pure-Lua rock
// (`inspect`) from luarocks.org through the native backend and asserts it
// deploys into cfg.Tree such that the real tarantool interpreter can
// require it. This is the regression gate for the source-fetch fix: before
// it, the native Install built against the rockspec download directory and
// failed with "no such file inspect.lua".
func TestNative_Install_RealRock_EndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH; native real-rock install needs a real interpreter to verify require: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin)) // <prefix>/bin/tarantool -> <prefix>
	incDir := filepath.Join(prefix, "include", "tarantool")

	if !reachable(ctx, "luarocks.org:443", 5*time.Second) {
		t.Skip("luarocks.org:443 not reachable; native real-rock install is network-gated")
	}

	tree := t.TempDir()
	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: tree,
		Servers:    []string{"https://luarocks.org/"},
		Tarantool: rocks.TarantoolConfig{
			Executable: ttBin,
			Prefix:     prefix,
			IncludeDir: incDir,
		},
	})
	require.NoError(t, err, "New")

	// `inspect` is the simplest target: a single pure-Lua module, no deps.
	err = r.Install(ctx, "inspect", client.InstallOpts{
		Version: ">= 3.0.0",
		Deps:    client.DepsAll,
	})
	require.NoError(t, err, "native Install(inspect)")

	// Gate 1: the module landed at the native deploy path, and the manifest
	// read path (r.Which) agrees.
	p, ok, err := r.Which(ctx, "inspect")
	require.NoError(t, err, "Which(inspect)")
	require.True(t, ok, "Which(inspect) not found after native Install")

	_, err = os.Stat(p)
	require.NoError(t, err, "Which path %q does not exist", p)

	deployed := filepath.Join(tree, "share", "tarantool", "inspect.lua")

	_, err = os.Stat(deployed)
	require.NoError(t, err, "deployed module missing at %s", deployed)

	// Gate 2: the real tarantool interpreter can require it with the tree on
	// its package path — the ultimate proof the rock is usable, not just
	// present.
	luaPath := filepath.Join(tree, "share", "tarantool", "?.lua") + ";" +
		filepath.Join(tree, "share", "tarantool", "?", "init.lua") + ";;"

	env := append(os.Environ(), "LUA_PATH="+luaPath)

	cmd := exec.CommandContext(ctx, ttBin, "-e", "assert(require('inspect')); os.exit(0)")
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	assert.NoError(t, err, "tarantool require('inspect') failed: %s", out)

	// Gate 3 (glr-52g): Remove undeploys the module and cleans the manifest, so
	// the file is gone and Which no longer resolves it.
	require.NoError(t, r.Remove(ctx, "inspect", client.RemoveOpts{}), "native Remove(inspect)")

	_, err = os.Stat(deployed)
	assert.True(t, os.IsNotExist(err), "deployed module should be gone after Remove, stat err = %v", err)

	_, ok, err = r.Which(ctx, "inspect")
	require.NoError(t, err, "Which after Remove")
	assert.False(t, ok, "Which must not resolve inspect after Remove")
}

// TestNative_Install_SrcRock_EndToEnd installs `say`, whose newest version
// is published only as a `.src.rock` (a zip the http backend must unpack)
// AND whose bundled source ships a `rockspecs/` directory of extra rockspec
// files. It exercises two parts of the fix the inspect case does not: the
// `.rock` archive unzip, and the top-level-only rockspec scan that ignores
// the bundled rockspecs. Asserts via r.Which (manifest + on-disk).
func TestNative_Install_SrcRock_EndToEnd(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin))

	if !reachable(ctx, "luarocks.org:443", 5*time.Second) {
		t.Skip("luarocks.org:443 not reachable; network-gated")
	}

	tree := t.TempDir()
	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: tree,
		Servers:    []string{"https://luarocks.org/"},
		Tarantool: rocks.TarantoolConfig{
			Executable: ttBin,
			Prefix:     prefix,
			IncludeDir: filepath.Join(prefix, "include", "tarantool"),
		},
	})
	require.NoError(t, err, "New")

	err = r.Install(ctx, "say", client.InstallOpts{
		Version: ">= 1.0",
		Deps:    client.DepsNone,
	})
	require.NoError(t, err, "native Install(say) from .src.rock")

	p, ok, err := r.Which(ctx, "say")
	require.NoError(t, err, "Which(say)")
	require.True(t, ok, "Which(say) not found after native Install")

	_, err = os.Stat(p)
	require.NoError(t, err, "Which path %q does not exist", p)
}
