package client_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// ExampleNew mirrors the README quick start: construct the facade against a
// tree and list what is installed (nothing, in a fresh tree).
func ExampleNew() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	r, err := client.New(rocks.Config{
		Tree:       dir,
		WorkingDir: ".",
		Servers:    []string{"http://rocks.tarantool.org/"},
	})
	if err != nil {
		panic(err)
	}

	installed, err := r.List(context.Background())
	if err != nil {
		panic(err)
	}

	fmt.Println("installed rocks:", len(installed))
	// Output: installed rocks: 0
}

// ExampleRocks_Which resolves a module name against the tree; a fresh tree
// has no modules, so the lookup misses.
func ExampleRocks_Which() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: "."})
	if err != nil {
		panic(err)
	}

	path, ok, err := r.Which(context.Background(), "no.such.module")
	if err != nil {
		panic(err)
	}

	fmt.Printf("%q %v\n", path, ok)
	// Output: "" false
}

// ExampleRocks_Search searches a rock server for every rock whose name
// contains the pattern. The server here is a directory on disk, which is what
// makes the example runnable offline; an https:// server is searched exactly
// the same way. Arch tells the offerings apart: "rockspec" must be built,
// "all" can be installed as-is.
func ExampleRocks_Search() {
	repo, err := os.MkdirTemp("", "rocks-repo")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(repo) }()

	manifest := `commands = {}
modules = {}
repository = {
   metrics = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "all" } },
   },
}
`
	if err := os.WriteFile(filepath.Join(repo, "manifest"), []byte(manifest), 0o600); err != nil {
		panic(err)
	}

	r, err := client.New(rocks.Config{Tree: repo, WorkingDir: ".", Servers: []string{repo}})
	if err != nil {
		panic(err)
	}

	found, err := r.Search(context.Background(), "metric", client.SearchOpts{})
	if err != nil {
		panic(err)
	}

	for _, m := range found {
		fmt.Println(m.Name, m.Version, m.Arch)
	}
	// Output:
	// metrics 1.0-1 rockspec
	// metrics 1.0-1 all
}

// ExampleRocks_Purge shows the backend contract: operations the native
// backend does not implement return rocks.ErrNotImplemented, discriminated
// with errors.Is.
func ExampleRocks_Purge() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: "."})
	if err != nil {
		panic(err)
	}

	err = r.Purge(context.Background(), client.PurgeOpts{})
	fmt.Println("not implemented:", errors.Is(err, rocks.ErrNotImplemented))
	// Output: not implemented: true
}

// ExampleWithBackend selects the gopher-lua backend, which boots an embedded
// LuaRocks VM lazily on the first write operation. Construction itself never
// touches the VM, so it succeeds before the tree even exists.
func ExampleWithBackend() {
	r, err := client.New(
		rocks.Config{Tree: "/opt/tt/.rocks", WorkingDir: "."},
		client.WithBackend(client.BackendLua),
	)
	if err != nil {
		panic(err)
	}

	fmt.Println("constructed:", r != nil)
	// Output: constructed: true
}

// ExampleRocks_Install runs the complete native install pipeline — remote
// manifest query, rockspec fetch, source fetch, builtin build, deploy — using
// an in-process rock server and a local source directory, then verifies the
// module resolves in the tree.
func ExampleRocks_Install() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o750); err != nil {
		panic(err)
	}

	if err := os.WriteFile(filepath.Join(srcDir, "demo.lua"), []byte("return {}\n"), 0o600); err != nil {
		panic(err)
	}

	spec := "package = \"demo\"\n" +
		"version = \"1.0-1\"\n" +
		"source = { url = \"file://" + srcDir + "\" }\n" +
		"build = { type = \"builtin\", modules = { demo = \"demo.lua\" } }\n"

	const manifest = `{"commands":{},"modules":{},` +
		`"repository":{"demo":{"1.0-1":[{"arch":"rockspec"}]}}}`

	mux := http.NewServeMux()
	mux.HandleFunc("/manifest-5.1.json", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(manifest))
	})
	mux.HandleFunc("/demo-1.0-1.rockspec", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(spec))
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	r, err := client.New(rocks.Config{
		Tree:       filepath.Join(dir, "tree"),
		WorkingDir: dir,
		Servers:    []string{srv.URL},
	})
	if err != nil {
		panic(err)
	}

	if err := r.Install(context.Background(), "demo", client.InstallOpts{}); err != nil {
		panic(err)
	}

	_, ok, err := r.Which(context.Background(), "demo")
	if err != nil {
		panic(err)
	}

	fmt.Println("demo installed:", ok)
	// Output: demo installed: true
}
