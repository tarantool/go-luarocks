// Package build implements the four supported rockspec build backends —
// builtin, cmake, make, command — plus the none no-op. RunBackend is the
// single public entry point used by the facade: it dispatches on the
// rockspec's build.type and builds the rock from srcDir — the builtin backend
// compiles/copies its modules and stages the result under destDir, the make,
// cmake and command backends install into the rock's install dir — for
// tree.Deploy. Flags carries the caller-tunable knobs (verbosity, extra
// CMake/make variables).
//
// The package never sets process environment variables. Every
// subprocess invocation builds its env via cmd.Env, layering on top of
// os.Environ() with the five canonical TARANTOOL_DIR / LUA_* vars and any
// rockspec-supplied K=V pairs.
//
// All subprocesses receive ctx via exec.CommandContext and stay in the
// caller's process group, so the terminal's Ctrl-C reaches them; cancelling
// ctx kills the running tool, and the call returns within about a second
// even if a process the tool started still holds its output. With
// rocks.Config.IsolateBuilds, on unix, each tool runs in a process group of
// its own, which cancelling ctx kills whole; the caller must then cancel ctx
// on SIGINT and SIGTERM itself.
//
// Output shared-library extension is `.so` on both linux and macOS
// (upstream luarocks sets `lib_extension = "so"` for unix unconditionally,
// even on macOS where the linker emits a -bundle).
//
// An unrecognized build.type surfaces rocks.ErrUnsupportedRockspecFeature;
// a C-extension module built without configured Tarantool headers surfaces
// rocks.ErrMissingTarantoolHeaders.
package build
