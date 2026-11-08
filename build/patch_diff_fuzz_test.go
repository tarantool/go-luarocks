package build_test

// Differential test of the port of upstream's patch tool: every patch goes
// through build's applyPatch, on a real directory, and through the vendored
// luarocks/tools/patch.lua, run by internal/luaref over its in-memory file
// system, as upstream fs.apply_patch calls it; whether the patch applied and
// every file afterwards must agree. `go test` runs the seeds below; `go test
// -run '^$' -fuzz FuzzApplyPatch ./build/` explores further.
//
// The differences accepted on purpose, each skipping only the comparison it
// would fail:
//
//   - D1 (containment): a name that leads out of the source dir fails the Go
//     side with rocks.ErrUnsafePath, where upstream patches wherever it leads.
//   - D2 (crash): where the Lua tool raises an error, a failure upstream as
//     well, the Go side fails too; the files it leaves behind are not
//     compared, since the Lua tool stops wherever the error hits.
//   - D3 (names): a patch holding a NUL byte is skipped: no file name on disk
//     can hold one, while the in-memory one can.

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	"github.com/tarantool/go-luarocks/internal/luaref"
)

// maxPatchInput bounds a fuzzed patch or file: the tool's line matching is
// quadratic in the file length.
const maxPatchInput = 4096

// refCwd is the source dir of the reference side.
const refCwd = "/src"

// refApply is fs_lua.apply_patch (fs/lua.lua:1098-1106) over the arguments
// it is called with.
const refApply = `
local patch = require("luarocks.tools.patch")
return function(name, data, create_delete)
  local p, all_ok = patch.read_patch(name, data)
  if not all_ok then
    return false
  end
  return patch.apply_patch(p, 1, create_delete) and true or false
end
`

// patchFiles is the source tree each patch is applied to, besides the
// fuzzed file a.txt.
var patchFiles = map[string]string{
	"sub/b.txt": "one\ntwo\nthree\nfour\nfive\nsix\nseven\n",
	"crlf.txt":  "alpha\r\nbeta\r\ngamma\r\n",
}

// patchSeeds are unified diffs against a.txt (whose seed contents are
// patchSeedFile) and the files of patchFiles, covering what the tool does:
// a plain change, several hunks and files, a hunk found at an offset and
// with fuzz, an already applied patch, line endings, file creation and
// deletion (rockspec_format 3.0 only), missing files and malformed input.
var patchSeeds = []string{
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n+LINE2\n line3\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,4 @@\n line1\n line2\n+inserted\n line3\n@@ -8,3 +9,2 @@\n line8\n-line9\n line10\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -3,3 +3,3 @@\n line1\n-line2\n+moved\n line3\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,4 +1,4 @@\n nomatch\n line2\n-line3\n+LINE3\n line4\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n line1\n-line0\n+line2\n line3\n",
	"--- a/sub/b.txt\n+++ b/sub/b.txt\n@@ -2,3 +2,3 @@\n two\n-three\n+THREE\n four\n" +
		"--- a/crlf.txt\n+++ b/crlf.txt\n@@ -1,2 +1,3 @@\n alpha\n+inserted\n beta\n",
	"--- a/crlf.txt\n+++ b/crlf.txt\n@@ -1,3 +1,3 @@\r\n alpha\r\n-beta\r\n+BETA\r\n gamma\r\n",
	"--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1,2 @@\n+hello\n+world\n",
	"--- a/gone.txt 1970-01-01 00:00:00.000000000 +0000\n+++ b/new2.txt 1970-01-01 00:00:00\n@@ -0,0 +1 @@\n+x\n",
	"--- a/sub/b.txt 2020-01-01\n+++ b/sub/b.txt 1970-01-01 00:00:00\n@@ -1,7 +0,0 @@\n-one\n-two\n-three\n-four\n-five\n-six\n-seven\n",
	"--- a/missing.txt\n+++ b/missing.txt\n@@ -1 +1 @@\n-a\n+b\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n line1\n-line2\n+x\n\\ No newline at end of file\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -9,3 +9,3 @@\n line9\n-line10\n+LAST\n line11\n",
	"garbage before\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-line1\n+first\n",
	"--- a/a.txt\n+++ b/a.txt\nnot a hunk\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,3 +1,3 @@\n line1\n-line2\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n line1\n-line2\n+a\n+b\n",
	"--- a/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-line1\n+x\n",
	"--- a/a.txt\nfoo\n+++ b/a.txt\n@@ -1 +1 @@\n-line1\n+x\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-line1\n+x\n--- a/sub\n+++ b/sub\n@@ -1 +1 @@\n-x\n+y\n",
	"--- a/../escape.txt\n+++ b/../escape.txt\n@@ -1 +1 @@\n-a\n+b\n",
	"--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,1 @@\n-line1\n+one\n@@ -1,1 +1,1 @@\n-line1\n+two\n",
	"",
	"--- a/a.txt\n+++ b/a.txt\n",
}

// patchSeedFile is the a.txt of the seeds.
const patchSeedFile = "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\nline11\n"

// FuzzApplyPatch applies data to a tree holding a.txt with contents file and
// patchFiles, through both sides, with createDelete as rockspec_format 3.0
// sets it.
func FuzzApplyPatch(f *testing.F) {
	for _, s := range patchSeeds {
		f.Add(s, patchSeedFile, false)
		f.Add(s, patchSeedFile, true)
	}

	f.Add("--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\r\n line1\r\n-line2\r\n+x\r\n", "line1\r\nline2\r\n", false)
	f.Add("--- a/a.txt\n+++ b/a.txt\n@@ -2,2 +2,2 @@\n b\n-c\n+C\n", "a\rb\rc\r", false)
	f.Add("--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n x\n-y\n+z\n", "x\ny", false)
	f.Add("--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-x\n+y\n", "", false)

	f.Fuzz(func(t *testing.T, data, file string, createDelete bool) {
		if len(data) > maxPatchInput || len(file) > maxPatchInput {
			t.Skip("input too long")
		}

		if strings.ContainsRune(data, 0) {
			t.Skip("D3: a NUL byte in a patch")
		}

		files := map[string]string{"a.txt": file}
		maps.Copy(files, patchFiles)

		goFiles, goErr := applyGo(t, files, data, createDelete)

		if errors.Is(goErr, rocks.ErrUnsafePath) {
			t.Skip("D1: containment")
		}

		luaOK, luaFiles, luaErr := applyLua(t, files, data, createDelete)
		if luaErr != nil {
			require.Error(t, goErr, "the Lua tool raised %v; the Go side must fail too", luaErr)

			return
		}

		require.NotErrorIs(t, goErr, build.ErrPatchCrash, "the Go side crashed where the Lua tool did not")
		require.Equal(t, luaOK, goErr == nil, "applied (Go error: %v)", goErr)
		require.Equal(t, luaFiles, goFiles, "the files after the patch")
	})
}

// applyGo applies data to files in a temp dir and returns the error and the
// files afterwards, keyed by their names under refCwd.
func applyGo(t *testing.T, files map[string]string, data string, createDelete bool) (map[string]string, error) {
	t.Helper()

	root := t.TempDir()

	for name, content := range files {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}

	err := build.ApplyPatch(root, "the.patch", data, createDelete)

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil || !d.Type().IsRegular() {
			return werr
		}

		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}

		body, rerr := os.ReadFile(p) //nolint:gosec // test temp path
		if rerr != nil {
			return rerr
		}

		out[refCwd+"/"+filepath.ToSlash(rel)] = string(body)

		return nil
	}))

	return out, err
}

// applyLua applies data to files through the reference and returns whether
// it applied, the Lua error it raised, if any, and the files afterwards.
func applyLua(t *testing.T, files map[string]string, data string, createDelete bool) (bool, map[string]string, error) {
	t.Helper()

	vm, err := luaref.New()
	require.NoError(t, err)
	t.Cleanup(vm.Close)

	for name, content := range files {
		vm.AddFile(name, content)
	}

	require.NoError(t, vm.WritableFiles(refCwd))

	chunk, err := vm.L.LoadString(refApply)
	require.NoError(t, err)

	res, err := vm.CallValue(chunk)
	require.NoError(t, err)

	res, err = vm.CallValue(res[0], lua.LString("the.patch"), lua.LString(data), lua.LBool(createDelete))
	if err != nil {
		return false, nil, err
	}

	return res[0] == lua.LTrue, vm.Files(), nil
}
