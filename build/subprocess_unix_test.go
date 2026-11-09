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
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// runCmdFunc is build.RunCmd or build.RunCmdIsolated.
type runCmdFunc func(ctx context.Context, name string, args []string, cwd string, extraEnv map[string]string) error

// startSleepingShell runs, through run, a shell that starts a `sleep 30` of
// its own and waits for it, and returns once the shell is up: a channel
// with run's result and the pids of the shell and of its sleep.
func startSleepingShell(ctx context.Context, t *testing.T, run runCmdFunc) (<-chan error, int, int) {
	t.Helper()

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")

	done := make(chan error, 1)

	go func() {
		done <- run(ctx, "sh", []string{"-c", `sleep 30 & echo "$$ $!" > "$0.tmp" && mv "$0.tmp" "$0"; wait`, pidFile}, dir, nil)
	}()

	var shell, sleep int

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(pidFile) //nolint:gosec // test temp path
		if err != nil {
			return false
		}

		f := strings.Fields(string(b))
		if len(f) != 2 {
			return false
		}

		shell, err = strconv.Atoi(f[0])
		if err != nil {
			return false
		}

		sleep, err = strconv.Atoi(f[1])

		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the shell starts")

	return done, shell, sleep
}

// requireReturnsAtOnce cancels the context and requires the runCmd whose
// result done carries to fail within 2 s.
func requireReturnsAtOnce(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()

	start := time.Now()

	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "a cancelled command fails")
	case <-time.After(5 * time.Second):
		t.Fatal("runCmd did not return after its context was cancelled")
	}

	assert.Less(t, time.Since(start), 2*time.Second, "runCmd returns at once")
}

// TestRunCmd_StaysInTheCallersProcessGroup — a build tool ran in a process
// group of its own, out of reach of the terminal's Ctrl-C, so a caller that
// died of it left make and the compilers running. By default the tool stays
// in the caller's group, and a cancelled runCmd still returns at once even
// though the tool's own child, which the cancellation does not kill, holds
// the output pipes.
func TestRunCmd_StaysInTheCallersProcessGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, shell, sleep := startSleepingShell(ctx, t, build.RunCmd)

	t.Cleanup(func() { _ = syscall.Kill(sleep, syscall.SIGKILL) })

	self, err := syscall.Getpgid(0)
	require.NoError(t, err)

	for _, pid := range []int{shell, sleep} {
		pgid, err := syscall.Getpgid(pid)
		require.NoError(t, err)
		assert.Equal(t, self, pgid, "process %d is in the caller's process group", pid)
	}

	requireReturnsAtOnce(t, cancel, done)
}

// TestRunCmd_CancelKillsTheProcessGroup — cancelling the context killed the
// shell alone: its `sleep`, like the compilers a make runs, lived on holding
// the output pipes, so runCmd did not return until it exited 30 s later.
// With the tool isolated, cancelling must kill the whole group and return
// at once, leaving no process behind.
func TestRunCmd_CancelKillsTheProcessGroup(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done, shell, sleep := startSleepingShell(ctx, t, build.RunCmdIsolated)

	t.Cleanup(func() { _ = syscall.Kill(sleep, syscall.SIGKILL) })

	pgid, err := syscall.Getpgid(shell)
	require.NoError(t, err)
	require.Equal(t, shell, pgid, "the shell leads a process group of its own")

	requireReturnsAtOnce(t, cancel, done)

	assert.Eventually(t, func() bool {
		return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH)
	}, 3*time.Second, 10*time.Millisecond, "no process of the group is left")
}

// TestRunBackend_IsolateBuilds checks that every backend runs its build
// tools in the caller's process group by default and each in a group of its
// own with Config.IsolateBuilds. A tool leads a group of its own exactly
// when a process group with its pid exists.
func TestRunBackend_IsolateBuilds(t *testing.T) {
	const probe = `if kill -0 -$$ 2>/dev/null; then echo own; else echo caller; fi`

	// The stub stands for make, cmake and cc: it logs its group and creates
	// the file a compiler is to write with -o.
	stub := func(log string) string {
		return `#!/bin/sh
` + probe + ` >> "` + log + `"
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then mkdir -p "$(dirname "$out")" && : > "$out"; fi
exit 0
`
	}

	tests := []struct {
		name string
		tool string
		file string
		spec func(log string) *rocks.Rockspec
	}{
		{"builtin", "cc", "src/foo.c", func(string) *rocks.Rockspec {
			return &rocks.Rockspec{Build: rocks.Build{
				Type:    "builtin",
				Modules: map[string]rocks.Module{"foo": {Path: "src/foo.c"}},
			}}
		}},
		{"cmake", "cmake", "CMakeLists.txt", func(string) *rocks.Rockspec {
			return &rocks.Rockspec{Package: "foo", Version: "1.0-1", Build: rocks.Build{Type: "cmake"}}
		}},
		{"make", "make", "Makefile", func(string) *rocks.Rockspec {
			return &rocks.Rockspec{Package: "foo", Version: "1.0-1", Build: rocks.Build{Type: "make"}}
		}},
		{"command", "", "", func(log string) *rocks.Rockspec {
			return &rocks.Rockspec{Build: rocks.Build{
				Type:           "command",
				BuildCommand:   probe + ` >> "` + log + `"`,
				InstallCommand: probe + ` >> "` + log + `"`,
			}}
		}},
	}

	for _, tt := range tests {
		for _, isolate := range []bool{false, true} {
			t.Run(tt.name+"/isolate="+strconv.FormatBool(isolate), func(t *testing.T) {
				dir := t.TempDir()
				log := filepath.Join(dir, "log")
				srcDir := filepath.Join(dir, "src")
				require.NoError(t, os.MkdirAll(srcDir, 0o750))

				if tt.file != "" {
					require.NoError(t, build.WriteFile(filepath.Join(srcDir, tt.file), "# stub\n"))
				}

				stubDir := filepath.Join(dir, "stub")
				if tt.tool != "" {
					require.NoError(t, build.WriteFile(filepath.Join(stubDir, tt.tool), stub(log)))
					require.NoError(t, build.ChmodX(filepath.Join(stubDir, tt.tool)))
				}

				t.Setenv("PATH", stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv("CC", "")

				cfg := rocks.Config{
					Tarantool:     rocks.TarantoolConfig{IncludeDir: "/opt/tt/include/tarantool"},
					IsolateBuilds: isolate,
				}
				require.NoError(t, build.RunBackend(context.Background(), tt.spec(log), srcDir, filepath.Join(dir, "dest"), cfg))

				raw, err := build.ReadFile(log)
				require.NoError(t, err, "no build tool ran")

				want := "caller"
				if isolate {
					want = "own"
				}

				for line := range strings.Lines(strings.TrimSpace(string(raw))) {
					assert.Equal(t, want, strings.TrimSpace(line), "the process group of a build tool")
				}
			})
		}
	}
}
