// Package rockspec implements the sandboxed gopher-lua evaluator for
// .rockspec source files. It is the only package on the rockspec parse
// path that imports gopher-lua (client imports it too). Eval yields a
// plain-data *rocks.Rockspec; downstream packages (fetch, build, deps, manif)
// consume that struct without touching Lua at all.
//
// The usual pipeline is Eval → MergePlatforms(spec, RuntimePlatforms()) →
// Validate: MergePlatforms folds the per-platform overlays (source.platforms,
// build.platforms, ...) into the base fields for the running OS, and Validate
// enforces required fields, the version grammar, the rockspec_format ceiling
// (SupportedRockspecFormat) and the build-type allowlist, surfacing
// rocks.ErrUnsupportedRockspecFeature for anything outside the implemented
// subset.
//
// Sandbox:
//
// The evaluator reproduces upstream's rockspec loading environment
// (core/persist.lua): the chunk runs against an initially-empty global table
// whose metatable __index records every undefined global READ and returns nil.
// NO standard libraries (base/string/table/math/os) are opened, so require,
// io, package, debug, and every library function are unreachable, and a
// rockspec that calls e.g. string.format or os.getenv fails to load — exactly
// as upstream, which then rejects the recorded globals (check_undeclared_globals).
//
// Tests in eval_test.go assert that library and host-reaching globals are
// unreachable from rockspec code.
package rockspec
