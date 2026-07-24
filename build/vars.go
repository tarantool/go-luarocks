package build

import (
	"path/filepath"
	"regexp"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// varRefRe matches a luarocks `$(NAME)` variable reference. NAME starts with a
// letter and continues with letters, digits or underscores — the pattern
// upstream util.variable_substitutions uses (util.lua:128-144).
var varRefRe = regexp.MustCompile(`\$\(([A-Za-z][A-Za-z0-9_]*)\)`)

// substituteVars expands every `$(NAME)` token in s using vars. A token whose
// NAME has no entry in vars is replaced by the empty string, mirroring
// upstream's warn_failed_matches behavior (util.lua:137) — the placeholder
// never survives into the emitted command line.
func substituteVars(s string, vars map[string]string) string {
	return varRefRe.ReplaceAllStringFunc(s, func(match string) string {
		name := varRefRe.FindStringSubmatch(match)[1]

		return vars[name] // "" when absent
	})
}

// substituteVarsSlice returns a copy of in with substituteVars applied to
// every element. The input slice is left unchanged.
func substituteVarsSlice(in []string, vars map[string]string) []string {
	if len(in) == 0 {
		return in
	}

	out := make([]string, len(in))
	for i, s := range in {
		out[i] = substituteVars(s, vars)
	}

	return out
}

// buildVars assembles the rockspec.variables map that upstream exposes to
// `$(NAME)` expansion, mirroring cfg.variables plus the per-rock paths
// configure_paths computes (rockspecs.lua). PREFIX and the LUADIR/LIBDIR/…
// family point at the rock's install directory under the tree; the LUA_*/CC/
// CFLAGS/LIBFLAG entries come from the resolved toolchain Flags.
func buildVars(spec *rocks.Rockspec, cfg rocks.Config) map[string]string {
	f := DeriveFlags(cfg)

	vars := map[string]string{
		"LUA_INCDIR": cfg.Tarantool.IncludeDir,
		"LUA_LIBDIR": f.LuaLibDir,
		"LUA_BINDIR": f.LuaBinDir,
		"LUA":        cfg.Tarantool.Executable,
		"CC":         f.CC,
		"CFLAGS":     strings.Join(f.CFLAGS, " "),
		"LIBFLAG":    strings.Join(f.LIBFLAG, " "),
		"PREFIX":     cfg.Tarantool.Prefix,
		"MAKE":       "make",
		"CMAKE":      "cmake",
	}

	if spec != nil && spec.Package != "" {
		installDir := tree.Paths{Tree: cfg.Tree}.InstallDir(spec.Package, spec.Version)
		vars["PREFIX"] = installDir
		vars["LUADIR"] = filepath.Join(installDir, "lua")
		vars["LIBDIR"] = filepath.Join(installDir, "lib")
		vars["CONFDIR"] = filepath.Join(installDir, "conf")
		vars["BINDIR"] = filepath.Join(installDir, "bin")
		vars["DOCDIR"] = filepath.Join(installDir, "doc")
	}

	return vars
}
