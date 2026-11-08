package build_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// TestRunCMake_PassThrough exercises only the dispatch table — we point
// "cmake" at a stub via PATH so we can verify the args reach the binary.
// We can't easily intercept argv without a stub binary, so use a
// shell-script PATH shim.
func TestRunCMake_PassThrough(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cmake"), "#!/bin/sh\necho \"args: $@\" >> \""+dir+"/log\"\nexit 0\n"))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cmake")))

	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), "# stub\n"))

	spec := &rocks.Rockspec{
		Package: "foo", Version: "1.0-1",
		Build: rocks.Build{
			Type: "cmake",
			Variables: map[string]string{
				"CMAKE_INSTALL_PREFIX": "/whatever",
				"LUA_INCLUDE_DIR":      "$(LUA_INCDIR)", // glr-f68: expanded
			},
		},
	}

	// Override PATH to point at our stub.
	t.Setenv("PATH", stubDir)

	cfg := rocks.Config{Tarantool: rocks.TarantoolConfig{IncludeDir: "/opt/tt/include"}}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, cfg), "cmake dispatch failed")

	data, err := build.ReadFile(filepath.Join(dir, "log"))
	require.NoError(t, err, "stub log missing")

	got := string(data)
	for _, want := range []string{
		// Configured out of tree in build.luarocks, as upstream cmake.run does,
		// with the rockspec's own CMAKE_INSTALL_PREFIX left alone.
		"args: -H. -Bbuild.luarocks -DCMAKE_INSTALL_PREFIX=/whatever",
		"-DLUA_INCLUDE_DIR=/opt/tt/include", // $(LUA_INCDIR) expanded, not literal
		"args: --build build.luarocks --config Release",
		"args: --build build.luarocks --target install --config Release",
	} {
		assert.Contains(t, got, want, "stub log missing %q", want)
	}
}

// TestRunCMake_DefaultInstallPrefix: a rockspec that sets no
// CMAKE_INSTALL_PREFIX installs into the rock's $(PREFIX) — its install dir,
// where upstream's rockspecs point it and tree.Deploy collects the files — and
// a spec without a rock name into destDir. Installing into destDir for every
// rock left whatever cmake put outside lua/, lib/ and bin/ out of the
// rock_manifest.
func TestRunCMake_DefaultInstallPrefix(t *testing.T) {
	cases := []struct {
		name string
		spec *rocks.Rockspec
		want func(destDir string) string
	}{
		{
			name: "rock",
			spec: &rocks.Rockspec{Package: "foo", Version: "1.0-1", Build: rocks.Build{Type: "cmake"}},
			want: func(string) string { return "/tree/share/tarantool/rocks/foo/1.0-1" },
		},
		{
			name: "hand-built spec",
			spec: &rocks.Rockspec{Build: rocks.Build{Type: "cmake"}},
			want: func(destDir string) string { return destDir },
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			stubDir := filepath.Join(dir, "stub")
			require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cmake"), "#!/bin/sh\necho \"args: $@\" >> \""+dir+"/log\"\nexit 0\n"))
			require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cmake")))

			srcDir := filepath.Join(dir, "src")
			destDir := filepath.Join(dir, "dest")
			require.NoError(t, build.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), "# stub\n"))

			t.Setenv("PATH", stubDir)
			require.NoError(t, build.RunBackend(context.Background(), c.spec, srcDir, destDir, rocks.Config{Tree: "/tree"}))

			data, err := build.ReadFile(filepath.Join(dir, "log"))
			require.NoError(t, err)
			assert.Contains(t, string(data), "-DCMAKE_INSTALL_PREFIX="+c.want(destDir)+"\n")
		})
	}
}

func TestRunCMake_PassTogglesGatedByFormat(t *testing.T) {
	newStub := func(t *testing.T) (stubDir, logPath string) {
		t.Helper()
		dir := t.TempDir()
		stubDir = filepath.Join(dir, "stub")
		logPath = filepath.Join(dir, "log")
		require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cmake"), "#!/bin/sh\necho \"args: $@\" >> \""+logPath+"\"\nexit 0\n"))
		require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cmake")))

		return stubDir, logPath
	}

	no := false

	// glr-42f: under format 3.0, install_pass=false suppresses --install.
	t.Run("format 3.0 honors install_pass", func(t *testing.T) {
		stubDir, logPath := newStub(t)
		srcDir := t.TempDir()
		require.NoError(t, build.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), "# stub\n"))

		spec := &rocks.Rockspec{
			Package: "foo", Version: "1.0-1", RockspecFormat: "3.0",
			Build: rocks.Build{Type: "cmake", InstallPass: &no},
		}

		t.Setenv("PATH", stubDir)
		require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}))

		raw, _ := build.ReadFile(logPath)
		assert.NotContains(t, string(raw), "--target install", "install_pass=false must suppress the install under format 3.0")
	})

	// Under format 1.0 the toggle is ignored — both phases run.
	t.Run("format 1.0 ignores install_pass", func(t *testing.T) {
		stubDir, logPath := newStub(t)
		srcDir := t.TempDir()
		require.NoError(t, build.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), "# stub\n"))

		spec := &rocks.Rockspec{
			Package: "foo", Version: "1.0-1", // no rockspec_format → 1.0
			Build: rocks.Build{Type: "cmake", InstallPass: &no},
		}

		t.Setenv("PATH", stubDir)
		require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, t.TempDir(), rocks.Config{}))

		raw, _ := build.ReadFile(logPath)
		assert.Contains(t, string(raw), "--target install", "format 1.0 must ignore install_pass and still install")
	})
}

func TestRunCMake_ForwardsCMakeEnvPaths(t *testing.T) {
	dir := t.TempDir()
	stubDir := filepath.Join(dir, "stub")
	require.NoError(t, build.WriteFile(filepath.Join(stubDir, "cmake"), "#!/bin/sh\necho \"args: $@\" >> \""+dir+"/log\"\nexit 0\n"))
	require.NoError(t, build.ChmodX(filepath.Join(stubDir, "cmake")))

	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")
	require.NoError(t, build.WriteFile(filepath.Join(srcDir, "CMakeLists.txt"), "# stub\n"))

	spec := &rocks.Rockspec{
		Package: "foo", Version: "1.0-1",
		Build: rocks.Build{
			Type: "cmake",
			// Declared CMAKE_LIBRARY_PATH must NOT be overridden by the env.
			Variables: map[string]string{"CMAKE_LIBRARY_PATH": "/declared"},
		},
	}

	t.Setenv("PATH", stubDir)
	t.Setenv("CMAKE_MODULE_PATH", "/opt/cmake/modules")
	t.Setenv("CMAKE_LIBRARY_PATH", "/env/lib")
	t.Setenv("CMAKE_INCLUDE_PATH", "")

	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}))

	data, err := build.ReadFile(filepath.Join(dir, "log"))
	require.NoError(t, err)

	got := string(data)
	assert.Contains(t, got, "-DCMAKE_MODULE_PATH=/opt/cmake/modules", "env CMAKE_MODULE_PATH not forwarded")
	assert.Contains(t, got, "-DCMAKE_LIBRARY_PATH=/declared", "declared var must win over env")
	assert.NotContains(t, got, "-DCMAKE_LIBRARY_PATH=/env/lib", "env must not override a declared var")
	assert.NotContains(t, got, "CMAKE_INCLUDE_PATH", "empty env var must be dropped")
}
