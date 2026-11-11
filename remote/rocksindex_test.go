package remote_test

// Tests for reading a tt-rocks-index.json: which servers are read from one,
// how its urls resolve, which entries it refuses, and that Query and Search
// carry what it says.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/remote"
)

// indexDir is the branch directory the fake GitHub serves the index from.
const indexDir = "org/rocks-repo/index"

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))

	return hex.EncodeToString(sum[:])
}

func marshalIndex(t *testing.T, idx any) []byte {
	t.Helper()

	b, err := json.Marshal(idx)
	require.NoError(t, err)

	return b
}

// helloVersion is an index version of hello 1.0-1 with its rockspec beside
// the index (a relative url) and a pure-Lua rock on a release page (an
// absolute one).
func helloVersion(gh *testenv.GitHub) remote.IndexVersion {
	rock := gh.Release("org/rocks-repo", "hello-1.0-1", "hello-1.0-1.all.rock", []byte("rock"))
	gh.Put(indexDir+"/hello-1.0-1.rockspec", []byte("spec"))

	return remote.IndexVersion{
		Rockspec:  &remote.IndexFile{URL: "hello-1.0-1.rockspec", SHA256: digestOf("spec")},
		Artifacts: []remote.IndexArtifact{{Arch: "all", URL: rock, SHA256: digestOf("rock"), Size: 4}},
	}
}

// githubIndex serves idx as the index of indexDir and returns the server URL
// of that directory.
func githubIndex(t *testing.T, gh *testenv.GitHub, idx any) string {
	t.Helper()

	gh.Put(indexDir+"/"+remote.RocksIndexFile, marshalIndex(t, idx))

	return gh.URL() + "/" + indexDir
}

func requestedManifest(gh *testenv.GitHub) bool {
	for _, r := range gh.Requests() {
		if strings.Contains(r, "/manifest") {
			return true
		}
	}

	return false
}

// TestRocksIndex_PreferredOverManifest pins that a server with an index is
// read from it alone: its rows carry the index's urls and digests, and no
// manifest is probed even though the server has one.
func TestRocksIndex_PreferredOverManifest(t *testing.T) {
	t.Parallel()

	gh := testenv.NewGitHub(t)
	v := helloVersion(gh)
	server := githubIndex(t, gh, remote.RocksIndex{
		Format:     1,
		Repository: map[string]map[string]remote.IndexVersion{"hello": {"1.0-1": v}},
	})
	gh.Put(indexDir+manifestLuaVersionedPath, []byte(`repository = { hello = { ["9.0-1"] = { { arch = "rockspec" } } } }`))

	idx := &remote.HTTPRemoteIndex{Servers: []string{server}, InsecureServers: gh.InsecureServers()}
	got, err := idx.Query(context.Background(), "hello", "")
	require.NoError(t, err)
	require.Len(t, got, 1, "the manifest's 9.0-1 must not be listed")

	spec := server + "/hello-1.0-1.rockspec"
	assert.Equal(t, rocks.VersionedRock{
		Name:       "hello",
		Version:    got[0].Version,
		URL:        v.Artifacts[0].URL,
		Arch:       "all",
		SHA256:     digestOf("rock"),
		SpecURL:    spec,
		SpecSHA256: digestOf("spec"),
		Artifacts: []rocks.Artifact{
			{Arch: "all", URL: v.Artifacts[0].URL, SHA256: digestOf("rock"), Size: 4},
			{Arch: "rockspec", URL: spec, SHA256: digestOf("spec")},
		},
	}, got[0])
	assert.Equal(t, "1.0-1", got[0].Version.Raw)
	assert.False(t, requestedManifest(gh), "a server with an index has no manifest probed: %v", gh.Requests())
}

// TestRocksIndex_FallsBackToManifest pins that a server without an index —
// one answering 404, 403 or 410 for it — is read from its manifests as
// before.
func TestRocksIndex_FallsBackToManifest(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusGone} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Path {
			case "/" + remote.RocksIndexFile:
				w.WriteHeader(status)
			case manifestLuaVersionedPath:
				_, _ = w.Write([]byte(luaManifest))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)

		got, err := (&remote.HTTPRemoteIndex{Servers: []string{srv.URL}}).Query(context.Background(), "metrics", "")
		require.NoError(t, err, "an index answered with %d is absent", status)
		require.Len(t, got, 1)
		assert.Equal(t, srv.URL+"/metrics-1.0.0-1.rockspec", got[0].URL)
		assert.Empty(t, got[0].SHA256, "a manifest publishes no digest")
		assert.Nil(t, got[0].Artifacts)
	}
}

// TestRocksIndex_BrokenIndexIsTheServersError pins that an index that is
// there but does not read fails the server instead of falling back to the
// manifests, which would drop the digests without a word.
func TestRocksIndex_BrokenIndexIsTheServersError(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		body   string
		status int
		want   string
	}{
		"not JSON":      {body: "<html>", want: "decode rock index"},
		"no format":     {body: `{"repository": {}}`, want: "has no format"},
		"string format": {body: `{"format": "1", "repository": {}}`, want: "not a positive integer"},
		"no repository": {body: `{"format": 1}`, want: "has no repository"},
		"wrong shape":   {body: `{"format": 1, "repository": {"hello": []}}`, want: "decode rock index"},
		"newer format":  {body: `{"format": 2, "repository": "whatever"}`, want: "upgrade tt"},
		"server error":  {status: http.StatusInternalServerError, want: "status 500"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var manifestHits atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/" + remote.RocksIndexFile:
					if tc.status != 0 {
						w.WriteHeader(tc.status)
					}

					_, _ = w.Write([]byte(tc.body))
				case manifestLuaVersionedPath:
					manifestHits.Add(1)

					_, _ = w.Write([]byte(luaManifest))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)

			_, err := (&remote.HTTPRemoteIndex{Servers: []string{srv.URL}}).Query(context.Background(), "metrics", "")
			require.ErrorContains(t, err, tc.want)
			assert.Zero(t, manifestHits.Load(), "the manifest must not be read past a broken index")

			if name == "newer format" {
				require.ErrorIs(t, err, rocks.ErrUnsupportedIndexFormat)
			}
		})
	}
}

// TestRocksIndex_UnknownFieldsIgnored pins the format's room to grow: a
// field it does not define, at any level, is skipped.
func TestRocksIndex_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()

	idx, err := remote.ReadRocksIndex(strings.NewReader(`{
  "format": 1, "generated": "2026-11-11",
  "repository": {"hello": {"1.0-1": {"signed": true,
    "rockspec": {"url": "hello-1.0-1.rockspec", "sha256": "` + digestOf("x") + `", "mirror": "x"}}}}
}`))
	require.NoError(t, err)
	assert.Equal(t, "hello-1.0-1.rockspec", idx.Repository["hello"]["1.0-1"].Rockspec.URL)
}

// TestRocksIndex_RefusedRock pins what an entry against the rules does: the
// rock is refused — asking for it is an error naming the entry — while the
// other rocks of the server still resolve.
func TestRocksIndex_RefusedRock(t *testing.T) {
	t.Parallel()

	good := remote.IndexVersion{
		Artifacts: []remote.IndexArtifact{{Arch: "all", URL: "bad-1.0-1.all.rock", SHA256: digestOf("x")}},
	}

	for name, tc := range map[string]struct {
		version string
		edit    func(v *remote.IndexVersion)
		want    string
	}{
		"wrong file name": {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "bad-1.0-1.linux-x86_64.rock" }, want: `want "bad-1.0-1.all.rock"`},
		"wrong name in a path": {edit: func(v *remote.IndexVersion) {
			v.Artifacts[0].URL = "https://example.org/bad-1.0-1.all.rock/evil.rock"
		}, want: `names the file "evil.rock"`},
		"plain http":     {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "http://rocks.example/bad-1.0-1.all.rock" }, want: "plain http"},
		"other scheme":   {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "ftp://rocks.example/bad-1.0-1.all.rock" }, want: `scheme "ftp"`},
		"parent segment": {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "../bad-1.0-1.all.rock" }, want: "not a path below"},
		"rooted path":    {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "/bad-1.0-1.all.rock" }, want: "not a path below"},
		"other host":     {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "//rocks.example/bad-1.0-1.all.rock" }, want: "not a plain path"},
		"query":          {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "bad-1.0-1.all.rock?x=1" }, want: "not a plain path"},
		"escape":         {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "bad-1.0-1%2Eall.rock" }, want: "not a plain path"},
		"no url":         {edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "" }, want: "no url"},
		"short sha256":   {edit: func(v *remote.IndexVersion) { v.Artifacts[0].SHA256 = "abc" }, want: "not 64 hex digits"},
		"no sha256":      {edit: func(v *remote.IndexVersion) { v.Artifacts[0].SHA256 = "" }, want: "not 64 hex digits"},
		"negative size":  {edit: func(v *remote.IndexVersion) { v.Artifacts[0].Size = -1 }, want: "negative"},
		"no arch":        {edit: func(v *remote.IndexVersion) { v.Artifacts[0].Arch = "" }, want: "no arch"},
		"arch rockspec":  {edit: func(v *remote.IndexVersion) { v.Artifacts[0].Arch = "rockspec" }, want: "not one of a packaged rock"},
		"arch twice":     {edit: func(v *remote.IndexVersion) { v.Artifacts = append(v.Artifacts, v.Artifacts[0]) }, want: "listed twice"},
		"no files":       {edit: func(v *remote.IndexVersion) { v.Artifacts = nil }, want: "lists no file"},
		"bad rockspec": {edit: func(v *remote.IndexVersion) {
			v.Rockspec = &remote.IndexFile{URL: "bad.rockspec", SHA256: digestOf("x")}
		}, want: "rockspec: url"},
		"no revision":     {version: "1.0", edit: func(v *remote.IndexVersion) { v.Artifacts[0].URL = "bad-1.0.all.rock" }, want: "has no revision"},
		"unparsable":      {version: "1.0-x", edit: func(*remote.IndexVersion) {}, want: `version "1.0-x"`},
		"slash in arch":   {edit: func(v *remote.IndexVersion) { v.Artifacts[0].Arch = "linux/x86_64" }, want: "single path element"},
		"bad version key": {version: "..", edit: func(*remote.IndexVersion) {}, want: "single path element"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			version := tc.version
			if version == "" {
				version = "1.0-1"
			}

			bad := good
			bad.Artifacts = append([]remote.IndexArtifact(nil), good.Artifacts...)
			tc.edit(&bad)

			dir := t.TempDir()
			writeIndex(t, dir, remote.RocksIndex{Format: 1, Repository: map[string]map[string]remote.IndexVersion{
				"bad":  {version: bad},
				"good": {"1.0-1": {Rockspec: &remote.IndexFile{URL: "good-1.0-1.rockspec", SHA256: digestOf("x")}}},
			}})

			idx := &remote.FileRemoteIndex{Server: dir}
			_, err := idx.Query(context.Background(), "bad", "")
			require.ErrorContains(t, err, `refusing rock "bad"`)
			require.ErrorContains(t, err, tc.want)

			got, err := idx.Query(context.Background(), "good", "")
			require.NoError(t, err, "a refused rock leaves the others of its server alone")
			require.Len(t, got, 1)
		})
	}
}

// TestRocksIndex_HTTPForInsecureHosts pins that an absolute http url is
// accepted for a host among the insecure servers only, and that indexes with
// different insecure servers do not share what one of them accepted.
func TestRocksIndex_HTTPForInsecureHosts(t *testing.T) {
	t.Parallel()

	mirror := "http://mirror.test:8080/hello-1.0-1.all.rock"
	dir := t.TempDir()
	writeIndex(t, dir, remote.RocksIndex{Format: 1, Repository: map[string]map[string]remote.IndexVersion{
		"hello": {"1.0-1": {Artifacts: []remote.IndexArtifact{{Arch: "all", URL: mirror, SHA256: digestOf("x")}}}},
	}})

	cache := &remote.ManifestCache{}

	_, err := (&remote.FileRemoteIndex{Server: dir, Cache: cache, InsecureServers: []string{"mirror.test"}}).
		Query(context.Background(), "hello", "")
	require.ErrorContains(t, err, "plain http", "the host is listed without the port the url names")

	got, err := remote.NewIndex(dir, remote.IndexOptions{Cache: cache, InsecureServers: []string{"MIRROR.test:8080"}}).
		Query(context.Background(), "hello", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, mirror, got[0].URL)
}

// TestRocksIndex_FileServer pins a local directory's index: a relative url is
// a path under the directory, keeping the form the server was configured in,
// an absolute one stays a URL, and a directory without one reads its
// manifest.
func TestRocksIndex_FileServer(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeIndex(t, dir, remote.RocksIndex{Format: 1, Repository: map[string]map[string]remote.IndexVersion{
		"hello": {"1.0-1": {
			Rockspec: &remote.IndexFile{URL: "https://example.org/r/hello-1.0-1.rockspec", SHA256: digestOf("spec")},
			Artifacts: []remote.IndexArtifact{
				{Arch: "all", URL: "blobs/hello-1.0-1.all.rock", SHA256: strings.ToUpper(digestOf("rock"))},
			},
		}},
	}})

	for _, server := range []string{dir, "file://" + dir} {
		got, err := remote.NewIndex(server, remote.IndexOptions{}).Query(context.Background(), "hello", "")
		require.NoError(t, err)
		require.Len(t, got, 1)

		prefix := strings.TrimSuffix(server, dir)
		assert.Equal(t, prefix+filepath.Join(dir, "blobs", "hello-1.0-1.all.rock"), got[0].URL, "server %s", server)
		assert.Equal(t, digestOf("rock"), got[0].SHA256, "a digest is lowercased")
		assert.Equal(t, "https://example.org/r/hello-1.0-1.rockspec", got[0].SpecURL)
	}

	plain := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(plain, "manifest"), []byte(luaManifest), 0o600))

	got, err := remote.NewIndex(plain, remote.IndexOptions{}).Query(context.Background(), "metrics", "")
	require.NoError(t, err, "a directory without an index reads its manifest")
	require.Len(t, got, 1)

	broken := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(broken, remote.RocksIndexFile), []byte("{"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(broken, "manifest"), []byte(luaManifest), 0o600))

	_, err = remote.NewIndex(broken, remote.IndexOptions{}).Query(context.Background(), "metrics", "")
	require.ErrorContains(t, err, "decode rock index", "a broken index is the directory's error")
}

// TestRocksIndex_YankedAndRockspecOnly pins that a yanked version is still
// listed, marked, and that a version with only a rockspec resolves to it, as
// a manifest's bare rockspec does — and so does one whose rocks are all for
// other platforms.
func TestRocksIndex_YankedAndRockspecOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	foreign := "foreign-os-cpu"
	writeIndex(t, dir, remote.RocksIndex{Format: 1, Repository: map[string]map[string]remote.IndexVersion{
		"hello": {
			"1.0-1": {Yanked: true, Rockspec: &remote.IndexFile{URL: "hello-1.0-1.rockspec", SHA256: digestOf("a")}},
			"2.0-1": {
				Rockspec:  &remote.IndexFile{URL: "hello-2.0-1.rockspec", SHA256: digestOf("b")},
				Artifacts: []remote.IndexArtifact{{Arch: foreign, URL: "hello-2.0-1." + foreign + ".rock", SHA256: digestOf("c")}},
			},
		},
	}})

	got, err := (&remote.FileRemoteIndex{Server: dir}).Query(context.Background(), "hello", "")
	require.NoError(t, err)
	require.Len(t, got, 2)

	assert.True(t, got[0].Yanked, "1.0-1 is yanked")
	assert.Equal(t, filepath.Join(dir, "hello-1.0-1.rockspec"), got[0].URL)
	assert.Equal(t, "rockspec", got[0].Arch)

	assert.False(t, got[1].Yanked)
	assert.Equal(t, "rockspec", got[1].Arch, "another platform's rock is not this host's")
	assert.Equal(t, digestOf("b"), got[1].SHA256)
	require.Len(t, got[1].Artifacts, 2, "the lock still learns every platform's digest")
	assert.Equal(t, foreign, got[1].Artifacts[0].Arch)
}

// TestRocksIndex_Search pins the second path to an artifact: a search lists
// the index's rows with their urls, digests and yanked marks, and a refused
// rock it matches fails the server only when nothing else was found.
func TestRocksIndex_Search(t *testing.T) {
	t.Parallel()

	gh := testenv.NewGitHub(t)
	v := helloVersion(gh)
	yanked := v
	yanked.Yanked = true
	yanked.Artifacts = nil
	yanked.Rockspec = &remote.IndexFile{URL: "hello-0.9-1.rockspec", SHA256: digestOf("old")}
	server := githubIndex(t, gh, remote.RocksIndex{Format: 1, Repository: map[string]map[string]remote.IndexVersion{
		"hello":  {"1.0-1": v, "0.9-1": yanked},
		"broken": {"1.0-1": {Rockspec: &remote.IndexFile{URL: "wrong.rockspec", SHA256: digestOf("x")}}},
	}})
	opts := remote.IndexOptions{InsecureServers: gh.InsecureServers()}

	got, err := remote.Search(context.Background(), []string{server}, opts, remote.Query{Name: "hello"})
	require.NoError(t, err)
	require.Len(t, got, 3)

	assert.Equal(t, remote.Match{
		Name: "hello", Version: "1.0-1", Arch: "all", Server: server,
		URL: v.Artifacts[0].URL, SHA256: digestOf("rock"),
	}, got[0])
	assert.Equal(t, server+"/hello-1.0-1.rockspec", got[1].URL)
	assert.Equal(t, "rockspec", got[1].Arch)
	assert.Equal(t, "0.9-1", got[2].Version)
	assert.True(t, got[2].Yanked, "a search lists a yanked version, marked")

	_, err = remote.Search(context.Background(), []string{server}, opts, remote.Query{Name: "broken"})
	require.ErrorContains(t, err, `refusing rock "broken"`)

	got, err = remote.Search(context.Background(), []string{server}, opts, remote.Query{Substring: true})
	require.NoError(t, err, "a refused rock does not hide what else matched")
	assert.Len(t, got, 3)
}

func writeIndex(t *testing.T, dir string, idx remote.RocksIndex) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(dir, remote.RocksIndexFile), marshalIndex(t, idx), 0o600))
}
