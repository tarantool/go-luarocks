package remote_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/internal/transport"
	"github.com/tarantool/go-luarocks/remote"
)

// manifestServer serves jsonManifest as the server's manifest-5.1, over TLS
// with a certificate no system root signs when tls is set.
func manifestServer(t *testing.T, tls bool) *httptest.Server {
	t.Helper()

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == manifest51Path {
			_, _ = w.Write([]byte(jsonManifest))

			return
		}

		http.NotFound(w, r)
	}))
	if tls {
		srv.StartTLS()
	} else {
		srv.Start()
	}

	t.Cleanup(srv.Close)

	return srv
}

// redirectTo is a TLS server that redirects every request to target.
func redirectTo(t *testing.T, target string) *httptest.Server {
	t.Helper()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// host is the host:port of a server URL.
func host(t *testing.T, raw string) string {
	t.Helper()

	u, err := url.Parse(raw)
	require.NoError(t, err)

	return u.Host
}

// TestHTTPRemoteIndex_InsecureServersPerHop — the index picked its transport
// by the server's host, so an insecure server redirecting to another host
// had that host's certificate go unchecked too, and a redirect from https to
// http was followed. Each hop must be judged by its own host, and the
// downgrade refused.
func TestHTTPRemoteIndex_InsecureServersPerHop(t *testing.T) {
	t.Parallel()

	listedOnly := func(t *testing.T, srv *httptest.Server) *remote.HTTPRemoteIndex {
		t.Helper()

		return &remote.HTTPRemoteIndex{Servers: []string{srv.URL}, InsecureServers: []string{host(t, srv.URL)}}
	}

	direct := manifestServer(t, true)
	got, err := listedOnly(t, direct).Query(context.Background(), "metrics", "")
	require.NoError(t, err, "a listed host is not verified")
	assert.NotEmpty(t, got)

	_, err = listedOnly(t, redirectTo(t, manifestServer(t, true).URL)).Query(context.Background(), "metrics", "")
	require.ErrorContains(t, err, "certificate", "the host redirected to must be verified")

	plain := manifestServer(t, false)
	idx := listedOnly(t, redirectTo(t, plain.URL))
	idx.InsecureServers = append(idx.InsecureServers, host(t, plain.URL))
	_, err = idx.Query(context.Background(), "metrics", "")
	require.ErrorIs(t, err, transport.ErrDowngrade)
}
