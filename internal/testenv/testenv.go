// Package testenv holds the environment discipline the module's tests share:
// what a test does when the machine lacks something it needs, and how a test
// keeps LuaRocks away from the developer's home and system configuration.
//
// # Missing prerequisites
//
// A test that needs more than Go — a tool on PATH, the tarantool headers, the
// network — ends through [Missingf] when that is absent. By default the test
// is skipped, so the suite stays usable on a machine that has only some of
// the prerequisites. A skip, however, passes: a CI job that runs the suite on
// a runner missing tarantool would skip every backend-parity test and still
// report success. Setting the environment variable named by [StrictEnv] to
// "1" turns each of those skips into a failure, so a job that is meant to
// exercise the full suite cannot go green without having run it.
//
// A missing network is the one exception: a run can be offline by design,
// as the sandboxed runner is by default. Setting the variable named by
// [OfflineEnv] to "1" declares that, and a test ending through [NoNetworkf]
// is then skipped even in strict mode — nothing is missing that the run
// meant to provide.
//
// # Hermetic home
//
// The lua engine hands HOME, the XDG base directories and
// LUAROCKS_SYSCONFDIR through to the embedded LuaRocks, which reads the
// user's and the system's config files from them and writes its download
// cache under the user's cache directory. The native engine reads
// LUAROCKS_SYSCONFDIR too, for the command wrappers it generates. [Hermetic]
// and [HermeticMain] point all of those at fresh directories, so a test run
// neither writes into the developer's home nor depends on their LuaRocks
// configuration. TMPDIR, PATH and the toolchain variables (CC, CFLAGS, …)
// are left alone: t.TempDir needs the first, and the builds need the rest.
//
// # Fixed umask
//
// The modes of the files and directories a test creates follow the process
// umask, which differs between machines. [SetSuiteUmask], called from
// TestMain, sets the usual one, so a suite that compares modes, such as the
// backend-parity harness, compares the same ones everywhere.
//
// # Leaks
//
// [CheckLeaks] ends a TestMain that fails the run when the tests leave
// goroutines behind, and [RequireNoFDLeak] fails a test whose operation,
// repeated, leaves file descriptors open. Both first close the idle
// connections of the HTTP transports the process shares, which are no leak
// ([CloseIdleConnections]).
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// StrictEnv is the environment variable that switches on strict mode. With
// it set to "1" a missing prerequisite fails the test; with it unset, or set
// to anything else, the test is skipped.
const StrictEnv = "GO_LUAROCKS_INTEGRATION_STRICT"

// OfflineEnv is the environment variable that declares a run offline by
// design. With it set to "1" a test that cannot reach the network is skipped
// even in strict mode.
const OfflineEnv = "GO_LUAROCKS_OFFLINE"

// dirPerm is the mode of the directories Hermetic and HermeticMain create.
const dirPerm = 0o700

// hermeticVars are the variables Hermetic and HermeticMain override.
var hermeticVars = []string{
	"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "LUAROCKS_SYSCONFDIR",
}

// original is the process environment for hermeticVars as it was before
// any test changed it: package initialization runs before TestMain.
var original = func() map[string]string {
	env := map[string]string{}

	for _, key := range hermeticVars {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}

	return env
}()

// Strict reports whether strict mode is on.
func Strict() bool {
	return os.Getenv(StrictEnv) == "1"
}

// Missingf ends the test tb because a prerequisite is missing. The message,
// formatted from format and args as by fmt.Sprintf, should say what is
// missing and why the test needs it. The test is skipped unless [Strict]
// reports true, in which case it fails. Like t.Skipf and t.Fatalf, Missingf
// must be called from the goroutine running the test.
func Missingf(tb testing.TB, format string, args ...any) {
	tb.Helper()

	if Strict() {
		tb.Fatalf("missing prerequisite ("+StrictEnv+"=1): "+format, args...)
	}

	tb.Skipf(format, args...)
}

// Offline reports whether the run is declared offline.
func Offline() bool {
	return os.Getenv(OfflineEnv) == "1"
}

// NoNetworkf ends the test tb because a network endpoint it needs is
// unreachable. In a run declared offline ([Offline]) the test is skipped;
// otherwise the network is a prerequisite like any other and NoNetworkf
// behaves as [Missingf].
func NoNetworkf(tb testing.TB, format string, args ...any) {
	tb.Helper()

	if Offline() {
		tb.Skipf("offline run ("+OfflineEnv+"=1): "+format, args...)
	}

	Missingf(tb, format, args...)
}

// Dirs are the directories a hermetic environment points LuaRocks at. They
// exist and start out empty; in particular SysconfDir holds no
// config-5.1.lua.
type Dirs struct {
	// Home is HOME.
	Home string
	// CacheHome is XDG_CACHE_HOME, Home/.cache; LuaRocks caches downloads
	// in its luarocks subdirectory.
	CacheHome string
	// ConfigHome is XDG_CONFIG_HOME, Home/.config.
	ConfigHome string
	// DataHome is XDG_DATA_HOME, Home/.local/share.
	DataHome string
	// SysconfDir is LUAROCKS_SYSCONFDIR, the directory LuaRocks reads the
	// system configuration from in place of /etc/luarocks.
	SysconfDir string
}

// newDirs creates the directories of a hermetic environment under root.
func newDirs(root string) (Dirs, error) {
	home := filepath.Join(root, "home")
	d := Dirs{
		Home:       home,
		CacheHome:  filepath.Join(home, ".cache"),
		ConfigHome: filepath.Join(home, ".config"),
		DataHome:   filepath.Join(home, ".local", "share"),
		SysconfDir: filepath.Join(root, "sysconf"),
	}

	for _, dir := range []string{d.CacheHome, d.ConfigHome, d.DataHome, d.SysconfDir} {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return Dirs{}, fmt.Errorf("testenv: %w", err)
		}
	}

	return d, nil
}

// env returns the value of each of hermeticVars in d.
func (d Dirs) env() map[string]string {
	return map[string]string{
		"HOME":                d.Home,
		"XDG_CACHE_HOME":      d.CacheHome,
		"XDG_CONFIG_HOME":     d.ConfigHome,
		"XDG_DATA_HOME":       d.DataHome,
		"LUAROCKS_SYSCONFDIR": d.SysconfDir,
	}
}

// Hermetic points HOME, XDG_CACHE_HOME, XDG_CONFIG_HOME, XDG_DATA_HOME and
// LUAROCKS_SYSCONFDIR at fresh directories under tb.TempDir for the rest of
// the test and returns them. It uses tb.Setenv, so it cannot be used in a
// parallel test or a test with parallel ancestors; for a package whose
// tests run in parallel, use [HermeticMain] instead.
func Hermetic(tb testing.TB) Dirs {
	tb.Helper()

	d, err := newDirs(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}

	for key, value := range d.env() {
		tb.Setenv(key, value)
	}

	return d
}

// HermeticMain runs the tests of m in a hermetic environment and returns the
// exit code for os.Exit: it does for the whole test binary what [Hermetic]
// does for one test, so it also covers parallel tests. Call it from
// TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(testenv.HermeticMain(m)) }
func HermeticMain(m *testing.M) int {
	root, err := os.MkdirTemp("", "go-luarocks-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)

		return 1
	}

	code := runHermetic(m, root)

	if err := os.RemoveAll(root); err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
	}

	return code
}

// runHermetic is HermeticMain once root exists.
func runHermetic(m *testing.M, root string) int {
	d, err := newDirs(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	for key, value := range d.env() {
		if err := os.Setenv(key, value); err != nil {
			fmt.Fprintln(os.Stderr, "testenv:", err)

			return 1
		}
	}

	return m.Run()
}

// Original returns the value the environment variable key had when the
// test binary started, before [Hermetic] or [HermeticMain] changed it, and
// whether it was set. It answers for the variables those two override;
// for any other key it reports the variable as unset.
func Original(key string) (string, bool) {
	v, ok := original[key]

	return v, ok
}
