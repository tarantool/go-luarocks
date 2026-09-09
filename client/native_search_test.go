package client_test

// Tests for the native backend's Search — the command layer of upstream's
// cmd/search.lua on top of remote.Search. What they pin is the part
// remote.Search does not do: argument handling, the source/binary split, and
// where the server list comes from. A local-directory registry is enough,
// since the transport is the search core's concern and is covered there.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
)

// searchRepoManifest offers alpha as a rockspec, a source rock and a portable
// binary, and beta as a rockspec only — the mix the --source / --binary split
// needs to be observable.
const searchRepoManifest = `commands = {}
modules = {}
repository = {
   alpha = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" } },
      ["2.0-1"] = { { arch = "rockspec" }, { arch = "all" } },
   },
   beta = {
      ["1.5-2"] = { { arch = "rockspec" } },
   },
}
`

const searchOtherManifest = `commands = {}
modules = {}
repository = {
   alpha = { ["3.0-1"] = { { arch = "rockspec" } } },
}
`

// newSearchServer writes a manifest into a fresh directory, which is a rock
// server in the form `luarocks --only-server` takes.
func newSearchServer(t *testing.T, manifest string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "manifest"), []byte(manifest), 0o600), "write manifest")

	return dir
}

// newSearchClient builds a native client whose only configured server is the
// given directory. The tree is irrelevant to a search but New requires one.
func newSearchClient(t *testing.T, servers ...string) *client.Rocks {
	t.Helper()

	r, err := client.New(rocks.Config{
		Tree:       t.TempDir(),
		WorkingDir: t.TempDir(),
		Servers:    servers,
		Tarantool: rocks.TarantoolConfig{
			// A prefix with no interpreter under it: the LuaJIT probe fails,
			// so the VM-provided luajit/luabitop entries are absent — the
			// same outcome upstream has when it cannot run the interpreter.
			Prefix:     filepath.Join(t.TempDir(), "no-such-prefix"),
			IncludeDir: "/opt/test/include",
			Version:    "3.9.0-1",
		},
	})
	require.NoError(t, err, "New")

	return r
}

// searchTriples renders results as "name version arch", which is the part of
// a row that does not depend on where the fixture landed on disk.
func searchTriples(found []client.SearchResult) []string {
	out := make([]string, 0, len(found))
	for _, m := range found {
		out = append(out, m.Name+" "+m.Version+" "+m.Arch)
	}

	return out
}

// TestNativeSearch_SourcesThenBinaries — with neither flag the listing is the
// source block followed by the binary block, each sorted by name ascending
// and version descending. A "binary" is an item whose arch is portable
// ("all") or the host's.
func TestNativeSearch_SourcesThenBinaries(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "alpha", client.SearchOpts{})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
		"alpha 2.0-1 all",
	}, searchTriples(found), "sources block first, then binaries")
}

// TestNativeSearch_SourceAndBinaryFlags — --source keeps only the first
// block, --binary only the second, and setting both keeps neither: upstream
// guards the two blocks with independent `if`s, so each flag suppresses the
// other's half.
func TestNativeSearch_SourceAndBinaryFlags(t *testing.T) {
	t.Parallel()

	repo := newSearchServer(t, searchRepoManifest)
	ctx := context.Background()

	cases := []struct {
		name string
		opts client.SearchOpts
		want []string
	}{
		{
			name: "source only",
			opts: client.SearchOpts{Source: true},
			want: []string{"alpha 2.0-1 rockspec", "alpha 1.0-1 rockspec", "alpha 1.0-1 src"},
		},
		{
			name: "binary only",
			opts: client.SearchOpts{Binary: true},
			want: []string{"alpha 2.0-1 all"},
		},
		{
			name: "both flags suppress both blocks",
			opts: client.SearchOpts{Source: true, Binary: true},
			want: []string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			r := newSearchClient(t, repo)

			found, err := r.Search(ctx, "alpha", c.opts)
			require.NoError(t, err, "Search")
			assert.Equal(t, c.want, searchTriples(found))
		})
	}
}

// TestNativeSearch_All lists everything the server holds regardless of the
// pattern and version handed in, which is what upstream's --all does by
// blanking both before building the query.
func TestNativeSearch_All(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "nosuchrock",
		client.SearchOpts{All: true, Version: "9.9-9"})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 2.0-1 rockspec",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
		"beta 1.5-2 rockspec",
		"lua 5.1-1 installed",
		"tarantool 3.9.0-1 installed",
		"alpha 2.0-1 all",
	}, searchTriples(found), "--all ignores both the pattern and the version")
}

// TestNativeSearch_ExactVersion filters to one version, the `==` constraint
// upstream's `version` positional builds.
func TestNativeSearch_ExactVersion(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "alpha", client.SearchOpts{Version: "1.0-1"})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"alpha 1.0-1 rockspec", "alpha 1.0-1 src"}, searchTriples(found))
}

// TestNativeSearch_SubstringPattern — the pattern is a plain substring of the
// name, not a glob and not a regular expression.
func TestNativeSearch_SubstringPattern(t *testing.T) {
	t.Parallel()

	repo := newSearchServer(t, searchRepoManifest)
	ctx := context.Background()

	found, err := newSearchClient(t, repo).Search(ctx, "et", client.SearchOpts{})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"beta 1.5-2 rockspec"}, searchTriples(found))

	found, err = newSearchClient(t, repo).Search(ctx, "a*a", client.SearchOpts{})
	require.NoError(t, err, "Search")
	assert.Empty(t, found, "a glob character must be matched literally")
}

// TestNativeSearch_PatternIsLowerCased mirrors util.namespaced_name_action,
// which lower-cases the name before the query is built.
func TestNativeSearch_PatternIsLowerCased(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "ALPHA", client.SearchOpts{Binary: true})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{"alpha 2.0-1 all"}, searchTriples(found))
}

// TestNativeSearch_EmptyResultIsNotAnError — a healthy server that holds
// nothing matching is an empty, non-nil slice and a nil error.
func TestNativeSearch_EmptyResultIsNotAnError(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "nosuchrock", client.SearchOpts{})
	require.NoError(t, err, "a search matching nothing is not an error")
	assert.NotNil(t, found, "an empty result is an empty slice, not nil")
	assert.Empty(t, found)
}

// TestNativeSearch_EmptyPatternWithoutAllErrors — upstream refuses a missing
// name positional; the Go API cannot tell an omitted argument from an empty
// string, so an empty pattern is read as the omission rather than as "list
// the entire server", which is what SearchOpts.All asks for explicitly.
func TestNativeSearch_EmptyPatternWithoutAllErrors(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	_, err := r.Search(context.Background(), "", client.SearchOpts{})
	require.Error(t, err, "an empty pattern without All must fail loudly")
	assert.Contains(t, err.Error(), "SearchOpts.All")
}

// TestNativeSearch_NoServersConfiguredErrors — "nothing is configured" must
// not read as "no such rock", the same guard the install path applies.
func TestNativeSearch_NoServersConfiguredErrors(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t)

	_, err := r.Search(context.Background(), "alpha", client.SearchOpts{})
	require.Error(t, err, "a search with no servers configured must fail loudly")
	assert.Contains(t, err.Error(), "no servers configured")
}

// TestNativeSearch_ServersArePrependedNotSubstituted — upstream's --server
// concatenates onto the FRONT of cfg.rocks_servers, so a search reports what
// both hold, with the override's rocks first. This differs from
// InstallOpts.Servers, which overrides the configured list outright.
func TestNativeSearch_ServersArePrependedNotSubstituted(t *testing.T) {
	t.Parallel()

	configured := newSearchServer(t, searchRepoManifest)
	override := newSearchServer(t, searchOtherManifest)

	r := newSearchClient(t, configured)

	found, err := r.Search(context.Background(), "alpha",
		client.SearchOpts{Source: true, Servers: []string{override}})
	require.NoError(t, err, "Search")
	assert.Equal(t, []string{
		"alpha 3.0-1 rockspec",
		"alpha 2.0-1 rockspec",
		"alpha 1.0-1 rockspec",
		"alpha 1.0-1 src",
	}, searchTriples(found), "both servers are searched")

	// The override's rock must be reported against the override's server.
	require.Equal(t, remote.NormalizeServer(override), found[0].Server)
	assert.Equal(t, remote.NormalizeServer(configured), found[1].Server)
}

// TestNativeSearch_ProvidedRocks — the rocks the VM supplies are reported
// under a pseudo server with the "installed" arch, so a user searching for
// one learns it is already there instead of being offered a rock to install.
func TestNativeSearch_ProvidedRocks(t *testing.T) {
	t.Parallel()

	r := newSearchClient(t, newSearchServer(t, searchRepoManifest))

	found, err := r.Search(context.Background(), "tarantool", client.SearchOpts{})
	require.NoError(t, err, "Search")
	require.Len(t, found, 1)
	assert.Equal(t, "tarantool 3.9.0-1 installed", searchTriples(found)[0],
		"Config.Tarantool.Version becomes the provided tarantool rock")
	assert.Equal(t, remote.ProvidedRepo, found[0].Server)
}

// TestNativeSearch_ProvidedTarantoolNeedsARevision — upstream keeps
// everything before the first dash of TT_CLI_TARANTOOL_VERSION, so a version
// with no revision at all yields no entry rather than a fabricated one.
func TestNativeSearch_ProvidedTarantoolNeedsARevision(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{
		Tree:       t.TempDir(),
		WorkingDir: t.TempDir(),
		Servers:    []string{newSearchServer(t, searchRepoManifest)},
		Tarantool: rocks.TarantoolConfig{
			Prefix:  filepath.Join(t.TempDir(), "no-such-prefix"),
			Version: "3.9.0",
		},
	})
	require.NoError(t, err, "New")

	found, err := r.Search(context.Background(), "tarantool", client.SearchOpts{})
	require.NoError(t, err, "Search")
	assert.Empty(t, found, "a version with no revision provides no tarantool rock")
}

// TestNativeSearch_NamespacedPattern — an "owner/rock" pattern selects the
// owner's namespace, whose manifest lives under manifests/<owner>/.
func TestNativeSearch_NamespacedPattern(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	nsDir := filepath.Join(repo, "manifests", "someone")
	require.NoError(t, os.MkdirAll(nsDir, 0o750), "mkdir namespace")
	require.NoError(t,
		os.WriteFile(filepath.Join(nsDir, "manifest"), []byte(searchOtherManifest), 0o600),
		"write namespaced manifest")

	r := newSearchClient(t, repo)

	found, err := r.Search(context.Background(), "Someone/Alpha", client.SearchOpts{})
	require.NoError(t, err, "Search")
	require.Len(t, found, 1)
	assert.Equal(t, "alpha 3.0-1 rockspec", searchTriples(found)[0])
	assert.Equal(t, "someone", found[0].Namespace, "the namespace is lower-cased and reported")
}
