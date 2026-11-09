package fetch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// leakRuns is how many times a leak test repeats its operations.
const leakRuns = 20

// leakTarGz returns a gzipped tarball holding pkg/pkg.lua.
func leakTarGz(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := []byte("return {}\n")

	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "pkg/pkg.lua", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}))
	_, err := tw.Write(body)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())

	return buf.Bytes()
}

// leakZip writes a zip archive holding pkg/pkg.lua at path.
func leakZip(t *testing.T, path string) {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	w, err := zw.Create("pkg/pkg.lua")
	require.NoError(t, err)
	_, err = w.Write([]byte("return {}\n"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600))
}

// TestFetch_FailingFetchesNoFDLeak fetches again and again from a server
// answering 404, a port nothing listens on and a file that is not there, and
// requires the process to hold no more file descriptors afterwards.
//
//nolint:paralleltest // It counts the file descriptors of the process.
func TestFetch_FailingFetchesNoFDLeak(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	refused := "http://" + ln.Addr().String() + "/pkg.tar.gz"
	require.NoError(t, ln.Close())

	urls := []string{
		srv.URL + "/pkg.tar.gz",
		refused,
		"file://" + filepath.Join(t.TempDir(), "absent.tar.gz"),
	}

	testenv.RequireNoFDLeak(t, leakRuns, func(int) {
		for _, u := range urls {
			_, err := fetch.Fetch(context.Background(), u, t.TempDir())
			require.Error(t, err, "fetch %s", u)
		}
	})
}

// TestFetch_UnpacksNoFDLeak downloads and unpacks a tarball over HTTP and
// unpacks a local zip again and again, and requires the process to hold no
// more file descriptors afterwards.
//
//nolint:paralleltest // It counts the file descriptors of the process.
func TestFetch_UnpacksNoFDLeak(t *testing.T) {
	tarball := leakTarGz(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	t.Cleanup(srv.Close)

	archive := filepath.Join(t.TempDir(), "pkg.zip")
	leakZip(t, archive)

	testenv.RequireNoFDLeak(t, leakRuns, func(int) {
		for _, u := range []string{srv.URL + "/pkg.tar.gz", "file://" + archive} {
			dir, err := fetch.Fetch(context.Background(), u, t.TempDir())
			require.NoError(t, err, "fetch %s", u)

			_, err = os.Stat(filepath.Join(dir, "pkg", "pkg.lua"))
			require.NoError(t, err, "the unpacked module of %s", u)
		}
	})
}
