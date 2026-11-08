package remote_test

// Differential tests of MakeManifest: every repository goes through the
// native indexer and through upstream `luarocks-admin make_manifest` run by
// internal/luaref over the same files in memory (luaref.VM.MakeManifest),
// and the two must write the same bytes — manifest, manifest-5.1 to 5.4 and
// index.html — or fail alike. `go test` runs the seeds; `go test -run '^$'
// -fuzz FuzzMakeManifest ./remote/` explores further.
//
// The differences accepted on purpose, each skipping only the input it
// would fail:
//
//   - MM1: luaref runs upstream on gopher-lua, whose pattern engine reads
//     the URL class of index.lua's detailed-description linker
//     ("[a-zA-Z0-9%.%%-_%+...]") as a range from the class %% to '_', which
//     matches nothing, where LuaJIT reads '%', '-' and '_' as literals.
//     MakeManifest follows LuaJIT, so a URL holding one of them in a
//     detailed description is linked whole; the lua backend ends the link
//     before it.
//   - MM2: gopher-lua's %s, %w and string.lower are Unicode-aware, LuaJIT's
//     are the C locale's, which MakeManifest follows; only input holding a
//     byte above 0x7F can tell them apart.
//   - MM3: versions of a rock that compare equal but are spelled
//     differently ("1.0-1", "1.0.0-1") are listed in index.html in an order
//     upstream's table.sort leaves unspecified; MakeManifest keeps byte
//     order.
//   - MM4: a file name holding a newline: upstream's `ls` prints it as two
//     lines, MakeManifest skips it; luaref's listing does neither.

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/luaref"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/rockspec"
)

// makeManifestOutputs are the files make_manifest writes for a rock server.
var makeManifestOutputs = []string{
	"manifest", "manifest-5.1", "manifest-5.2", "manifest-5.3", "manifest-5.4", "index.html",
}

// newMakeManifestVM boots the upstream VM with the platforms of the host,
// whose overrides MakeManifest applies to the rockspecs it reads, and closes
// it when tb ends.
func newMakeManifestVM(tb testing.TB) *luaref.VM {
	tb.Helper()

	vm, err := luaref.New(rockspec.RuntimePlatforms()...)
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(vm.Close)

	return vm
}

// mmSpec returns a rockspec of name and version with the extra top-level
// fields.
func mmSpec(name, version, extra string) string {
	return "package = '" + name + "'\nversion = '" + version + "'\n" +
		"source = { url = 'https://example.org/" + name + "-" + version + ".tar.gz' }\n" + extra + "\n"
}

// mmCase is one repository of the differential seeds.
type mmCase struct {
	name  string
	files map[string]string
	dirs  map[string][]string
}

// mmCases are the hand-picked repositories: the shapes tt indexes (wasm
// binary rocks for three arches, mirrors of rockspecs, source and pure-Lua
// rocks over several versions) and the edges of upstream's parsing.
var mmCases = []mmCase{
	{name: "empty"},
	{name: "wasm arches", files: map[string]string{
		"tt-wasm-0.1.0-1.linux-x86_64.rock":   "x",
		"tt-wasm-0.1.0-1.linux-aarch64.rock":  "x",
		"tt-wasm-0.1.0-1.macosx-aarch64.rock": "x",
		"tt-wasm-0.2.0-1.linux-x86_64.rock":   "x",
		"tt-wasm-0.2.0-1.macosx-aarch64.rock": "x",
	}},
	{name: "mirror", files: map[string]string{
		"crud-1.4.0-1.rockspec": mmSpec("crud", "1.4.0-1",
			"dependencies = { 'lua >= 5.1', 'tarantool', 'checks >= 3.1' }\n"+
				"description = { summary = 'CRUD', detailed = [[\nRows.\n\nSee https://github.com/tarantool/crud.\n]], "+
				"homepage = 'https://github.com/tarantool/crud', license = 'BSD' }"),
		"crud-1.4.0-1.src.rock":   "x",
		"crud-1.5.0-1.rockspec":   mmSpec("crud", "1.5.0-1", "dependencies = { 'lua ~> 5.1' }"),
		"crud-1.5.0-1.all.rock":   "x",
		"checks-3.1.0-1.all.rock": "x",
		"checks-3.2.0-1.rockspec": mmSpec("checks", "3.2.0-1", "dependencies = { 'lua >= 5.3' }"),
		"luatest-1.0.0-1.rockspec": mmSpec("luatest", "1.0.0-1",
			"dependencies = { 'lua < 5.3' }\nexternal_dependencies = { OPENSSL = { header = 'x.h' }, "+
				"platforms = { linux = { LIBZ = {}, OPENSSL = {} }, macosx = { ICONV = {} } } }"),
		"metrics-scm-1.rockspec":            mmSpec("metrics", "scm-1", "dependencies = { 'lua == 5.2' }"),
		"metrics-2.0.0-1.linux-x86_64.rock": "x",
		"metrics-2.0.0-1.src.rock":          "x",
	}},
	{name: "lua dependency forms", files: map[string]string{
		"a-1.0-1.rockspec": mmSpec("a", "1.0-1", "dependencies = { 'ns/lua >= 5.3' }"),
		"b-1.0-1.rockspec": mmSpec("b", "1.0-1", "dependencies = { 'lua>=5.2,<5.4' }"),
		"c-1.0-1.rockspec": mmSpec("c", "1.0-1", "dependencies = { 'luasocket', 'lua ~= 5.1' }"),
		"d-1.0-1.rockspec": mmSpec("d", "1.0-1",
			"dependencies = { 'lua >= 5.1', platforms = { linux = { 'lua >= 5.4' } } }"),
		"e-1.0-1.rockspec": mmSpec("e", "1.0-1", "dependencies = { 'lua >= 5.4', 'lua >= 5.1' }"),
		"f-1.0-1.rockspec": mmSpec("f", "1.0-1", "rockspec_format = '3.0'\ndependencies = { 'lua @>= 5.3' }"),
		"g-1.0-1.rockspec": mmSpec("g", "1.0-1", "dependencies = { 'lua ~> 5' }"),
		"h-1.0-1.rockspec": mmSpec("h", "1.0-1", "dependencies = { 'lua >= .5' }"),
	}},
	{name: "rockspecs upstream cannot load", files: map[string]string{
		"syntax-1.0-1.rockspec":   "package = ",
		"runtime-1.0-1.rockspec":  "error('boom')",
		"other-1.0-1.rockspec":    mmSpec("another", "1.0-1", "dependencies = { 'lua >= 5.4' }"),
		"Upper-1.0-1.rockspec":    mmSpec("Upper", "1.0-1", "dependencies = { 'lua >= 5.4' }"),
		"future-1.0-1.rockspec":   mmSpec("future", "1.0-1", "rockspec_format = '4.0'\ndependencies = { 'lua >= 5.4' }"),
		"baddep-1.0-1.rockspec":   mmSpec("baddep", "1.0-1", "dependencies = { 'lua >= 5.4', '>= 1' }"),
		"badop-1.0-1.rockspec":    mmSpec("badop", "1.0-1", "dependencies = { 'lua => 5.4' }"),
		"shebang-1.0-1.rockspec":  "#!/usr/bin/lua\n" + mmSpec("shebang", "1.0-1", "dependencies = { 'lua >= 5.4' }"),
		"numver-1-1.rockspec":     "package = 'numver'\nversion = 1\nsource = { url = 'x' }\ndependencies = { 'lua >= 5.4' }",
		"unknown-1.0-1.rockspec":  mmSpec("unknown", "1.0-1", "whatever = 1\ndependencies = { 'lua >= 5.4' }"),
		"badbuild-1.0-1.rockspec": mmSpec("badbuild", "1.0-1", "build = { type = 'rust' }\ndependencies = { 'lua >= 5.4' }"),
	}},
	{name: "file names", files: map[string]string{
		"foo-1.0-1.rockspec.bak":         "x",
		"foo-1.0-1.rockspecXrockspec":    mmSpec("foo", "1.0-1", ""),
		"-1.0-1.all.rock":                "x",
		".hidden-1.0-1.all.rock":         "x",
		"a\\bar-1.0-1.all.rock":          "x",
		"multi-dash-name-1.0-1.all.rock": "x",
		"weird$v-1.0$x-1.src.rock":       "x",
		"foo-1.0-1.all.rock":             "x",
		"rock":                           "x",
		"rockspec":                       "x",
		"README":                         "x",
		"manifest":                       "old",
		"manifest-5.1":                   "old",
	}},
	{name: "subdirectories", files: map[string]string{"x-1.0-1.all.rock": "x"}, dirs: map[string][]string{
		"manifests": {"ns"},
		"docs":      {"a.txt"},
	}},
	{name: "installed rock layout", dirs: map[string][]string{"foo": {"1.0-1"}}},
	{name: "index fields", files: map[string]string{
		"p-1.0-1.rockspec": mmSpec("P", "1.0-1", "description = { summary = '$license and $package', detailed = 'a  b\\n\\nc' }"),
		"p-2.0-1.all.rock": "x",
		"q-1.0-1.rockspec": "package = 'q'\nversion = '1.0-1'\nsource = { url = 'x', file = 'y.lua' }\ndescription = 'text'\n",
		"r-1.0-1.rockspec": mmSpec("r", "1.0-1", "description = { homepage = 7, license = 3.5 }\nexternal_dependencies = {}"),
		"s-1.0-1.all.rock": "x",
		"s-1.0-1.rockspec": "x = ",
	}},
	{name: "upstream raises in the filter", files: map[string]string{
		"n-1.0-1.rockspec": "package = 1\nversion = '1.0-1'\nsource = { url = 'x' }\n",
	}},
	{name: "upstream raises in the index", files: map[string]string{
		"n-1.0-1.rockspec": mmSpec("n", "1.0-1", "description = { detailed = 1 }"),
	}},
	{name: "source without url", files: map[string]string{
		"n-1.0-1.rockspec": "package = 'n'\nversion = '1.0-1'\nsource = {}\n",
	}},
}

// runReference runs upstream over the repository and returns the outputs
// it wrote, by name.
func runReference(t *testing.T, vm *luaref.VM, c mmCase) (files map[string]string, ok bool, msg string, skip bool) {
	t.Helper()

	written, _, ok, msg, err := vm.MakeManifest(luaref.Repo{Files: c.files, Dirs: c.dirs})
	if err != nil {
		if strings.Contains(err.Error(), "context") {
			return nil, false, "", true
		}

		t.Fatalf("luaref: %v", err)
	}

	files = map[string]string{}

	for full, content := range written {
		files[strings.TrimPrefix(full, luaref.RepoDir+"/")] = content
	}

	return files, ok, msg, false
}

// runNative materializes the repository in a temp dir, runs MakeManifest
// and returns the outputs, by name. A failing MakeManifest must leave the
// directory as it was.
func runNative(t *testing.T, c mmCase, index bool) (files map[string]string, err error) {
	t.Helper()

	dir := t.TempDir()

	for n, content := range c.files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(content), 0o600))
	}

	for d, entries := range c.dirs {
		require.NoError(t, os.Mkdir(filepath.Join(dir, d), 0o750))

		for _, e := range entries {
			require.NoError(t, os.WriteFile(filepath.Join(dir, d, e), nil, 0o600))
		}
	}

	before := snapshot(t, dir)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = remote.MakeManifest(ctx, dir, remote.MakeManifestOpts{Index: index})

	after := snapshot(t, dir)

	if err != nil {
		require.Equal(t, before, after, "a failing MakeManifest changed the directory")

		return nil, err
	}

	files = map[string]string{}

	for _, n := range makeManifestOutputs {
		if content, ok := after[n]; ok {
			files[n] = content
		}
	}

	return files, nil
}

// snapshot returns the content of every file directly in dir, by name.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	out := map[string]string{}

	for _, e := range entries {
		p := filepath.Join(dir, e.Name())

		switch {
		case e.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(p)
			require.NoError(t, err)

			out[e.Name()] = "-> " + target
		case e.IsDir():
			out[e.Name()+"/"] = ""
		default:
			b, err := os.ReadFile(p)
			if err != nil {
				out[e.Name()] = "unreadable: " + err.Error()

				continue
			}

			out[e.Name()] = string(b)
		}
	}

	return out
}

// compareMakeManifest runs c through both sides and compares them. accepted
// names the accepted difference that explains a mismatch in a file, if one
// does.
//
// Upstream writes the manifests and then index.html, so it can fail in the
// index after writing the manifests; MakeManifest computes everything
// first and then writes nothing. So the manifests upstream wrote are
// compared with those of a MakeManifest without Index, and its failure
// with that of one with Index.
func compareMakeManifest(t *testing.T, vm *luaref.VM, c mmCase, accepted func(file string) string) {
	t.Helper()

	want, ok, msg, skip := runReference(t, vm, c)
	if skip {
		t.Skip("the reference timed out")
	}

	_, manifestsWritten := want["manifest"]

	got, err := runNative(t, c, false)
	if !manifestsWritten {
		require.Error(t, err, "upstream fails before writing the manifests (%s), MakeManifest does not", msg)

		return
	}

	require.NoError(t, err, "MakeManifest fails where upstream writes the manifests")

	compareFiles(t, want, got, makeManifestOutputs[:5], accepted)

	got, err = runNative(t, c, true)
	if !ok {
		require.Error(t, err, "upstream fails writing index.html (%s), MakeManifest does not", msg)

		return
	}

	require.NoError(t, err, "MakeManifest with Index fails where upstream does not")

	compareFiles(t, want, got, makeManifestOutputs, accepted)
}

// compareFiles requires the files names of got to equal those of want.
func compareFiles(t *testing.T, want, got map[string]string, names []string, accepted func(file string) string) {
	t.Helper()

	for _, n := range names {
		if want[n] == got[n] {
			continue
		}

		if accepted != nil {
			if id := accepted(n); id != "" {
				t.Skipf("accepted difference %s in %s", id, n)
			}
		}

		require.Equal(t, want[n], got[n], "%s differs from upstream's", n)
	}
}

//nolint:paralleltest,tparallel // The subtests share one VM, which is not safe for concurrent use.
func TestMakeManifest_MatchesUpstream(t *testing.T) {
	t.Parallel()

	vm := newMakeManifestVM(t)

	for _, c := range mmCases {
		t.Run(c.name, func(t *testing.T) {
			compareMakeManifest(t, vm, c, nil)
		})
	}
}

// FuzzMakeManifest adds one file, of a fuzzed name and content, to a fixed
// repository, and the content again as a rockspec of a fixed name.
func FuzzMakeManifest(f *testing.F) {
	f.Add("zz-1.0-1.rockspec", mmSpec("zz", "1.0-1", "dependencies = { 'lua >= 5.2' }"))
	f.Add("crud-1.4.0-1.all.rock", "")
	f.Add("crud-1.4.00-1.linux-aarch64.rock", "")
	f.Add("fz-1.0-1.src.rock", mmSpec("fz", "1.0-1",
		"description = { detailed = 'see http://a.org/x and https://b.org/y?z=1;q' }"))
	f.Add("x", mmSpec("fz", "1.0-1", "external_dependencies = { platforms = { [1] = { A = {} } } }"))

	vm := newMakeManifestVM(f)

	base := mmCases[2].files

	f.Fuzz(func(t *testing.T, name, content string) {
		if name == "" || name == "." || name == ".." || len(name) > 200 ||
			strings.ContainsAny(name, "/\x00") || len(content) > 1<<12 {
			t.Skip()
		}

		files := map[string]string{"fz-1.0-1.rockspec": content}
		maps.Copy(files, base)

		files[name] = content

		accepted := func(file string) string {
			switch {
			case strings.Contains(name, "\n"):
				return "MM4"
			case !isASCII(name) || !isASCII(content):
				return "MM2"
			case file == "index.html" && strings.Contains(content, "http"):
				return "MM1"
			case file == "index.html" && hasVersionTie(t, vm, files):
				return "MM3"
			}

			return ""
		}

		compareMakeManifest(t, vm, mmCase{name: name, files: files}, accepted)
	})
}

func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}

	return true
}

// hasVersionTie reports whether two file names of files give one rock two
// versions upstream compares equal.
func hasVersionTie(t *testing.T, vm *luaref.VM, files map[string]string) bool {
	t.Helper()

	byName := map[string][]string{}

	for n := range files {
		name, version, _, ok := remote.ParseRockFileName(n)
		if ok && !slices.Contains(byName[name], version) {
			byName[name] = append(byName[name], version)
		}
	}

	for _, versions := range byName {
		for i, a := range versions {
			for _, b := range versions[i+1:] {
				rel, err := vm.CompareVersions(a, b)
				require.NoError(t, err)

				if !rel.Less && !rel.Greater {
					return true
				}
			}
		}
	}

	return false
}
