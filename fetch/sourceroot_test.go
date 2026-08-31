package fetch_test

// The Result.SourceRoot contract, one test per answer a backend can give.
// Only the backend knows what it produced — the URL does not say — and these
// pin each backend's answer so a caller can trust the flag instead of
// re-deriving it from the scheme.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/fetch"
)

// tarGz builds a .tar.gz whose members are the given path→content pairs.
func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name,
			Size: int64(len(body)),
			Mode: 0o644,
		}), "tar header %s", name)

		_, err := tw.Write([]byte(body))
		require.NoError(t, err, "tar write %s", name)
	}

	require.NoError(t, tw.Close(), "tar close")
	require.NoError(t, gz.Close(), "gzip close")

	return buf.Bytes()
}

// TestSources_GitCloneIsSourceRoot: the git backend returns the clone
// directory, which is the source root — upstream's git backend hands back the
// module directory the build enters directly (fetch/git.lua:164,
// build.lua:158-168), with no find_base_dir step in between.
func TestSources_GitCloneIsSourceRoot(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	g.commit(readmeFile, "hi", "c1")

	dst := t.TempDir()
	res, err := fetch.Sources(context.Background(), g.url(), dst, fetch.Options{})
	require.NoError(t, err, "Sources")

	assert.True(t, res.SourceRoot, "a git clone is already the source root")
	assert.Equal(t, filepath.Join(dst, filepath.Base(g.dir)), res.Path, "clone directory")
}

// TestSources_LocalTreeIsSourceRoot: the file backend copies a directory
// verbatim, so destDir is the source root. This is the trap the git bug would
// otherwise repeat here — the copied tree carries whatever subdirectories the
// user's checkout has, including one named like the URL's last segment.
func TestSources_LocalTreeIsSourceRoot(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "checks")
	mustWrite(t, filepath.Join(src, "CMakeLists.txt"), "project(checks)\n")
	mustWrite(t, filepath.Join(src, "checks", "init.lua"), "return {}\n")

	dst := t.TempDir()
	res, err := fetch.Sources(context.Background(), "file://"+src, dst, fetch.Options{})
	require.NoError(t, err, "Sources")

	assert.True(t, res.SourceRoot, "a copied tree is already the source root")
	assert.Equal(t, dst, res.Path, "copy destination")
}

// TestSources_LocalArchiveIsUnpackDir: the file backend's other path expands
// an archive into destDir, so the caller still has to resolve the real root
// inside it.
func TestSources_LocalArchiveIsUnpackDir(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	archive := filepath.Join(src, "foo-1.0.tar.gz")
	require.NoError(t,
		os.WriteFile(archive, tarGz(t, map[string]string{"foo-1.0/foo.lua": "return {}\n"}), 0o600),
		"write archive")

	dst := t.TempDir()
	res, err := fetch.Sources(context.Background(), "file://"+archive, dst, fetch.Options{})
	require.NoError(t, err, "Sources")

	assert.False(t, res.SourceRoot, "an unpacked archive still needs a base-dir descent")
	assert.Equal(t, dst, res.Path, "unpack directory")
}

// TestSources_HTTPDownloadIsUnpackDir: the http backend always produces an
// unpack directory, archive or not.
func TestSources_HTTPDownloadIsUnpackDir(t *testing.T) {
	t.Parallel()

	body := tarGz(t, map[string]string{"foo-1.0/foo.lua": "return {}\n"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dst := t.TempDir()
	res, err := fetch.Sources(context.Background(), srv.URL+"/foo-1.0.tar.gz", dst, fetch.Options{})
	require.NoError(t, err, "Sources")

	assert.False(t, res.SourceRoot, "an http fetch unpacks into destDir")
	assert.Equal(t, dst, res.Path, "unpack directory")
}

// TestSources_PassesBackendAnswerThrough: the dispatcher reports what the
// backend said rather than deciding for itself, and FetchWith is the same call
// with the flag dropped.
//
//nolint:paralleltest // mutates the package-global backends dispatch table
func TestSources_PassesBackendAnswerThrough(t *testing.T) {
	root := &recordingBackend{sourceRoot: true}
	unpack := &recordingBackend{sourceRoot: false}
	restore := fetch.SwapBackends(map[string]fetch.Backend{
		"git":  root,
		"http": unpack,
	})

	defer restore()

	dst := t.TempDir()

	res, err := fetch.Sources(context.Background(), "git://example.com/r.git", dst, fetch.Options{})
	require.NoError(t, err, "Sources git")
	assert.True(t, res.SourceRoot, "git backend answer")

	res, err = fetch.Sources(context.Background(), "http://example.com/r.tar.gz", dst, fetch.Options{})
	require.NoError(t, err, "Sources http")
	assert.False(t, res.SourceRoot, "http backend answer")

	path, err := fetch.FetchWith(context.Background(), "git://example.com/r.git", dst, fetch.Options{})
	require.NoError(t, err, "FetchWith")
	assert.Equal(t, dst, path, "FetchWith returns Result.Path")
}
