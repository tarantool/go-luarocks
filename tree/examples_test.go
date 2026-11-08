package tree_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

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
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	tr, err := tree.Open(rocks.Config{Tree: dir})
	if err != nil {
		panic(err)
	}

	path, ok := tr.Which("no.such.module")
	fmt.Printf("%q %v\n", path, ok)
	// Output: "" false
}

// ExampleTree_Update deploys two versions of a rock in one transaction, reads
// the tree back to see which holds the module's plain path, and prunes the
// tree to a pinned version.
func ExampleTree_Update() {
	dir, err := os.MkdirTemp("", "rocks")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	src := filepath.Join(dir, "src")
	if err := os.MkdirAll(src, 0o750); err != nil {
		panic(err)
	}

	if err := os.WriteFile(filepath.Join(src, "demo.lua"), []byte("return {}\n"), 0o600); err != nil {
		panic(err)
	}

	tr, err := tree.Open(rocks.Config{Tree: filepath.Join(dir, "tree")})
	if err != nil {
		panic(err)
	}

	ctx := context.Background()

	err = tr.Update(ctx, func(tx *tree.Tx) error {
		for _, version := range []string{"1.0-1", "2.0-1"} {
			spec := &rocks.Rockspec{Package: "demo", Version: version, Build: rocks.Build{
				Type:    "builtin",
				Modules: map[string]rocks.Module{"demo": {Path: "demo.lua"}},
			}}

			err := tx.Deploy(spec, func(t *tree.Tree) (*rocks.RockManifest, error) {
				return t.Deploy(spec, src, src)
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		panic(err)
	}

	v, err := tree.Inspect(ctx, tr.Tree)
	if err != nil {
		panic(err)
	}

	fmt.Println("installed:", v.Rocks())

	for _, r := range v.Rocks() {
		files, err := v.Files(r)
		if err != nil {
			panic(err)
		}

		for _, f := range files {
			rel, _ := filepath.Rel(tr.Tree, f.Path)
			fmt.Println(r, f.Entry, "at", filepath.ToSlash(rel), "plain:", f.Plain)
		}
	}

	err = tr.Update(ctx, func(tx *tree.Tx) error {
		removed, err := tx.Prune(tree.KeepVersions(map[string]string{"demo": "2.0-1"}), tree.PruneOpts{})
		fmt.Println("pruned:", removed)

		return err
	})
	if err != nil {
		panic(err)
	}

	v, err = tree.Inspect(ctx, tr.Tree)
	if err != nil {
		panic(err)
	}

	fmt.Println("installed:", v.Rocks())
	// Output:
	// installed: [demo/2.0-1 demo/1.0-1]
	// demo/2.0-1 demo.lua at share/tarantool/demo.lua plain: true
	// demo/1.0-1 demo.lua at share/tarantool/demo_1_0_1-demo.lua plain: false
	// pruned: [demo/1.0-1]
	// installed: [demo/2.0-1]
}
