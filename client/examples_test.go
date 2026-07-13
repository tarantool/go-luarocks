package client_test

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// ExampleNew mirrors the README quick start: construct the facade against a
// tree and list what is installed (nothing, in a fresh tree).
func ExampleNew() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	r, err := client.New(rocks.Config{
		Tree:       dir,
		WorkingDir: ".",
		Servers:    []string{"http://rocks.tarantool.org/"},
	})
	if err != nil {
		log.Fatal(err)
	}

	installed, err := r.List(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("installed rocks:", len(installed))
	// Output: installed rocks: 0
}

// ExampleRocks_Which resolves a module name against the tree; a fresh tree
// has no modules, so the lookup misses.
func ExampleRocks_Which() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: "."})
	if err != nil {
		log.Fatal(err)
	}

	path, ok, err := r.Which(context.Background(), "no.such.module")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%q %v\n", path, ok)
	// Output: "" false
}

// ExampleRocks_Search shows the backend contract: operations the native
// backend does not implement return rocks.ErrNotImplemented, discriminated
// with errors.Is.
func ExampleRocks_Search() {
	dir, err := os.MkdirTemp("", "rocks-tree")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: "."})
	if err != nil {
		log.Fatal(err)
	}

	_, err = r.Search(context.Background(), "metrics", client.SearchOpts{})
	fmt.Println("not implemented:", errors.Is(err, rocks.ErrNotImplemented))
	// Output: not implemented: true
}

// ExampleWithBackend selects the gopher-lua backend, which boots an embedded
// LuaRocks VM lazily on the first write operation.
func ExampleWithBackend() {
	r, err := client.New(
		rocks.Config{Tree: "/opt/tt/.rocks", WorkingDir: "."},
		client.WithBackend(client.BackendLua),
	)
	if err != nil {
		log.Fatal(err)
	}
	_ = r
}

// ExampleRocks_Install installs a rock and its transitive dependencies into
// the tree. (Runs a real fetch/build; shown for documentation.)
func ExampleRocks_Install() {
	r, err := client.New(rocks.Config{
		Tree:       "/opt/tt/.rocks",
		WorkingDir: ".",
		Servers:    []string{"http://rocks.tarantool.org/"},
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tarantool",
			IncludeDir: "/opt/tarantool/include/tarantool",
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := r.Install(context.Background(), "metrics", client.InstallOpts{}); err != nil {
		log.Fatal(err)
	}
}
