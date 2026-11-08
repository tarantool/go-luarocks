package build

import (
	"context"
	"fmt"

	rocks "github.com/tarantool/go-luarocks"
)

// RunBackend builds spec from the sources in srcDir: it writes the
// build.extra_files and applies the build.patches to the sources, as
// upstream build.apply_patches does, then dispatches to the build backend
// selected by spec.Build.Type.
//
// Supported types:
//
//   - ""        → builtin (upstream default for format ≥ 3.0)
//   - "builtin" → builtin
//   - "cmake"   → cmake
//   - "make"    → make
//   - "command" → command
//   - "none"    → no backend (the patches are still applied)
//
// Any other value yields ErrUnsupportedRockspecFeature wrapped with the
// observed type string (typed errors callers can branch on).
//
// srcDir is the unpacked source tree the rock will be built against.
// destDir is the staging root the builtin backend writes its outputs into.
// The make, cmake and command backends install where the rockspec's
// $(PREFIX), $(LUADIR), … variables point instead — the rock's install dir
// under cfg.Tree, as upstream path.configure_paths sets them — except that a
// cmake build of a spec without a rock name installs under destDir. The
// facade is responsible for orchestrating srcDir (post-fetch) and destDir
// (the `<tree>/.../build/` working area handed to tree.Deploy).
func RunBackend(ctx context.Context, spec *rocks.Rockspec, srcDir, destDir string, cfg rocks.Config) error {
	switch spec.Build.Type {
	case "", "builtin", "cmake", "make", "command", "none":
	default:
		return fmt.Errorf("build: type %q: %w", spec.Build.Type, rocks.ErrUnsupportedRockspecFeature)
	}

	if err := applyPatches(spec, srcDir); err != nil {
		return err
	}

	switch spec.Build.Type {
	case "", "builtin":
		return runBuiltin(ctx, spec, srcDir, destDir, cfg)
	case "cmake":
		return runCMake(ctx, spec, srcDir, destDir, cfg)
	case "make":
		return runMake(ctx, spec, srcDir, destDir, cfg)
	case "command":
		return runCommand(ctx, spec, srcDir, destDir, cfg)
	default: // "none"
		return nil
	}
}
