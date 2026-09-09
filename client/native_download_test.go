package client_test

// Tests for the native backend's Download — the command layer of upstream's
// cmd/download.lua. What they pin is which artifact a query resolves to
// (pick_latest_version's binary > src > rockspec preference and the flags
// that override it), where it lands, and that the bytes arrive untouched.
//
// The fixture registry holds the ACTUAL files its manifest advertises, each
// with its own contents, so a case can tell alpha-2.0-1.all.rock from
// alpha-2.0-1.src.rock by more than its name.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// downloadRepoManifest offers alpha twice — once as a rockspec-and-source
// pair, once with a portable binary alongside them — and beta as a rockspec
// only. Both halves are needed: the version pick has to be observable
// separately from the arch pick.
const downloadRepoManifest = `commands = {}
modules = {}
repository = {
   alpha = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" } },
      ["2.0-1"] = { { arch = "rockspec" }, { arch = "src" }, { arch = "all" } },
   },
   beta = {
      ["1.5-2"] = { { arch = "rockspec" } },
   },
}
`

// downloadOtherManifest is a second server holding a NEWER alpha, used to
// tell "the override was searched too" from "the override replaced the
// configured list".
const downloadOtherManifest = `commands = {}
modules = {}
repository = {
   alpha = { ["3.0-1"] = { { arch = "rockspec" } } },
}
`

// downloadRepoFiles are the artifacts downloadRepoManifest advertises, named
// by path.make_url's rules.
var downloadRepoFiles = []string{
	"alpha-1.0-1.rockspec",
	"alpha-1.0-1.src.rock",
	"alpha-2.0-1.rockspec",
	"alpha-2.0-1.src.rock",
	"alpha-2.0-1.all.rock",
	"beta-1.5-2.rockspec",
}

// artifactBody gives every fixture file distinct contents, so a test that
// checks the bytes is checking WHICH file arrived and not merely that
// something did.
func artifactBody(name string) []byte { return []byte("body of " + name + "\n") }

// newDownloadServer materializes a manifest plus its artifacts as a
// directory, which is a rock server in the form `luarocks --only-server`
// takes and the only form that keeps this test offline.
func newDownloadServer(t *testing.T, manifest string, files ...string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "manifest"), []byte(manifest), 0o600), "write manifest")

	for _, f := range files {
		require.NoError(t,
			os.WriteFile(filepath.Join(dir, f), artifactBody(f), 0o600), "write artifact %s", f)
	}

	return dir
}

// newDownloadClient builds a native client downloading into work from the
// given servers. The tree is irrelevant to a download but New requires one.
func newDownloadClient(t *testing.T, work string, servers ...string) *client.Rocks {
	t.Helper()

	r, err := client.New(rocks.Config{
		Tree:       t.TempDir(),
		WorkingDir: work,
		Servers:    servers,
		Tarantool: rocks.TarantoolConfig{
			// A prefix with no interpreter under it: the LuaJIT probe fails,
			// so the VM provides only `lua` and `tarantool` — enough for the
			// provided-rock case and no subprocess for the rest.
			Prefix:     filepath.Join(t.TempDir(), "no-such-prefix"),
			IncludeDir: "/opt/test/include",
			Version:    "3.9.0-1",
		},
	})
	require.NoError(t, err, "New")

	return r
}

// requireArtifact asserts that got is the named fixture file, inside work,
// with the fixture's own bytes — the last of which is also what proves a
// `.rock` was not expanded on the way in.
func requireArtifact(t *testing.T, work, got, want string) {
	t.Helper()

	assert.Equal(t, filepath.Join(work, want), got, "downloaded path")

	body, err := os.ReadFile(got)
	require.NoError(t, err, "read the downloaded file")
	assert.Equal(t, artifactBody(want), body, "the bytes of %s", want)
}

// TestNativeDownload_Picks is pick_latest_version and the three flags that
// narrow the arch set ahead of it, over one local registry.
func TestNativeDownload_Picks(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)

	cases := []struct {
		name string
		rock string
		opts client.DownloadOpts
		want string
	}{
		// The highest version is 2.0-1, and among its three offerings the
		// portable binary wins over the source rock and the bare rockspec.
		{name: "default prefers the binary", rock: "alpha", want: "alpha-2.0-1.all.rock"},
		{
			name: "source", rock: "alpha",
			opts: client.DownloadOpts{Source: true}, want: "alpha-2.0-1.src.rock",
		},
		{
			name: "rockspec", rock: "alpha",
			opts: client.DownloadOpts{Rockspec: true}, want: "alpha-2.0-1.rockspec",
		},
		{
			name: "explicit arch", rock: "alpha",
			opts: client.DownloadOpts{Arch: "all"}, want: "alpha-2.0-1.all.rock",
		},
		// 1.0-1 has no binary, so src beats the rockspec there.
		{
			name: "exact version", rock: "alpha",
			opts: client.DownloadOpts{Version: "1.0-1"}, want: "alpha-1.0-1.src.rock",
		},
		{name: "only a rockspec exists", rock: "beta", want: "beta-1.5-2.rockspec"},
		// The name is lower-cased before the query, as it is for search.
		{name: "name is lower-cased", rock: "ALPHA", want: "alpha-2.0-1.all.rock"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			work := t.TempDir()
			opts := c.opts
			opts.Servers = []string{repo}

			got, err := newDownloadClient(t, work).Download(context.Background(), c.rock, opts)
			require.NoError(t, err, "Download %s", c.rock)
			requireArtifact(t, work, got, c.want)

			entries, err := os.ReadDir(work)
			require.NoError(t, err, "read the working directory")
			assert.Len(t, entries, 1, "a single download must leave exactly one file")
		})
	}
}

// TestNativeDownload_LocalServerFileURL — a server written as a file:// URL
// resolves to the same artifact as the bare path, since the URL the index
// builds keeps whichever form the caller configured.
func TestNativeDownload_LocalServerFileURL(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)
	work := t.TempDir()

	got, err := newDownloadClient(t, work, "file://"+repo).
		Download(context.Background(), "alpha", client.DownloadOpts{})
	require.NoError(t, err, "Download")
	requireArtifact(t, work, got, "alpha-2.0-1.all.rock")
}

// TestNativeDownload_HTTPServer exercises the download branch of get_file
// rather than the copy branch: the same fixture served over HTTP.
func TestNativeDownload_HTTPServer(t *testing.T) {
	t.Parallel()

	served := map[string][]byte{"/manifest": []byte(downloadRepoManifest)}
	for _, f := range downloadRepoFiles {
		served["/"+f] = artifactBody(f)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := served[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	work := t.TempDir()

	got, err := newDownloadClient(t, work, srv.URL).
		Download(context.Background(), "alpha", client.DownloadOpts{})
	require.NoError(t, err, "Download over HTTP")
	requireArtifact(t, work, got, "alpha-2.0-1.all.rock")
}

// TestNativeDownload_All downloads every match. Both return-value branches of
// the lua backend's rule are exercised, since matching that rule is the whole
// reason Download does not simply return a list.
func TestNativeDownload_All(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)

	t.Run("several files return the directory", func(t *testing.T) {
		t.Parallel()

		work := t.TempDir()

		got, err := newDownloadClient(t, work).Download(context.Background(), "alpha",
			client.DownloadOpts{All: true, Servers: []string{repo}})
		require.NoError(t, err, "Download --all alpha")
		assert.Equal(t, work, got, "more than one new file: the answer is the directory")

		for _, f := range []string{
			"alpha-1.0-1.rockspec", "alpha-1.0-1.src.rock",
			"alpha-2.0-1.rockspec", "alpha-2.0-1.src.rock", "alpha-2.0-1.all.rock",
		} {
			body, err := os.ReadFile(filepath.Join(work, f))
			require.NoError(t, err, "%s must have been downloaded", f)
			assert.Equal(t, artifactBody(f), body, "the bytes of %s", f)
		}

		// beta matched no query here: --all with a name is still an exact
		// name match upstream, not a widening.
		_, err = os.Stat(filepath.Join(work, "beta-1.5-2.rockspec"))
		require.Error(t, err, "--all with a name must not download other rocks")
	})

	t.Run("one file returns that file", func(t *testing.T) {
		t.Parallel()

		work := t.TempDir()

		got, err := newDownloadClient(t, work).Download(context.Background(), "beta",
			client.DownloadOpts{All: true, Servers: []string{repo}})
		require.NoError(t, err, "Download --all beta")
		requireArtifact(t, work, got, "beta-1.5-2.rockspec")
	})

	t.Run("no name downloads every rock", func(t *testing.T) {
		t.Parallel()

		work := t.TempDir()

		got, err := newDownloadClient(t, work).Download(context.Background(), "",
			client.DownloadOpts{All: true, Servers: []string{repo}})
		require.NoError(t, err, "Download --all")
		assert.Equal(t, work, got)

		entries, err := os.ReadDir(work)
		require.NoError(t, err, "read the working directory")
		assert.Len(t, entries, len(downloadRepoFiles),
			"an empty name under --all is a substring query for everything")
	})
}

// TestNativeDownload_AllReportsFailuresWithoutAbandoningTheRest — a member
// that cannot be fetched must not stop the others, and must still be
// reported. The manifest advertises a source rock whose file is missing.
func TestNativeDownload_AllReportsFailuresWithoutAbandoningTheRest(t *testing.T) {
	t.Parallel()

	// Everything but alpha-1.0-1.src.rock exists on disk.
	present := []string{
		"alpha-1.0-1.rockspec",
		"alpha-2.0-1.rockspec", "alpha-2.0-1.src.rock", "alpha-2.0-1.all.rock",
	}
	repo := newDownloadServer(t, downloadRepoManifest, present...)
	work := t.TempDir()

	_, err := newDownloadClient(t, work).Download(context.Background(), "alpha",
		client.DownloadOpts{All: true, Servers: []string{repo}})
	require.Error(t, err, "a member that cannot be fetched must be reported")
	assert.Contains(t, err.Error(), "alpha-1.0-1.src.rock", "the error must name the member")

	for _, f := range present {
		_, statErr := os.Stat(filepath.Join(work, f))
		require.NoError(t, statErr, "%s must have been downloaded despite the failed sibling", f)
	}
}

// TestNativeDownload_ServersPrependRatherThanReplace pins the --server
// semantics Search documents: the override is searched IN ADDITION to the
// configured servers, ahead of them.
func TestNativeDownload_ServersPrependRatherThanReplace(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)
	other := newDownloadServer(t, downloadOtherManifest, "alpha-3.0-1.rockspec")

	t.Run("the override is searched", func(t *testing.T) {
		t.Parallel()

		work := t.TempDir()

		got, err := newDownloadClient(t, work, repo).Download(context.Background(), "alpha",
			client.DownloadOpts{Servers: []string{other}})
		require.NoError(t, err, "Download")
		requireArtifact(t, work, got, "alpha-3.0-1.rockspec")
	})

	t.Run("the configured servers survive", func(t *testing.T) {
		t.Parallel()

		work := t.TempDir()

		got, err := newDownloadClient(t, work, repo).Download(context.Background(), "beta",
			client.DownloadOpts{Servers: []string{other}})
		require.NoError(t, err, "a rock only the configured server has must still be found")
		requireArtifact(t, work, got, "beta-1.5-2.rockspec")
	})
}

func TestNativeDownload_Errors(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)

	cases := []struct {
		name string
		rock string
		opts client.DownloadOpts
		want string
	}{
		{
			name: "no such version", rock: "alpha",
			opts: client.DownloadOpts{Version: "9.9-9"},
			want: "No results matching query were found",
		},
		{name: "no such rock", rock: "gamma", want: "could not find a result named gamma"},
		{
			name: "source and rockspec together", rock: "alpha",
			opts: client.DownloadOpts{Source: true, Rockspec: true},
			want: "mutually exclusive",
		},
		{
			name: "source and arch together", rock: "alpha",
			opts: client.DownloadOpts{Source: true, Arch: "all"},
			want: "mutually exclusive",
		},
		{name: "empty name without All", rock: "", want: "enter a rock name"},
		// The VM already provides `lua`, so there is nothing to download and
		// upstream says so instead of consulting a server.
		{name: "provided by the VM", rock: "lua", want: "already provided by VM"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			work := t.TempDir()
			opts := c.opts
			opts.Servers = []string{repo}

			_, err := newDownloadClient(t, work).Download(context.Background(), c.rock, opts)
			require.Error(t, err, "Download %q must fail", c.rock)
			assert.Contains(t, err.Error(), c.want)

			entries, err := os.ReadDir(work)
			require.NoError(t, err, "read the working directory")
			assert.Empty(t, entries, "a failed download must leave nothing behind")
		})
	}
}

// TestNativeDownload_NotFoundNamesTheQuery — the error carries the rock as
// upstream's format_rock_name spells it, version included, and the
// check-lua-versions hint download.download appends.
func TestNativeDownload_NotFoundNamesTheQuery(t *testing.T) {
	t.Parallel()

	repo := newDownloadServer(t, downloadRepoManifest, downloadRepoFiles...)

	_, err := newDownloadClient(t, t.TempDir()).Download(context.Background(), "alpha",
		client.DownloadOpts{Version: "9.9-9", Servers: []string{repo}})
	require.Error(t, err, "Download must fail")
	assert.Contains(t, err.Error(), "could not find a result named alpha 9.9-9")
	assert.Contains(t, err.Error(), "--check-lua-versions")
}

// TestNativeDownload_NoServersConfigured — nothing to search is a server
// problem, not a missing rock.
func TestNativeDownload_NoServersConfigured(t *testing.T) {
	t.Parallel()

	_, err := newDownloadClient(t, t.TempDir()).
		Download(context.Background(), "alpha", client.DownloadOpts{})
	require.Error(t, err, "Download without servers must fail")
	assert.Contains(t, err.Error(), "no servers configured")
}
