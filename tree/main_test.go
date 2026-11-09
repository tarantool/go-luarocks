package tree_test

import (
	"os"
	"testing"

	"github.com/tarantool/go-luarocks/internal/testenv"
	"go.uber.org/goleak"
)

// TestMain fails the run when the tests leave goroutines behind, such as one
// still waiting for the tree lock.
func TestMain(m *testing.M) {
	os.Exit(testenv.CheckLeaks(m.Run(), func() error { return goleak.Find() }))
}
