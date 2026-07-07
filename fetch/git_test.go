package fetch_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tarantool/go-luarocks/fetch"
)

func TestStripGitPlus(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"git+https://example.com/r.git": "https://example.com/r.git",
		"git+ssh://git@example.com/r":   "ssh://git@example.com/r",
		"git+file:///tmp/r":             "file:///tmp/r",
		"git://example.com/r.git":       "git://example.com/r.git",
	}
	for in, want := range cases {
		assert.Equal(t, want, fetch.StripGitPlus(in), "stripGitPlus(%q)", in)
	}
}

func TestShallowScheme(t *testing.T) {
	t.Parallel()

	yes := []string{"git", "git+file"}
	no := []string{"git+http", "git+https", "git+ssh"}

	for _, s := range yes {
		assert.True(t, fetch.ShallowScheme(s), "shallowScheme(%q) = false, want true", s)
	}

	for _, s := range no {
		assert.False(t, fetch.ShallowScheme(s), "shallowScheme(%q) = true, want false", s)
	}
}

func TestRepoNameFromURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"https://example.com/foo/bar.git": "bar",
		"git://example.com/baz":           "baz",
		"git@github.com:user/proj.git":    "proj",
		"":                                "repo",
	}
	for in, want := range cases {
		assert.Equal(t, want, fetch.RepoNameFromURL(in), "repoNameFromURL(%q)", in)
	}
}

func TestRefOpt(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "v1", fetch.RefOpt(fetch.Options{Tag: "v1", Branch: "dev"}), "Tag should win")
	assert.Equal(t, "dev", fetch.RefOpt(fetch.Options{Branch: "dev"}), "Branch fallback")
	assert.Empty(t, fetch.RefOpt(fetch.Options{}), "default")
	// glr-srt: a "master" tag/branch is dropped so git clones the remote default.
	assert.Empty(t, fetch.RefOpt(fetch.Options{Branch: "master"}), "master branch dropped")
	assert.Empty(t, fetch.RefOpt(fetch.Options{Tag: "master"}), "master tag dropped")
	assert.Equal(t, "v1", fetch.RefOpt(fetch.Options{Tag: "v1", Branch: "master"}), "Tag wins, master branch irrelevant")
}

// TestRefName pins the go-git ref the backend checks out. Unlike git's
// `--branch`, go-git needs a fully-qualified ref, so a Tag must become a tag
// ref and a Branch a branch ref — and Tag must win when both are set.
func TestRefName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "refs/tags/v1.0", fetch.RefName(fetch.Options{Tag: "v1.0"}), "tag → tag ref")
	assert.Equal(t, "refs/heads/main", fetch.RefName(fetch.Options{Branch: "main"}), "branch → branch ref")
	assert.Equal(t, "refs/tags/v1.0", fetch.RefName(fetch.Options{Tag: "v1.0", Branch: "main"}),
		"Tag should win over Branch")
}

func TestSSHUser(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"ssh://git@example.com/r.git":   "git",
		"ssh://alice@example.com/r.git": "alice",
		"ssh://example.com/r.git":       "git", // no userinfo → default
		"https://example.com/r.git":     "git", // non-ssh, but still defaults
	}
	for in, want := range cases {
		assert.Equal(t, want, fetch.SSHUser(in), "sshUser(%q)", in)
	}
}

func TestRewriteGitHubGitURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// glr-349/glr-lga: git:// on GitHub is rewritten to git+https.
		"git://github.com/user/repo.git":     "git+https://github.com/user/repo.git",
		"git://www.github.com/user/repo.git": "git+https://www.github.com/user/repo.git",
		// Non-GitHub git:// and other schemes are untouched.
		"git://example.com/user/repo.git":      "git://example.com/user/repo.git",
		"git+https://github.com/user/repo.git": "git+https://github.com/user/repo.git",
		"https://github.com/user/repo.git":     "https://github.com/user/repo.git",
	}
	for in, want := range cases {
		assert.Equal(t, want, fetch.RewriteGitHubGitURL(in), "rewriteGitHubGitURL(%q)", in)
	}
}

func TestStripSCPScheme(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		// glr-9e8: scp-style (colon then non-digit) drops the ssh:// prefix.
		"ssh://git@example.com:group/repo.git": "git@example.com:group/repo.git",
		// Real port (colon then digit) stays as an ssh:// URL.
		"ssh://git@example.com:22/repo.git": "ssh://git@example.com:22/repo.git",
		// Path-only ssh URL (no colon in authority) is untouched.
		"ssh://git@example.com/repo.git": "ssh://git@example.com/repo.git",
	}
	for in, want := range cases {
		assert.Equal(t, want, fetch.StripSCPScheme(in), "stripSCPScheme(%q)", in)
	}
}
