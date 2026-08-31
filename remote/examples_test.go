package remote_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	"github.com/tarantool/go-luarocks/remote"
)

// ExampleHTTPRemoteIndex_Query queries an in-process rock server's manifest
// for every known version of a rock.
func ExampleHTTPRemoteIndex_Query() {
	const manifest = `{"commands":{},"modules":{},` +
		`"repository":{"metrics":{"1.0.0-1":[{"arch":"rockspec"}]}}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest-5.1.json" {
			_, _ = w.Write([]byte(manifest))

			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	idx := &remote.HTTPRemoteIndex{Servers: []string{srv.URL}}

	got, err := idx.Query(context.Background(), "metrics", "")
	if err != nil {
		panic(err)
	}

	fmt.Println(len(got), got[0].Name, got[0].Version.Raw)
	// Output: 1 metrics 1.0.0-1
}

// ExampleFileRemoteIndex_Query resolves a rock from a local directory — the
// offline case upstream serves with `luarocks --only-server=/path/to/repo`.
func ExampleFileRemoteIndex_Query() {
	const manifest = `repository = { metrics = { ["1.0.0-1"] = { { arch = "rockspec" } } } }`

	dir, err := os.MkdirTemp("", "rockrepo")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	if err := os.WriteFile(filepath.Join(dir, "manifest"), []byte(manifest), 0o600); err != nil {
		panic(err)
	}

	idx := &remote.FileRemoteIndex{Server: dir}

	got, err := idx.Query(context.Background(), "metrics", "")
	if err != nil {
		panic(err)
	}

	fmt.Println(len(got), got[0].Name, got[0].Version.Raw,
		filepath.Base(got[0].URL))
	// Output: 1 metrics 1.0.0-1 metrics-1.0.0-1.rockspec
}

// ExampleNewIndex picks the index implied by each server's form, so one
// configured server list can mix a local mirror with remote servers.
func ExampleNewIndex() {
	for _, server := range []string{"https://rocks.tarantool.org", "/srv/rocks", "file:///mnt/mirror"} {
		fmt.Printf("%s -> %T\n", server, remote.NewIndex(server, remote.IndexOptions{}))
	}
	// Output:
	// https://rocks.tarantool.org -> *remote.HTTPRemoteIndex
	// /srv/rocks -> *remote.FileRemoteIndex
	// file:///mnt/mirror -> *remote.FileRemoteIndex
}
