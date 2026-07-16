package remote_test

import (
	"archive/zip"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/remote"
)

// manifestLuaVersionedPath and manifestLuaVersionedZipPath name the
// versioned Lua-source manifest probes (see HTTPRemoteIndex.load's probe
// order); kept as constants here so this file doesn't duplicate the literal
// strings already used repeatedly in remote_test.go (goconst).
const (
	manifestLuaVersionedPath    = "/manifest-5.1"
	manifestLuaVersionedZipPath = manifestLuaVersionedPath + ".zip"
)

// TestHTTPRemoteIndex_NameNotInManifest — a manifest that parses fine but
// never mentions the queried rock name returns an empty, error-free result
// (the `versions, ok := mf.repository[name]; !ok` branch).
func TestHTTPRemoteIndex_NameNotInManifest(t *testing.T) {
	t.Parallel()

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, jsonManifest)}}

	got, err := idx.Query(context.Background(), "does-not-exist", "")
	require.NoError(t, err)
	assert.Empty(t, got, "unknown rock name must yield an empty, non-error result")
}

// TestHTTPRemoteIndex_UnparsableVersionKeySkipped — a version key that
// fails deps.ParseVersion (e.g. the empty string) is skipped rather than
// aborting the whole query, as long as at least one other version resolves.
func TestHTTPRemoteIndex_UnparsableVersionKeySkipped(t *testing.T) {
	t.Parallel()

	const body = `{"commands":{},"modules":{},"repository":{"foo":{` +
		`"":[{"arch":"src"}],` +
		`"1.0-1":[{"arch":"src"}]` +
		`}}}`

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, body)}}

	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err, "a good version alongside a bad one must still resolve")
	require.Len(t, got, 1)
	assert.Equal(t, "1.0-1", got[0].Version.Raw)
}

// TestHTTPRemoteIndex_OnlyUnparsableVersionKeyErrors — when every version
// key for the rock fails to parse, Query surfaces the parse error instead
// of silently returning an empty slice.
func TestHTTPRemoteIndex_OnlyUnparsableVersionKeyErrors(t *testing.T) {
	t.Parallel()

	const body = `{"commands":{},"modules":{},"repository":{"foo":{"":[{"arch":"src"}]}}}`

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, body)}}

	_, err := idx.Query(context.Background(), "foo", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse version")
}

// TestHTTPRemoteIndex_ZipParseError — a malformed .zip response for the
// versioned manifest is a non-fatal probe failure; the loader falls back to
// the next probe in the list.
func TestHTTPRemoteIndex_ZipParseError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case manifestLuaVersionedZipPath:
			_, _ = w.Write([]byte("this is not a zip file"))
		case manifestLuaVersionedPath:
			_, _ = w.Write([]byte(luaManifest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

	got, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "a broken .zip probe must fall back to the next probe")
	require.Len(t, got, 1)
}

// TestHTTPRemoteIndex_EmptyZipArchive — a well-formed but empty .zip
// response is also a non-fatal probe failure.
func TestHTTPRemoteIndex_EmptyZipArchive(t *testing.T) {
	t.Parallel()

	var zbuf strings.Builder

	zw := zip.NewWriter(&zbuf)
	require.NoError(t, zw.Close(), "close empty zip")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case manifestLuaVersionedZipPath:
			_, _ = w.Write([]byte(zbuf.String()))
		case manifestLuaVersionedPath:
			_, _ = w.Write([]byte(luaManifest))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

	got, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "an empty .zip archive must fall back to the next probe")
	require.Len(t, got, 1)
}

// TestHTTPRemoteIndex_AllProbesFail_JSONDecodeError — every Lua-source probe
// 404s and the final JSON probe returns invalid JSON: load must report the
// decode failure.
func TestHTTPRemoteIndex_AllProbesFail_JSONDecodeError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte("{not valid json"))

			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

	_, err := idx.Query(context.Background(), "metrics", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode JSON manifest")
}

// TestHTTPRemoteIndex_LuaParseError — a Lua-source probe that responds with
// unparsable content is a non-fatal probe failure (the "parse manifest"
// branch); the loader moves on to the next probe. Every probe here is
// unusable, so the query as a whole still errors, but the hit count proves
// the malformed-Lua probe was reached and its parse failure handled.
func TestHTTPRemoteIndex_LuaParseError(t *testing.T) {
	t.Parallel()

	hits := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits[r.URL.Path]++

		switch r.URL.Path {
		case manifestLuaVersionedPath:
			_, _ = w.Write([]byte("this = is not valid lua {{{"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

	_, err := idx.Query(context.Background(), "metrics", "")
	require.Error(t, err, "every probe is unusable, so the overall query must fail")
	assert.NotZero(t, hits[manifestLuaVersionedPath], "the malformed-Lua probe must have been reached")
}

// TestHTTPRemoteIndex_ProjectManifestErrors covers every shape rejected by
// projectManifest / toArchArr, plus the one non-array shape it accepts as a
// deliberate fallback.
func TestHTTPRemoteIndex_ProjectManifestErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name:    "missing repository key",
			body:    `{"commands":{},"modules":{}}`,
			wantErr: "missing `repository` key",
		},
		{
			name:    "repository not a map",
			body:    `{"repository":"nope"}`,
			wantErr: "repository is",
		},
		{
			name:    "rock versions not a map",
			body:    `{"repository":{"foo":"nope"}}`,
			wantErr: "repository.foo is",
		},
		{
			name:    "arch entry not a map",
			body:    `{"repository":{"foo":{"1.0-1":["nope"]}}}`,
			wantErr: "entry 0 is",
		},
		{
			name:    "arch value neither array nor map",
			body:    `{"repository":{"foo":{"1.0-1":"nope"}}}`,
			wantErr: "expected array or map",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, c.body)}}

			_, err := idx.Query(context.Background(), "foo", "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

// TestHTTPRemoteIndex_ArchEntryAsBareMap — the JSON serialization of the
// upstream manifest can flatten a single-element arch array into a bare
// map; toArchArr must accept that shape too.
func TestHTTPRemoteIndex_ArchEntryAsBareMap(t *testing.T) {
	t.Parallel()

	const body = `{"repository":{"foo":{"1.0-1":{"arch":"src"}}}}`

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, body)}}

	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0].URL, "/foo-1.0-1.src.rock"), "URL = %q", got[0].URL)
}

// TestHTTPRemoteIndex_InstalledArchURL — the "installed" pseudo-arch maps to
// the per-rock rockspec path under the tree layout, per makeRockURL.
func TestHTTPRemoteIndex_InstalledArchURL(t *testing.T) {
	t.Parallel()

	const body = `{"repository":{"foo":{"1.0-1":[{"arch":"installed"}]}}}`

	idx := &remote.HTTPRemoteIndex{Servers: []string{serveJSON(t, body)}, Arch: "linux-x86_64"}

	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, strings.HasSuffix(got[0].URL, "/foo/1.0-1/foo-1.0-1.rockspec"), "URL = %q", got[0].URL)
}

// TestHTTPRemoteIndex_GetURLParseError — a server entry that produces an
// unparsable URL (an invalid percent-escape) surfaces the "parse url"
// failure from get, propagated all the way up through Query.
func TestHTTPRemoteIndex_GetURLParseError(t *testing.T) {
	t.Parallel()

	idx := &remote.HTTPRemoteIndex{Servers: []string{"http://%zz"}}

	_, err := idx.Query(context.Background(), "foo", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse url")
}

// TestHTTPRemoteIndex_ConnectionRefused — a server address with nothing
// listening surfaces a transport-level failure from client.Do, and (mirroring
// TestHTTPRemoteIndex_OneServerDownStillResolves) does not abort resolution
// against a healthy peer.
func TestHTTPRemoteIndex_ConnectionRefused(t *testing.T) {
	t.Parallel()

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := closed.URL
	closed.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte(jsonManifest))

			return
		}

		http.NotFound(w, r)
	}))
	defer good.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{deadURL, good.URL}}

	got, err := idx.Query(context.Background(), "metrics", "")
	require.NoError(t, err, "an unreachable peer must not abort resolution against a healthy one")
	assert.NotEmpty(t, got)
}

// TestHTTPRemoteIndex_TLSInsecureSkipVerify — InsecureServers opts a
// matching host out of certificate verification; without a match (or
// without the option at all) a self-signed server's manifest is
// unreachable.
func TestHTTPRemoteIndex_TLSInsecureSkipVerify(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte(jsonManifest))

			return
		}

		http.NotFound(w, r)
	}))
	// t.Cleanup (not defer) so the server stays up for the t.Parallel()
	// subtests below, which the testing package runs only after this
	// function's synchronous body returns.
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	t.Run("matching host skips verification", func(t *testing.T) {
		t.Parallel()

		idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}, InsecureServers: []string{u.Host}}

		got, err := idx.Query(context.Background(), "metrics", "")
		require.NoError(t, err, "InsecureServers must allow the self-signed cert through")
		require.Len(t, got, 1)
	})

	t.Run("no matching host still verifies", func(t *testing.T) {
		t.Parallel()

		idx := &remote.HTTPRemoteIndex{
			Servers:         []string{srv.URL},
			InsecureServers: []string{"unrelated.example.com"},
		}

		_, err := idx.Query(context.Background(), "metrics", "")
		require.Error(t, err, "a non-matching InsecureServers entry must not skip verification")
	})

	t.Run("unset still verifies", func(t *testing.T) {
		t.Parallel()

		idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

		_, err := idx.Query(context.Background(), "metrics", "")
		require.Error(t, err, "certificate verification must apply by default")
	})
}

// TestLuaOSName covers every branch of the unexported luaOSName switch.
func TestLuaOSName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "macosx", remote.LuaOSName("darwin"))
	assert.Equal(t, "linux", remote.LuaOSName("linux"))
	assert.Equal(t, "windows", remote.LuaOSName("windows"), "unmapped GOOS values pass through unchanged")
}

// TestLuaCPUName covers every branch of the unexported luaCPUName switch.
func TestLuaCPUName(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "x86_64", remote.LuaCPUName("amd64"))
	assert.Equal(t, "x86", remote.LuaCPUName("386"))
	assert.Equal(t, "aarch64", remote.LuaCPUName("arm64"))
	assert.Equal(t, "riscv64", remote.LuaCPUName("riscv64"), "unmapped GOARCH values pass through unchanged")
}
