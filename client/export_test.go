package client

import (
	"log/slog"

	rocks "github.com/tarantool/go-luarocks"
	lua "github.com/yuin/gopher-lua"
)

// This file is the white-box bridge for the external (client_test) test
// package. It re-exports the unexported types, constructors and dispatch
// internals that the luaEngine tests exercise, so those tests can live in
// package client_test while still driving internal dispatch machinery.

// LuaEngine aliases the unexported luaEngine so tests can hold a typed handle.
type LuaEngine = luaEngine

// NewRocksWithEngine builds a *Rocks wired to the supplied Engine, the way the
// dispatcher tests inject a fake backend.
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
