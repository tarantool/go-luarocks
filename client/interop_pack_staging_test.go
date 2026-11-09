//go:build integration_luaengine

package client_test

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/client"
)

// compareArchivesBuiltBy is compareArchives for the rocks packed from a tree
// built by built: upstream's pack copies a rock's install dir
// whole, so from a tree the native backend built it packs the build/ staging
// dir the native build keeps there, which the native Pack leaves out. The
// lua backend's archive is compared without it then.
func compareArchivesBuiltBy(t *testing.T, label string, built client.Backend, native, lua string) {
	t.Helper()

	if built == client.BackendNative {
		lua = withoutStagingDir(t, lua)
	}

	compareArchives(t, label, native, lua)
}

// withoutStagingDir copies the zip archive at path, but for its entries
// under build/, to a fresh directory and returns the copy's path.
func withoutStagingDir(t *testing.T, path string) string {
	t.Helper()

	zr, err := zip.OpenReader(path)
	require.NoError(t, err, "open %s", path)

	defer func() { _ = zr.Close() }()

	out := filepath.Join(t.TempDir(), filepath.Base(path))

	f, err := os.Create(out) //nolint:gosec // test temp path
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	for _, e := range zr.File {
		if strings.HasPrefix(e.Name, "build/") {
			continue
		}

		h := e.FileHeader

		w, err := zw.CreateHeader(&h)
		require.NoError(t, err)

		rc, err := e.Open()
		require.NoError(t, err)

		_, err = io.Copy(w, rc) //nolint:gosec // test archive
		require.NoError(t, err)
		require.NoError(t, rc.Close())
	}

	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	return out
}
