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
)

func TestCMake_TrivialProject(t *testing.T) {
	if _, err := exec.LookPath("cmake"); err != nil {
		t.Skipf("cmake not on PATH")
	}

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
