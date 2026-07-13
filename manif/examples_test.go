package manif_test

import (
	"errors"
	"fmt"
	"log"
	"os"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/manif"
)

// ExampleWrite serializes a Go value into upstream LuaRocks "assignments"
// format. Top-level keys are emitted in sorted order.
func ExampleWrite() {
	_ = manif.Write(os.Stdout, map[string]any{
		"greeting": "hello",
		"farewell": "bye",
	})
	// Output:
	// farewell = "bye"
	// greeting = "hello"
}

// ExampleWrite_nested shows nested tables (3-space indent, no trailing comma
// on the last entry of each table).
func ExampleWrite_nested() {
	_ = manif.Write(os.Stdout, map[string]any{
		"outer": map[string]any{
			"inner":   map[string]any{"leaf": "value"},
			"sibling": "s",
		},
	})
	// Output:
	// outer = {
	//    inner = {
	//       leaf = "value"
	//    },
	//    sibling = "s"
	// }
}

// ExampleParse decodes an assignments-mode manifest into native Go values.
// Integer literals decode to int64; strings to string.
func ExampleParse() {
	v, err := manif.Parse([]byte("name = \"metrics\"\ncount = 3\n"))
	if err != nil {
		log.Fatal(err)
	}
	m := v.(map[string]any)
	fmt.Printf("%v %v\n", m["name"], m["count"])
	// Output: metrics 3
}

// ExampleParse_error shows that every error Parse returns wraps the ErrParse
// sentinel; discriminate it with errors.Is.
func ExampleParse_error() {
	_, err := manif.Parse([]byte("x = function() end"))
	fmt.Println("is ErrParse:", errors.Is(err, manif.ErrParse))
	// Output: is ErrParse: true
}

// ExampleFileStore round-trips a tree manifest through disk: WriteTree emits
// <dir>/manifest, ReadTree parses it back. Indexed fields are printed (never
// the temp path, never a raw map) to keep the output deterministic.
func ExampleFileStore() {
	dir, err := os.MkdirTemp("", "rocks")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	store := manif.FileStore{}
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"metrics": {"1.0.0-1": {Arch: "installed"}},
		},
		Modules:      map[string][]string{"metrics": {"metrics/1.0.0-1"}},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	if err := store.WriteTree(dir, in); err != nil { // file lands at <dir>/manifest
		log.Fatal(err)
	}

	out, err := store.ReadTree(dir)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(out.Repository["metrics"]["1.0.0-1"].Arch, out.Modules["metrics"][0])
	// Output: installed metrics/1.0.0-1
}
