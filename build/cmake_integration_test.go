//go:build integration

package build

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

func TestCMake_TrivialProject(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		testenv.Missingf(t, "cmake not on PATH")
	}

	testenv.Hermetic(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")

	// Minimal CMakeLists that installs a stub file.
	cmakelists := `cmake_minimum_required(VERSION 3.10)
project(stub C)
file(WRITE "${CMAKE_CURRENT_BINARY_DIR}/hello.txt" "hi\n")
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/hello.txt" DESTINATION share/stub)
`
	require.NoError(t, writeFile(filepath.Join(srcDir, "CMakeLists.txt"), cmakelists))

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "cmake"}}
	require.NoError(t, RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "cmake build failed")
	assert.True(t, fileExists(filepath.Join(destDir, "share/stub/hello.txt")), "expected destDir/share/stub/hello.txt")
}

// TestCMake_BuildsOutOfTree: the configure step runs in build.luarocks under
// the source dir, as upstream cmake.run's `-H. -Bbuild.luarocks` does, so a
// source tree that ships its own Makefile next to CMakeLists.txt keeps it. An
// in-tree `cmake .` generated a Makefile over it — in the developer's own
// checkout, under Make.
func TestCMake_BuildsOutOfTree(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		testenv.Missingf(t, "cmake not on PATH")
	}

	testenv.Hermetic(t)

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")

	const makefile = "# hand-written, not generated\nall:\n\t@echo own\n"

	require.NoError(t, writeFile(filepath.Join(srcDir, "CMakeLists.txt"),
		"cmake_minimum_required(VERSION 3.10)\nproject(stub NONE)\ninstall(FILES CMakeLists.txt DESTINATION share/stub)\n"))
	require.NoError(t, writeFile(filepath.Join(srcDir, "Makefile"), makefile))

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "cmake"}}
	require.NoError(t, RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "cmake build failed")

	got, err := readFile(filepath.Join(srcDir, "Makefile"))
	require.NoError(t, err)
	assert.Equal(t, makefile, string(got), "the source tree's Makefile must survive the build")
	assert.True(t, fileExists(filepath.Join(srcDir, "build.luarocks", "CMakeCache.txt")), "the build dir is build.luarocks")
	assert.False(t, fileExists(filepath.Join(srcDir, "CMakeCache.txt")), "nothing is configured in tree")
}

// TestCMake_FindTarantoolFromTarantoolDir: the two FindTarantool.cmake
// variants rocks carry find the headers through $ENV{TARANTOOL_DIR} - the
// one of memcached and cbench, which looks for tarantool/module.h in it with
// no suffix, and the one of the module template, which adds an include
// suffix - for headers that are not under the prefix, as the headers of a
// version of a scope root of tt are not.
func TestCMake_FindTarantoolFromTarantoolDir(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		testenv.Missingf(t, "cmake not on PATH")
	}

	testenv.Hermetic(t)

	for _, tc := range []struct {
		name  string
		hints string
	}{
		{name: "no suffix", hints: "HINTS ENV TARANTOOL_DIR"},
		{name: "include suffix", hints: "HINTS ${TARANTOOL_DIR} ENV TARANTOOL_DIR PATH_SUFFIXES include"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srcDir := filepath.Join(dir, "src")
			destDir := filepath.Join(dir, "dest")
			incDir := filepath.Join(dir, "root", "versions", "tarantool", "tarantool_3.9.0", "include", "tarantool")

			require.NoError(t, writeFile(filepath.Join(incDir, "module.h"), "/* module.h */\n"))

			// NO_DEFAULT_PATH: the headers of a Tarantool in /usr must not
			// be what is found.
			cmakelists := `cmake_minimum_required(VERSION 3.10)
project(stub NONE)
find_path(TARANTOOL_INCLUDE_DIR tarantool/module.h ` + tc.hints + ` NO_DEFAULT_PATH)
if(NOT TARANTOOL_INCLUDE_DIR)
    message(FATAL_ERROR "Could NOT find Tarantool")
endif()
file(WRITE "${CMAKE_CURRENT_BINARY_DIR}/found.txt" "${TARANTOOL_INCLUDE_DIR}")
install(FILES "${CMAKE_CURRENT_BINARY_DIR}/found.txt" DESTINATION share/stub)
`
			require.NoError(t, writeFile(filepath.Join(srcDir, "CMakeLists.txt"), cmakelists))

			spec := &rocks.Rockspec{Build: rocks.Build{Type: "cmake"}}
			cfg := rocks.Config{Tarantool: rocks.TarantoolConfig{
				Prefix:     filepath.Join(dir, "root"),
				IncludeDir: incDir,
			}}
			require.NoError(t, RunBackend(context.Background(), spec, srcDir, destDir, cfg), "cmake build failed")

			got, err := readFile(filepath.Join(destDir, "share", "stub", "found.txt"))
			require.NoError(t, err)
			assert.Equal(t, filepath.Dir(incDir), string(got))
		})
	}
}
