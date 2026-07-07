package fetch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/fetch"
)

func TestHTTPBackend_DownloadsRaw(t *testing.T) {
	t.Parallel()

	body := []byte("plain content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "go-luarocks/0.1", r.Header.Get("User-Agent"), "missing default User-Agent")

		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/file.bin", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	b, err := os.ReadFile(filepath.Join(dst, "file.bin")) //nolint:gosec // test reads from a t.TempDir() path
	if assert.NoError(t, err, "downloaded body") {
		assert.Equal(t, body, b, "downloaded body mismatch")
	}
}

func TestHTTPBackend_UnpacksZip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("inner/file.lua")
	_, _ = f.Write([]byte("inside zip"))

	require.NoError(t, zw.Close())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/pkg.zip", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")
	_, err = os.Stat(filepath.Join(dst, "inner", "file.lua"))
	require.NoError(t, err, "expected extracted file.lua")
	_, err = os.Stat(filepath.Join(dst, "pkg.zip"))
	assert.True(t, os.IsNotExist(err), "archive should be removed after unpack: stat err=%v", err)
}

// TestHTTPBackend_UnpacksSrcRock verifies that `.src.rock` (and `.rock`)
// archives — which are zip files under a luarocks-specific extension — are
// unpacked like .zip. Before this, the http backend wrote the .src.rock out
// verbatim, so the rockspec inside was never found ("no .rockspec under …").
func TestHTTPBackend_UnpacksSrcRock(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)
	// Top-level rockspec, as a real .src.rock carries it.
	rs, _ := zw.Create("say-1.4.1-1.rockspec")
	_, _ = rs.Write([]byte("package='say'\n"))
	// Bundled source file.
	src, _ := zw.Create("say/src/say.lua")
	_, _ = src.Write([]byte("return {}\n"))

	require.NoError(t, zw.Close())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/say-1.4.1-1.src.rock", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	_, err = os.Stat(filepath.Join(dst, "say-1.4.1-1.rockspec"))
	require.NoError(t, err, "expected extracted rockspec")
	_, err = os.Stat(filepath.Join(dst, "say", "src", "say.lua"))
	require.NoError(t, err, "expected extracted bundled source")
	_, err = os.Stat(filepath.Join(dst, "say-1.4.1-1.src.rock"))
	assert.True(t, os.IsNotExist(err), "archive should be removed after unpack: stat err=%v", err)
}

func TestHTTPBackend_UnpacksTarGz(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("from tar.gz")
	_ = tw.WriteHeader(&tar.Header{Name: "lib/x.lua", Size: int64(len(body)), Mode: 0o644})
	_, _ = tw.Write(body)
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/pkg.tar.gz", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	b, err := os.ReadFile(filepath.Join(dst, "lib", "x.lua")) //nolint:gosec // test reads from a t.TempDir() path
	if assert.NoError(t, err, "expected extracted x.lua") {
		assert.Equal(t, body, b, "expected extracted x.lua")
	}
}

func TestHTTPBackend_UnpacksTarBz2(t *testing.T) {
	t.Parallel()

	// glr-i30: a .tar.bz2 download is bunzip2'd and untarred. The fixture is a
	// checked-in archive because the stdlib bzip2 package cannot write.
	body, err := os.ReadFile("testdata/pkg.tar.bz2")
	require.NoError(t, err, "read fixture")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err = fetch.FetchHTTP(context.Background(), srv.URL+"/pkg.tar.bz2", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	got, err := os.ReadFile(filepath.Join(dst, "lib", "x.lua")) //nolint:gosec // test reads from a t.TempDir() path
	require.NoError(t, err, "expected extracted x.lua")
	assert.Equal(t, "from tar.bz2", string(got))
}

func TestHTTPBackend_SourceFileDrivesUnpack(t *testing.T) {
	t.Parallel()

	// glr-bla/glr-ehw: when the URL path has no usable extension, source.file
	// (Options.File) names the download and drives archive detection.
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := []byte("named by source.file")
	_ = tw.WriteHeader(&tar.Header{Name: "lib/x.lua", Size: int64(len(payload)), Mode: 0o644})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	// URL path is "/download" (no archive extension); only Options.File tells
	// the backend this is a tar.gz.
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/download?ref=v1", dst,
		fetch.Options{File: "foo-1.0.tar.gz"})
	require.NoError(t, err, "Fetch")

	got, err := os.ReadFile(filepath.Join(dst, "lib", "x.lua")) //nolint:gosec // test reads from a t.TempDir() path
	require.NoError(t, err, "expected extracted x.lua from source.file-named archive")
	assert.Equal(t, payload, got)
}

func TestHTTPBackend_RedirectLimit(t *testing.T) {
	t.Parallel()

	var srv *httptest.Server

	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path, http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/loop", t.TempDir(), fetch.Options{})
	require.Error(t, err, "expected error after redirect limit")
}

func TestHTTPBackend_UserAgentOverride(t *testing.T) {
	t.Parallel()

	var seen string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/x", t.TempDir(), fetch.Options{UserAgent: "custom/9"})
	require.NoError(t, err, "Fetch")
	assert.Equal(t, "custom/9", seen, "UA")
}

func TestHTTPBackend_InsecureServers(t *testing.T) {
	t.Parallel()
	// TLS server with self-signed cert; verify the InsecureServers path
	// works without needing a real CA.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("secured"))
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/x.bin", dst, fetch.Options{
		InsecureServers: []string{u.Host},
	})
	require.NoError(t, err, "Fetch with InsecureServers")

	b, _ := os.ReadFile(filepath.Join(dst, "x.bin")) //nolint:gosec // test reads from a t.TempDir() path
	assert.Equal(t, "secured", string(b), "body")

	// Without InsecureServers, the TLS handshake should fail.
	if _, err := fetch.FetchHTTP(context.Background(), srv.URL+"/x.bin", t.TempDir(), fetch.Options{}); err == nil {
		t.Error("expected TLS verification failure without InsecureServers")
	} else if !strings.Contains(err.Error(), "x509") && !strings.Contains(err.Error(), "tls") && !strings.Contains(err.Error(), "certificate") {
		t.Logf("error: %v (proceeding — may be transport-specific)", err)
	}
}
