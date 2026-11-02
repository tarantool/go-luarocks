package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// TestLuaBackend_FailureSaysWhy pins that the error of a failed LuaRocks
// command carries the message LuaRocks printed. The output reaches only the
// logger, so the error used to say that the command exited with code 1 and
// not why.
func TestLuaBackend_FailureSaysWhy(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(repo, "manifest"), []byte("repository = {}\n"), 0o600), "write manifest")

	tree := t.TempDir()
	r, err := client.New(rocks.Config{Tree: tree, WorkingDir: tree, Servers: []string{repo}},
		client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	err = r.Install(context.Background(), "nosuchrock", client.InstallOpts{})
	require.Error(t, err, "Install of a rock no server has")
	assert.Contains(t, err.Error(), "exited with code 1: Error: No results matching query were found",
		"the error carries the message LuaRocks printed")
}
