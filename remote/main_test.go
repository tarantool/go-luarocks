package remote_test

import (
	"os"
	"testing"

	"github.com/tarantool/go-luarocks/internal/testenv"
	"go.uber.org/goleak"
)

// TestMain fails the run when the tests leave goroutines behind, such as the
// read loop of a connection whose response body nobody closed.
func TestMain(m *testing.M) {
	os.Exit(testenv.CheckLeaks(m.Run(), func() error { return goleak.Find() }))
}
