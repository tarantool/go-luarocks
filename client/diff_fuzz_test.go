package client_test

// Differential tests: every input goes through the native helper and through
// the upstream function it mirrors, run by internal/luaref, and the two must
// agree. `go test` runs the seeds; `go test -run '^$' -fuzz FuzzXxx
// ./client/` explores further.

import (
	"testing"

	lua "github.com/yuin/gopher-lua"

	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/luaref"
)

// maxInput bounds the fuzzed strings: upstream's patterns take quadratic time
// on long ones, and real names and URLs are short.
const maxInput = 256

func newVM(f *testing.F) *luaref.VM {
	f.Helper()

	vm, err := luaref.New()
	if err != nil {
		f.Fatalf("boot reference: %v", err)
	}

	f.Cleanup(vm.Close)

	return vm
}

// FuzzDeduceBaseDir checks the source-directory guess against
// dir.deduce_base_dir, which upstream applies to every source URL.
func FuzzDeduceBaseDir(f *testing.F) {
	for _, s := range []string{
		"https://example.com/foo-1.0.tar.gz", "https://example.com/foo-1.0.tgz",
		"https://example.com/foo-1.0.zip", "https://example.com/foo-1.0.tar.bz2",
		"https://example.com/foo-1.0.tar", "git+https://github.com/x/foo.git",
		"git://github.com/x/foo", "file:///tmp/foo-1.0.tar.gz", "foo.lua",
		"http://localhost:8080/file/a_rock.lua", "foo.tar.tar.gz", "foo.gz.tar",
		"foo.unknown", "foo", "a/b/c", "https://example.com/v1.0.tar.gz",
	} {
		f.Add(s)
	}

	vm := newVM(f)

	f.Fuzz(func(t *testing.T, url string) {
		if len(url) > maxInput {
			return
		}

		got := client.DeduceBaseDir(url)

		res, err := vm.Call("luarocks.dir", "deduce_base_dir", lua.LString(url))
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if want := lua.LVAsString(res[0]); got != want {
			t.Fatalf("deduceBaseDir(%q) = %q, upstream %q", url, got, want)
		}
	})
}

// namespacedSrc runs util.namespaced_name_action the way the command-line
// parser does and returns the name and namespace it stores.
const namespacedSrc = `
local util = require("luarocks.util")
return function(arg)
   local args = {}
   util.namespaced_name_action(args, "name", arg)
   return args.name, args.namespace
end`

// FuzzSplitNamespacedName checks the command-line rock argument split
// against util.namespaced_name_action.
func FuzzSplitNamespacedName(f *testing.F) {
	for _, s := range []string{
		"foo", "Foo", "user/foo", "User/Foo", "foo-1.0-1.rockspec",
		"foo-1.0-1.src.rock", "a/b/c", "/foo", "foo/", "/", "", "FOO.ROCK",
	} {
		f.Add(s)
	}

	vm := newVM(f)

	loader, err := vm.L.LoadString(namespacedSrc)
	if err != nil {
		f.Fatal(err)
	}

	res, err := vm.CallValue(loader)
	if err != nil {
		f.Fatal(err)
	}

	fn := res[0]

	f.Fuzz(func(t *testing.T, arg string) {
		if len(arg) > maxInput {
			return
		}

		name, ns := client.SplitNamespacedName(arg)

		res, err := vm.CallValue(fn, lua.LString(arg))
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		wantName, wantNS := lua.LVAsString(res[0]), ""
		if len(res) > 1 {
			wantNS = lua.LVAsString(res[1])
		}

		if name != wantName || ns != wantNS {
			t.Fatalf("splitNamespacedName(%q) = (%q, %q), upstream (%q, %q)", arg, name, ns, wantName, wantNS)
		}
	})
}
