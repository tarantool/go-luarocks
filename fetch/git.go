package fetch

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// scpStyleRe matches an scp-style ssh URL — "ssh://" then a host authority
// (no slash) then a colon followed by a NON-digit (a path, not a port). This
// is upstream git_ssh's ^ssh://[^/]+:[^%d] test (fetch/git_ssh.lua:22-27).
var scpStyleRe = regexp.MustCompile(`^ssh://[^/]+:\D`)

// scmDevRe matches the in-development version forms (scm-N / dev-N) upstream
// pins to a commit identifier (fetch/git.lua:60).
var scmDevRe = regexp.MustCompile(`^(scm|dev)-\d+$`)

// gitShortHashLen is the abbreviated-hash length upstream uses (%h ≈ 7).
const gitShortHashLen = 7

// gitIdentifier formats the HEAD commit as "YYYYMMDD.HHMMSS.<shorthash>" from
// the commit's author timestamp, mirroring upstream's
// `git log --pretty=format:%ai_%h` reduction (fetch/git.lua:60-77).
func gitIdentifier(repo *git.Repository) (string, error) {
	head, err := repo.Head()
	if err != nil {
		return "", err
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", err
	}

	when := commit.Author.When
	short := head.Hash().String()[:gitShortHashLen]

	return when.Format("20060102.150405.") + short, nil
}

// gitBackend clones a git repository into destDir using the go-git library
// (no `git` binary required). The dispatcher routes every `git*` scheme here.
//
// Shallow vs full clone follows upstream luarocks:
//
//	git, git+file        → Depth 1 (local / direct git protocol)
//	git+http, git+https,
//	git+ssh              → full clone (some servers reject shallow)
//
// The `git+` prefix is stripped before the URL is handed to go-git; bare
// `git` keeps the git:// protocol. Options.Tag / Options.Branch selects the
// reference to check out (a tag ref or a branch ref respectively).
type gitBackend struct{}

func (gitBackend) Fetch(ctx context.Context, rawURL, destDir string, opts Options) (string, error) {
	scheme, err := schemeOf(rawURL)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(destDir, dirPerm); err != nil {
		return "", fmt.Errorf("fetch.git: mkdir %q: %w", destDir, err)
	}

	cloneURL := stripGitPlus(rawURL)

	// scp-style ssh URLs ("ssh://git@host:group/repo.git") must drop the
	// ssh:// prefix so the ":group" segment is read as a path, not a port
	// (upstream git_ssh.lua:22-27; go-git otherwise treats "group" as a
	// non-numeric port and errors).
	if scheme == "git+ssh" {
		cloneURL = stripSCPScheme(cloneURL)
	}

	repoDir := filepath.Join(destDir, repoNameFromURL(cloneURL))

	co := &git.CloneOptions{
		URL: cloneURL,
		// Upstream hard-codes --recursive so submodules are always populated
		// (fetch/git.lua:115); without this, submodule dirs are empty stubs.
		RecurseSubmodules: git.DefaultSubmoduleRecursionDepth,
	}

	if shallowScheme(scheme) {
		co.Depth = 1
	}

	if refOpt(opts) != "" {
		// The caller distinguishes Tag from Branch, so we resolve the
		// fully-qualified ref instead of relying on git's `--branch`
		// auto-detection. SingleBranch keeps the clone to that one ref,
		// matching `git clone --branch <ref> --single-branch`.
		co.ReferenceName = refName(opts)
		co.SingleBranch = true
	}

	// go-git does not consult ssh-agent automatically; wire it up for the
	// git+ssh scheme on a best-effort basis (anonymous clone otherwise).
	if scheme == "git+ssh" {
		if auth, authErr := sshAuth(cloneURL); authErr == nil {
			co.Auth = auth
		}
	}

	// PlainCloneContext honors ctx for network/transport cancellation
	// and creates repoDir itself; it mutates no process state.
	repo, err := git.PlainCloneContext(ctx, repoDir, false, co)
	if err != nil {
		return "", fmt.Errorf("fetch.git: clone %q: %w", cloneURL, err)
	}

	// For an scm-/dev- version with no explicit tag, pin the source to the HEAD
	// commit (upstream fetch/git.lua:60-77). Compute it before the .git strip.
	if opts.IdentifierOut != nil && opts.Tag == "" && scmDevRe.MatchString(opts.Version) {
		if id, ierr := gitIdentifier(repo); ierr == nil {
			*opts.IdentifierOut = id
		}
	}

	// Return a clean export: upstream git.get_sources deletes the VCS
	// metadata after checkout (fetch/git.lua:158-159) so .git/ and .gitignore
	// never leak into the deployed rock or its rock_manifest.
	if err := os.RemoveAll(filepath.Join(repoDir, ".git")); err != nil {
		return "", fmt.Errorf("fetch.git: strip .git: %w", err)
	}

	if err := os.Remove(filepath.Join(repoDir, ".gitignore")); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("fetch.git: strip .gitignore: %w", err)
	}

	return repoDir, nil
}

// stripSCPScheme converts an scp-style ssh URL ("ssh://git@host:path") to the
// native scp form git understands ("git@host:path"). A URL whose colon is
// followed by a digit is a real "ssh://host:port/path" and is left untouched.
// Mirrors upstream git_ssh's ^ssh://[^/]+:[^%d] rewrite.
func stripSCPScheme(cloneURL string) string {
	if scpStyleRe.MatchString(cloneURL) {
		return strings.TrimPrefix(cloneURL, "ssh://")
	}

	return cloneURL
}

// refName resolves the go-git reference to check out from the rockspec
// options: a tag ref when Tag is set, otherwise a branch ref. Callers must
// check refOpt first — when both are empty this returns the branch form of
// the empty string, which is not a valid clone target.
func refName(opts Options) plumbing.ReferenceName {
	if opts.Tag != "" {
		return plumbing.NewTagReferenceName(opts.Tag)
	}

	return plumbing.NewBranchReferenceName(opts.Branch)
}

// sshAuth builds an ssh-agent auth method for a git+ssh clone. It fails if no
// agent is reachable, in which case the caller falls back to an
// unauthenticated clone.
func sshAuth(cloneURL string) (*ssh.PublicKeysCallback, error) {
	return ssh.NewSSHAgentAuth(sshUser(cloneURL))
}

// sshUser extracts the remote user from a git+ssh URL, defaulting to "git"
// (the conventional account for git hosting) when the URL carries no userinfo.
func sshUser(cloneURL string) string {
	if u, err := url.Parse(cloneURL); err == nil && u.User != nil && u.User.Username() != "" {
		return u.User.Username()
	}

	return "git"
}

// stripGitPlus removes a leading `git+` from the URL scheme so the value
// is acceptable as a git remote.
func stripGitPlus(rawURL string) string {
	if strings.HasPrefix(rawURL, "git+") {
		return rawURL[len("git+"):]
	}

	return rawURL
}

// shallowScheme reports whether `--depth=1` is safe for this scheme. We
// follow upstream's conservatism: only bare `git` and `git+file`.
func shallowScheme(scheme string) bool {
	return scheme == "git" || scheme == "git+file"
}

// refOpt picks the git ref to checkout. Tag wins over Branch when both
// are set (caller bug, but defined behavior).
func refOpt(opts Options) string {
	v := opts.Branch
	if opts.Tag != "" {
		v = opts.Tag
	}

	// Upstream nils out a tag/branch of "master" (fetch/git.lua:119) so no
	// --branch is passed and git clones the remote's default branch — a repo
	// whose default is "main" (with no "master" ref) would otherwise fail.
	if v == "master" {
		return ""
	}

	return v
}

// repoNameFromURL extracts the directory name git would default to (the
// last path segment with a trailing `.git` stripped). Falls back to
// "repo" if extraction fails.
func repoNameFromURL(rawURL string) string {
	// Try net/url first.
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		base := filepath.Base(u.Path)

		base = strings.TrimSuffix(base, ".git")

		if base != "" && base != "/" && base != "." {
			return base
		}
	}
	// SCP-form fallback: user@host:path/to/repo.git
	if i := strings.LastIndex(rawURL, "/"); i >= 0 && i < len(rawURL)-1 {
		base := rawURL[i+1:]

		base = strings.TrimSuffix(base, ".git")

		if base != "" {
			return base
		}
	}

	return "repo"
}
