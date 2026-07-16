package rockspec_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/rockspec"
)

// exampleRockspec is a plain literal-table rockspec. The evaluator sandbox
// forbids all Lua stdlib (string.format, require, os, ...), so the fixture uses
// only literal assignments.
const exampleRockspec = `package = "example"
version = "1.0-1"
source = {
   url = "https://example.com/example-1.0.tar.gz",
}
description = {
   summary = "An example rock",
}
build = {
   type = "builtin",
   modules = {
      ["example"] = "src/example.lua",
   },
}
`

// ExampleEval evaluates a rockspec file into a *rocks.Rockspec. Eval takes a
// file path; its second argument is unused, so pass a zero rocks.RockspecConfig.
func ExampleEval() {
	dir, err := os.MkdirTemp("", "rockspec-example")
	if err != nil {
		panic(err)
	}

	defer func() { _ = os.RemoveAll(dir) }()

	path := filepath.Join(dir, "example-1.0-1.rockspec")
	if err := os.WriteFile(path, []byte(exampleRockspec), 0o644); err != nil {
		panic(err)
	}

	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	if err != nil {
		panic(err)
	}

	fmt.Println(spec.Package, spec.Version, spec.Build.Type)
	fmt.Println(spec.Description.Summary)
	// Output:
	// example 1.0-1 builtin
	// An example rock
}

// ExampleValidate checks a rockspec's required fields and feature support. A
// well-formed spec validates to nil; an unsupported build type is rejected with
// the typed rocks.ErrUnsupportedRockspecFeature sentinel.
func ExampleValidate() {
	good := &rocks.Rockspec{
		Package: "example", Version: "1.0-1",
		Source:   rocks.Source{URL: "https://example.com/example-1.0.tar.gz"},
		HasBuild: true, Build: rocks.Build{Type: "builtin"},
	}
	fmt.Println("good:", rockspec.Validate(good))

	bad := &rocks.Rockspec{
		Package: "example", Version: "1.0-1",
		Source:   rocks.Source{URL: "https://example.com/example-1.0.tar.gz"},
		HasBuild: true, Build: rocks.Build{Type: "frobnicate"},
	}
	err := rockspec.Validate(bad)
	fmt.Println("bad is unsupported-feature:", errors.Is(err, rocks.ErrUnsupportedRockspecFeature))
	// Output:
	// good: <nil>
	// bad is unsupported-feature: true
}

// ExampleRuntimePlatforms returns the platform-name slice for the current OS,
// ordered least-specific to most-specific. The first element is always "unix"
// (the rest are GOOS-dependent), so only index [0] is asserted here.
func ExampleRuntimePlatforms() {
	plats := rockspec.RuntimePlatforms()
	fmt.Println(plats[0])
	// Output: unix
}

// ExampleMergePlatforms folds a per-platform build overlay into a spec. Scalar
// fields overwrite the base: the "unix" overlay's build type replaces the
// generic "make" type, and MergePlatforms clears the overlay afterwards.
func ExampleMergePlatforms() {
	spec := &rocks.Rockspec{
		Package: "example", Version: "1.0-1",
		Build: rocks.Build{
			Type: "make",
			Platforms: map[string]rocks.Build{
				"unix": {Type: "builtin"},
			},
		},
	}

	rockspec.MergePlatforms(spec, []string{"unix"})

	fmt.Println(spec.Build.Type)
	fmt.Println(spec.Build.Platforms == nil)
	// Output:
	// builtin
	// true
}
