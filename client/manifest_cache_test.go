package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
)

// TestNative_ServerListFetchesManifestOnce pins the client's manifest cache:
// calls that each name the same server list fetch its manifest once per
// client. tt installs a resolved closure one rock at a time, each Install
// naming the registries, and every call used to download every manifest
// again.
func TestNative_ServerListFetchesManifestOnce(t *testing.T) {
	t.Parallel()

	specs := map[string][]byte{}

	for _, name := range []string{"hello", "dep"} {
		src := t.TempDir()
		specPath := stageRockSource(t, src, name, "1.0-1", "")

		body, err := os.ReadFile(specPath)
		require.NoError(t, err, "read %s rockspec", name)

		specs["/"+filepath.Base(specPath)] = body
	}

	var manifestRequests atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/manifest") {
			manifestRequests.Add(1)
		}

		switch r.URL.Path {
		case "/manifest-5.1":
			_, _ = w.Write([]byte(`repository = {
   hello = { ["1.0-1"] = { { arch = "rockspec" } } },
   dep = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`))
		default:
			if body, ok := specs[r.URL.Path]; ok {
				_, _ = w.Write(body)

				return
			}

			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	r := newNativeClient(t, t.TempDir(), t.TempDir())
	servers := []string{srv.URL}

	require.NoError(t,
		r.Install(context.Background(), "hello", client.InstallOpts{Servers: servers, Deps: client.DepsNone}),
		"Install hello")

	first := manifestRequests.Load()
	require.Positive(t, first, "the first install reads the manifest")

	require.NoError(t,
		r.Install(context.Background(), "dep", client.InstallOpts{Servers: servers, Deps: client.DepsNone}),
		"Install dep")

	_, err := r.Search(context.Background(), "dep", client.SearchOpts{Servers: servers})
	require.NoError(t, err, "Search dep")

	assert.Equal(t, first, manifestRequests.Load(),
		"the second install and the search reuse the manifest the first one fetched")
}
