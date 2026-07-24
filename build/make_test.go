package build_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestRunMake_DefaultsAndOverrides(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	// Stub make logs argv + whether the vars leaked into the environment. They
	// must be passed as make command-line K=V assignments (argv), NOT env.
	stubScript := `#!/bin/sh
echo "args: $@ | env BUILDVAR=${BUILDVAR-unset} INSTVAR=${INSTVAR-unset}" >> "` + dir + `/log"
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), stubScript))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile"), "all:\n\t@true\n"))

	spec := &rocks.Rockspec{
		Package: "foo", Version: "1.0-1",
		Build: rocks.Build{
			Type:             "make",
			BuildVariables:   map[string]string{"BUILDVAR": "b"},
			InstallVariables: map[string]string{"INSTVAR": "i"},
		},
	}

	t.Setenv("PATH", stubDir)
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "make backend failed")

	got := string(mustReadLog(t, filepath.Join(dir, "log")))
	lines := strings.Split(strings.TrimSpace(got), "\n")
	require.Len(t, lines, 2, "expected build + install phase lines:\n%s", got)

	// glr-2ph: variables reach make as argv assignments, not environment.
	assert.Contains(t, lines[0], "BUILDVAR=b", "build phase must pass BUILDVAR as argv")
	assert.Contains(t, lines[1], "install", "second line is the install phase")
	assert.Contains(t, lines[1], "INSTVAR=i", "install phase must pass INSTVAR as argv")
	assert.Contains(t, got, "env BUILDVAR=unset", "vars must NOT leak into the environment")
	assert.Contains(t, got, "INSTVAR=unset", "vars must NOT leak into the environment")
}

func mustReadLog(t *testing.T, path string) []byte {
	t.Helper()

	b, err := build.ReadFile(path)
	require.NoError(t, err, "stub log missing")

	return b
}

func TestRunMake_CustomTargets(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubScript := `#!/bin/sh
echo "args: $@" >> "` + dir + `/log"
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), stubScript))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile"), "x:\n\t@true\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:          "make",
			BuildTarget:   "all-release",
			InstallTarget: "install-stripped",
		},
	}

	t.Setenv("PATH", stubDir)
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}), "make backend failed")

	got, _ := build.ReadFile(filepath.Join(dir, "log"))
	assert.Contains(t, string(got), "args: all-release", "expected build target 'all-release'")
	assert.Contains(t, string(got), "args: install-stripped", "expected install target 'install-stripped'")
}

func TestRunMake_BuildVariablesFoldedIntoBothPasses(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubScript := `#!/bin/sh
echo "args: $@ CC=${CC-unset}" >> "` + dir + `/log"
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), stubScript))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile"), "x:\n\t@true\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "make",
			// glr-esy: build.variables must reach BOTH build and install passes.
			Variables: map[string]string{"CC": "clang"},
		},
	}

	t.Setenv("PATH", stubDir)
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}))

	raw, _ := build.ReadFile(filepath.Join(dir, "log"))
	got := string(raw)
	assert.Equal(t, 2, strings.Count(got, "CC=clang"), "CC=clang must reach both make passes; log:\n%s", got)
}

func TestRunMake_SubstitutesVarsAndAutoInjectsCC(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), "#!/bin/sh\necho \"args: $@\" >> \""+dir+"/log\"\nexit 0\n"))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile"), "all:\n\t@true\n"))
	destDir := filepath.Join(dir, "dest")

	spec := &rocks.Rockspec{
		Package: "foo", Version: "1.0-1",
		Build: rocks.Build{
			Type: "make",
			// glr-027: the canonical luaposix pattern — $(LIBDIR)/$(LUADIR) must
			// expand to real paths, not survive verbatim.
			InstallVariables: map[string]string{"INST_LIBDIR": "$(LIBDIR)", "INST_LUADIR": "$(LUADIR)"},
		},
	}

	t.Setenv("PATH", stubDir)
	t.Setenv("CC", "") // force DeriveFlags default "cc"
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))

	got := string(mustReadLog(t, filepath.Join(dir, "log")))
	// $(LIBDIR)/$(LUADIR) expand into destDir; the literal placeholder is gone.
	assert.Contains(t, got, "INST_LIBDIR="+filepath.Join(destDir, "lib"), "INST_LIBDIR must expand to destDir/lib")
	assert.Contains(t, got, "INST_LUADIR="+filepath.Join(destDir, "lua"), "INST_LUADIR must expand to destDir/lua")
	assert.NotContains(t, got, "$(LIBDIR)", "no unexpanded placeholder may survive")
	// glr-vz4: CC auto-injected as an argv assignment on both phases.
	assert.Equal(t, 2, strings.Count(got, "CC=cc"), "CC must be auto-injected on both phases; log:\n%s", got)
}

func TestRunMake_SkipsBuildPassFalse(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), "#!/bin/sh\necho \"args: $@\" >> \""+dir+"/log\"\nexit 0\n"))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile"), "x:\n\t@true\n"))

	no := false
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:      "make",
			BuildPass: &no, // glr-42f: build_pass=false → build phase skipped
		},
	}

	t.Setenv("PATH", stubDir)
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}))

	raw, _ := build.ReadFile(filepath.Join(dir, "log"))
	got := string(raw)
	assert.Equal(t, 1, strings.Count(got, "args:"), "only the install phase should run; log:\n%s", got)
	assert.Contains(t, got, "args: install", "install phase must still run")
}

func TestRunMake_HonorsMakefile(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	stubScript := `#!/bin/sh
echo "args: $@" >> "` + dir + `/log"
exit 0
`
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "make"), stubScript))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "make")))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "Makefile.unix"), "x:\n\t@true\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:     "make",
			Makefile: "Makefile.unix", // glr-o1z
		},
	}

	t.Setenv("PATH", stubDir)
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}))

	raw, _ := build.ReadFile(filepath.Join(dir, "log"))
	got := string(raw)
	assert.Equal(t, 2, strings.Count(got, "-f Makefile.unix"), "-f Makefile.unix must be on both passes; log:\n%s", got)
}
