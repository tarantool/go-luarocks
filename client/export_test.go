package client

import (
	"fmt"
	"io"
	"log/slog"
	"slices"
	"time"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
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

// FreshPackagePath returns package.path of a VM straight out of newVM, before
// any dispatch has run on it: what every require LuaRocks makes searches until
// luarocks.loader appends the rock trees.
func (e *LuaEngine) FreshPackagePath() (string, error) {
	L, err := e.newVM()
	if err != nil {
		return "", err
	}

	defer L.Close()

	return L.GetField(L.GetGlobal("package"), "path").String(), nil
}

// Output runs one lua backend command and returns what it printed.
func (e *LuaEngine) Output(argv []string) (string, error) { return e.callViaState(argv) }

// UnresolvedRequires runs the commands one after another on a single fresh VM,
// the way a warm-up and a dispatch share one, and returns each module a require
// asked for that package.preload does not hold — each one a lookup on
// package.path. A searcher placed ahead of preload records them;
// luarocks.loader later puts its own ahead of that one, which consults only
// the tree manifests before handing over.
func (e *LuaEngine) UnresolvedRequires(argvs ...[]string) ([]string, error) {
	L, err := e.newVM()
	if err != nil {
		return nil, err
	}

	defer L.Close()

	pkg := L.GetGlobal("package")
	preload := L.GetField(pkg, "preload")

	var names []string

	L.SetGlobal("glr_test_searcher", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if L.GetField(preload, name) == lua.LNil && !slices.Contains(names, name) {
			names = append(names, name)
		}

		L.Push(lua.LString("\n\tnot recorded by glr_test_searcher"))

		return 1
	}))

	if err := L.DoString("table.insert(package.loaders, 1, glr_test_searcher)"); err != nil {
		return nil, err
	}

	restore := redirectIO(L, io.Discard, io.Discard)
	defer restore()

	for _, argv := range argvs {
		if err := L.DoString(dispatchString(wrapperExec, "go-luarocks", argv)); err != nil {
			if code, ok := parseOsExit(err.Error()); !ok || code != 0 {
				return nil, fmt.Errorf("%v: %w", argv, err)
			}
		}
	}

	return names, nil
}

// Call exposes luaEngine.call for the boot/help smoke tests.
func (e *LuaEngine) Call(argv []string) error { return e.call(argv) }

// Probe evaluates a Lua expression on a pooled VM and returns the result as a
// string. The VM is warm, so cfg.init has already run and cfg is populated —
// which is what the config tests read. It is retired afterwards like any other
// dispatch, so a probe cannot leak state into a later call.
func (e *LuaEngine) Probe(expr string) (string, error) {
	L, warmed, err := e.acquire()
	if err != nil {
		return "", err
	}

	defer e.release(L, warmed)

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
	L, warmed, err := e.acquire()
	if err != nil {
		return err
	}

	defer e.release(L, warmed)

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

// AddProvider exposes the provider-list ordering helper for unit tests.
func AddProvider(list []string, provider string) []string {
	return addProvider(list, provider)
}

// ManifestProviders exposes the manifest provider-lookup builder for unit tests.
func ManifestProviders(m *rocks.Manifest) func(kind tree.ItemKind, item string) []string {
	return manifestProviders(m)
}

// ResolveInstalledDeps exposes the per-entry dependency resolver for unit tests.
func ResolveInstalledDeps(spec *rocks.Rockspec, m *rocks.Manifest) map[string]string {
	return resolveInstalledDeps(spec, m)
}

// RocksTrees exposes the rock-tree list of a Config for unit tests.
func RocksTrees(cfg rocks.Config) []string { return rocksTrees(cfg) }

// RemoveProvider exposes the provider-removal helper for unit tests.
func RemoveProvider(index map[string][]string, provider string) { removeProvider(index, provider) }

// PreloadPaths returns the embedded-FS path of every module the engine
// preloads.
func PreloadPaths() []string {
	paths := make([]string, 0, len(luaPreloadMap))
	for _, path := range luaPreloadMap {
		paths = append(paths, path)
	}

	return paths
}

// AbsentModules returns the modules the engine answers with a failing stub
// instead of their source.
func AbsentModules() []string { return slices.Clone(luaAbsentModules) }

// DeduceBaseDir exposes deduceBaseDir for the differential tests.
func DeduceBaseDir(url string) string { return deduceBaseDir(url) }

// SplitNamespacedName exposes splitNamespacedName for the differential tests.
func SplitNamespacedName(arg string) (string, string) { return splitNamespacedName(arg) }

// BundledSource exposes bundledSource for the containment tests.
var BundledSource = bundledSource

// CopyBackFiles exposes copyBackFiles for the containment tests.
var CopyBackFiles = copyBackFiles
