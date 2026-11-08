package build_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// TestBuiltin_ExternalDependencyIncdir is the shape of http 1.9.1-1: a
// module whose incdirs name $(TARANTOOL_INCDIR) of an external dependency on
// tarantool/module.h. The native backend expanded every <NAME>_INCDIR,
// _LIBDIR, _BINDIR and _DIR to "", so the compiler got an empty -I and the
// build failed. They must name where the dependency was found, here under
// the Tarantool prefix. The header name is unique, so no system directory
// looked at before the prefix holds it.
func TestBuiltin_ExternalDependencyIncdir(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cc"),
		"#!/bin/sh\necho \"args: $@\" > \""+dir+"/cc.log\"\n"))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cc")))

	prefix := filepath.Join(dir, "tarantool")
	require.NoError(t, build.WriteFile(filepath.Join(prefix, "include", "glr-extdep-probe", "module.h"), "/* */\n"))

	srcDir := filepath.Join(dir, "src")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "lib.c"), "/* */\n"))

	spec := &rocks.Rockspec{
		Package: "http",
		Version: "1.9.1-1",
		ExternalDependencies: map[string]rocks.ExternalDep{
			"TARANTOOL": {Header: "glr-extdep-probe/module.h"},
		},
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"http.lib": {Sources: []string{"lib.c"}, Incdirs: []string{"$(TARANTOOL_INCDIR)"}},
			},
		},
	}
	cfg := rocks.Config{
		Tree:      filepath.Join(dir, "tree"),
		Tarantool: rocks.TarantoolConfig{Prefix: prefix, IncludeDir: filepath.Join(prefix, "include", "tarantool")},
	}

	t.Setenv("PATH", stubDir)
	t.Setenv("CC", "")

	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, filepath.Join(dir, "dest"), cfg))

	data, err := build.ReadFile(filepath.Join(dir, "cc.log"))
	require.NoError(t, err, "missing cc log")
	assert.Contains(t, string(data), " -I"+filepath.Join(prefix, "include")+" ",
		"$(TARANTOOL_INCDIR) must name the prefix's include dir")
}

// TestBuildVars_ExternalDependencyVars — every variable of an external
// dependency names where it was found: the prefix and its bin, include and
// lib dirs, plus the files found. The library has the extension of the
// host's shared libraries, the only one its lookup takes.
func TestBuildVars_ExternalDependencyVars(t *testing.T) {
	t.Parallel()

	lib := "libglr-extdep-probe.so"
	if runtime.GOOS == "darwin" {
		lib = "libglr-extdep-probe.dylib"
	}

	prefix := t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(prefix, "include", "glr-extdep-probe.h"), ""))
	require.NoError(t, build.WriteFile(filepath.Join(prefix, "lib", lib), ""))

	spec := &rocks.Rockspec{
		Package: "foo",
		Version: "1.0-1",
		ExternalDependencies: map[string]rocks.ExternalDep{
			"PROBE": {Header: "glr-extdep-probe.h", Library: "glr-extdep-probe"},
		},
	}
	vars := build.BuildVars(spec, rocks.Config{Tree: t.TempDir(), Tarantool: rocks.TarantoolConfig{Prefix: prefix}})

	assert.Equal(t, prefix, vars["PROBE_DIR"])
	assert.Equal(t, filepath.Join(prefix, "bin"), vars["PROBE_BINDIR"])
	assert.Equal(t, filepath.Join(prefix, "include"), vars["PROBE_INCDIR"])
	assert.Equal(t, filepath.Join(prefix, "lib"), vars["PROBE_LIBDIR"])
	assert.Equal(t, "glr-extdep-probe.h", vars["PROBE_INCDIR_FILE"])
	assert.Equal(t, lib, vars["PROBE_LIBDIR_FILE"])
	assert.Equal(t, filepath.Join(vars["PREFIX"], "bin"), vars["BINDIR"],
		"the rock's own BINDIR is not the dependency's")
}

// TestExternalDepVars_Lookup pins the lookup of check_external_deps: the
// prefixes in order, each directory's candidate subdirectories in order, the
// file-name patterns of a bare library name, and a dependency found nowhere,
// which gets the directories of the first prefix instead of failing the
// build, as the native backend keeps doing.
func TestExternalDepVars_Lookup(t *testing.T) {
	t.Parallel()

	first, second := t.TempDir(), t.TempDir()
	require.NoError(t, build.WriteFile(filepath.Join(first, "include", "a.h"), ""))
	require.NoError(t, build.WriteFile(filepath.Join(second, "include", "b.h"), ""))
	require.NoError(t, build.WriteFile(filepath.Join(second, "lib", "x86_64-linux-gnu", "libb.so.1"), ""))
	require.NoError(t, build.WriteFile(filepath.Join(second, "lib64", "libc.a"), ""))

	spec := &rocks.Rockspec{ExternalDependencies: map[string]rocks.ExternalDep{
		"A":       {Header: "a.h"},
		"B":       {Header: "b.h", Library: "b"},
		"C":       {Library: "c"},
		"MISSING": {Header: "glr-no-such-header.h"},
	}}

	vars := build.ExternalDepVars(spec, "linux", "x86_64-linux-gnu", []string{first, second})

	assert.Equal(t, map[string]string{
		"A_DIR":    first,
		"A_BINDIR": filepath.Join(first, "bin"), "A_INCDIR": filepath.Join(first, "include"),
		"A_LIBDIR": filepath.Join(first, "lib"), "A_INCDIR_FILE": "a.h",

		"B_DIR":    second,
		"B_BINDIR": filepath.Join(second, "bin"), "B_INCDIR": filepath.Join(second, "include"),
		"B_LIBDIR": filepath.Join(second, "lib", "x86_64-linux-gnu"), "B_INCDIR_FILE": "b.h",
		"B_LIBDIR_FILE": "matching libb.so.*",

		"C_DIR":    second,
		"C_BINDIR": filepath.Join(second, "bin"), "C_INCDIR": filepath.Join(second, "include"),
		"C_LIBDIR": filepath.Join(second, "lib64"), "C_LIBDIR_FILE": "libc.a",

		"MISSING_DIR":    first,
		"MISSING_BINDIR": filepath.Join(first, "bin"), "MISSING_INCDIR": filepath.Join(first, "include"),
		"MISSING_LIBDIR": filepath.Join(first, "lib"),
	}, vars)
}

// TestExternalDepsPrefixes — the prefixes looked under are the system dirs,
// then the Tarantool prefix, /usr when none is configured, as hardcoded.lua
// sets PREFIX.
func TestExternalDepsPrefixes(t *testing.T) {
	t.Parallel()

	assert.Equal(t, []string{"/usr/local", "/usr", "/", "/opt/tt"},
		build.ExternalDepsPrefixes(rocks.Config{Tarantool: rocks.TarantoolConfig{Prefix: "/opt/tt"}}, "linux"))
	assert.Equal(t, []string{"/usr/local", "/usr", "/", "/usr"},
		build.ExternalDepsPrefixes(rocks.Config{}, "linux"))
	assert.Equal(t, []string{"/usr/local", "/usr", "/", "/opt/tt", "/usr/local/opt"},
		build.ExternalDepsPrefixes(rocks.Config{Tarantool: rocks.TarantoolConfig{Prefix: "/opt/tt"}}, "darwin"))
}
