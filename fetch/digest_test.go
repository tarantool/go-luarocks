package fetch_test

// Tests for Options.SHA256: every fetch that retrieves one file checks it
// before anything unpacks or keeps it, and a source a digest cannot describe
// refuses the check instead of skipping it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/fetch"
)

const digestRock = "hello-1.0-1.all.rock"

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// releaseServer serves body the way GitHub serves a release asset: the
// download URL answers 302 to an object URL elsewhere, with a query string,
// and only that one carries the bytes. It counts the requests it gets.
func releaseServer(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		switch {
		case strings.HasPrefix(r.URL.Path, "/releases/download/"):
			http.Redirect(w, r, "/objects/0123?token=x", http.StatusFound)
		case r.URL.Path == "/objects/0123":
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &hits
}

// requireEmptyDir fails unless dir holds nothing: no artifact, no `.part`
// file and nothing unpacked.
func requireEmptyDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a file that failed its check is left in %s", dir)
}

// requireMismatch checks that err is a checksum mismatch naming the file and
// both digests.
func requireMismatch(t *testing.T, err error, file, got, want string) {
	t.Helper()

	require.ErrorIs(t, err, rocks.ErrChecksumMismatch)
	require.ErrorContains(t, err, file)
	require.ErrorContains(t, err, got)
	require.ErrorContains(t, err, want)
}

func TestFetch_SHA256_HTTPVerifiesAfterRedirect(t *testing.T) {
	t.Parallel()

	body := zipBytes(t, "hello.lua", "return 1\n")
	srv, _ := releaseServer(t, body)
	url := srv.URL + "/releases/download/hello-1.0-1/" + digestRock

	for _, want := range []string{sha256Hex(body), strings.ToUpper(sha256Hex(body))} {
		dest := t.TempDir()

		res, err := fetch.Sources(context.Background(), url, dest, fetch.Options{SHA256: want})
		require.NoError(t, err, "a matching digest, in either case")
		requireFileBody(t, filepath.Join(res.Path, "hello.lua"), "return 1\n")
	}
}

func TestFetch_SHA256_HTTPMismatch(t *testing.T) {
	t.Parallel()

	body := zipBytes(t, "hello.lua", "return 1\n")
	srv, _ := releaseServer(t, body)
	wrong := sha256Hex([]byte("another rock"))
	dest := t.TempDir()

	_, err := fetch.Sources(context.Background(), srv.URL+"/releases/download/hello-1.0-1/"+digestRock, dest,
		fetch.Options{SHA256: wrong})
	requireMismatch(t, err, digestRock, sha256Hex(body), wrong)
	requireEmptyDir(t, dest)
}

func TestFetch_SHA256_MalformedFailsBeforeFetching(t *testing.T) {
	t.Parallel()

	srv, hits := releaseServer(t, []byte("x"))
	local := filepath.Join(t.TempDir(), digestRock)
	require.NoError(t, os.WriteFile(local, []byte("x"), 0o600))

	for _, bad := range []string{"abc", strings.Repeat("g", 64), sha256Hex([]byte("x")) + "00"} {
		for _, url := range []string{srv.URL + "/releases/download/v/" + digestRock, local} {
			_, err := fetch.Sources(context.Background(), url, t.TempDir(), fetch.Options{SHA256: bad})
			require.ErrorContains(t, err, "is not a sha256 digest", "Sources %s with %q", url, bad)

			_, err = fetch.File(context.Background(), url, t.TempDir(), fetch.Options{SHA256: bad})
			require.ErrorContains(t, err, "is not a sha256 digest", "File %s with %q", url, bad)
		}
	}

	assert.Zero(t, hits.Load(), "a malformed digest must fail before any request")
}

func TestFetch_SHA256_FileBackend(t *testing.T) {
	t.Parallel()

	body := zipBytes(t, "hello.lua", "return 1\n")
	src := filepath.Join(t.TempDir(), digestRock)
	require.NoError(t, os.WriteFile(src, body, 0o600))

	for _, url := range []string{src, "file://" + src} {
		dest := t.TempDir()
		res, err := fetch.Sources(context.Background(), url, dest, fetch.Options{SHA256: sha256Hex(body)})
		require.NoError(t, err, "a matching digest for %s", url)
		requireFileBody(t, filepath.Join(res.Path, "hello.lua"), "return 1\n")

		wrong := sha256Hex([]byte("another rock"))
		dest = t.TempDir()
		_, err = fetch.Sources(context.Background(), url, dest, fetch.Options{SHA256: wrong})
		requireMismatch(t, err, digestRock, sha256Hex(body), wrong)
		requireEmptyDir(t, dest)
	}
}

// TestFetch_SHA256_NotAFile pins that a source a digest cannot describe — a
// local directory, a git repository — fails when a digest is asked for,
// rather than being fetched unverified.
func TestFetch_SHA256_NotAFile(t *testing.T) {
	t.Parallel()

	digest := sha256Hex([]byte("x"))

	_, err := fetch.Sources(context.Background(), t.TempDir(), t.TempDir(), fetch.Options{SHA256: digest})
	require.ErrorContains(t, err, "not one file a digest can check", "a local directory")

	// .invalid never resolves (RFC 2606): the refusal comes before the clone.
	_, err = fetch.Sources(context.Background(), "git+https://rocks.invalid/hello.git", t.TempDir(),
		fetch.Options{SHA256: digest})
	require.ErrorContains(t, err, "not one file a digest can check", "a git repository")
}

func TestFile_SHA256(t *testing.T) {
	t.Parallel()

	body := zipBytes(t, "hello.lua", "return 1\n")
	srv, _ := releaseServer(t, body)
	local := filepath.Join(t.TempDir(), digestRock)
	require.NoError(t, os.WriteFile(local, body, 0o600))

	for _, url := range []string{srv.URL + "/releases/download/hello-1.0-1/" + digestRock, local, "file://" + local} {
		dest := t.TempDir()
		got, err := fetch.File(context.Background(), url, dest, fetch.Options{SHA256: sha256Hex(body)})
		require.NoError(t, err, "a matching digest for %s", url)
		assert.Equal(t, filepath.Join(dest, digestRock), got, "the file keeps the URL's name, not the redirect's")

		wrong := sha256Hex([]byte("another rock"))
		dest = t.TempDir()
		_, err = fetch.File(context.Background(), url, dest, fetch.Options{SHA256: wrong})
		requireMismatch(t, err, digestRock, sha256Hex(body), wrong)
		requireEmptyDir(t, dest)
	}
}

func requireFileBody(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path) //nolint:gosec // test reads from a t.TempDir() path
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}
