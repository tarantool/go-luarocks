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
	"github.com/tarantool/go-luarocks/internal/transport"
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

// redirectingTLSServer is a TLS server, with a certificate no system root
// signs, that redirects every request to target.
func redirectingTLSServer(t *testing.T, target string) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// bodyServer serves body for every request, over TLS when tls is set.
func bodyServer(t *testing.T, tls bool, body string) *httptest.Server {
	t.Helper()

	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) })

	srv := httptest.NewUnstartedServer(h)
	if tls {
		srv.StartTLS()
	} else {
		srv.Start()
	}

	t.Cleanup(srv.Close)

	return srv
}

// hostOf is the host:port of a server URL.
func hostOf(t *testing.T, raw string) string {
	t.Helper()

	u, err := url.Parse(raw)
	require.NoError(t, err)

	return u.Host
}

// TestHTTPBackend_InsecureServersPerHop — the transport was picked by the
// host of the URL asked for, so a host listed in InsecureServers that
// redirected to another host had that host's certificate go unchecked too,
// and a redirect from https to plain http was followed. Each hop must be
// judged by its own host, and the downgrade refused.
func TestHTTPBackend_InsecureServersPerHop(t *testing.T) {
	t.Parallel()

	t.Run("redirect to another host is verified", func(t *testing.T) {
		t.Parallel()

		other := bodyServer(t, true, "unverified")
		listed := redirectingTLSServer(t, other.URL)

		_, err := fetch.FetchHTTP(context.Background(), listed.URL+"/x.bin", t.TempDir(), fetch.Options{
			InsecureServers: []string{hostOf(t, listed.URL)},
		})
		require.ErrorContains(t, err, "certificate", "the other host's certificate must be verified")
	})

	t.Run("redirect from https to http is refused", func(t *testing.T) {
		t.Parallel()

		plain := bodyServer(t, false, "in the clear")
		listed := redirectingTLSServer(t, plain.URL)

		_, err := fetch.FetchHTTP(context.Background(), listed.URL+"/x.bin", t.TempDir(), fetch.Options{
			InsecureServers: []string{hostOf(t, listed.URL), hostOf(t, plain.URL)},
		})
		require.ErrorIs(t, err, transport.ErrDowngrade)
	})

	t.Run("redirect from http to a listed https host is followed", func(t *testing.T) {
		t.Parallel()

		target := bodyServer(t, true, "upgraded")

		plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
		}))
		t.Cleanup(plain.Close)

		dst := t.TempDir()
		_, err := fetch.FetchHTTP(context.Background(), plain.URL+"/x.bin", dst, fetch.Options{
			InsecureServers: []string{hostOf(t, plain.URL), hostOf(t, target.URL)},
		})
		require.NoError(t, err)

		b, err := os.ReadFile(filepath.Join(dst, "x.bin")) //nolint:gosec // test temp path
		require.NoError(t, err)
		assert.Equal(t, "upgraded", string(b))
	})
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
