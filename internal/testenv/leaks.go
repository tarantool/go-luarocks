package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tarantool/go-luarocks/internal/transport"
)

// CloseIdleConnections closes the idle connections of the HTTP transports
// the rock server index and the source fetch share for the life of the
// process (internal/transport). Each idle connection holds two goroutines and
// a file descriptor, and is no leak; one still busy after this, such as one
// whose response body nobody closed, is.
func CloseIdleConnections() {
	const host = "leak-check.invalid"

	transport.For(host, nil).CloseIdleConnections()
	transport.For(host, []string{host}).CloseIdleConnections()
}

// CheckLeaks is the end of a TestMain that checks the test binary for leaked
// goroutines: given the exit code of m.Run and find, which reports the
// goroutines left running (goleak.Find, which a package's tests import:
// testenv itself depends on nothing outside the module), it returns the exit
// code for os.Exit. A run that failed keeps its code and is not checked, so a
// leak a failure left does not hide it.
//
//	func TestMain(m *testing.M) {
//		os.Exit(testenv.CheckLeaks(m.Run(), func() error { return goleak.Find() }))
//	}
func CheckLeaks(code int, find func() error) int {
	if code != 0 {
		return code
	}

	CloseIdleConnections()

	if err := find(); err != nil {
		fmt.Fprintln(os.Stderr, "testenv: the tests leaked goroutines:", err)

		return 1
	}

	return 0
}

// fdDir lists the file descriptors of the process, on Linux.
const fdDir = "/proc/self/fd"

// fdSettle is how long RequireNoFDLeak waits for descriptors closed by
// goroutines that outlive an operation, such as the one waiting for a
// subprocess, to go.
const fdSettle = 2 * time.Second

// fdPoll is how often RequireNoFDLeak counts the descriptors meanwhile.
const fdPoll = 100 * time.Millisecond

// openFDs returns what each open file descriptor of the process refers to,
// by descriptor, or false where the system does not tell.
func openFDs() (map[string]string, bool) {
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return nil, false
	}

	out := make(map[string]string, len(entries))

	for _, e := range entries {
		// The directory read itself holds one, gone by now.
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}

		out[e.Name()] = target
	}

	return out, true
}

// RequireNoFDLeak runs op once to warm up whatever it opens for the life of
// the process, then n times more, and fails tb when the process then holds
// more file descriptors than after the warm-up, naming the new ones. The
// count is the process's, so the test must not run in parallel with others.
// Where the system does not list the descriptors of a process (anything but
// Linux) the test is skipped.
func RequireNoFDLeak(tb testing.TB, n int, op func(i int)) {
	tb.Helper()

	if _, ok := openFDs(); !ok {
		tb.Skipf("%s does not list the file descriptors of the process", fdDir)
	}

	op(-1)
	CloseIdleConnections()

	before, _ := openFDs()

	for i := range n {
		op(i)
	}

	deadline := time.Now().Add(fdSettle)

	for {
		CloseIdleConnections()

		after, _ := openFDs()
		if len(after) <= len(before) {
			return
		}

		if time.Now().After(deadline) {
			var fresh []string

			for fd, target := range after {
				if before[fd] != target {
					fresh = append(fresh, fd+" -> "+target)
				}
			}

			slices.Sort(fresh)
			tb.Fatalf("%d operations leaked %d file descriptors (%d open before, %d after):\n%s",
				n, len(after)-len(before), len(before), len(after), strings.Join(fresh, "\n"))
		}

		time.Sleep(fdPoll)
	}
}
