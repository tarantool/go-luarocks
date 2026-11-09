//go:build unix

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	lua "github.com/yuin/gopher-lua"
	"golang.org/x/sys/unix"
)

// luaProcScript exercises os.execute and io.popen the ways LuaRocks and a
// build backend may use them, and returns every result it got, each as its
// count and values. It is an expression for LuaEngine.Probe.
const luaProcScript = `(function()
  local out = {}
  local function rec(...)
    local t = {}
    for i = 1, select("#", ...) do t[i] = tostring((select(i, ...))) end
    out[#out + 1] = select("#", ...) .. ": " .. table.concat(t, ", ")
  end

  rec(os.execute("true"))
  rec(os.execute("exit 3"))
  rec(os.execute("kill -9 $$"))
  rec(os.execute(""))
  rec(os.execute("cd /nonexistent 2>/dev/null && true"))
  rec(pcall(os.execute))
  rec(pcall(os.execute, {}))

  local p = io.popen("printf 'one\\ntwo\\n3.5 rest\\nlast'")
  rec(io.type(p))
  rec(tostring(p))
  rec(p:read())
  rec(p:read("*l"))
  rec(p:read("*n"))
  rec(p:read(3))
  rec(p:read(0))
  rec(p:read("*l", "*a"))
  rec(p:read("*a"))
  rec(p:read("*l"))
  rec(p:read(0))
  rec(p:read(2))
  rec(p:read("*n"))
  rec(pcall(p.read, p, "*x"))
  rec(pcall(p.read, p, "x"))
  rec(p:write("x"))
  rec(p:seek())
  rec(p:flush())
  rec(p:setvbuf("no"))
  rec(p:close())
  rec(io.type(p))
  rec(tostring(p))
  rec(pcall(p.read, p))
  rec(p:close())

  p = io.popen("printf 'a\\n\\nb\\r\\nc'", "r")
  local lines = {}
  for l in p:lines() do lines[#lines + 1] = "[" .. l .. "]" end
  rec(table.concat(lines))
  rec(io.close(p))

  p = io.popen("printf '%5000s' x", nil)
  rec(#p:read("*l"))
  rec(p:close())

  p = io.popen("printf '%5000s\\n' x")
  local lens = {}
  for l in p:lines() do lens[#lens + 1] = #l end
  rec(table.concat(lens, " "))
  p:close()

  rec(io.popen("exit 5"):close())
  rec(io.popen("kill -9 $$"):close())
  rec(io.popen("", "r"):close())
  rec(pcall(io.popen, "true", "rb"))
  rec(pcall(io.popen))

  local w = io.popen("true", "w")
  rec(io.type(w))
  rec(tostring(w))
  rec(w:read())
  rec(w:lines())
  rec(w:close())
  rec(io.type(w))

  rec(io.type(io.stdout))
  rec(io.type(42))
  rec(pcall(io.close, 42))

  return table.concat(out, "\n")
end)()`

// TestLuaEngine_ProcessesAsGopherLua — the engine runs os.execute and
// io.popen itself, under the context of the call, and must keep every result
// LuaRocks and the build backends it loads get from them as they were with
// gopher-lua's own functions: the 0 and 1 of os.execute, a pipe's reads,
// lines, exit status and messages, and io.type and io.close on a pipe.
func TestLuaEngine_ProcessesAsGopherLua(t *testing.T) {
	t.Parallel()

	stock := lua.NewState()
	defer stock.Close()

	require.NoError(t, stock.DoString("return "+luaProcScript), "the script under gopher-lua")
	want := stock.Get(-1).String()

	dir := t.TempDir()
	e := client.NewLuaEngine(newLuaStubCfg(t, dir), nil, nil)

	got, err := e.Probe(luaProcScript)
	require.NoError(t, err, "the script under the engine")
	assert.Equal(t, want, got, "the engine's os.execute and io.popen return what gopher-lua's do")
}

// luaStdScript writes to the standard output and error of the process from
// Lua and from a command os.execute runs.
const luaStdScript = `io.stdout:write("lua: out\n")
io.stderr:write("lua: err\n")
os.execute("echo sh: out; echo sh: err >&2")`

// TestLuaEngine_ExecuteKeepsStderrApart — `go test -json` sets os.Stderr to
// os.Stdout (testing, under -test.v=test2json), and the engine's os.execute
// handed a command the files the two variables held at the call, which
// exec.Cmd then took for one destination of both: the command's stderr went
// to the process's stdout, into what Exec printed, and
// TestLuaEngine_TypedMethodsIgnoreConfigFile found sh's "not found" there. A
// command's output and errors must go to file descriptors 1 and 2, as
// io.stdout and io.stderr write, and as with gopher-lua's os.execute when
// nothing reassigned the variables.
//
//nolint:paralleltest // redirects file descriptors 1 and 2 and sets os.Stderr, which are process-wide
func TestLuaEngine_ExecuteKeepsStderrApart(t *testing.T) {
	saved := os.Stderr

	t.Cleanup(func() { os.Stderr = saved })

	stock := lua.NewState()
	defer stock.Close()

	// gopher-lua's os.execute takes os.Stderr at the call, which -json makes
	// os.Stdout, so it runs with one of fd 2 of its own.
	wantOut, wantErr := captureStdio(t, func() error {
		fd, err := unix.FcntlInt(2, unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			return err
		}

		os.Stderr = os.NewFile(uintptr(fd), "stderr")
		defer func() {
			_ = os.Stderr.Close()
			os.Stderr = saved
		}()

		return stock.DoString(luaStdScript)
	})
	require.Equal(t, "lua: out\nsh: out\n", wantOut, "what gopher-lua writes to fd 1")
	require.Equal(t, "lua: err\nsh: err\n", wantErr, "what gopher-lua writes to fd 2")

	e := client.NewLuaEngine(newLuaStubCfg(t, t.TempDir()), nil, nil)

	os.Stderr = os.Stdout // as testing does under -test.v=test2json

	gotOut, gotErr := captureStdio(t, func() error { return e.Run(context.Background(), luaStdScript) })
	assert.Equal(t, wantOut, gotOut, "what the engine writes to fd 1")
	assert.Equal(t, wantErr, gotErr, "what the engine writes to fd 2")
}

// captureStdio runs fn with file descriptors 1 and 2 redirected into pipes
// of their own and returns what was written to each.
func captureStdio(t *testing.T, fn func() error) (stdout, stderr string) {
	t.Helper()

	stderr = captureFd(t, 2, func() error {
		stdout = captureFd(t, 1, fn)

		return nil
	})

	return stdout, stderr
}

// TestLuaEngine_PopenWriteCloses — gopher-lua's close of a "w" pipe waited
// for the child without closing its input, so a child that reads its input to
// the end never exited and the close hung. The engine's closes the input
// first, as C Lua's pclose does.
func TestLuaEngine_PopenWriteCloses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	e := client.NewLuaEngine(newLuaStubCfg(t, dir), nil, nil)

	got, err := e.Probe(`(function()
  local w = io.popen("cat > '` + out + `'", "w")
  assert(w:write("one ", 2, "\n"))
  assert(w:setvbuf("full", 1024))
  assert(w:write("buffered"))
  return w:close()
end)()`)
	require.NoError(t, err, "write to a pipe")
	assert.Equal(t, "0", got, "the exit status of cat")

	b, err := os.ReadFile(out) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "one 2\nbuffered", string(b), "what cat read")
}
