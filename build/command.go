package build

import (
	"context"

	rocks "github.com/tarantool/go-luarocks"
)

// runCommand implements the `command` build backend.
//
// Each of spec.Build.BuildCommand and spec.Build.InstallCommand, when
// non-empty, is executed verbatim via `sh -c <cmd>` from srcDir with the
// buildEnv(cfg) overlay applied to the inherited process environment.
//
// Either may be empty — the corresponding phase is then a no-op.
//
// This is intentionally minimal: upstream luarocks injects only CC into
// the env (cfg.variables.CC), but for Tarantool builds we expose the full
// canonical set so rockspec commands can `${LUA_INCDIR}` etc. just like
// other backends.
func runCommand(ctx context.Context, spec *rocks.Rockspec, srcDir, _ string, cfg rocks.Config) error {
	env := buildEnv(cfg)

	// Upstream command.run sets env CC = cfg.variables.CC for the build/install
	// commands (command.lua:20-24). Overlay it so a `$CC ...` command uses the
	// configured toolchain rather than the inherited/empty CC.
	if cc := DeriveFlags(cfg).CC; cc != "" {
		env = overlay(env, map[string]string{"CC": cc})
	}

	// Expand luarocks $(NAME) placeholders in Go BEFORE handing the command to
	// sh (upstream command.run → util.variable_substitutions). POSIX sh reads
	// $(...) as command substitution, so a luarocks $(PREFIX) would otherwise
	// run PREFIX as a program; only genuine shell $VAR the author wrote survive
	// to the shell.
	vars := buildVars(spec, cfg)

	if spec.Build.BuildCommand != "" {
		cmd := substituteVars(spec.Build.BuildCommand, vars)

		err := runCmd(ctx, "sh", []string{"-c", cmd}, srcDir, env)
		if err != nil {
			return err
		}
	}

	if spec.Build.InstallCommand != "" {
		cmd := substituteVars(spec.Build.InstallCommand, vars)

		err := runCmd(ctx, "sh", []string{"-c", cmd}, srcDir, env)
		if err != nil {
			return err
		}
	}

	return nil
}
