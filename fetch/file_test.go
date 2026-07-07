package fetch_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // test computes md5 to match source.md5 verification
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/fetch"
)

func md5hex(b []byte) string {
	sum := md5.Sum(b) //nolint:gosec // test helper

	return hex.EncodeToString(sum[:])
}

func TestFileBackend_CopiesTree(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()

	mustWrite(t, filepath.Join(src, "a.lua"), "a")
	mustWrite(t, filepath.Join(src, "sub", "b.lua"), "b")

	got, err := fetch.FetchFile(context.Background(), "file://"+src, dst, fetch.Options{})
	require.NoError(t, err, "Fetch")
	assert.Equal(t, dst, got)

	b, err := os.ReadFile(filepath.Join(dst, "a.lua")) //nolint:gosec // test reads from a t.TempDir() path
	if assert.NoError(t, err, "a.lua") {
		assert.Equal(t, "a", string(b), "a.lua")
	}

	b, err = os.ReadFile(filepath.Join(dst, "sub", "b.lua")) //nolint:gosec // test reads from a t.TempDir() path
	if assert.NoError(t, err, "sub/b.lua") {
		assert.Equal(t, "b", string(b), "sub/b.lua")
	}
}

func TestFileBackend_SingleFile(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "foo.rockspec"), "rockspec=true")

	_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "foo.rockspec"), dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	b, err := os.ReadFile(filepath.Join(dst, "foo.rockspec")) //nolint:gosec // test reads from a t.TempDir() path
	if assert.NoError(t, err, "copy") {
		assert.Equal(t, "rockspec=true", string(b), "copy")
	}
}

func TestFileBackend_HonorsSourceFile(t *testing.T) {
	t.Parallel()

	// glr-4pu: opts.File overrides the URL-derived basename, so an
	// extension-less source is saved under source.file.
	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "download"), "payload=1")

	_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "download"), dst,
		fetch.Options{File: "foo-1.0.rockspec"})
	require.NoError(t, err, "Fetch")

	_, err = os.Stat(filepath.Join(dst, "foo-1.0.rockspec"))
	require.NoError(t, err, "expected file saved under source.file name")
}

func TestFileBackend_MD5Verification(t *testing.T) {
	t.Parallel()

	body := "rockspec=true"

	// Correct md5 (and an anchored prefix of it) both pass; a wrong one fails.
	t.Run("match", func(t *testing.T) {
		t.Parallel()
		src, dst := t.TempDir(), t.TempDir()
		mustWrite(t, filepath.Join(src, "foo.rockspec"), body)
		_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "foo.rockspec"), dst,
			fetch.Options{MD5: md5hex([]byte(body))})
		require.NoError(t, err)
	})

	t.Run("prefix match", func(t *testing.T) {
		t.Parallel()
		src, dst := t.TempDir(), t.TempDir()
		mustWrite(t, filepath.Join(src, "foo.rockspec"), body)
		_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "foo.rockspec"), dst,
			fetch.Options{MD5: md5hex([]byte(body))[:8]})
		require.NoError(t, err, "anchored prefix should pass")
	})

	t.Run("mismatch", func(t *testing.T) {
		t.Parallel()
		src, dst := t.TempDir(), t.TempDir()
		mustWrite(t, filepath.Join(src, "foo.rockspec"), body)
		_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "foo.rockspec"), dst,
			fetch.Options{MD5: "deadbeef"})
		require.ErrorContains(t, err, "MD5 check for foo.rockspec has failed")
	})
}

func TestFileBackend_UnpacksArchive(t *testing.T) {
	t.Parallel()

	// glr-6tr: a file:// source pointing at an archive must be extracted, not
	// left as a copied tarball.
	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("from local tar.gz")
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "lib/x.lua", Size: int64(len(body)), Mode: 0o644}))
	_, _ = tw.Write(body)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	src := t.TempDir()
	archive := filepath.Join(src, "pkg.tar.gz")
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))

	dst := t.TempDir()
	_, err := fetch.FetchFile(context.Background(), "file://"+archive, dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	got, err := os.ReadFile(filepath.Join(dst, "lib", "x.lua")) //nolint:gosec // test reads from a t.TempDir() path
	require.NoError(t, err, "expected extracted x.lua")
	assert.Equal(t, body, got)

	// The archive itself is removed after extraction.
	_, statErr := os.Stat(filepath.Join(dst, "pkg.tar.gz"))
	assert.True(t, os.IsNotExist(statErr), "archive should be removed after unpack")
}

func TestFetchWith_SchemelessPathIsFile(t *testing.T) {
	t.Parallel()

	// glr-bvp: a schemeless local path routes to the file backend instead of
	// being rejected for having no scheme.
	src := t.TempDir()
	path := filepath.Join(src, "mymod.rockspec")
	require.NoError(t, os.WriteFile(path, []byte("rockspec=true"), 0o644))

	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), path, dst, fetch.Options{})
	require.NoError(t, err, "schemeless path should fetch via file backend")

	b, err := os.ReadFile(filepath.Join(dst, "mymod.rockspec")) //nolint:gosec // test reads from a t.TempDir() path
	require.NoError(t, err)
	assert.Equal(t, "rockspec=true", string(b))
}

func TestFileBackend_MissingSource(t *testing.T) {
	t.Parallel()

	dst := t.TempDir()
	_, err := fetch.FetchFile(context.Background(), "file:///nope/nada/nothing", dst, fetch.Options{})
	require.Error(t, err, "expected error for missing source")
}

func mustWrite(t *testing.T, p, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750), "mkdir")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600), "write")
}
