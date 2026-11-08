package fetch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"io/fs"
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

// The tests below hand the fetch backends a source.file, a URL or an archive
// whose names leave the destination dir, and check that the fetch fails, or
// keeps the entry inside, and touches nothing outside. Each lays out one temp
// dir holding the destination and an "outside" sibling the escaping name
// points at, so a regression lands in the test's own temp dir.

type fetchLayout struct {
	parent, dest, outside string
}

func newFetchLayout(t *testing.T) fetchLayout {
	t.Helper()

	parent := t.TempDir()
	l := fetchLayout{
		parent:  parent,
		dest:    filepath.Join(parent, "dest"),
		outside: filepath.Join(parent, "outside"),
	}

	mustWrite(t, filepath.Join(l.outside, "victim"), "keep")

	return l
}

// assertOutsideIntact checks that the outside dir still holds just its victim
// file, unchanged.
func (l fetchLayout) assertOutsideIntact(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(l.outside)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	assert.Equal(t, []string{"victim"}, names, "nothing may be added to or removed from %s", l.outside)

	body, err := os.ReadFile(filepath.Join(l.outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(body))
}

// TestHTTPBackend_SourceFileStaysInDestDir: source.file names the download,
// so "../outside/victim" replaced the victim with the body, and an archive
// name there was unpacked and then deleted, all without running build code.
func TestHTTPBackend_SourceFileStaysInDestDir(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pwned"))
	}))
	defer srv.Close()

	for _, file := range []string{"../outside/victim", "/abs/victim", ".", ".."} {
		l := newFetchLayout(t)

		_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/x.tar.gz", l.dest, fetch.Options{File: file})
		require.ErrorIs(t, err, rocks.ErrUnsafePath, file)
		l.assertOutsideIntact(t)
	}
}

// TestHTTPBackend_URLNamingParentStaysInDestDir: without source.file the
// download is named after the last element of the URL path, which can be
// "..".
func TestHTTPBackend_URLNamingParentStaysInDestDir(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("pwned"))
	}))
	defer srv.Close()

	l := newFetchLayout(t)
	dest := filepath.Join(l.outside, "dest")

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/a/..", dest, fetch.Options{})
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.parent, ".outside.part"))

	_, err = fetch.File(context.Background(), srv.URL+"/a/..", dest, fetch.Options{})
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.NoFileExists(t, filepath.Join(l.parent, ".outside.part"))
}

// TestFileBackend_SourceFileStaysInDestDir: the file backend copies a local
// source under source.file, so "../outside/victim" truncated the victim and
// wrote the source over it.
func TestFileBackend_SourceFileStaysInDestDir(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"../outside/victim", "/abs/victim", "."} {
		l := newFetchLayout(t)
		src := filepath.Join(l.parent, "download")
		mustWrite(t, src, "pwned")

		_, err := fetch.FetchFile(context.Background(), "file://"+src, l.dest, fetch.Options{File: file})
		require.ErrorIs(t, err, rocks.ErrUnsafePath, file)
		l.assertOutsideIntact(t)
	}
}

// zipEntry is one member of a test zip archive.
type zipEntry struct {
	name string
	mode fs.FileMode
	body string
}

func zipArchive(t *testing.T, entries []zipEntry) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)

		w, err := zw.CreateHeader(h)
		require.NoError(t, err)

		_, err = w.Write([]byte(e.body))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func tarArchive(t *testing.T, hdrs []*tar.Header) []byte {
	t.Helper()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)

	for _, h := range hdrs {
		body := []byte("pwned")
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(body))
		}

		require.NoError(t, tw.WriteHeader(h))

		if h.Typeflag == tar.TypeReg {
			_, err := tw.Write(body)
			require.NoError(t, err)
		}
	}

	require.NoError(t, tw.Close())

	return buf.Bytes()
}

// fetchArchive writes archive as name beside dest and fetches it into dest
// with the file backend, which unpacks it.
func (l fetchLayout) fetchArchive(t *testing.T, name string, archive []byte) error {
	t.Helper()

	src := filepath.Join(l.parent, name)
	require.NoError(t, os.WriteFile(src, archive, 0o600))

	_, err := fetch.FetchFile(context.Background(), "file://"+src, l.dest, fetch.Options{})

	return err
}

// TestUnpack_EscapingEntries: an archive entry that reaches above the
// destination fails the unpack, and one with an absolute name stays inside
// it, as tar and unzip strip the leading slash.
func TestUnpack_EscapingEntries(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"../outside/victim", "a/../../outside/victim"} {
		l := newFetchLayout(t)
		require.ErrorIs(t, l.fetchArchive(t, "x.zip", zipArchive(t, []zipEntry{{name, 0o644, "pwned"}})),
			rocks.ErrUnsafePath, "zip %s", name)
		l.assertOutsideIntact(t)

		l = newFetchLayout(t)
		require.ErrorIs(t, l.fetchArchive(t, "x.tar", tarArchive(t, []*tar.Header{{Name: name, Typeflag: tar.TypeReg, Mode: 0o644}})),
			rocks.ErrUnsafePath, "tar %s", name)
		l.assertOutsideIntact(t)
	}

	l := newFetchLayout(t)
	abs := filepath.Join(l.outside, "victim")
	require.NoError(t, l.fetchArchive(t, "x.zip", zipArchive(t, []zipEntry{{abs, 0o644, "pwned"}})))
	assert.FileExists(t, filepath.Join(l.dest, abs))
	l.assertOutsideIntact(t)

	l = newFetchLayout(t)
	require.NoError(t, l.fetchArchive(t, "x.tar", tarArchive(t, []*tar.Header{{Name: abs, Typeflag: tar.TypeReg, Mode: 0o644}})))
	assert.FileExists(t, filepath.Join(l.dest, abs))
	l.assertOutsideIntact(t)
}

// TestUnpack_SymlinkThenFileThroughIt is the "zip slip" through a link: a
// first entry makes a symlink to outside, a second writes beneath it. Neither
// format extracts a symlink, so the second entry lands inside.
func TestUnpack_SymlinkThenFileThroughIt(t *testing.T) {
	t.Parallel()

	l := newFetchLayout(t)
	err := l.fetchArchive(t, "x.zip", zipArchive(t, []zipEntry{
		{"link", fs.ModeSymlink | 0o777, l.outside},
		{"link/victim", 0o644, "pwned"},
	}))
	// The link became a regular file holding its target, so nothing can be
	// written beneath it.
	require.Error(t, err)
	l.assertOutsideIntact(t)

	l = newFetchLayout(t)
	require.NoError(t, l.fetchArchive(t, "x.tar", tarArchive(t, []*tar.Header{
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: l.outside},
		{Name: "link/victim", Typeflag: tar.TypeReg, Mode: 0o644},
	})))
	assert.FileExists(t, filepath.Join(l.dest, "link", "victim"))
	l.assertOutsideIntact(t)
}

// TestUnpack_ThroughExistingSymlink: a symlink already in the destination,
// pointing out, does not let an entry beneath it out.
func TestUnpack_ThroughExistingSymlink(t *testing.T) {
	t.Parallel()

	for name, archive := range map[string]func(t *testing.T) []byte{
		"x.zip": func(t *testing.T) []byte {
			t.Helper()

			return zipArchive(t, []zipEntry{{"link/victim", 0o644, "pwned"}})
		},
		"x.tar": func(t *testing.T) []byte {
			t.Helper()

			return tarArchive(t, []*tar.Header{{Name: "link/victim", Typeflag: tar.TypeReg, Mode: 0o644}})
		},
	} {
		l := newFetchLayout(t)
		require.NoError(t, os.MkdirAll(l.dest, 0o750))
		require.NoError(t, os.Symlink(l.outside, filepath.Join(l.dest, "link")))

		require.ErrorIs(t, l.fetchArchive(t, name, archive(t)), rocks.ErrUnsafePath, name)
		l.assertOutsideIntact(t)
	}
}

// TestUnpack_DropsSpecialModeBits: the setuid, setgid and sticky bits of an
// archive entry are not kept.
func TestUnpack_DropsSpecialModeBits(t *testing.T) {
	t.Parallel()

	l := newFetchLayout(t)
	require.NoError(t, l.fetchArchive(t, "x.zip", zipArchive(t, []zipEntry{
		{"bin/suid", fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o755, "#!/bin/sh\n"},
	})))

	st, err := os.Stat(filepath.Join(l.dest, "bin", "suid"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), st.Mode(), "zip entry mode")

	l = newFetchLayout(t)
	require.NoError(t, l.fetchArchive(t, "x.tar", tarArchive(t, []*tar.Header{
		{Name: "bin/suid", Typeflag: tar.TypeReg, Mode: 0o7755},
	})))

	st, err = os.Stat(filepath.Join(l.dest, "bin", "suid"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o755), st.Mode(), "tar entry mode")
}
