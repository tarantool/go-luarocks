package remote_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/remote"
)

func TestLocalServerPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		server   string
		wantPath string
		wantOK   bool
	}{
		{name: "absolute path", server: "/srv/rocks", wantPath: "/srv/rocks", wantOK: true},
		{name: "relative path", server: "./repo", wantPath: "./repo", wantOK: true},
		{name: "bare name", server: "repo", wantPath: "repo", wantOK: true},
		{name: "file URL", server: "file:///srv/rocks", wantPath: "/srv/rocks", wantOK: true},
		{name: "file URL relative", server: "file://repo", wantPath: "repo", wantOK: true},
		{name: "file URL uppercase", server: "FILE:///srv/rocks", wantPath: "/srv/rocks", wantOK: true},
		// A drive-letter path has no "://" and must never be read as a host.
		{name: "windows backslash", server: `C:\rocks\repo`, wantPath: `C:\rocks\repo`, wantOK: true},
		{name: "windows forward slash", server: "C:/rocks/repo", wantPath: "C:/rocks/repo", wantOK: true},
		// A scheme-less string is a path even when it looks like a host: that is
		// what upstream dir.split_url decides, and diverging would make the same
		// config mean different things to tt and to luarocks.
		{name: "scheme-less host-shaped", server: "rocks.example.org/dist",
			wantPath: "rocks.example.org/dist", wantOK: true},
		{name: "https", server: "https://rocks.tarantool.org"},
		{name: "http", server: "http://localhost:8080/dist"},
		{name: "git", server: "git+https://example.org/repo"},
		{name: "empty", server: ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			got, ok := remote.LocalServerPath(c.server)
			assert.Equal(t, c.wantOK, ok, "locality of %q", c.server)
			assert.Equal(t, c.wantPath, got, "path of %q", c.server)
		})
	}
}

// TestNewIndex_SelectsByServerForm pins the dispatch: the caller writes one
// server string and gets whichever index that form implies.
func TestNewIndex_SelectsByServerForm(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		server string
		local  bool
	}{
		{name: "bare path", server: "/srv/rocks", local: true},
		{name: "file URL", server: "file:///srv/rocks", local: true},
		{name: "windows path", server: `C:\rocks`, local: true},
		{name: "https", server: "https://rocks.tarantool.org", local: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := remote.NewIndex(c.server, remote.IndexOptions{LuaVersion: "5.1", Arch: "linux-x86_64"})

			if c.local {
				file, ok := idx.(*remote.FileRemoteIndex)
				require.True(t, ok, "%q must select the file index, got %T", c.server, idx)
				assert.Equal(t, c.server, file.Server)
				assert.Equal(t, "linux-x86_64", file.Arch, "options must reach the file index")

				return
			}

			httpIdx, ok := idx.(*remote.HTTPRemoteIndex)
			require.True(t, ok, "%q must select the HTTP index, got %T", c.server, idx)
			assert.Equal(t, []string{c.server}, httpIdx.Servers)
		})
	}
}

func TestNewIndexes_MixedList(t *testing.T) {
	t.Parallel()

	got := remote.NewIndexes(
		[]string{"/srv/rocks", "https://rocks.tarantool.org", "file:///mnt/mirror"},
		remote.IndexOptions{InsecureServers: []string{"rocks.tarantool.org"}},
	)
	require.Len(t, got, 3, "one index per server")
	assert.IsType(t, &remote.FileRemoteIndex{}, got[0])
	assert.IsType(t, &remote.HTTPRemoteIndex{}, got[1])
	assert.IsType(t, &remote.FileRemoteIndex{}, got[2])
}

// TestOrderedIndex_MixedLocalAndHTTP is the whole point of the feature: a
// local directory and an HTTP server in one list, consulted in configuration
// order, first-found-wins — including the case where the local mirror is
// second and only reached because the HTTP server does not carry the rock.
func TestOrderedIndex_MixedLocalAndHTTP(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)
	// The HTTP server publishes a DIFFERENT version of the same rock, so the
	// result says which member answered.
	srv := serveJSON(t, `{"repository":{"stat":{"9.9.9-1":[{"arch":"all"}]}}}`)

	cases := []struct {
		name    string
		servers []string
		want    []string
	}{
		{name: "local first", servers: []string{dir, srv}, want: []string{"0.3.1-1", "0.3.2-1"}},
		{name: "http first", servers: []string{srv, dir}, want: []string{"9.9.9-1"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := remote.NewOrderedIndex(remote.NewIndexes(c.servers, remote.IndexOptions{})...)

			got, err := idx.Query(context.Background(), statRock, "")
			require.NoError(t, err, "Query")
			assert.Equal(t, c.want, versionsOf(got), "first server that has the rock wins")
		})
	}
}

// TestOrderedIndex_SkipsBrokenMember — an unusable member (here a local
// directory that does not exist) must not hide a rock the next member has.
func TestOrderedIndex_SkipsBrokenMember(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent")
	idx := remote.NewOrderedIndex(
		remote.NewIndexes([]string{missing, newStatRepo(t)}, remote.IndexOptions{})...,
	)

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "a broken first server must not abort the query")
	assert.Equal(t, []string{"0.3.1-1", "0.3.2-1"}, versionsOf(got))
}

// TestOrderedIndex_AllMembersFail — when nothing yielded a rock and at least
// one member errored, the error is surfaced rather than swallowed into an
// empty result the caller would report as "rock not found".
func TestOrderedIndex_AllMembersFail(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent")
	idx := remote.NewOrderedIndex(remote.NewIndexes([]string{missing}, remote.IndexOptions{})...)

	_, err := idx.Query(context.Background(), statRock, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "query \"stat\" across servers")
}

// TestOrderedIndex_NothingFound — every member answered successfully and none
// had the rock: an empty, error-free result, which the caller turns into
// "not found".
func TestOrderedIndex_NothingFound(t *testing.T) {
	t.Parallel()

	idx := remote.NewOrderedIndex(remote.NewIndexes([]string{newStatRepo(t)}, remote.IndexOptions{})...)

	got, err := idx.Query(context.Background(), "no-such-rock", "")
	require.NoError(t, err, "an absent rock is not an error")
	assert.Empty(t, got)
}

// TestOrderedIndex_Empty — an aggregate with no members stands for a client
// with no servers configured, which must stay an error. Answering "not found"
// there is the misclassification that sends the caller after a missing rock
// instead of a missing server, and it is what a facade built from an empty
// Config.Servers would otherwise start reporting.
func TestOrderedIndex_Empty(t *testing.T) {
	t.Parallel()

	_, err := remote.NewOrderedIndex().Query(context.Background(), statRock, "")
	require.Error(t, err, "no members must be a configuration error")
	assert.Contains(t, err.Error(), "no servers configured")
}

// TestOrderedIndex_HTTPInsecureOptionReachesHTTPIndex — the TLS escape hatch
// is HTTP-only, but it must survive the shared IndexOptions.
func TestOrderedIndex_HTTPInsecureOptionReachesHTTPIndex(t *testing.T) {
	t.Parallel()

	idx := remote.NewIndex("https://example.org",
		remote.IndexOptions{InsecureServers: []string{"example.org"}, UserAgent: "ua/1"})

	httpIdx, ok := idx.(*remote.HTTPRemoteIndex)
	require.True(t, ok)
	assert.Equal(t, []string{"example.org"}, httpIdx.InsecureServers)
	assert.Equal(t, "ua/1", httpIdx.UserAgent)
}

// fakeIndex is a RemoteIndex returning canned results, used to drive
// OrderedIndex without standing up a server or a directory.
type fakeIndex struct {
	rocksOut []rocks.VersionedRock
	err      error
}

func (f fakeIndex) Query(
	_ context.Context, _, _ string,
) ([]rocks.VersionedRock, error) {
	return f.rocksOut, f.err
}

// TestOrderedIndex_LastErrorSurfaces — with several failing members the
// surfaced error is the last one, wrapped with the queried name.
func TestOrderedIndex_LastErrorSurfaces(t *testing.T) {
	t.Parallel()

	first := errors.New("first boom")
	last := errors.New("last boom")

	idx := remote.NewOrderedIndex(fakeIndex{err: first}, fakeIndex{err: last})

	_, err := idx.Query(context.Background(), statRock, "")
	require.Error(t, err)
	require.ErrorIs(t, err, last)
	require.NotErrorIs(t, err, first)
}

// TestOrderedIndex_LocalMirrorOfSameManifest — the mirror case end to end: a
// local copy of a server's manifest, first in the list, answers with the same
// versions the server would have, but with on-disk artifact paths.
func TestOrderedIndex_LocalMirrorOfSameManifest(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)
	origin := serveManifestOverHTTP(t, statManifest)

	idx := remote.NewOrderedIndex(remote.NewIndexes([]string{dir, origin}, remote.IndexOptions{})...)

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"0.3.1-1", "0.3.2-1"}, versionsOf(got))
	assert.Equal(t, filepath.Join(dir, rockNewFn), urlOf(t, got, statNew),
		"the mirror answered, so the URL is a local path")
}
