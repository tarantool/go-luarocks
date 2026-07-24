package client

import (
	"log/slog"

	rocks "github.com/tarantool/go-luarocks"
	lua "github.com/yuin/gopher-lua"
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

// Boot exposes luaEngine.boot for the boot smoke tests.
func (e *LuaEngine) Boot() error { return e.boot() }

// Call exposes luaEngine.call for the boot/help smoke tests.
func (e *LuaEngine) Call(argv []string) error { return e.call(argv) }

// LState exposes the cached *lua.LState so tests can probe in-VM state.
func (e *LuaEngine) LState() *lua.LState { return e.lstate }

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
