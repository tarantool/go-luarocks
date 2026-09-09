package remote_test

// Tests for remote.Search, the port of upstream search.search_repos. What
// they pin is the behavior a `luarocks search` depends on and an install path
// does not: substring matching, a cross-server merge (rather than
// first-found-wins), the exact-version early stop, and the porcelain result
// order.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/remote"
)

// searchManifest lists two rocks with a mix of arches, which is what makes it
// usable for both the arch filter and the source/binary distinction:
// alpha 1.0-1 is offered as a rockspec and a source rock, alpha 2.0-1 as a
// rockspec and a portable binary, and beta 1.5-2 as a rockspec only. The
// foreign binary arch on alpha 1.0-1 must never be accepted on any host.
const searchManifest = `commands = {}
modules = {}
repository = {
   alpha = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" }, { arch = "nintendo64-mips" } },
      ["2.0-1"] = { { arch = "rockspec" }, { arch = "all" } },
   },
   beta = {
      ["1.5-2"] = { { arch = "rockspec" } },
   },
}
`

// otherManifest is a second server's offering of the SAME rock, so a merge
// across servers is observable: it holds a version the first server does not
// (alpha 3.0-1) and repeats one it does (alpha 1.0-1, as a source rock).
const otherManifest = `commands = {}
modules = {}
repository = {
   alpha = {
      ["1.0-1"] = { { arch = "src" } },
      ["3.0-1"] = { { arch = "rockspec" } },
   },
}
`

// newSearchRepo writes a manifest into a fresh directory and returns the
// directory, which is a rock server in the form `luarocks --only-server` takes.
func newSearchRepo(t *testing.T, manifest string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "manifest"), []byte(manifest), 0o600), "write manifest")

	return dir
}

// newSearchHTTPRepo serves a manifest over HTTP under the plain
// `manifest-5.1` name and returns the base URL, plus a counter of how many
// requests it saw — which is what makes an early stop observable.
func newSearchHTTPRepo(t *testing.T, manifest string) (string, *atomic.Int64) {
	t.Helper()

	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)

		if r.URL.Path == "/manifest-5.1" {
			_, _ = w.Write([]byte(manifest))

			return
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL, &hits
}

// triple renders a match as "name version arch", the part of a row that does
// not depend on where the fixture happened to land on disk.
func triple(m remote.Match) string {
	return m.Name + " " + m.Version + " " + m.Arch
}

func triples(matches []remote.Match) []string {
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, triple(m))
	}

	return out
}

// TestSearch_SubstringMatchesEveryRockContainingTheName — the search query is
// a plain substring test, so "lph" finds "alpha" and nothing else. It is
// deliberately not a glob: a pattern character must be matched literally.
func TestSearch_SubstringMatchesEveryRockContainingTheName(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "lph", Substring: true})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 2.0-1 all",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
	}, triples(found), "substring match over the manifest")
}

// TestSearch_ExactNameIgnoresSubstrings — without Substring the name must
// match in full, which is the query shape the install path uses.
func TestSearch_ExactNameIgnoresSubstrings(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)
	ctx := context.Background()

	found, err := remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "lph"})
	require.NoError(t, err, "Search for a substring without Substring")
	assert.Empty(t, found, "an exact-name query must not match a substring")

	found, err = remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "beta"})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"beta 1.5-2 rockspec"}, triples(found))
}

// TestSearch_EmptyNameWithSubstringMatchesEverything is how upstream
// implements --all: the name is blanked and substring matching is left on.
func TestSearch_EmptyNameWithSubstringMatchesEverything(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "", Substring: true})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 2.0-1 all",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
		"beta 1.5-2 rockspec",
	}, triples(found))
}

// TestSearch_VersionConstraintFilters — constraints are AND'd and applied to
// the parsed version, so a range selects versions the string comparison would
// not.
func TestSearch_VersionConstraintFilters(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	cs, err := deps.ParseConstraints(">= 2.0")
	require.NoError(t, err, "ParseConstraints")

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "alpha", Constraints: cs})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"alpha 2.0-1 rockspec", "alpha 2.0-1 all"}, triples(found))
}

// TestSearch_DefaultArchSetAcceptsPseudoArchesAndTheHost — the default query
// arch set is {src, all, rockspec, installed} plus the host arch, so a
// manifest entry built for another machine is dropped while a portable one is
// kept. IndexOptions.Arch stands in for cfg.arch so the assertion does not
// depend on the machine running it.
func TestSearch_DefaultArchSetAcceptsPseudoArchesAndTheHost(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)
	ctx := context.Background()

	found, err := remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "alpha"})
	require.NoError(t, err, "Search")
	assert.NotContains(t, triples(found), "alpha 1.0-1 nintendo64-mips",
		"a foreign binary arch must never be accepted")

	found, err = remote.Search(ctx, []string{repo},
		remote.IndexOptions{Arch: "nintendo64-mips"},
		remote.Query{Name: "alpha"})
	require.NoError(t, err, "Search with the fixture's arch as the host arch")
	assert.Contains(t, triples(found), "alpha 1.0-1 nintendo64-mips",
		"the host arch must be accepted")
}

// TestSearch_ExplicitArchSetIsExactlyThatSet — upstream's queries.new adds
// cfg.arch only to the FALLBACK arch table, never to one built from an arch
// string, so `src|rockspec` accepts those two and nothing else. That is what
// find_src_or_rockspec relies on to exclude binary rocks.
func TestSearch_ExplicitArchSetIsExactlyThatSet(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{repo},
		remote.IndexOptions{Arch: "nintendo64-mips"},
		remote.Query{Name: "alpha", Arch: []string{"src", "rockspec"}})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
	}, triples(found), "neither the portable binary nor the host arch may appear")
}

// TestSearch_MergesAcrossEveryServer — store_result APPENDS, so a rock
// offered by two servers is reported twice and a version only the second
// server has is still found. This is the rule that differs from the install
// path's OrderedIndex, where the first server to answer wins.
func TestSearch_MergesAcrossEveryServer(t *testing.T) {
	t.Parallel()

	first := newSearchRepo(t, searchManifest)
	second := newSearchRepo(t, otherManifest)

	found, err := remote.Search(context.Background(), []string{first, second},
		remote.IndexOptions{}, remote.Query{Name: "alpha"})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 3.0-1 rockspec",
		"alpha 2.0-1 rockspec",
		"alpha 2.0-1 all",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
		"alpha 1.0-1 src",
	}, triples(found), "both servers' offerings, newest version first")

	// The duplicate 1.0-1 src rows must name the two different servers, which
	// is the whole point of reporting both.
	var servers []string

	for _, m := range found {
		if m.Version == "1.0-1" && m.Arch == "src" {
			servers = append(servers, m.Server)
		}
	}

	assert.Equal(t, []string{remote.NormalizeServer(first), remote.NormalizeServer(second)}, servers,
		"servers are reported in configuration order")
}

// TestSearch_StopsAtAnExactVersionMatch — search_repos breaks out of the
// server loop once the rock named exactly as queried is found at the exact
// version asked for, so later servers are never contacted. The HTTP server
// second in the list is the instrument: it must see no request at all.
func TestSearch_StopsAtAnExactVersionMatch(t *testing.T) {
	t.Parallel()

	first := newSearchRepo(t, searchManifest)
	second, hits := newSearchHTTPRepo(t, otherManifest)

	cs, err := deps.ParseConstraints("1.0-1")
	require.NoError(t, err, "ParseConstraints")

	found, err := remote.Search(context.Background(), []string{first, second},
		remote.IndexOptions{}, remote.Query{Name: "alpha", Constraints: cs})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"alpha 1.0-1 rockspec", "alpha 1.0-1 src"}, triples(found))
	assert.Zero(t, hits.Load(), "the second server must not be contacted after an exact match")
}

// TestSearch_WithoutAnExactVersionEveryServerIsWalked is the other half of
// the early stop: a query with no `==` constraint has nothing to stop on, so
// the second server is read.
func TestSearch_WithoutAnExactVersionEveryServerIsWalked(t *testing.T) {
	t.Parallel()

	first := newSearchRepo(t, searchManifest)
	second, hits := newSearchHTTPRepo(t, otherManifest)

	found, err := remote.Search(context.Background(), []string{first, second},
		remote.IndexOptions{}, remote.Query{Name: "alpha"})
	require.NoError(t, err, "Search")
	assert.Contains(t, triples(found), "alpha 3.0-1 rockspec", "the second server's rock")
	assert.NotZero(t, hits.Load(), "the second server must be contacted")
}

// TestSearch_HTTPServer covers the other transport end to end: an HTTP server
// is searched by the same code and its matches carry fetchable URLs.
func TestSearch_HTTPServer(t *testing.T) {
	t.Parallel()

	base, _ := newSearchHTTPRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{base}, remote.IndexOptions{},
		remote.Query{Name: "alpha"})
	require.NoError(t, err, "Search")
	require.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 2.0-1 all",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
	}, triples(found))

	assert.Equal(t, base+"/alpha-2.0-1.rockspec", found[0].URL, "rockspec URL")
	assert.Equal(t, base+"/alpha-2.0-1.all.rock", found[1].URL, "binary rock URL")
	assert.Equal(t, base, found[0].Server, "the server keeps its scheme and loses its trailing slash")
}

// TestSearch_LocalServerURLsAndServerForm — a directory server resolves to
// on-disk paths, and the reported Server is the normalized directory whether
// it was configured bare or as a file:// URL, matching what the porcelain
// listing prints.
func TestSearch_LocalServerURLsAndServerForm(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)
	ctx := context.Background()

	bare, err := remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "beta"})
	require.NoError(t, err, "Search a bare path")
	require.Len(t, bare, 1)
	assert.Equal(t, filepath.Join(repo, "beta-1.5-2.rockspec"), bare[0].URL)
	assert.Equal(t, repo, bare[0].Server)

	urlForm, err := remote.Search(ctx, []string{"file://" + repo}, remote.IndexOptions{},
		remote.Query{Name: "beta"})
	require.NoError(t, err, "Search a file:// URL")
	require.Len(t, urlForm, 1)
	assert.Equal(t, "file://"+filepath.Join(repo, "beta-1.5-2.rockspec"), urlForm[0].URL,
		"the configured form is preserved in the artifact URL")
	assert.Equal(t, repo, urlForm[0].Server,
		"but the reported server drops the scheme, as dir.normalize does")
}

// TestSearch_DeadServerAmongLiveOnesIsSkipped — one unusable server must not
// hide what the others hold, and a search that then finds something is a
// success, not a partial failure.
func TestSearch_DeadServerAmongLiveOnesIsSkipped(t *testing.T) {
	t.Parallel()

	dead := filepath.Join(t.TempDir(), "absent")
	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{dead, repo},
		remote.IndexOptions{}, remote.Query{Name: "beta"})
	require.NoError(t, err, "a dead server ahead of a live one must not fail the search")
	assert.Equal(t, []string{"beta 1.5-2 rockspec"}, triples(found))
}

// TestSearch_AllServersDeadIsAnError — the flip side: when nothing was found
// AND a server failed, the failure is what the caller needs to see. A missing
// server must not be reported as a missing rock.
func TestSearch_AllServersDeadIsAnError(t *testing.T) {
	t.Parallel()

	dead := filepath.Join(t.TempDir(), "absent")

	_, err := remote.Search(context.Background(), []string{dead}, remote.IndexOptions{},
		remote.Query{Name: "beta"})
	require.Error(t, err, "an unusable server must fail loudly")
	assert.Contains(t, err.Error(), dead, "the error must name the server")
}

// TestSearch_NoMatchIsAnEmptyResult — a rock genuinely absent from a healthy
// server is not an error.
func TestSearch_NoMatchIsAnEmptyResult(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "nosuchrock", Substring: true})
	require.NoError(t, err, "Search")
	assert.Empty(t, found)
}

// TestSearch_ResultOrder pins print_result_tree's porcelain order: names
// ascending, versions DESCENDING (util.sortedpairs is handed
// vers.compare_versions, which is a `>` comparison), and manifest item order
// within one version.
func TestSearch_ResultOrder(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Substring: true})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 2.0-1 all",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
		"beta 1.5-2 rockspec",
	}, triples(found), "names ascending, versions newest first, manifest item order within a version")
}

// TestSearch_ProvidedRocksAreAppended — search_repos adds the rocks the VM
// supplies after every server has been walked, under a pseudo server and the
// "installed" arch, so a user searching for one is told it is already there.
func TestSearch_ProvidedRocksAreAppended(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)
	provided := map[string]string{"lua": "5.1-1", "tarantool": "3.9.0-1"}

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "lua", Substring: true, Provided: provided})
	require.NoError(t, err, "Search")
	require.Len(t, found, 1)
	assert.Equal(t, "lua 5.1-1 installed", triple(found[0]))
	assert.Equal(t, remote.ProvidedRepo, found[0].Server)
	assert.Empty(t, found[0].URL, "a VM-provided rock has no artifact to fetch")
}

// TestSearch_ProvidedRocksHonorTheQuery — provided rocks go through the same
// satisfies() call as a manifest row, so an arch set without "installed"
// excludes them, a namespaced query excludes them (they have no namespace),
// and a version constraint applies.
func TestSearch_ProvidedRocksHonorTheQuery(t *testing.T) {
	t.Parallel()

	repo := newSearchRepo(t, searchManifest)
	provided := map[string]string{"lua": "5.1-1"}
	ctx := context.Background()

	found, err := remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "lua", Substring: true, Arch: []string{"src", "rockspec"}, Provided: provided})
	require.NoError(t, err, "Search with an explicit arch set")
	assert.Empty(t, found, `an arch set without "installed" excludes VM-provided rocks`)

	cs, err := deps.ParseConstraints(">= 5.4")
	require.NoError(t, err, "ParseConstraints")

	found, err = remote.Search(ctx, []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "lua", Substring: true, Constraints: cs, Provided: provided})
	require.NoError(t, err, "Search with a constraint")
	assert.Empty(t, found, "a constraint the provided version does not satisfy excludes it")
}

// TestSearch_NamespaceReadsTheNamespacedManifest — a namespaced query reads
// <server>/manifests/<ns>/manifest and stamps the namespace onto every row,
// which is what manifest_search does.
func TestSearch_NamespaceReadsTheNamespacedManifest(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	nsDir := filepath.Join(repo, "manifests", "someone")
	require.NoError(t, os.MkdirAll(nsDir, 0o750), "mkdir namespace")
	require.NoError(t,
		os.WriteFile(filepath.Join(nsDir, "manifest"), []byte(otherManifest), 0o600), "write manifest")

	found, err := remote.Search(context.Background(), []string{repo}, remote.IndexOptions{},
		remote.Query{Name: "alpha", Namespace: "someone", Provided: map[string]string{"lua": "5.1-1"}})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"alpha 3.0-1 rockspec", "alpha 1.0-1 src"}, triples(found))
	assert.Equal(t, "someone", found[0].Namespace, "rows carry the queried namespace")

	for _, m := range found {
		assert.NotEqual(t, remote.ProvidedRepo, m.Server,
			"VM-provided rocks have no namespace, so a namespaced query excludes them")
	}
}

// TestNormalizeServer pins dir.normalize, which is what makes Match.Server
// comparable with the lua backend's porcelain output.
func TestNormalizeServer(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"/srv/rocks", "/srv/rocks"},
		{"/srv/rocks/", "/srv/rocks"},
		{"/srv/rocks//", "/srv/rocks"},
		{"file:///srv/rocks", "/srv/rocks"},
		{"FILE:///srv/rocks/", "/srv/rocks"},
		{"/srv/./rocks", "/srv/rocks"},
		{"/srv/nested/../rocks", "/srv/rocks"},
		{"http://rocks.tarantool.org/", "http://rocks.tarantool.org"},
		{"https://rocks.example.org/dist/", "https://rocks.example.org/dist"},
		{`C:\rocks\repo`, "C:/rocks/repo"},
		{"'/srv/rocks'", "/srv/rocks"},
		{"relative/repo", "relative/repo"},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, remote.NormalizeServer(c.in), "NormalizeServer(%q)", c.in)
	}
}

// TestHostArch reports the LuaRocks cfg.arch spelling for the host, which the
// caller needs to tell a binary result from a source one.
func TestHostArch(t *testing.T) {
	t.Parallel()

	arch := remote.HostArch()
	assert.Contains(t, arch, "-", "cfg.arch is <os>-<cpu>: %q", arch)
	assert.NotContains(t, []string{"src", "all", "rockspec", "installed"}, arch,
		"the host arch must not collide with a pseudo-arch")
}
