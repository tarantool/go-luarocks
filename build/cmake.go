package build

import (
	"context"
	"os"
	"sort"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// cmakeEnvVars are the environment variables upstream cmake.run copies into
// the -D cache variables when set (cmake.lua:19-21), so cmake's find_package
// can locate custom modules/libs/includes the user pointed at via the env.
var cmakeEnvVars = []string{"CMAKE_MODULE_PATH", "CMAKE_LIBRARY_PATH", "CMAKE_INCLUDE_PATH"}

// cmakeBuildDir is the build directory upstream cmake.run configures, under
// the source dir (cmake.lua:51).
const cmakeBuildDir = "build.luarocks"

// runCMake implements the `cmake` build backend.
//
// Sequence (mirrors upstream luarocks/src/luarocks/build/cmake.lua; luarocks
// does NOT inject LUA_INCLUDE_DIR, CMAKE_MODULE_LINKER_FLAGS, etc. — the
// rockspec supplies them via build.variables):
//
//  1. cmake -H. -Bbuild.luarocks -D<K>=<V> ...                 (configure)
//  2. cmake --build build.luarocks --config Release            (build)
//  3. cmake --build build.luarocks --target install --config Release
//
// The working directory is srcDir, and the build happens out of tree in its
// build.luarocks subdirectory, so the configure step cannot overwrite a
// Makefile or anything else of the source tree.
//
// One piece of plumbing is ours: a rockspec that sets no CMAKE_INSTALL_PREFIX
// gets it set to the rock's $(PREFIX), its install dir, where tree.Deploy picks
// the installed files up, rather than left to CMake's /usr/local default as
// upstream leaves it. Without a rock name (a hand-built spec) the prefix is
// destDir. A rockspec that sets CMAKE_INSTALL_PREFIX — to "$(PREFIX)", as
// rocks do — installs exactly where upstream's would.
func runCMake(ctx context.Context, spec *rocks.Rockspec, srcDir, destDir string, cfg rocks.Config) error {
	env := buildEnv(cfg)
	vars := buildVars(spec, cfg)

	// Effective -D cache variables: build.variables (with $(NAME) expansion),
	// plus the three CMAKE_*_PATH env vars when set and not already declared
	// (upstream copies them in unconditionally, but a nil os.getenv drops the
	// key — so only add non-empty ones).
	dvars := make(map[string]string, len(spec.Build.Variables)+len(cmakeEnvVars))
	for k, v := range spec.Build.Variables {
		dvars[k] = substituteVars(v, vars)
	}

	for _, name := range cmakeEnvVars {
		if _, ok := dvars[name]; ok {
			continue
		}

		if v := os.Getenv(name); v != "" {
			dvars[name] = v
		}
	}

	if _, ok := dvars["CMAKE_INSTALL_PREFIX"]; !ok {
		dvars["CMAKE_INSTALL_PREFIX"] = destDir
		if spec.Package != "" {
			dvars["CMAKE_INSTALL_PREFIX"] = vars["PREFIX"]
		}
	}

	keys := make([]string, 0, len(dvars))
	for k := range dvars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	dirs := []string{"-H.", "-B" + cmakeBuildDir}
	configure := make([]string, 0, len(dirs)+len(keys))
	configure = append(configure, dirs...)

	for _, k := range keys {
		configure = append(configure, "-D"+k+"="+dvars[k])
	}

	err := runCmd(ctx, "cmake", configure, srcDir, env)
	if err != nil {
		return err
	}

	// Upstream cmake.run honors build_pass/install_pass ONLY for rockspec_format
	// >= 3.0; older formats force both phases (cmake.lua:56-73).
	honorPasses := rockspecFormatAtLeast3(spec.RockspecFormat)

	if !honorPasses || passEnabled(spec.Build.BuildPass) {
		if err := runCmd(ctx, "cmake", []string{"--build", cmakeBuildDir, "--config", "Release"}, srcDir, env); err != nil {
			return err
		}
	}

	if honorPasses && !passEnabled(spec.Build.InstallPass) {
		return nil
	}

	return runCmd(ctx, "cmake", []string{"--build", cmakeBuildDir, "--target", "install", "--config", "Release"}, srcDir, env)
}

// rockspecFormatAtLeast3 reports whether the rockspec_format (default "1.0") is
// >= "3.0", the threshold above which cmake.run honors build/install_pass.
func rockspecFormatAtLeast3(format string) bool {
	if format == "" {
		format = "1.0"
	}

	fv, err := deps.ParseVersion(format)
	if err != nil {
		return false
	}

	ceiling, err := deps.ParseVersion("3.0")
	if err != nil {
		return false
	}

	return deps.Compare(fv, ceiling) >= 0
}
