package client_test

import (
	"archive/zip"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/client"
)

// TestNativePack_LeavesBuildDirOut: the binary rock the native Pack makes of
// a rock it built holds no build/ staging dir, which the build keeps in the
// rock's install dir and its rock_manifest does not list; unpacked, the rock
// is the rock_manifest's files.
func TestNativePack_LeavesBuildDirOut(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := makeRollback(t, root, "1.0-1", "bin/roll-tool")
	require.NoError(t, err, "Make")
	require.DirExists(t, filepath.Join(root, "share", "tarantool", "rocks", "roll", "1.0-1", "build"),
		"the staging dir the build keeps")

	r, err := client.New(cfg)
	require.NoError(t, err, "New")

	archive, err := r.Pack(context.Background(), "roll", client.PackOpts{})
	require.NoError(t, err, "Pack")

	zr, err := zip.OpenReader(archive)
	require.NoError(t, err, "open %s", archive)

	defer func() { _ = zr.Close() }()

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
		assert.False(t, f.Name == "build" || strings.HasPrefix(f.Name, "build/"), "packed %s", f.Name)
	}

	assert.Contains(t, names, "lua/roll.lua", "the rock's own files")

	dir := filepath.Join(t.TempDir(), "roll")
	require.NoError(t, r.Unpack(context.Background(), archive, dir), "Unpack")
	assert.NoDirExists(t, filepath.Join(dir, "build"), "the unpacked rock")
}
