package fetch_test

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/fetch"
)

// readmeFile is the file committed by the git fixtures below.
const readmeFile = "README"

// gitFixture builds a real git repository on disk with go-git (no `git`
// binary), so the git-backend tests stay hermetic. Commits use a fixed
// signature so the resulting hashes are deterministic.
type gitFixture struct {
	t   *testing.T
	dir string
	r   *git.Repository
	wt  *git.Worktree
}

func newGitFixture(t *testing.T) *gitFixture {
	t.Helper()

	dir := t.TempDir()

	r, err := git.PlainInit(dir, false)
	require.NoError(t, err, "PlainInit")

	wt, err := r.Worktree()
	require.NoError(t, err, "Worktree")

	return &gitFixture{t: t, dir: dir, r: r, wt: wt}
}

// commit writes file with content, stages it, and commits, returning the hash.
func (g *gitFixture) commit(file, content, msg string) plumbing.Hash {
	g.t.Helper()

	mustWrite(g.t, filepath.Join(g.dir, file), content)

	_, err := g.wt.Add(file)
	require.NoError(g.t, err, "Add %s", file)

	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}

	h, err := g.wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(g.t, err, "Commit %q", msg)

	return h
}

// tag creates a lightweight tag pointing at h.
func (g *gitFixture) tag(name string, h plumbing.Hash) {
	g.t.Helper()

	_, err := g.r.CreateTag(name, h, nil)
	require.NoError(g.t, err, "CreateTag %s", name)
}

// url is the git+file:// URL the fetch backend clones from.
func (g *gitFixture) url() string { return "git+file://" + g.dir }

// closedTCPAddr returns a loopback host:port that refuses connections: the
// port was just bound and released, so nothing is listening on it.
func closedTCPAddr(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err, "Listen")

	addr := ln.Addr().String()
	require.NoError(t, ln.Close(), "Close listener")

	return addr
}

func TestGitFetch_CloneAndCleanExport(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	g.commit(readmeFile, "hi", "c1")
	g.commit(".gitignore", "*.o\n", "c2")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), g.url(), dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	want := filepath.Join(dst, fetch.RepoNameFromURL(fetch.StripGitPlus(g.url())))
	assert.Equal(t, want, out, "clone directory")

	b, err := os.ReadFile(filepath.Join(out, readmeFile))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "hi", string(b), "cloned content")

	// The returned tree is a clean export: no VCS metadata leaks.
	_, err = os.Stat(filepath.Join(out, ".git"))
	assert.True(t, os.IsNotExist(err), ".git must be stripped")
	_, err = os.Stat(filepath.Join(out, ".gitignore"))
	assert.True(t, os.IsNotExist(err), ".gitignore must be stripped")
}

func TestGitFetch_TagCheckout(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	first := g.commit(readmeFile, "one", "c1")
	g.tag("v1.0", first)
	g.commit(readmeFile, "two", "c2")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), g.url(), dst, fetch.Options{Tag: "v1.0"})
	require.NoError(t, err, "Fetch tag")

	b, err := os.ReadFile(filepath.Join(out, readmeFile))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "one", string(b), "tagged content, not branch tip")
}

func TestGitFetch_ScmIdentifier(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	h := g.commit(readmeFile, "hi", "c1")

	var id string

	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), g.url(), dst, fetch.Options{
		Version:       "scm-1",
		IdentifierOut: &id,
	})
	require.NoError(t, err, "Fetch")

	// The identifier is the commit author timestamp plus the 7-char short hash.
	want := time.Unix(0, 0).Format("20060102.150405.") + h.String()[:7]
	assert.Equal(t, want, id, "scm identifier")
}

func TestGitFetch_ReleaseVersionSkipsIdentifier(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	g.commit(readmeFile, "hi", "c1")

	var id string

	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), g.url(), dst, fetch.Options{
		Version:       "1.0-1",
		IdentifierOut: &id,
	})
	require.NoError(t, err, "Fetch")
	assert.Empty(t, id, "release version must not get a git identifier")
}

func TestGitFetch_UnknownTagErrors(t *testing.T) {
	t.Parallel()

	g := newGitFixture(t)
	g.commit(readmeFile, "hi", "c1")

	_, err := fetch.FetchWith(context.Background(), g.url(), t.TempDir(),
		fetch.Options{Tag: "does-not-exist"})
	require.Error(t, err, "unknown tag must error")
}

func TestGitFetch_MissingRepoErrors(t *testing.T) {
	t.Parallel()

	src := "git+file://" + filepath.Join(t.TempDir(), "nope")

	_, err := fetch.FetchWith(context.Background(), src, t.TempDir(), fetch.Options{})
	require.Error(t, err, "clone of a missing repository must error")
	assert.Contains(t, err.Error(), "clone", "error should come from the clone step")
}

func TestGitFetch_DestDirNotCreatable(t *testing.T) {
	t.Parallel()

	blocker := filepath.Join(t.TempDir(), "f")
	mustWrite(t, blocker, "not a dir")

	dst := filepath.Join(blocker, "sub")

	_, err := fetch.FetchWith(context.Background(), "git+file:///ignored", dst, fetch.Options{})
	require.Error(t, err, "mkdir under a regular file must error")
	assert.Contains(t, err.Error(), "mkdir", "error should come from the mkdir step")
}

func TestGitFetch_SSHConnectionRefused(t *testing.T) {
	t.Parallel()

	// Exercises the git+ssh branches (scp rewrite check, ssh-agent auth wiring)
	// without any network: the loopback port refuses the connection.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	src := "git+ssh://git@" + closedTCPAddr(t) + "/team/repo.git"

	_, err := fetch.FetchWith(ctx, src, t.TempDir(), fetch.Options{})
	require.Error(t, err, "clone from a refused port must error")
}

func TestHTTPFetch_ErrorStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/missing.tar.gz", t.TempDir(), fetch.Options{})
	require.Error(t, err, "4xx must fail the fetch")
	assert.Contains(t, err.Error(), "status 404", "error should carry the status code")
}

func TestHTTPFetch_MD5Mismatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/f.bin", t.TempDir(),
		fetch.Options{MD5: "deadbeef"})
	require.Error(t, err, "wrong md5 must fail the fetch")
	assert.Contains(t, err.Error(), "MD5 check", "error should name the md5 check")
}

func TestHTTPFetch_RootPathSavesAsDownload(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("root body"))
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	b, err := os.ReadFile(filepath.Join(dst, "download"))
	require.NoError(t, err, "unusable basename falls back to \"download\"")
	assert.Equal(t, "root body", string(b), "body")
}

func TestHTTPFetch_InvalidURL(t *testing.T) {
	t.Parallel()

	_, err := fetch.FetchHTTP(context.Background(), "http://exa mple.com/x", t.TempDir(), fetch.Options{})
	require.Error(t, err, "unparsable URL must error")
	assert.Contains(t, err.Error(), "parse", "error should come from URL parsing")
}

func TestHTTPFetch_ConnectionRefused(t *testing.T) {
	t.Parallel()

	src := "http://" + closedTCPAddr(t) + "/x.bin"

	_, err := fetch.FetchHTTP(context.Background(), src, t.TempDir(), fetch.Options{})
	require.Error(t, err, "refused connection must error")
}

func TestHTTPFetch_DestDirNotCreatable(t *testing.T) {
	t.Parallel()

	blocker := filepath.Join(t.TempDir(), "f")
	mustWrite(t, blocker, "not a dir")

	dst := filepath.Join(blocker, "sub")

	// The mkdir failure precedes any network I/O, so the URL is never dialed.
	_, err := fetch.FetchHTTP(context.Background(), "http://127.0.0.1:1/x", dst, fetch.Options{})
	require.Error(t, err, "mkdir under a regular file must error")
	assert.Contains(t, err.Error(), "mkdir", "error should come from the mkdir step")
}

func TestHTTPFetch_TruncatedBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Declare more bytes than are written; the server closes the connection
		// short, so the client's body copy fails mid-stream.
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/f.bin", t.TempDir(), fetch.Options{})
	require.Error(t, err, "truncated body must error")
	assert.Contains(t, err.Error(), "copy body", "error should come from the body copy")
}

func TestHTTPFetch_CorruptZip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not a zip archive"))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/bad.zip", t.TempDir(), fetch.Options{})
	require.Error(t, err, "corrupt zip must error")
	assert.Contains(t, err.Error(), "unpack", "error should come from the unpack step")
}

func TestHTTPFetch_CorruptTarGz(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("this is not gzip data"))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/bad.tar.gz", t.TempDir(), fetch.Options{})
	require.Error(t, err, "corrupt tar.gz must error")
}

func TestHTTPFetch_CorruptTar(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
	}))
	defer srv.Close()

	_, err := fetch.FetchHTTP(context.Background(), srv.URL+"/bad.tar", t.TempDir(), fetch.Options{})
	require.Error(t, err, "corrupt tar must error")
}

func TestHTTPFetch_PlainTarWithDirAndSymlink(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	body := []byte("from plain tar")

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/", Typeflag: tar.TypeDir, Mode: 0o755,
	}), "write dir header")
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/x.lua", Size: int64(len(body)), Mode: 0o644,
	}), "write file header")

	_, err := tw.Write(body)
	require.NoError(t, err, "write file body")

	// Symlink entries are skipped by extraction, never materialized.
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "pkg/link.lua", Typeflag: tar.TypeSymlink, Linkname: "x.lua", Mode: 0o777,
	}), "write symlink header")
	require.NoError(t, tw.Close(), "close tar")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err = fetch.FetchHTTP(context.Background(), srv.URL+"/pkg.tar", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	got, err := os.ReadFile(filepath.Join(dst, "pkg", "x.lua"))
	require.NoError(t, err, "expected extracted x.lua")
	assert.Equal(t, body, got, "extracted content")

	_, err = os.Lstat(filepath.Join(dst, "pkg", "link.lua"))
	assert.True(t, os.IsNotExist(err), "symlink entries must be skipped")

	_, err = os.Stat(filepath.Join(dst, "pkg.tar"))
	assert.True(t, os.IsNotExist(err), "archive should be removed after unpack")
}

func TestHTTPFetch_TarPathTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	tw := tar.NewWriter(&buf)
	evil := []byte("evil")

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "../evil.txt", Size: int64(len(evil)), Mode: 0o644,
	}), "write traversal header")

	_, err := tw.Write(evil)
	require.NoError(t, err, "write body")
	require.NoError(t, tw.Close(), "close tar")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "sub")

	_, err = fetch.FetchHTTP(context.Background(), srv.URL+"/evil.tar", dst, fetch.Options{})
	require.Error(t, err, "traversal entry must error")
	assert.Contains(t, err.Error(), "escapes dst", "error should flag the traversal")

	_, err = os.Stat(filepath.Join(filepath.Dir(dst), "evil.txt"))
	assert.True(t, os.IsNotExist(err), "traversal file must not be created")
}

func TestHTTPFetch_ZipPathTraversal(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	f, err := zw.CreateHeader(&zip.FileHeader{Name: "../evil.txt"})
	require.NoError(t, err, "create traversal entry")

	_, err = f.Write([]byte("evil"))
	require.NoError(t, err, "write body")
	require.NoError(t, zw.Close(), "close zip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "sub")

	_, err = fetch.FetchHTTP(context.Background(), srv.URL+"/evil.zip", dst, fetch.Options{})
	require.Error(t, err, "traversal entry must error")
	assert.Contains(t, err.Error(), "escapes dst", "error should flag the traversal")
}

func TestHTTPFetch_ZipWithDirectoryEntry(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	_, err := zw.Create("sub/")
	require.NoError(t, err, "create dir entry")

	f, err := zw.Create("sub/a.lua")
	require.NoError(t, err, "create file entry")

	_, err = f.Write([]byte("a"))
	require.NoError(t, err, "write body")
	require.NoError(t, zw.Close(), "close zip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	defer srv.Close()

	dst := t.TempDir()
	_, err = fetch.FetchHTTP(context.Background(), srv.URL+"/pkg.zip", dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	st, err := os.Stat(filepath.Join(dst, "sub"))
	require.NoError(t, err, "expected extracted dir")
	assert.True(t, st.IsDir(), "dir entry must extract as a directory")

	_, err = os.Stat(filepath.Join(dst, "sub", "a.lua"))
	require.NoError(t, err, "expected extracted a.lua")
}

// countingCtx is a context.Context whose Err starts failing after the first
// call, so cancellation can be observed mid-walk without timing dependence.
type countingCtx struct {
	calls int
}

func (c *countingCtx) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *countingCtx) Done() <-chan struct{} { return nil }

func (c *countingCtx) Value(any) any { return nil }

func (c *countingCtx) Err() error {
	c.calls++
	if c.calls > 1 {
		return context.Canceled
	}

	return nil
}

func TestFileFetch_CanceledContext(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.lua"), "a")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := fetch.FetchFile(ctx, "file://"+src, t.TempDir(), fetch.Options{})
	require.ErrorIs(t, err, context.Canceled, "pre-canceled context must abort the fetch")
}

func TestFileFetch_CancelDuringCopy(t *testing.T) {
	t.Parallel()

	// The first ctx.Err check (at Fetch entry) passes; the per-entry check
	// inside the tree copy then sees the cancellation.
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.lua"), "a")

	_, err := fetch.FetchFile(&countingCtx{}, "file://"+src, t.TempDir(), fetch.Options{})
	require.ErrorIs(t, err, context.Canceled, "cancellation during the copy must abort")
}

func TestFileFetch_UnreadableSubdir(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root; permission checks are bypassed")
	}

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.lua"), "a")

	locked := filepath.Join(src, "locked")
	require.NoError(t, os.Mkdir(locked, 0o000), "mkdir locked")
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) })

	_, err := fetch.FetchFile(context.Background(), "file://"+src, t.TempDir(), fetch.Options{})
	require.Error(t, err, "unreadable subdir must fail the walk")
}

func TestFileFetch_DestBlockedByDirectory(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.lua"), "a")

	// destDir/<base> already exists as a directory, so the copy cannot open
	// the destination file.
	dst := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dst, "a.lua"), 0o750), "mkdir blocker")

	_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "a.lua"), dst,
		fetch.Options{})
	require.Error(t, err, "destination blocked by a directory must error")
}

func TestFileFetch_DestDirNotCreatable(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.lua"), "a")

	blocker := filepath.Join(t.TempDir(), "f")
	mustWrite(t, blocker, "not a dir")

	_, err := fetch.FetchFile(context.Background(), "file://"+src, filepath.Join(blocker, "sub"),
		fetch.Options{})
	require.Error(t, err, "mkdir under a regular file must error")
	assert.Contains(t, err.Error(), "mkdir", "error should come from the mkdir step")
}

func TestFileFetch_UnpackError(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "bad.tar.gz"), "this is not gzip data")

	_, err := fetch.FetchFile(context.Background(), "file://"+filepath.Join(src, "bad.tar.gz"),
		t.TempDir(), fetch.Options{})
	require.Error(t, err, "corrupt local archive must error")
	assert.Contains(t, err.Error(), "unpack", "error should come from the unpack step")
}

func TestSchemeOf_ParseFallbacks(t *testing.T) {
	t.Parallel()

	t.Run("opaque scheme without slashes", func(t *testing.T) {
		t.Parallel()

		got, err := fetch.SchemeOf("mailto:someone@example.com")
		require.NoError(t, err, "SchemeOf")
		assert.Equal(t, "mailto", got, "opaque URL scheme")
	})

	t.Run("schemeless path", func(t *testing.T) {
		t.Parallel()

		_, err := fetch.SchemeOf("./relative/path")
		require.Error(t, err, "schemeless input must error")
		assert.Contains(t, err.Error(), "no scheme", "error should say the scheme is missing")
	})

	t.Run("unparsable input", func(t *testing.T) {
		t.Parallel()

		_, err := fetch.SchemeOf("%zz")
		require.Error(t, err, "invalid URL escape must error")
	})
}
