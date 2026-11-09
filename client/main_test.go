package client_test

import (
	"os"
	"testing"

	"github.com/tarantool/go-luarocks/internal/testenv"
	"go.uber.org/goleak"
)

// TestMain runs the whole package in a hermetic home. Most of its tests boot
// the lua engine, whose LuaRocks reads the user's and the system's config and
// writes a download cache under the user's home, and most of them run in
// parallel, which rules out a per-test t.Setenv. It runs under a fixed
// umask, so the backend-parity harness compares the same modes on every
// machine, and fails the run when the tests leave goroutines behind.
func TestMain(m *testing.M) {
	testenv.SetSuiteUmask()
	os.Exit(testenv.CheckLeaks(testenv.HermeticMain(m), func() error { return goleak.Find() }))
}
