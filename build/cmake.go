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

// runCMake implements the `cmake` build backend.
//
// Sequence (pure pass-through — luarocks itself does NOT auto-inject
// LUA_INCLUDE_DIR, CMAKE_INSTALL_PREFIX, CMAKE_MODULE_LINKER_FLAGS, etc.;
// the rockspec is responsible for supplying them via build.variables):
//
//  1. cmake . -D<K>=<V> ...        (configure)
//  2. cmake --build .              (build)
//  3. cmake --install . --prefix destDir
//
// The working directory is srcDir. Builds happen in-tree, matching
// upstream's `-H. -Bbuild.luarocks` shape closely enough for our purposes
// (upstream uses `build.luarocks` as a subdir; we let cmake default to
// in-tree which gives the same outputs on the install step).
//
// The install --prefix argument is the one piece of plumbing we DO inject:
// the facade needs the rock's output rooted at destDir, and cmake's
// --install --prefix is the cleanest way to achieve that without
// post-hoc moving. This matches the spirit of upstream which expects
// CMAKE_INSTALL_PREFIX to be set to $(PREFIX); --prefix on --install
// overrides it.
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

	keys := make([]string, 0, len(dvars))
	for k := range dvars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	configure := make([]string, 0, 1+len(keys))
	configure = append(configure, ".")

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
		if err := runCmd(ctx, "cmake", []string{"--build", "."}, srcDir, env); err != nil {
			return err
		}
	}

	if honorPasses && !passEnabled(spec.Build.InstallPass) {
		return nil
	}

	return runCmd(ctx, "cmake", []string{"--install", ".", "--prefix", destDir}, srcDir, env)
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
