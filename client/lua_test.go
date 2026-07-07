package client_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
	lua "github.com/yuin/gopher-lua"
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

func TestLuaEngine_BootSucceeds_NoOp(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)
	require.NoError(t, e.Boot(), "boot")
	require.NotNil(t, e.LState(), "boot did not cache an LState")
}

func TestLuaEngine_OsGetenvOverride(t *testing.T) {
	t.Parallel()

	cfg := luaTestCfg(t)
	cfg.Tarantool.Prefix = "/test-prefix"
	e := client.NewLuaEngine(cfg, manif.FileStore{}, nil)
	require.NoError(t, e.Boot(), "boot")
	require.NoError(t, e.LState().DoString("return require('luarocks.core.hardcoded').PREFIX"), "probe DoString")
	got := e.LState().Get(-1)
	e.LState().Pop(1)
	require.True(t, got.Type() == lua.LTString && got.String() == "/test-prefix",
		"hardcoded.PREFIX = %v (%s); want \"/test-prefix\"", got, got.Type())
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
