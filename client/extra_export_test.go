package client

import (
	"context"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// This file extends export_test.go with additional white-box hooks used by
// coverage_extra_test.go (package client_test): small pure helpers and the
// native/lua engine internals whose error paths are not reachable through the
// public facade alone.

// NormalizeOpenMode exposes the io.open mode-string rewrite for unit tests.
func NormalizeOpenMode(mode string) string { return normalizeOpenMode(mode) }

// DepsModeArg exposes the DepsPolicy → --deps-mode mapping for unit tests.
func DepsModeArg(p DepsPolicy) string { return depsModeArg(p) }

// UnwrapPathErr exposes the os.PathError unwrapping helper for unit tests.
func UnwrapPathErr(err error) error { return unwrapPathErr(err) }

// SysconfDir exposes the LUAROCKS_SYSCONFDIR resolution for unit tests.
func SysconfDir() string { return sysconfDir() }

// ParseOsExit exposes the os.exit sentinel parser for unit tests.
func ParseOsExit(msg string) (int, bool) { return parseOsExit(msg) }

// EvalAndPrepare exposes the eval+merge+validate pipeline for unit tests.
func EvalAndPrepare(specPath string, cfg rocks.Config) (*rocks.Rockspec, error) {
	return evalAndPrepare(specPath, cfg)
}

// ZipDir exposes the recursive zip helper so its error paths can be unit-tested.
func ZipDir(outPath, srcDir string) error { return zipDir(outPath, srcDir) }

// ZipSingleFile exposes the single-entry zip helper for error-path unit tests.
func ZipSingleFile(outPath, srcPath, entryName string) error {
	return zipSingleFile(outPath, srcPath, entryName)
}

// DirFiles exposes the Download directory-diff helper for unit tests.
func DirFiles(dir string) map[string]bool { return dirFiles(dir) }

// LuaEngineCleanup exposes luaEngine.cleanup so the temp-dir/LState teardown
// can be exercised deterministically instead of waiting for the finalizer.
func LuaEngineCleanup(e *LuaEngine) { e.cleanup() }

// NativeInstalledLookup exposes nativeEngine.installedLookup for unit tests.
func NativeInstalledLookup(e *NativeEngine) (deps.InstalledLookup, error) {
	return e.installedLookup()
}

// NativeInstallStep exposes nativeEngine.installStep so the nil-Rockspec
// fetch branch can be driven directly with a file:// step URL.
func NativeInstallStep(e *NativeEngine, ctx context.Context, step rocks.InstallStep) error {
	return e.installStep(ctx, step)
}

// NativeFetchSource exposes nativeEngine.fetchSource so the source-root
// decision — backend-reported root vs. archive descent — can be unit-tested
// against real fetches without driving a whole build.
func NativeFetchSource(
	e *NativeEngine, ctx context.Context, spec *rocks.Rockspec, tmp string,
) (string, error) {
	return e.fetchSource(ctx, spec, tmp)
}
