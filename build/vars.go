package build

import (
	"maps"
	"os"
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
// CFLAGS/LIBFLAG entries come from the resolved toolchain Flags; and each
// external dependency gets its <NAME>_DIR, <NAME>_BINDIR, <NAME>_INCDIR and
// <NAME>_LIBDIR (addExternalDepVars).
func buildVars(spec *rocks.Rockspec, cfg rocks.Config) map[string]string {
	f := DeriveFlags(cfg)

	vars := maps.Clone(toolVariables)
	maps.Copy(vars, map[string]string{
		"LUA_INCDIR": cfg.Tarantool.IncludeDir,
		"LUA_LIBDIR": f.LuaLibDir,
		"LUA_BINDIR": f.LuaBinDir,
		"LUA":        luaVariable(cfg, f),
		"CC":         f.CC,
		"CFLAGS":     strings.Join(f.CFLAGS, " "),
		"LDFLAGS":    strings.Join(f.LDFLAGS, " "),
		"LIBFLAG":    strings.Join(f.LIBFLAG, " "),
		"PREFIX":     cfg.Tarantool.Prefix,
		"MAKE":       "make",
		"CMAKE":      "cmake",
		// The linker is the compiler, as upstream's LD is CC's value.
		"LD":            f.CC,
		"AR":            envOr("AR", "ar"),
		"RANLIB":        envOr("RANLIB", "ranlib"),
		"LIB_EXTENSION": strings.TrimPrefix(f.Ext, "."),
		"OBJ_EXTENSION": "o",
	})

	if spec != nil && spec.Package != "" {
		installDir := tree.Paths{Tree: cfg.Tree}.InstallDir(spec.Package, spec.Version)
		vars["PREFIX"] = installDir
		vars["LUADIR"] = filepath.Join(installDir, "lua")
		vars["LIBDIR"] = filepath.Join(installDir, "lib")
		vars["CONFDIR"] = filepath.Join(installDir, "conf")
		vars["BINDIR"] = filepath.Join(installDir, "bin")
		vars["DOCDIR"] = filepath.Join(installDir, "doc")
	}

	addExternalDepVars(spec, cfg, vars)

	return vars
}

// toolVariables are the cfg.variables upstream's make_defaults sets to a
// fixed value on unix (core/cfg.lua:217-265, 384), the no-cert flags as
// use_defaults sets them, since certificates are not checked by default
// (core/cfg.lua:552-555).
var toolVariables = map[string]string{
	"CVS": "cvs", "GIT": "git", "SSCM": "sscm", "SVN": "svn", "HG": "hg",
	"GPG":   "gpg",
	"RSYNC": "rsync", "WGET": "wget", "SCP": "scp", "CURL": "curl",
	"PWD": "pwd", "MKDIR": "mkdir", "RMDIR": "rmdir", "CP": "cp", "LS": "ls", "RM": "rm",
	"FIND": "find", "CHMOD": "chmod", "ICACLS": "icacls", "MKTEMP": "mktemp",
	"ZIP": "zip", "UNZIP": "unzip -n", "GUNZIP": "gunzip", "BUNZIP2": "bunzip2", "TAR": "tar",
	"MD5SUM": "md5sum", "OPENSSL": "openssl", "MD5": "md5", "TOUCH": "touch",
	"SEVENZ":         "7z",
	"RSYNCFLAGS":     "--exclude=.git -Oavz",
	"CURLNOCERTFLAG": "-k",
	"WGETNOCERTFLAG": "--no-check-certificate",
	"TEST":           "test",
}

// luaVariable is $(LUA), the interpreter: Tarantool.Executable, else
// tarantool in LUA_BINDIR, as upstream derives it, with hardcoded.lua's
// /usr/bin when there is none.
func luaVariable(cfg rocks.Config, f Flags) string {
	if cfg.Tarantool.Executable != "" {
		return cfg.Tarantool.Executable
	}

	binDir := f.LuaBinDir
	if binDir == "" {
		binDir = "/usr/bin"
	}

	return filepath.Join(binDir, "tarantool")
}

// envOr is the environment variable name, or def when it is unset.
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}

	return def
}
