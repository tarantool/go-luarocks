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
