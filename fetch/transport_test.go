package fetch_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/fetch"
)

// countingServer serves body on every path and counts the connections
// clients open to it.
func countingServer(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()

	var conns atomic.Int32

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	return srv, &conns
}

// TestHTTPFetch_ReusesTheConnection pins the shared transport: fetches from
// one host go over one kept-alive connection rather than a new TCP connection
// and TLS handshake each, which is what a transport built per request cost.
func TestHTTPFetch_ReusesTheConnection(t *testing.T) {
	t.Parallel()

	srv, conns := countingServer(t, "return {}\n")

	for range 3 {
		_, err := fetch.FetchWith(context.Background(), srv.URL+"/mod.lua", t.TempDir(), fetch.Options{})
		require.NoError(t, err, "fetch")
	}

	assert.Equal(t, int32(1), conns.Load(), "connections opened for three fetches")
}
