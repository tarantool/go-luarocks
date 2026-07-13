package remote_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"

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
		log.Fatal(err)
	}
	fmt.Println(len(got), got[0].Name, got[0].Version.Raw)
	// Output: 1 metrics 1.0.0-1
}
