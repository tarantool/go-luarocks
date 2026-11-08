//go:build integration

package build

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

func TestCommand_TrivialShell(t *testing.T) {
	testenv.Hermetic(t)

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:           "command",
			BuildCommand:   "printf b > " + logFile,
			InstallCommand: "printf i >> " + logFile,
		},
	}
	require.NoError(t, RunBackend(context.Background(), spec, dir, dir, rocks.Config{}), "command backend failed")
	data, _ := readFile(logFile)
	assert.Equal(t, "bi", string(data))
}
