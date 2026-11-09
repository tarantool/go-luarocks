//go:build unix

package testenv

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
)

// The umask is a property of the process, which every goroutine shares, so a
// test cannot change it while others run. RunWithUmask runs a test in a child
// process of the test binary instead, whose umask that test, the only one it
// runs, sets through UmaskChild.
const (
	// umaskEnv carries the umask of a child process, in octal.
	umaskEnv = "GO_LUAROCKS_TEST_UMASK"
	// umaskArgEnv carries the argument of a child process.
	umaskArgEnv = "GO_LUAROCKS_TEST_UMASK_ARG"
)

// SuiteUmask is the umask SetSuiteUmask sets: the usual one, under which a
// tree is readable by everyone.
const SuiteUmask = 0o022

// SetSuiteUmask sets the umask of the process to SuiteUmask, so that the
// modes of the files and directories a suite creates do not depend on the
// umask of whoever runs it, and a suite comparing them compares the same
// modes everywhere. Call it from TestMain, before any test runs: the umask is
// shared by every goroutine of the process.
func SetSuiteUmask() {
	syscall.Umask(SuiteUmask)
}

// RunWithUmask runs the test of this test binary named name in a child
// process whose umask is mask, passing it arg, which the test reads with
// UmaskChild, and fails tb when the child fails.
func RunWithUmask(tb testing.TB, name string, mask int, arg string) {
	tb.Helper()

	exe, err := os.Executable()
	if err != nil {
		tb.Fatalf("testenv.RunWithUmask: %v", err)
	}

	cmd := exec.CommandContext(tb.Context(), exe, "-test.run=^"+name+"$", "-test.count=1", "-test.v")

	cmd.Env = append(os.Environ(), umaskEnv+"="+strconv.FormatInt(int64(mask), 8), umaskArgEnv+"="+arg)

	out, err := cmd.CombinedOutput()
	if err != nil {
		tb.Fatalf("testenv.RunWithUmask: %s under umask %#o: %v\n%s", name, mask, err, out)
	}
}

// UmaskChild sets the umask of the process to the one RunWithUmask asked for
// and returns the argument it passed, when the test runs in a child process
// of RunWithUmask; otherwise it skips the test. The umask is restored when
// the test ends, so that what the test binary writes on exit, such as its
// coverage data, is not created under it.
func UmaskChild(tb testing.TB) string {
	tb.Helper()

	mask, ok := os.LookupEnv(umaskEnv)
	if !ok {
		tb.Skip("run by testenv.RunWithUmask only")
	}

	m, err := strconv.ParseInt(mask, 8, 32)
	if err != nil {
		tb.Fatalf("testenv.UmaskChild: %s=%q: %v", umaskEnv, mask, err)
	}

	old := syscall.Umask(int(m))

	tb.Cleanup(func() { syscall.Umask(old) })

	return os.Getenv(umaskArgEnv)
}
