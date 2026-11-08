package client_test

import (
	"os"
	"testing"

	"github.com/tarantool/go-luarocks/internal/testenv"
)

// TestMain runs the whole package in a hermetic home. Most of its tests boot
// the lua engine, whose LuaRocks reads the user's and the system's config and
// writes a download cache under the user's home, and most of them run in
// parallel, which rules out a per-test t.Setenv.
func TestMain(m *testing.M) {
	os.Exit(testenv.HermeticMain(m))
}
