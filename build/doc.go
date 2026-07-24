// Package build implements the four supported rockspec build backends —
// builtin, cmake, make, command — plus the none no-op. RunBackend is the
// single public entry point used by the facade: it dispatches on the
// rockspec's build.type, compiles/copies the rock's modules from srcDir, and
// stages the result under destDir for tree.Deploy. Flags carries the
// caller-tunable knobs (verbosity, extra CMake/make variables).
//
// The package never sets process environment variables. Every
// subprocess invocation builds its env via cmd.Env, layering on top of
// os.Environ() with the five canonical TARANTOOL_DIR / LUA_* vars and any
// rockspec-supplied K=V pairs.
//
// All subprocesses receive ctx via exec.CommandContext. Output
// shared-library extension is `.so` on both linux and macOS (upstream
// luarocks sets `lib_extension = "so"` for unix unconditionally,
// even on macOS where the linker emits a -bundle).
//
// An unrecognized build.type surfaces rocks.ErrUnsupportedRockspecFeature;
// a C-extension module built without configured Tarantool headers surfaces
// rocks.ErrMissingTarantoolHeaders.
package build
