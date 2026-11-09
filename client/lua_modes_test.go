//go:build unix

package client_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
	lua "github.com/yuin/gopher-lua"
)

// TestLuaModes_UnderUmask: the directories and manifests the lua backend
// creates in a tree have the modes upstream LuaRocks gives them, 0777 and
// 0666 less the process umask. It created the rocks dir of an existing tree,
// and share and share/tarantool with it, 0750 whatever the umask, and every
// file it wrote through io.open, the manifest and each rock_manifest, 0600.
func TestLuaModes_UnderUmask(t *testing.T) {
	t.Parallel()

	for _, mask := range []int{0o022, 0o077} {
		root := t.TempDir()
		testenv.RunWithUmask(t, "TestLuaModes_Child", mask, root)

		tree := filepath.Join(root, "stub", "tree")
		perm := os.FileMode(mask)

		var dirs, manifests int

		require.NoError(t, filepath.WalkDir(tree, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == tree {
				return err
			}

			want := 0o777 &^ perm

			switch {
			case d.IsDir():
				dirs++
			case d.Name() == "manifest" || d.Name() == "rock_manifest":
				manifests++
				want = 0o666 &^ perm
			default:
				return nil
			}

			st, err := d.Info()
			if err != nil {
				return err
			}

			assert.Equal(t, want.String(), st.Mode().Perm().String(), "%s under umask %#o", p, mask)

			return nil
		}))

		assert.Positive(t, dirs, "the directories of %s", tree)
		assert.Equal(t, 2, manifests, "the manifest and the rock_manifest of %s", tree)
	}
}

// TestLuaModes_Child is the body of the child processes of
// TestLuaModes_UnderUmask: it makes a pure-Lua rock with the lua backend into
// root/stub/tree, which exists and is empty.
func TestLuaModes_Child(t *testing.T) {
	t.Parallel()

	root := testenv.UmaskChild(t)
	cfg := newLuaStubCfg(t, filepath.Join(root, "stub"))

	require.NoError(t, os.WriteFile(filepath.Join(cfg.WorkingDir, "modes.lua"), []byte("return 1\n"), 0o666))

	spec := filepath.Join(cfg.WorkingDir, "modes-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(spec, []byte(`package = "modes"
version = "1.0-1"
source = { url = "file://localhost/modes.tar.gz" }
build = { type = "builtin", modules = { modes = "modes.lua" } }
`), 0o666))

	r, err := client.New(cfg, client.WithBackend(client.BackendLua))
	require.NoError(t, err)
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: spec}))
}

// luaOpenScript opens files in dir, which it leaves as it found it, in the
// ways a file is created and fails to be, and returns every result it got. It
// is an expression for LuaEngine.Probe.
func luaOpenScript(dir string) string {
	return `(function(dir)
  local out = {}
  local function rec(...)
    local t = {}
    for i = 1, select("#", ...) do t[i] = tostring((select(i, ...))) end
    out[#out + 1] = select("#", ...) .. ": " .. table.concat(t, ", ")
  end
  local p = dir .. "/f"

  for _, mode in ipairs({ "w", "wb", "w+", "wb+", "a", "ab", "a+", "ab+" }) do
    local f = io.open(p, mode)
    rec(mode, io.type(f), f:write("one"))
    rec(f:seek("set"), f:read("*a"))
    f:close()
    f = io.open(p, mode)
    f:write("two")
    f:close()
    rec(io.open(p):read("*a"))
    os.remove(p)
  end

  rec(io.open(p))
  rec(io.open(p, "r+"))
  rec(io.open(p, "rb+"))
  rec(io.open(dir .. "/missing/f", "w"))
  rec(io.open(dir, "w"))
  rec(io.open(dir, "a"))
  rec(pcall(io.open, p, "x"))
  rec(pcall(io.open, p, nil))
  rec(pcall(io.open, nil, "w"))
  rec(pcall(io.open))
  rec(io.open(p, "r"))

  local o = io.output()
  rec(io.type(io.output(p)))
  rec(io.write("three"))
  io.output():close()
  io.output(o)
  rec(io.open(p):read("*a"))
  os.remove(p)
  rec(pcall(io.output, dir .. "/missing/f"))
  rec(pcall(io.output, 42))
  rec(io.open(p))

  return table.concat(out, "\n")
end)(` + quoteLuaString(dir) + `)`
}

// TestLuaEngine_OpenAsGopherLua — the engine wraps io.open and io.output,
// to create files by the umask, and must keep every other result of theirs
// as gopher-lua's own.
func TestLuaEngine_OpenAsGopherLua(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := filepath.Join(dir, "files")
	require.NoError(t, os.Mkdir(files, 0o777))

	stock := lua.NewState()
	defer stock.Close()

	require.NoError(t, stock.DoString("return "+luaOpenScript(files)), "the script under gopher-lua")
	want := stock.Get(-1).String()

	e := client.NewLuaEngine(newLuaStubCfg(t, dir), nil, nil)

	got, err := e.Probe(luaOpenScript(files))
	require.NoError(t, err, "the script under the engine")
	assert.Equal(t, want, got, "the engine's io.open and io.output return what gopher-lua's do")
}

// TestLuaEngine_OpenUnderUnwritableUmask: under a umask that takes the
// owner's write permission, a file 0666 less the umask cannot be opened for
// writing once created, while gopher-lua's, created by the open itself, can.
// io.open must still create and write the file.
func TestLuaEngine_OpenUnderUnwritableUmask(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	newLuaStubCfg(t, filepath.Join(root, "stub")) // the child cannot make it
	testenv.RunWithUmask(t, "TestLuaEngine_OpenUnderUnwritableUmask_Child", 0o277, root)

	b, err := os.ReadFile(filepath.Join(root, "f")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "written", string(b), "what io.open wrote")
}

// TestLuaEngine_OpenUnderUnwritableUmask_Child is the body of the child
// process of TestLuaEngine_OpenUnderUnwritableUmask.
func TestLuaEngine_OpenUnderUnwritableUmask_Child(t *testing.T) {
	t.Parallel()

	root := testenv.UmaskChild(t)
	e := client.NewLuaEngine(newLuaStubCfg(t, filepath.Join(root, "stub")), nil, nil)

	got, err := e.Probe(`(function()
  local f, err = io.open(` + quoteLuaString(filepath.Join(root, "f")) + `, "w")
  if not f then return err end
  assert(f:write("written"))
  f:close()
  return "ok"
end)()`)
	require.NoError(t, err)
	assert.Equal(t, "ok", got, "io.open of a new file")
}
