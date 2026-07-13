package tree_test

import (
	"fmt"
	"log"
	"os"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// ExamplePaths computes the conventional subdirectories of a rocks tree.
// All methods are pure functions of Tree; they touch no disk.
func ExamplePaths() {
	p := tree.Paths{Tree: "/opt/tt/.rocks"}

	fmt.Println(p.RocksDir())
	fmt.Println(p.InstallDir("metrics", "1.0-1"))
	fmt.Println(p.DeployLuaDir())
	fmt.Println(p.DeployLibDir())
	fmt.Println(p.BinDir())
	fmt.Println(p.ConfDir("metrics", "1.0-1"))
	fmt.Println(p.DocDir("metrics", "1.0-1"))
	// Output:
	// /opt/tt/.rocks/share/tarantool/rocks
	// /opt/tt/.rocks/share/tarantool/rocks/metrics/1.0-1
	// /opt/tt/.rocks/share/tarantool
	// /opt/tt/.rocks/lib/tarantool
	// /opt/tt/.rocks/bin
	// /opt/tt/.rocks/share/tarantool/rocks/metrics/1.0-1/conf
	// /opt/tt/.rocks/share/tarantool/rocks/metrics/1.0-1/doc
}

// ExampleMungedPath shows the conflict-resolution filename LuaRocks uses when
// two rock versions would deploy the same target: the "<pkg>_<ver>" token
// (dots and dashes turned to underscores) is joined onto the path remainder.
func ExampleMungedPath() {
	fmt.Println(tree.MungedPath(
		"/opt/tt/.rocks/share/tarantool",
		"/opt/tt/.rocks/share/tarantool/demo.lua",
		"demo", "1.0.0-1",
	))
	// Output: /opt/tt/.rocks/share/tarantool/demo_1_0_0_1-demo.lua
}

// ExampleTree_Which resolves a dotted module to its on-disk file; a fresh tree
// has nothing deployed, so the lookup misses.
func ExampleTree_Which() {
	dir, err := os.MkdirTemp("", "rocks")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	tr, err := tree.Open(rocks.Config{Tree: dir})
	if err != nil {
		log.Fatal(err)
	}

	path, ok := tr.Which("no.such.module")
	fmt.Printf("%q %v\n", path, ok)
	// Output: "" false
}
