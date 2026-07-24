package fetch

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// Options tunes a Fetch invocation. The zero value is the documented
// default: no insecure hosts, no extra User-Agent override, no source
// metadata. Pass via FetchWith.
type Options struct {
	// InsecureServers lists URL hosts for which the http backend skips
	// TLS certificate verification. Escape hatch for rocks.tarantool.org.
	InsecureServers []string

	// UserAgent overrides the default User-Agent header sent by
	// the http backend.
	UserAgent string

	// Tag is the value of `source.tag` from the rockspec, passed to the
	// git backend as the tag ref `refs/tags/<tag>`. Empty means default branch.
	Tag string

	// Branch is the value of `source.branch` from the rockspec, passed
	// to the git backend as the branch ref `refs/heads/<branch>`. Set Tag or Branch but
	// not both; if both are set Tag wins.
	Branch string

	// File is the value of `source.file` from the rockspec. When set, the http
	// and file backends save the download under this name (driving archive-type
	// detection) instead of deriving it from the URL path. Empty means derive
	// from the URL (upstream: source.file or dir.base_name(url)).
	File string

	// MD5 is the value of `source.md5` from the rockspec. When set, the http
	// and file backends verify the fetched archive's md5 before unpacking and
	// abort on mismatch (upstream fetch.get_sources → fs.check_md5). Empty
	// means no verification.
	MD5 string

	// Version is the rockspec version. When it is an scm-/dev- version with no
	// source.tag, the git backend computes a commit identifier from HEAD.
	Version string

	// IdentifierOut, when non-nil, receives the git commit identifier the git
	// backend computes for an scm-/dev- version (out-param; avoids widening the
	// Backend result for this optional metadata).
	IdentifierOut *string
}

// Backend is the per-scheme fetch implementation. The dispatch table
// registers one Backend per scheme group (http*, git*, file).
type Backend interface {
	Fetch(ctx context.Context, rawURL, destDir string, opts Options) (string, error)
}

// Fetch retrieves rawURL into destDir using the default options and
// returns the on-disk path to the unpacked working tree.
//
// Equivalent to FetchWith(ctx, rawURL, destDir, Options{}).
func Fetch(ctx context.Context, rawURL, destDir string) (string, error) {
	return FetchWith(ctx, rawURL, destDir, Options{})
}

// FetchWith is the options-bearing form of Fetch.
func FetchWith(ctx context.Context, rawURL, destDir string, opts Options) (string, error) {
	if rawURL == "" {
		return "", errors.New("fetch: empty URL")
	}

	rawURL = rewriteGitHubGitURL(rawURL)

	// A schemeless URL is a local file path: upstream dir.split_url defaults
	// any string lacking "://" to the file protocol with the whole string as
	// the pathname (core/dir.lua:41-51), so "mymod-1.0.tar.gz", "/tmp/foo",
	// "./rel" fetch via the file backend rather than being rejected.
	if !strings.Contains(rawURL, "://") {
		return fileBackend{}.Fetch(ctx, rawURL, destDir, opts)
	}

	scheme, err := schemeOf(rawURL)
	if err != nil {
		return "", err
	}

	b, err := backendFor(scheme)
	if err != nil {
		return "", err
	}

	return b.Fetch(ctx, rawURL, destDir, opts)
}

// rewriteGitHubGitURL rewrites the unauthenticated git:// protocol on GitHub
// hosts to git+https:// — GitHub disabled the git-daemon protocol in 2021, so
// a git://github.com/... clone always fails. Mirrors upstream fetch_sources
// (fetch.lua:505-511), which rewrites ^git://github.com/ and
// ^git://www.github.com/ before dispatching.
func rewriteGitHubGitURL(rawURL string) string {
	if strings.HasPrefix(rawURL, "git://github.com/") ||
		strings.HasPrefix(rawURL, "git://www.github.com/") {
		return "git+https://" + strings.TrimPrefix(rawURL, "git://")
	}

	return rawURL
}

// schemeOf returns the lowercase URL scheme (the part before `://`). For
// `git+ssh://...` it returns `git+ssh`. Empty rawURL is an error.
func schemeOf(rawURL string) (string, error) {
	if rawURL == "" {
		return "", errors.New("fetch: empty URL")
	}
	// net/url.Parse rejects `git+ssh://...` in some Go versions; do a
	// manual split to avoid that and to preserve case-insensitive match.
	if i := strings.Index(rawURL, "://"); i > 0 {
		return strings.ToLower(rawURL[:i]), nil
	}
	// Fall back to net/url for SCP-like forms (we don't claim to support
	// those, but the error message stays consistent).
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("fetch: parse %q: %w", rawURL, err)
	}

	if u.Scheme == "" {
		return "", fmt.Errorf("fetch: URL %q has no scheme", rawURL)
	}

	return strings.ToLower(u.Scheme), nil
}

// backendFor returns the registered Backend for the given scheme, or
// ErrUnsupportedRockspecFeature wrapped with the scheme.
//
//nolint:ireturn // dispatch factory: returns the Backend interface selected for the scheme
func backendFor(scheme string) (Backend, error) {
	if b, ok := backends[scheme]; ok {
		return b, nil
	}

	return nil, fmt.Errorf("fetch: scheme %q: %w", scheme, rocks.ErrUnsupportedRockspecFeature)
}

// backends is the scheme → Backend dispatch table, fixed at compile time.
// Tests may override entries by saving and restoring the original.
var backends = map[string]Backend{
	"http":      httpBackend{},
	"https":     httpBackend{},
	"git":       gitBackend{},
	"git+file":  gitBackend{},
	"git+http":  gitBackend{},
	"git+https": gitBackend{},
	"git+ssh":   gitBackend{},
	"file":      fileBackend{},
}
