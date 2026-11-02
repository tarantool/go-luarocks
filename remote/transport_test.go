package remote_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/remote"
)

// TestHTTPRemoteIndex_ReusesTheConnection pins the shared transport for
// manifests: indexes that each load the manifest of one server, with no
// cache between them, still reach it over one kept-alive connection.
func TestHTTPRemoteIndex_ReusesTheConnection(t *testing.T) {
	t.Parallel()

	var conns atomic.Int32

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != manifestLuaVersionedPath {
			http.NotFound(w, r)

			return
		}

		_, _ = w.Write([]byte(`repository = { hello = { ["1.0-1"] = { { arch = "rockspec" } } } }`))
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)

	for range 3 {
		idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

		found, err := idx.Query(context.Background(), "hello", "")
		require.NoError(t, err, "Query")
		require.Len(t, found, 1, "hello rows")
	}

	assert.Equal(t, int32(1), conns.Load(), "connections opened for three indexes")
}
