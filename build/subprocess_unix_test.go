//go:build unix

package build_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/build"
)

// TestRunCmd_CancelKillsTheProcessGroup — cancelling the context killed the
// shell alone: its `sleep`, like the compilers a make runs, lived on holding
// the output pipes, so runCmd did not return until it exited 30 s later.
// Cancelling must kill the whole group and return at once, leaving no
// process behind.
func TestRunCmd_CancelKillsTheProcessGroup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- build.RunCmd(ctx, "sh", []string{"-c", `echo $$ > "$0.tmp" && mv "$0.tmp" "$0"; sleep 30; true`, pidFile}, dir, nil)
	}()

	var pgid int

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(pidFile) //nolint:gosec // test temp path
		if err != nil {
			return false
		}

		pgid, err = strconv.Atoi(strings.TrimSpace(string(b)))

		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the shell starts")

	// Let the shell get to its sleep.
	time.Sleep(100 * time.Millisecond)

	start := time.Now()

	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "a cancelled command fails")
	case <-time.After(5 * time.Second):
		t.Fatal("runCmd did not return after its context was cancelled")
	}

	assert.Less(t, time.Since(start), 2*time.Second, "runCmd returns at once")

	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
	}, 3*time.Second, 10*time.Millisecond, "no process of the group is left")
}
