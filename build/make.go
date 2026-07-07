package build

import (
	"context"
	"maps"
	"path/filepath"
	"sort"

	rocks "github.com/tarantool/go-luarocks"
)

// runMake implements the `make` build backend.
//
// Sequence (mirrors upstream luarocks/src/luarocks/build/make.lua):
//
//  1. make [<build_target>] K=V ...   (build phase)
//  2. make <install_target> K=V ...   (install phase, default target "install")
//
// The build/install variables are passed as make COMMAND-LINE assignments
// (K=V argv), not environment (make.lua:26-31): command-line assignments beat
// Makefile-internal assignments, whereas environment variables lose to them —
// so a rockspec override of e.g. CFLAGS must be argv to actually take effect.
// Every value is $(VAR)-expanded against the rockspec.variables map, and CC is
// auto-injected from the toolchain when the rockspec leaves it unset
// (auto_variables, make.lua:71-80).
//
// LUADIR/LIBDIR/CONFDIR/BINDIR/DOCDIR are pointed into destDir — the per-rock
// subtree tree.Deploy scans for make-installed files — so the canonical
// install_variables = { INST_LIBDIR = '$(LIBDIR)', INST_LUADIR = '$(LUADIR)' }
// pattern installs where the deploy step can find and relocate the files.
func runMake(ctx context.Context, spec *rocks.Rockspec, srcDir, destDir string, cfg rocks.Config) error {
	env := buildEnv(cfg)

	// rockspec.variables for $(NAME) expansion, with the install dirs remapped
	// into destDir (the subtree Deploy scans).
	vars := buildVars(spec, cfg)
	vars["PREFIX"] = destDir
	vars["LUADIR"] = filepath.Join(destDir, "lua")
	vars["LIBDIR"] = filepath.Join(destDir, "lib")
	vars["CONFDIR"] = filepath.Join(destDir, "conf")
	vars["BINDIR"] = filepath.Join(destDir, "bin")
	vars["DOCDIR"] = filepath.Join(destDir, "doc")

	// build.variables fold into BOTH maps (make.lua:59-64), then $(VAR) expand
	// and auto-inject CC.
	bv := prepareMakeVars(mergeVars(spec.Build.BuildVariables, spec.Build.Variables), vars, cfg)
	iv := prepareMakeVars(mergeVars(spec.Build.InstallVariables, spec.Build.Variables), vars, cfg)

	// A rockspec build.makefile selects a non-default Makefile via `-f <name>`,
	// prepended before the target on both phases (make.lua:52-57).
	var makefileArgs []string
	if spec.Build.Makefile != "" {
		makefileArgs = []string{"-f", spec.Build.Makefile}
	}

	// Build phase — skipped when build.build_pass == false (make.lua:46-47).
	if passEnabled(spec.Build.BuildPass) {
		args := append([]string{}, makefileArgs...)
		if spec.Build.BuildTarget != "" {
			args = append(args, spec.Build.BuildTarget)
		}

		args = append(args, kvArgs(bv)...)

		if err := runCmd(ctx, "make", args, srcDir, env); err != nil {
			return err
		}
	}

	// Install phase — skipped when build.install_pass == false (make.lua:85-90).
	if !passEnabled(spec.Build.InstallPass) {
		return nil
	}

	installTarget := spec.Build.InstallTarget
	if installTarget == "" {
		installTarget = "install"
	}

	args := append([]string{}, makefileArgs...)
	args = append(args, installTarget)
	args = append(args, kvArgs(iv)...)

	return runCmd(ctx, "make", args, srcDir, env)
}

// prepareMakeVars expands $(VAR) in every value against subst and auto-injects
// CC from the toolchain when the map has no CC entry (auto_variables). A nil
// input still yields the CC entry, so make always receives CC=.
func prepareMakeVars(vars, subst map[string]string, cfg rocks.Config) map[string]string {
	out := make(map[string]string, len(vars)+1)
	for k, v := range vars {
		out[k] = substituteVars(v, subst)
	}

	if _, ok := out["CC"]; !ok {
		out["CC"] = DeriveFlags(cfg).CC
	}

	return out
}

// kvArgs renders vars as sorted "K=V" make command-line assignments.
func kvArgs(vars map[string]string) []string {
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	args := make([]string, 0, len(vars))
	for _, k := range keys {
		args = append(args, k+"="+vars[k])
	}

	return args
}

// passEnabled reports whether a build/install pass runs: nil (absent) defaults
// to true; an explicit false skips it.
func passEnabled(p *bool) bool {
	return p == nil || *p
}

// mergeVars returns a copy of pass with over layered on top (over wins),
// mirroring upstream's unconditional copy of build.variables into each
// pass-specific variable map.
func mergeVars(pass, over map[string]string) map[string]string {
	if len(pass) == 0 && len(over) == 0 {
		return nil
	}

	out := make(map[string]string, len(pass)+len(over))
	maps.Copy(out, pass)
	maps.Copy(out, over)

	return out
}

// overlay returns a copy of base with vars layered on top. base is left
// unchanged. Used by the command backend to inject CC into the child env.
func overlay(base, vars map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(vars))
	maps.Copy(out, base)

	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		out[k] = vars[k]
	}

	return out
}
