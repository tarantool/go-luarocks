package remote_test

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/remote"
)

const manifest51Path = "/manifest-5.1.json"

// luaManifest is a minimal Lua-source manifest containing one rock
// "metrics" with version 1.0.0-1 listed as the rockspec arch.
const luaManifest = `commands = {}
modules = {}
repository = {
   metrics = {
      ["1.0.0-1"] = {
         {
            arch = "rockspec",
         },
      },
   },
}
`

const jsonManifest = `{
  "commands": {},
  "modules": {},
  "repository": {
    "metrics": {
      "1.0.0-1": [
        { "arch": "rockspec" }
      ]
    }
  }
}`

func TestHTTPRemoteIndex_JSONFirst(t *testing.T) {
	t.Parallel()

	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++

		switch r.URL.Path {
		case manifest51Path:
			_, _ = w.Write([]byte(jsonManifest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	rocks, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query")
	require.Len(t, rocks, 1)
	assert.Equal(t, "1.0.0-1", rocks[0].Version.Raw, "Version")
	assert.NotZero(t, hits[manifest51Path], "did not probe manifest-5.1.json (hits = %v)", hits)
}

func TestHTTPRemoteIndex_FallbackToLua51(t *testing.T) {
	t.Parallel()

	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++

		switch r.URL.Path {
		case manifest51Path:
			http.NotFound(w, r)
		case "/manifest-5.1":
			_, _ = w.Write([]byte(luaManifest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	rocks, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query")
	require.Len(t, rocks, 1)

	// Upstream order: the .zip versioned manifest is probed first (404 here),
	// then the plain unsuffixed manifest-5.1 is used. The .json variant (last)
	// is never reached because manifest-5.1 already succeeds.
	if hits["/manifest-5.1.zip"] == 0 || hits["/manifest-5.1"] == 0 {
		t.Errorf("expected .zip then unsuffixed manifest to be tried; hits = %v", hits)
	}

	assert.Zero(t, hits[manifest51Path], "the .json variant must not be reached once manifest-5.1 succeeds")

	want := strings.TrimRight(srv.URL, "/") + "/metrics-1.0.0-1.rockspec"
	assert.Equal(t, want, rocks[0].URL, "URL")
}

func TestHTTPRemoteIndex_UnzipsVersionedManifest(t *testing.T) {
	t.Parallel()

	// glr-qz7: a server exposing only the zipped versioned manifest
	// (manifest-5.1.zip) must be unzipped and parsed, not skipped.
	var zbuf bytes.Buffer

	zw := zip.NewWriter(&zbuf)
	w, err := zw.Create("manifest-5.1")
	require.NoError(t, err)
	_, err = w.Write([]byte(luaManifest))
	require.NoError(t, err)
	require.NoError(t, zw.Close())

	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.URL.Path == "/manifest-5.1.zip" {
			_, _ = w.Write(zbuf.Bytes())

			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	rocks, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query")
	require.Len(t, rocks, 1)
	assert.Equal(t, "1.0.0-1", rocks[0].Version.Raw)
	assert.NotZero(t, hits["/manifest-5.1.zip"], "the .zip manifest must be probed")
}

func TestHTTPRemoteIndex_NamespacedQuery(t *testing.T) {
	t.Parallel()

	// glr-m03: a namespaced query fetches /manifests/<ns>/manifest-5.1 and the
	// base server /manifest-5.1 must NOT be consulted.
	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++
		if r.URL.Path == "/manifests/myuser/manifest-5.1" {
			_, _ = w.Write([]byte(luaManifest)) // provides metrics 1.0.0-1

			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	got, err := idx.Query(context.Background(), "metrics", "myuser")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "1.0.0-1", got[0].Version.Raw)
	assert.NotZero(t, hits["/manifests/myuser/manifest-5.1"], "namespaced manifest must be fetched")
	assert.Zero(t, hits["/manifest-5.1"], "base manifest must NOT be consulted for a namespaced query")
}

func TestHTTPRemoteIndex_MergesArchesAcrossServers(t *testing.T) {
	t.Parallel()

	// glr-azs: foo 1.0-1 is offered as {rockspec} on server1 and {src} on
	// server2. The merged pick must choose the src item (server2), not the
	// first server's rockspec.
	body := func(arch string) string {
		return `{"commands":{},"modules":{},"repository":{"foo":{"1.0-1":[{"arch":"` + arch + `"}]}}}`
	}
	server1 := serveJSON(t, body("rockspec"))
	server2 := serveJSON(t, body("src"))

	idx := &remote.HTTPRemoteIndex{Servers: []string{server1, server2}, Arch: "linux-x86_64"}
	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	require.Len(t, got, 1, "one merged version row")
	assert.True(t, strings.HasSuffix(got[0].URL, "/foo-1.0-1.src.rock"),
		"cross-server merge must pick the src item from server2; got %q", got[0].URL)
	assert.True(t, strings.HasPrefix(got[0].URL, strings.TrimRight(server2, "/")),
		"URL must point at server2 (the src provider); got %q", got[0].URL)
}

func TestHTTPRemoteIndex_FallbackToBareManifest(t *testing.T) {
	t.Parallel()

	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++

		switch r.URL.Path {
		case "/manifest":
			_, _ = w.Write([]byte(luaManifest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	rocks, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query")
	require.Len(t, rocks, 1)
	assert.NotZero(t, hits["/manifest"], "expected /manifest probe; hits = %v", hits)
}

func TestHTTPRemoteIndex_MultiServerCache(t *testing.T) {
	t.Parallel()

	hits := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			hits++
			_, _ = w.Write([]byte(jsonManifest))

			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	_, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query #1")
	_, err = idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "Query #2")
	assert.Equal(t, 1, hits, "manifest fetched %d times, want 1 (cache miss)", hits)
}

func TestHTTPRemoteIndex_OneServerDownStillResolves(t *testing.T) {
	t.Parallel()

	// glr-i6f: a down/malformed server must not abort the whole query when the
	// rock is available on another configured server.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // deliberately 404 every path
	}))
	defer down.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte(jsonManifest))

			return
		}

		http.NotFound(w, r)
	}))
	defer good.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{down.URL, good.URL}}
	got, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "a rock on the second server must still resolve")
	assert.NotEmpty(t, got, "expected metrics from the healthy server")
}

func TestHTTPRemoteIndex_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}
	_, err := idx.Query(context.Background(), "metrics", "")
	require.Error(t, err, "expected error when all manifest variants 404")
}

func TestHTTPRemoteIndex_NoServers(t *testing.T) {
	t.Parallel()

	idx := &remote.HTTPRemoteIndex{}
	_, err := idx.Query(context.Background(), "metrics", "")
	require.Error(t, err, "expected error when no servers configured")
}

// serveJSON stands up a test server returning body at the 5.1 JSON manifest.
func serveJSON(t *testing.T, body string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte(body))

			return
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

func TestHTTPRemoteIndex_ExcludesForeignArchOnly(t *testing.T) {
	t.Parallel()

	// glr-mme: on a linux-x86_64 host, a version listed only under macosx-x86_64
	// must be filtered out (results:satisfies fails), not installed.
	const body = `{"commands":{},"modules":{},"repository":{
	  "foo": { "1.0-1": [ { "arch": "macosx-x86_64" } ] }
	}}`

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, body)}, Arch: "linux-x86_64"}

	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	assert.Empty(t, got, "foreign-arch-only version must be excluded")
}

func TestHTTPRemoteIndex_PrefersHostBinaryOverSrcAndRockspec(t *testing.T) {
	t.Parallel()

	// glr-ecu: a ready-to-install host binary wins over src and rockspec.
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "binary beats src",
			body: `{"commands":{},"modules":{},"repository":{"foo":{"1.0-1":[{"arch":"src"},{"arch":"linux-x86_64"}]}}}`,
			want: "/foo-1.0-1.linux-x86_64.rock",
		},
		{
			name: "binary beats rockspec",
			body: `{"commands":{},"modules":{},"repository":{"foo":{"1.0-1":[{"arch":"rockspec"},{"arch":"linux-x86_64"}]}}}`,
			want: "/foo-1.0-1.linux-x86_64.rock",
		},
		{
			name: "src beats rockspec",
			body: `{"commands":{},"modules":{},"repository":{"foo":{"1.0-1":[{"arch":"rockspec"},{"arch":"src"}]}}}`,
			want: "/foo-1.0-1.src.rock",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, c.body)}, Arch: "linux-x86_64"}

			got, err := idx.Query(context.Background(), "foo", "")
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.True(t, strings.HasSuffix(got[0].URL, c.want), "URL %q should end with %q", got[0].URL, c.want)
		})
	}
}
