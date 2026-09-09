package fetch_test

// Tests for fetch.File, the raw single-file retrieval `luarocks download`
// needs. What they pin is everything that separates it from Fetch: the file
// arrives under its own name, its bytes are untouched, and an archive is NOT
// expanded — a .rock is a zip, and unpacking one would leave the caller with
// a tree and no rock.

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/fetch"
)

// zipBytes builds a minimal zip archive holding one named entry, which is
// what a .rock file is.
func zipBytes(t *testing.T, name, body string) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	w, err := zw.Create(name)
	require.NoError(t, err, "zip create")

	_, err = w.Write([]byte(body))
	require.NoError(t, err, "zip write")
	require.NoError(t, zw.Close(), "zip close")

	return buf.Bytes()
}

func TestFile_CopiesLocalFile(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dest := t.TempDir()
	body := []byte("rockspec body\n")

	spec := filepath.Join(src, "hello-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(spec, body, 0o600), "write source")

	for _, form := range []struct{ name, url string }{
		{name: "bare path", url: spec},
		{name: "file URL", url: "file://" + spec},
	} {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()

			into := filepath.Join(dest, form.name)

			got, err := fetch.File(context.Background(), form.url, into, fetch.Options{})
			require.NoError(t, err, "File(%s)", form.url)
			assert.Equal(t, filepath.Join(into, "hello-1.0-1.rockspec"), got,
				"the copy must keep the source basename")

			landed, err := os.ReadFile(got)
			require.NoError(t, err, "read the copy")
			assert.Equal(t, body, landed, "the bytes must arrive unchanged")
		})
	}
}

// TestFile_DoesNotUnpackRock is the reason File exists at all: fetch.Sources
// would expand this zip into destDir and delete it.
func TestFile_DoesNotUnpackRock(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dest := t.TempDir()
	archive := zipBytes(t, "hello-1.0-1.rockspec", "package = 'hello'\n")

	rock := filepath.Join(src, "hello-1.0-1.all.rock")
	require.NoError(t, os.WriteFile(rock, archive, 0o600), "write rock")

	got, err := fetch.File(context.Background(), rock, dest, fetch.Options{})
	require.NoError(t, err, "File")
	assert.Equal(t, filepath.Join(dest, "hello-1.0-1.all.rock"), got)

	landed, err := os.ReadFile(got)
	require.NoError(t, err, "read the rock")
	assert.Equal(t, archive, landed, "the rock file must arrive byte-identical, not expanded")

	entries, err := os.ReadDir(dest)
	require.NoError(t, err, "read destination")
	assert.Len(t, entries, 1, "nothing but the rock itself may appear in the destination")
}

func TestFile_DownloadsOverHTTP(t *testing.T) {
	t.Parallel()

	body := []byte("source rock payload")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello-1.0-1.src.rock" {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	dest := t.TempDir()

	got, err := fetch.File(context.Background(), srv.URL+"/hello-1.0-1.src.rock", dest, fetch.Options{})
	require.NoError(t, err, "File")
	assert.Equal(t, filepath.Join(dest, "hello-1.0-1.src.rock"), got,
		"the download must keep the URL's last path segment")

	landed, err := os.ReadFile(got)
	require.NoError(t, err, "read the download")
	assert.Equal(t, body, landed)

	// The `.part` staging file must not survive a completed transfer.
	entries, err := os.ReadDir(dest)
	require.NoError(t, err, "read destination")
	assert.Len(t, entries, 1, "only the finished file may remain")
}

func TestFile_HTTPNotFoundErrors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	dest := t.TempDir()

	_, err := fetch.File(context.Background(), srv.URL+"/missing-1.0-1.rockspec", dest, fetch.Options{})
	require.Error(t, err, "a 404 must fail loudly")
	assert.Contains(t, err.Error(), "404")

	entries, err := os.ReadDir(dest)
	require.NoError(t, err, "read destination")
	assert.Empty(t, entries, "a failed download must leave nothing behind, not even the .part file")
}

func TestFile_MissingLocalFileErrors(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent-1.0-1.rockspec")

	_, err := fetch.File(context.Background(), missing, t.TempDir(), fetch.Options{})
	require.Error(t, err, "a missing source must fail")
	assert.Contains(t, err.Error(), missing, "the error must name the file")
}

// TestFile_SameFileErrors pins upstream fs.copy's refusal: copying a file
// onto itself would truncate it, so it must fail instead of emptying the
// server's own artifact.
func TestFile_SameFileErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := filepath.Join(dir, "hello-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(spec, []byte("package = 'hello'\n"), 0o600), "write source")

	_, err := fetch.File(context.Background(), spec, dir, fetch.Options{})
	require.Error(t, err, "copying a file onto itself must fail")
	assert.Contains(t, err.Error(), "same file")

	body, err := os.ReadFile(spec)
	require.NoError(t, err, "read source")
	assert.NotEmpty(t, body, "the source must not have been truncated")
}

func TestFile_RejectsUnsupportedScheme(t *testing.T) {
	t.Parallel()

	_, err := fetch.File(context.Background(), "git+https://example.com/x.git", t.TempDir(), fetch.Options{})
	require.Error(t, err, "a git URL has no single file to hand back")
	assert.ErrorIs(t, err, rocks.ErrUnsupportedRockspecFeature)
}

func TestFile_RejectsEmptyArguments(t *testing.T) {
	t.Parallel()

	_, err := fetch.File(context.Background(), "", t.TempDir(), fetch.Options{})
	require.Error(t, err, "an empty URL must fail")

	_, err = fetch.File(context.Background(), "/tmp/x", "", fetch.Options{})
	require.Error(t, err, "an empty destination must fail")
}

// TestFile_DirectorySourceErrors — a directory has no basename to save under
// and copying a tree is Fetch's job, not this one's.
func TestFile_DirectorySourceErrors(t *testing.T) {
	t.Parallel()

	_, err := fetch.File(context.Background(), t.TempDir(), t.TempDir(), fetch.Options{})
	require.Error(t, err, "a directory source must fail")
	assert.Contains(t, err.Error(), "directory")
}
