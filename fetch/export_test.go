package fetch

import "context"

// This file bridges unexported identifiers to the external `fetch_test`
// package so white-box tests can exercise them through an exported surface
// while the test files themselves stay in `_test` packages (testpackage).

// SchemeOf exposes schemeOf for external tests.
func SchemeOf(rawURL string) (string, error) { return schemeOf(rawURL) }

// StripGitPlus exposes stripGitPlus for external tests.
func StripGitPlus(rawURL string) string { return stripGitPlus(rawURL) }

// ShallowScheme exposes shallowScheme for external tests.
func ShallowScheme(scheme string) bool { return shallowScheme(scheme) }

// RepoNameFromURL exposes repoNameFromURL for external tests.
func RepoNameFromURL(rawURL string) string { return repoNameFromURL(rawURL) }

// RefOpt exposes refOpt for external tests.
func RefOpt(opts Options) string { return refOpt(opts) }

// RefName exposes refName for external tests as its string form (e.g.
// "refs/tags/v1"), avoiding a plumbing import in the test package.
func RefName(opts Options) string { return refName(opts).String() }

// SSHUser exposes sshUser for external tests.
func SSHUser(cloneURL string) string { return sshUser(cloneURL) }

// RewriteGitHubGitURL exposes rewriteGitHubGitURL for external tests.
func RewriteGitHubGitURL(rawURL string) string { return rewriteGitHubGitURL(rawURL) }

// StripSCPScheme exposes stripSCPScheme for external tests.
func StripSCPScheme(cloneURL string) string { return stripSCPScheme(cloneURL) }

// FetchFile runs the file:// backend directly for external tests.
func FetchFile(ctx context.Context, rawURL, destDir string, opts Options) (Result, error) {
	return fileBackend{}.Fetch(ctx, rawURL, destDir, opts)
}

// FetchHTTP runs the http(s) backend directly for external tests.
func FetchHTTP(ctx context.Context, rawURL, destDir string, opts Options) (Result, error) {
	return httpBackend{}.Fetch(ctx, rawURL, destDir, opts)
}

// SwapBackends replaces the dispatch table and returns a restore func.
func SwapBackends(m map[string]Backend) func() {
	saved := backends
	backends = m

	return func() { backends = saved }
}
