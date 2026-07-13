package build_test

import (
	"context"
	"fmt"
	"slices"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// ExampleDeriveFlags resolves the compile/link toolchain for a build. Only the
// configuration-derived, platform-stable fields are shown here (CC, LIBFLAG,
// and the full CFLAGS slice vary by host/environment).
func ExampleDeriveFlags() {
	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tt",
			IncludeDir: "/opt/tt/include/tarantool",
		},
	}
	flags := build.DeriveFlags(cfg)

	fmt.Println("ext:", flags.Ext)
	fmt.Println("fPIC:", slices.Contains(flags.CFLAGS, "-fPIC"))
	fmt.Println("incdir:", flags.LuaIncDir)
	fmt.Println("libdir:", flags.LuaLibDir)
	fmt.Println("bindir:", flags.LuaBinDir)
	// Output:
	// ext: .so
	// fPIC: true
	// incdir: /opt/tt/include/tarantool
	// libdir: /opt/tt/lib
	// bindir: /opt/tt/bin
}

// ExampleRunBackend_none shows the "none" build type: a no-op that ignores
// srcDir/destDir and returns nil. (Every other type needs a real toolchain.)
func ExampleRunBackend_none() {
	spec := &rocks.Rockspec{Build: rocks.Build{Type: "none"}}
	err := build.RunBackend(context.Background(), spec, "", "", rocks.Config{})
	fmt.Println(err)
	// Output: <nil>
}
