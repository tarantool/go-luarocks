package client_test

import (
	"runtime"
	"testing"
	"time"
	"weak"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// TestLuaEngine_CollectedWhenUnreachable checks that an engine nothing
// references any more is collected, the warm VM its last dispatch left in its
// pool with it. The functions a VM holds used to capture the engine, which
// carries a finalizer, and a cycle through an object with a finalizer is
// never collected: every lua-backend client leaked a VM, some 5 MB, for the
// life of the process.
func TestLuaEngine_CollectedWhenUnreachable(t *testing.T) {
	t.Parallel()

	engine := func() weak.Pointer[client.LuaEngine] {
		e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, nil)
		require.NoError(t, e.Call([]string{"help"}), "help")
		require.True(t, client.WaitForPooledVM(e), "a warm VM is pooled after the dispatch")

		return weak.Make(e)
	}()

	collected := func() bool {
		runtime.GC()

		return engine.Value() == nil
	}

	require.Eventually(t, collected, 10*time.Second, 10*time.Millisecond, "the unreachable engine is collected")
}
