package client_test

// Differential tests: every input goes through the native helper and through
// the upstream function it mirrors, run by internal/luaref, and the two must
// agree. `go test` runs the seeds; `go test -run '^$' -fuzz FuzzXxx
// ./client/` explores further.

import (
	"strings"
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

// Accepted divergences. An input that hits one skips the comparison it would
// fail, and only that one; the IDs name the entries of the divergence list.
// All of them come from Go library functions the helpers use where upstream
// has its own, and none arises from a real source URL or rock name.
//
//   - B1: a URL that is empty or only slashes. filepath.Base gives "." or
//     "/"; upstream's dir.base_name gives the URL back.
//   - B2: a URL whose only slashes end it ("foo/"). filepath.Base drops
//     them; dir.base_name, finding no slash before the name, gives the URL
//     back, slashes and all.
//   - B3: a URL with a backslash. dir.base_name splits at it too;
//     filepath.Base on Unix does not.
//   - S1: a rock argument that strings.ToLower changes beyond ASCII: it
//     folds Unicode letters and turns invalid UTF-8 into U+FFFD, where Lua's
//     string.lower maps ASCII letters only.

// baseNameException reports B1, B2 or B3.
func baseNameException(url string) bool {
	trimmed := strings.TrimRight(url, "/")

	return trimmed == "" || // B1
		(trimmed != url && !strings.Contains(trimmed, "/")) || // B2
		strings.Contains(url, `\`) // B3
}

// lowersBeyondASCII reports S1: strings.ToLower changes a byte beyond ASCII.
func lowersBeyondASCII(s string) bool {
	ascii := []byte(s)
	for i, c := range ascii {
		if c >= 'A' && c <= 'Z' {
			ascii[i] = c + 'a' - 'A'
		}
	}

	return strings.ToLower(s) != string(ascii)
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
		if len(url) > maxInput || baseNameException(url) {
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
		if len(arg) > maxInput || lowersBeyondASCII(arg) {
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
