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

func TestMake_TrivialMakefile(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skipf("make not on PATH")
	}

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")

	mk := `all:
	@echo built
install:
	@mkdir -p $(DESTDIR)/out && echo done > $(DESTDIR)/out/marker
`
	require.NoError(t, writeFile(filepath.Join(srcDir, "Makefile"), mk))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:             "make",
			InstallVariables: map[string]string{"DESTDIR": destDir},
		},
	}
	require.NoError(t, RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "make build failed")
	assert.True(t, fileExists(filepath.Join(destDir, "out/marker")), "expected destDir/out/marker")
}
