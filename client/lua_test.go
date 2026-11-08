package client_test

import (
	"fmt"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// luaEngine must satisfy the Engine contract (mirrors the nativeEngine
// assertion in native_test.go); the New() wiring relies on this.
var _ client.Engine = (*client.LuaEngine)(nil)

// luaTestCfg returns a Config with the required Tree and a WorkingDir, both
// pointing at a fresh temp dir, plus a Tarantool prefix for env-override
// assertions.
func luaTestCfg(t *testing.T) rocks.Config {
	t.Helper()
	dir := t.TempDir()

	return rocks.Config{
		Tree:       dir,
		WorkingDir: dir,
	}
}

// TestLuaEngine_ConcurrentCalls — one client used from many goroutines: each
// call takes a VM and starts warming the next one in the background, and
// waits for that warm-up when it returns. The calls shared one WaitGroup for
// those warm-ups, so one call's Wait ran while another's acquire added to it
// from zero, which the WaitGroup contract forbids and which panics with
// "WaitGroup is reused before previous Wait has returned". Every call must
// return its own result, and none may panic.
func TestLuaEngine_ConcurrentCalls(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 16
		calls      = 50
	)

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)

	var wg sync.WaitGroup

	errs := make(chan error, goroutines*calls)

	for g := range goroutines {
		wg.Go(func() {
			for i := range calls {
				want := g*calls + i

				got, err := e.Probe(strconv.Itoa(want))
				if err == nil && got != strconv.Itoa(want) {
					err = fmt.Errorf("probe %d returned %s", want, got)
				}

				if err != nil {
					errs <- err
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
}

func TestLuaEngine_BootSucceeds_NoOp(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)
	require.NoError(t, e.Boot(), "boot")
}

func TestLuaEngine_OsGetenvOverride(t *testing.T) {
	t.Parallel()

	cfg := luaTestCfg(t)
	cfg.Tarantool.Prefix = "/test-prefix"
	e := client.NewLuaEngine(cfg, manif.FileStore{}, nil)

	got, err := e.Probe("require('luarocks.core.hardcoded').PREFIX")
	require.NoError(t, err, "probe")
	require.Equal(t, "/test-prefix", got, "hardcoded.PREFIX")
}

// probeHardcoded returns hardcoded.lua's value for key as seen by a pooled VM.
func probeHardcoded(t *testing.T, e *client.LuaEngine, key string) string {
	t.Helper()

	got, err := e.Probe("require('luarocks.core.hardcoded')." + key)
	require.NoError(t, err, "probe hardcoded.%s", key)

	return got
}

func TestLuaEngine_LuaBinDirFromExecutable(t *testing.T) {
	t.Parallel()

	// A flat SDK: the binary sits at the prefix root, there is no <prefix>/bin.
	cfg := luaTestCfg(t)
	cfg.Tarantool.Prefix = "/sdk/te350"
	cfg.Tarantool.Executable = "/sdk/te350/tarantool"
	e := client.NewLuaEngine(cfg, manif.FileStore{}, nil)
	require.NoError(t, e.Boot(), "boot")
	assert.Equal(t, "/sdk/te350", probeHardcoded(t, e, "PREFIX"))
	assert.Equal(t, "/sdk/te350", probeHardcoded(t, e, "LUA_BINDIR"))
}

func TestLuaEngine_LuaBinDirFromPrefix(t *testing.T) {
	t.Parallel()

	cfg := luaTestCfg(t)
	cfg.Tarantool.Prefix = "/opt/tt"
	e := client.NewLuaEngine(cfg, manif.FileStore{}, nil)
	require.NoError(t, e.Boot(), "boot")
	assert.Equal(t, "/opt/tt/bin", probeHardcoded(t, e, "LUA_BINDIR"))
}

func TestLuaEngine_WrapperExec_Help(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)
	require.NoError(t, e.Call([]string{"help"}), "call help")
}

func TestLuaEngine_MutexSerializes(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)

	var wg sync.WaitGroup

	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			errs[idx] = e.Call([]string{"help"})
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "concurrent call %d", i)
	}
}
