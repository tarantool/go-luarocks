//go:build integration

package fetch_test

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/fetch"
)

// repoBuilder constructs a real git repository on disk using go-git (no `git`
// binary), so the integration suite is self-contained. Commits use a fixed
// signature, making the resulting commit hashes deterministic.
type repoBuilder struct {
	t   *testing.T
	dir string
	r   *git.Repository
	wt  *git.Worktree
}

func newRepoBuilder(t *testing.T) *repoBuilder {
	t.Helper()

	dir := t.TempDir()

	r, err := git.PlainInit(dir, false)
	require.NoError(t, err, "PlainInit")

	wt, err := r.Worktree()
	require.NoError(t, err, "Worktree")

	return &repoBuilder{t: t, dir: dir, r: r, wt: wt}
}

// commit writes file with content, stages it, and commits, returning the hash.
func (b *repoBuilder) commit(file, content, msg string) plumbing.Hash {
	b.t.Helper()

	require.NoError(b.t, os.WriteFile(filepath.Join(b.dir, file), []byte(content), 0o644))

	_, err := b.wt.Add(file)
	require.NoError(b.t, err, "Add %s", file)

	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}

	h, err := b.wt.Commit(msg, &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(b.t, err, "Commit %q", msg)

	return h
}

// tag creates a lightweight tag pointing at h.
func (b *repoBuilder) tag(name string, h plumbing.Hash) {
	b.t.Helper()

	_, err := b.r.CreateTag(name, h, nil)
	require.NoError(b.t, err, "CreateTag %s", name)
}

// branch creates and checks out a new branch off the current HEAD.
func (b *repoBuilder) branch(name string) {
	b.t.Helper()

	require.NoError(b.t, b.wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(name),
		Create: true,
	}), "checkout -b %s", name)
}

// url is the git+file:// URL the fetch backend clones from.
func (b *repoBuilder) url() string { return "git+file://" + b.dir }

// TestGitBackend_LocalClone is the baseline: clone a single-commit repo and
// confirm the file lands in the working tree.
func TestGitBackend_LocalClone(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "hi", "init")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	got, err := os.ReadFile(filepath.Join(out, "README"))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "hi", string(got))

	// glr-bx7: the returned tree is a clean export — no VCS metadata leaks.
	_, err = os.Stat(filepath.Join(out, ".git"))
	assert.True(t, os.IsNotExist(err), ".git must be stripped from the fetched tree")
}

// TestGitBackend_CloneDirName pins the returned path: the backend clones into
// destDir/<repo-name>, mirroring git's default directory naming.
func TestGitBackend_CloneDirName(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "hi", "init")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	want := filepath.Join(dst, fetch.RepoNameFromURL(fetch.StripGitPlus(b.url())))
	assert.Equal(t, want, out, "clone directory")
}

// TestGitBackend_TagCheckout verifies a Tag option resolves to the tagged
// commit, not the branch tip — the parity-critical behavior of refName.
func TestGitBackend_TagCheckout(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	first := b.commit("README", "one", "first")
	b.tag("v1.0", first)
	b.commit("README", "two", "second")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{Tag: "v1.0"})
	require.NoError(t, err, "Fetch tag")

	// The tree is a clean export (no .git; glr-bx7), so checkout correctness
	// is verified by content: the tag's README differs from the branch tip.
	got, err := os.ReadFile(filepath.Join(out, "README"))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "one", string(got), "tagged content, not branch tip")
}

// TestGitBackend_BranchCheckout verifies a Branch option checks out that
// branch's content.
func TestGitBackend_BranchCheckout(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "one", "first")
	b.branch("feature")
	b.commit("feature.txt", "feat", "add feature")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{Branch: "feature"})
	require.NoError(t, err, "Fetch branch")

	_, err = os.Stat(filepath.Join(out, "feature.txt"))
	assert.NoError(t, err, "expected feature.txt from feature branch")
}

// TestGitBackend_TagWinsOverBranch documents that when both are set the tag
// wins, matching refOpt/refName precedence.
func TestGitBackend_TagWinsOverBranch(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	first := b.commit("README", "one", "first")
	b.tag("v1.0", first)
	b.branch("feature")
	b.commit("feature.txt", "feat", "add feature")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst,
		fetch.Options{Tag: "v1.0", Branch: "feature"})
	require.NoError(t, err, "Fetch")

	// Clean export (glr-bx7): verify the tag won by content — the tag predates
	// the feature branch, so feature.txt must be absent and README is "one".
	got, err := os.ReadFile(filepath.Join(out, "README"))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "one", string(got), "tag content, not branch tip")
	_, err = os.Stat(filepath.Join(out, "feature.txt"))
	assert.Error(t, err, "feature.txt must be absent when the tag wins")
}

// TestGitBackend_ShallowDepth confirms a git+file clone is shallow (Depth 1):
// only the tip commit is fetched even when the repo has history.
func TestGitBackend_ShallowDepth(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "one", "first")
	b.commit("README", "two", "second")

	dst := t.TempDir()
	out, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{})
	require.NoError(t, err, "Fetch")

	// Depth=1 is applied at clone time (see the TestShallowScheme unit test);
	// it leaves no observable trace once the .git metadata is stripped from
	// the clean export (glr-bx7), so here we only confirm the tip content.
	got, err := os.ReadFile(filepath.Join(out, "README"))
	require.NoError(t, err, "read cloned README")
	assert.Equal(t, "two", string(got), "shallow clone keeps the tip")
}

// TestGitBackend_BadRefErrors confirms an unknown ref surfaces an error rather
// than silently cloning the default branch.
func TestGitBackend_BadRefErrors(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "one", "first")

	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{Tag: "does-not-exist"})
	assert.Error(t, err, "unknown tag must error")
}

// TestGitBackend_MatchesGitBinary is the direct compatibility check: the
// go-git backend and a real `git clone` of the same repo must agree on the
// resulting HEAD and file contents. Skipped when git is not on PATH.
func TestGitBackend_MatchesGitBinary(t *testing.T) {
	t.Parallel()

	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}

	b := newRepoBuilder(t)
	b.commit("README", "one", "first")
	b.commit("README", "two", "second")

	// go-git backend clone.
	goDst := t.TempDir()
	goOut, err := fetch.FetchWith(context.Background(), b.url(), goDst, fetch.Options{})
	require.NoError(t, err, "go-git Fetch")

	// Real git binary clone of the same repo (shallow, matching the backend).
	binDir := filepath.Join(t.TempDir(), "bin-clone")

	cmd := exec.CommandContext(context.Background(), gitBin,
		"clone", "--depth=1", "file://"+b.dir, binDir)

	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	binOut, err := cmd.CombinedOutput()
	require.NoError(t, err, "git clone: %s", binOut)

	// The go-git backend returns a clean export (no .git; glr-bx7), so parity
	// with the git binary is checked on the working tree content rather than
	// via HEAD.
	goReadme, err := os.ReadFile(filepath.Join(goOut, "README"))
	require.NoError(t, err, "read go-git README")
	binReadme, err := os.ReadFile(filepath.Join(binDir, "README"))
	require.NoError(t, err, "read git README")
	assert.Equal(t, string(binReadme), string(goReadme), "content must match git")

	_, err = os.Stat(filepath.Join(goOut, ".git"))
	assert.True(t, os.IsNotExist(err), "go-git export must have no .git")
}

// TestGitBackend_ScmIdentifier — glr-wp4: an scm-/dev- version with no tag
// pins the source to a HEAD commit identifier (YYYYMMDD.HHMMSS.<shorthash>).
func TestGitBackend_ScmIdentifier(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	h := b.commit("README", "hi", "init")

	var id string
	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{
		Version:       "scm-1",
		IdentifierOut: &id,
	})
	require.NoError(t, err, "Fetch")

	// The identifier reflects the commit's own author timestamp (Unix(0) in the
	// commit's timezone, as upstream's %ai does) plus the 7-char short hash.
	want := time.Unix(0, 0).Format("20060102.150405.") + h.String()[:7]
	assert.Equal(t, want, id, "scm identifier")
}

// TestGitBackend_NoIdentifierForReleaseVersion — a normal release version does
// not get an identifier.
func TestGitBackend_NoIdentifierForReleaseVersion(t *testing.T) {
	t.Parallel()

	b := newRepoBuilder(t)
	b.commit("README", "hi", "init")

	var id string
	dst := t.TempDir()
	_, err := fetch.FetchWith(context.Background(), b.url(), dst, fetch.Options{
		Version:       "1.0-1",
		IdentifierOut: &id,
	})
	require.NoError(t, err, "Fetch")
	assert.Empty(t, id, "release version must not get a git identifier")
}

// networkErr reports whether err is a transport failure rather than a fetch
// defect, so the one network-dependent test below can skip on an offline
// machine without also skipping a real regression (a moved tag, a changed
// layout) — those surface as non-transport errors and still fail.
func networkErr(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}

	for _, s := range []string{"dial tcp", "no such host", "connection refused",
		"i/o timeout", "TLS handshake", "network is unreachable"} {
		if strings.Contains(err.Error(), s) {
			return true
		}
	}

	return false
}

// TestGitBackend_ChecksIsSourceRoot is the live case the source-root flag was
// introduced for: tarantool/checks has no source.dir in its rockspec and ships
// a checks/ directory next to CMakeLists.txt, so an archive-style descent from
// the clone lands in checks/checks — a directory CMake rejects for having no
// CMakeLists.txt. Unlike its neighbours here this test needs the network; it
// skips when GitHub is unreachable and fails on anything else.
func TestGitBackend_ChecksIsSourceRoot(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dst := t.TempDir()

	res, err := fetch.Sources(ctx, "git+https://github.com/tarantool/checks.git", dst,
		fetch.Options{Tag: "3.3.0"})
	if err != nil && networkErr(err) {
		t.Skipf("network unavailable: %v", err)
	}

	require.NoError(t, err, "clone tarantool/checks")

	assert.True(t, res.SourceRoot, "a clone is the source root")
	assert.Equal(t, filepath.Join(dst, "checks"), res.Path, "clone directory")
	assert.FileExists(t, filepath.Join(res.Path, "CMakeLists.txt"),
		"the build root must be the directory holding CMakeLists.txt")

	// The trap itself: the repository really does contain a subdirectory named
	// like itself, which is what the archive heuristic used to descend into.
	fi, statErr := os.Stat(filepath.Join(res.Path, "checks"))
	require.NoError(t, statErr, "checks/checks must exist for this regression to be meaningful")
	assert.True(t, fi.IsDir(), "checks/checks is the module directory")
}
