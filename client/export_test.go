package client

import (
	"log/slog"
	"time"

	rocks "github.com/tarantool/go-luarocks"
)

// This file is the white-box bridge for the external (client_test) test
// package. It re-exports the unexported types, constructors and dispatch
// internals that the engine/luaEngine tests exercise, so those tests can live
// in package client_test (satisfying the testpackage linter) while still
// driving internal dispatch machinery.

// NativeEngine aliases the unexported nativeEngine for backend-type assertions.
type NativeEngine = nativeEngine

// LuaEngine aliases the unexported luaEngine so tests can hold a typed handle.
type LuaEngine = luaEngine

// NewRocksWithEngine builds a *Rocks wired to the supplied Engine, the way the
// dispatcher tests inject a fake backend (they previously used the unexported
// engine struct field directly).
func NewRocksWithEngine(e Engine) *Rocks {
	return &Rocks{engine: e}
}

// Engine returns the backend the facade dispatches to, for backend-selection
// assertions.
//
//nolint:ireturn // test bridge: returns the Engine interface so external tests can type-assert the concrete backend
func (r *Rocks) Engine() Engine { return r.engine }

// NewLuaEngine constructs a luaEngine, mirroring the package-internal
// constructor used by the engine tests.
func NewLuaEngine(cfg rocks.Config, store rocks.ManifestStore, logger *slog.Logger) *LuaEngine {
	return newLuaEngine(cfg, store, logger)
}

// Boot builds and warms one VM and discards it, so the boot smoke tests can
// assert the VM comes up without holding one.
func (e *LuaEngine) Boot() error {
	L, err := e.warmVM()
	if err != nil {
		return err
	}

	L.Close()

	return nil
}

// Call exposes luaEngine.call for the boot/help smoke tests.
func (e *LuaEngine) Call(argv []string) error { return e.call(argv) }

// Probe evaluates a Lua expression on a pooled VM and returns the result as a
// string. The VM is warm, so cfg.init has already run and cfg is populated —
// which is what the config tests read. It is retired afterwards like any other
// dispatch, so a probe cannot leak state into a later call.
func (e *LuaEngine) Probe(expr string) (string, error) {
	L, err := e.acquire()
	if err != nil {
		return "", err
	}

	defer e.release(L)

	if err := L.DoString("return " + expr); err != nil {
		return "", err
	}

	v := L.Get(-1)
	L.Pop(1)

	return v.String(), nil
}

// Contaminate runs a Lua statement on a pooled VM and then retires that VM
// exactly as a dispatch would. It is the dirtying half of the state-isolation
// tests: whatever module-level state the statement writes — a cfg flag, a
// server appended to cfg.rocks_servers, a scheduled rollback, a pushed
// directory, a cached manifest — must be invisible to every later call.
//
// It deliberately does NOT reset anything. There is no reset path to test: the
// isolation comes from retiring the VM, so a test that could only pass by
// clearing state would be testing a mechanism the engine does not have.
func (e *LuaEngine) Contaminate(stmt string) error {
	L, err := e.acquire()
	if err != nil {
		return err
	}

	defer e.release(L)

	return L.DoString(stmt)
}

// WaitForPooledVM blocks until the background warm-up has restocked the pool,
// or gives up. Tests that assert on pool contents need it because release
// warms the replacement asynchronously.
func WaitForPooledVM(e *LuaEngine) bool {
	const (
		attempts = 200
		delay    = 25 * time.Millisecond
	)

	for range attempts {
		if len(e.pool) > 0 {
			return true
		}

		time.Sleep(delay)
	}

	return false
}

// SetCallImpl installs the dispatch test seam (the unexported callImpl field)
// so argv-recording tests can intercept dispatch without booting the VM.
func (e *LuaEngine) SetCallImpl(fn func(argv []string) (string, error)) {
	e.callImpl = fn
}

// FindRockspecIn exposes findRockspecIn so the top-level-scan behavior can be
// unit-tested without a network fetch.
func FindRockspecIn(dir string) (string, error) { return findRockspecIn(dir) }

// FindSourceBaseDir exposes findSourceBaseDir so the find_base_dir descent
// logic can be unit-tested against synthetic on-disk layouts.
func FindSourceBaseDir(dir string, spec *rocks.Rockspec) (string, error) {
	return findSourceBaseDir(dir, spec)
}

// WrapBinScripts exposes the should_wrap_bin_scripts decision for unit tests.
func WrapBinScripts(spec *rocks.Rockspec) bool { return wrapBinScripts(spec) }

// CheckSupportedPlatforms exposes the supported-platforms gate for unit tests.
func CheckSupportedPlatforms(spec *rocks.Rockspec, plats []string) error {
	return checkSupportedPlatforms(spec, plats)
}

// UpsertProvider exposes the active-provider ordering helper for unit tests.
func UpsertProvider(list []string, provider string, active bool) []string {
	return upsertProvider(list, provider, active)
}

// ManifestProvider exposes the manifest provider-lookup builder for unit tests.
func ManifestProvider(m *rocks.Manifest) func(item string) (string, string, bool) {
	return manifestProvider(m)
}

// ResolveInstalledDeps exposes the per-entry dependency resolver for unit tests.
func ResolveInstalledDeps(spec *rocks.Rockspec, m *rocks.Manifest) map[string]string {
	return resolveInstalledDeps(spec, m)
}

// RemoveProvider exposes the provider-removal helper for unit tests.
func RemoveProvider(index map[string][]string, provider string) { removeProvider(index, provider) }
