//go:build unix

package client_test

// The shell command lines the lua engine's shims build where upstream splices
// a path in unquoted (internal/luarocks/extra). Each test runs the shim with a
// stand-in for the program it runs, which records what it was given, under a
// path holding the characters the shell reads specially.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	lua "github.com/yuin/gopher-lua"
)

// hostileComponent is a directory name holding what sh reads specially in
// and out of quotes — a single quote, a double quote, a backslash, a dollar
// sign before a name and before a command, and a backtick — and a rockspec
// variable reference.
const hostileComponent = "it's \"q\" \\ $HOME $(PREFIX) $(touch dollar-ran) `touch backtick-ran`"

// writeRecorder writes an executable shell script at path that appends each
// of its arguments, NUL-terminated, to record and exits 0 — when only is
// not empty, for a call whose arguments include it alone.
func writeRecorder(t *testing.T, path, record, only string) {
	t.Helper()

	script := "#!/bin/sh\nprintf '%s\\0' \"$@\" >> " + shellQuote(record) + "\n"
	if only != "" {
		script = "#!/bin/sh\ncase \"$*\" in *" + shellQuote(only) + "*) printf '%s\\0' \"$@\" >> " +
			shellQuote(record) + ";; esac\n"
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(script), 0o700)) //nolint:gosec // an executable stand-in
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// recorded returns the arguments the recorder appended to record.
func recorded(t *testing.T, record string) []string {
	t.Helper()

	b, err := os.ReadFile(record) //nolint:gosec // test temp path
	require.NoError(t, err, "what the stand-in recorded")

	return strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
}

// luaQuote returns s as a Lua string literal, for the printable ASCII
// strings of these tests.
func luaQuote(s string) string { return strconv.Quote(s) }

// TestLuaEngine_IsLuaQuotesTheFileName — fs.is_lua tells whether a command
// script is Lua by running `<lua> -e "if loadfile('<file>') then ... end"`,
// with the file name spliced into the chunk and the chunk into the command
// line unquoted: under a path holding a single quote the chunk did not parse,
// and under one holding a dollar sign or a backtick the shell expanded it and
// ran what it named, so the deploy took a Lua script for a binary and copied
// it unwrapped. The interpreter must get a chunk that loads exactly the file,
// and nothing else may run.
func TestLuaEngine_IsLuaQuotesTheFileName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := newLuaStubCfg(t, dir)
	record := filepath.Join(dir, "record")
	writeRecorder(t, filepath.Join(cfg.Tarantool.Prefix, "bin", "tarantool"), record, "loadfile")

	script := filepath.Join(dir, hostileComponent, "bin", "cli")
	require.NoError(t, os.MkdirAll(filepath.Dir(script), 0o750))
	require.NoError(t, os.WriteFile(script, []byte("print('cli')\n"), 0o600))

	e := client.NewLuaEngine(cfg, nil, nil)
	got, err := e.Probe(`require("luarocks.fs").is_lua(` + luaQuote(script) + `)`)
	require.NoError(t, err, "fs.is_lua")
	assert.Equal(t, "true", got, "what fs.is_lua makes of the interpreter's exit 0")

	args := recorded(t, record)
	require.Len(t, args, 2, "the interpreter's arguments: %q", args)
	assert.Equal(t, "-e", args[0])

	// Run the chunk with a loadfile that records its argument.
	L := lua.NewState()
	defer L.Close()

	var loaded []string

	L.SetGlobal("loadfile", L.NewFunction(func(L *lua.LState) int {
		loaded = append(loaded, L.CheckString(1))
		L.Push(lua.LTrue)

		return 1
	}))
	L.SetGlobal("os", L.NewTable())
	L.SetField(L.GetGlobal("os"), "exit", L.NewFunction(func(*lua.LState) int { return 0 }))

	require.NoError(t, L.DoString(args[1]), "the chunk the interpreter got: %s", args[1])
	assert.Equal(t, []string{script}, loaded, "what the chunk loads")

	for _, marker := range []string{"dollar-ran", "backtick-ran"} {
		assert.NoFileExists(t, filepath.Join(cfg.WorkingDir, marker), "a command the file name holds ran")
	}
}

// TestLuaEngine_SubstitutesRockspecVariablesOnce — util.variable_substitutions,
// which the build backends run on the values they pass on, removed every
// $(NAME) its result still held, the ones a substituted value held among
// them, so under a tree or working dir whose path holds one, as
// "cost$(PREFIX)" does, the $(PREFIX), $(LUADIR), … of a build named
// another directory. Only a $(NAME) the rockspec writes that names no
// variable goes, with a warning, as upstream removes it.
func TestLuaEngine_SubstitutesRockspecVariablesOnce(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(newLuaStubCfg(t, t.TempDir()), nil, nil)
	got, err := e.Probe(`(function()
  local stderr = io.stderr
  local warned = {}
  io.stderr = { write = function(_, s) warned[#warned + 1] = s end }
  local t = { dir = "$(LUADIR)/m", flags = "-I$(INCDIR) $(NOPE)-x", n = 1 }
  require("luarocks.util").variable_substitutions(t, { LUADIR = "/t/cost$HOME$(PREFIX)/lua", INCDIR = 5 })
  io.stderr = stderr
  return table.concat({ t.dir, t.flags, tostring(t.n), table.concat(warned) }, "|")
end)()`)
	require.NoError(t, err, "util.variable_substitutions")
	assert.Equal(t, "/t/cost$HOME$(PREFIX)/lua/m|-I5 -x|1|Warning: unmatched variable NOPE\n", got)
}

// TestLuaEngine_CMakeQuotesTheRockspecVariables — the cmake build backend
// substitutes $(PREFIX), $(LUADIR) and the other rockspec variables into the
// values it passes to sh as -D<name>="<value>", unescaped, so under a tree
// whose path holds a dollar sign the shell expanded it and CMake got another
// prefix, and installed beside the tree. Every path a rockspec variable holds
// must reach CMake as it is; what the rockspec writes in a value itself still
// goes through sh, as upstream passes it.
func TestLuaEngine_CMakeQuotesTheRockspecVariables(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := newLuaStubCfg(t, dir)
	record := filepath.Join(dir, "record")
	cmake := filepath.Join(dir, "stub", "cmake")
	writeRecorder(t, cmake, record, "")

	prefix := filepath.Join(dir, hostileComponent, "tree", "rocks", "q", "1.0-1")
	luadir := filepath.Join(prefix, "lua")

	e := client.NewLuaEngine(cfg, nil, nil)
	got, err := e.Probe(`(function()
  local spec = setmetatable({
    build = { type = "cmake", variables = {
      CMAKE_INSTALL_PREFIX = "$(PREFIX)",
      LUADIR = "$(LUADIR)",
      OWN = "own $USER",
    } },
    variables = { CMAKE = ` + luaQuote(cmake) + `, PREFIX = ` + luaQuote(prefix) + `, LUADIR = ` + luaQuote(luadir) + ` },
  }, { __index = {
    type = function() return "rockspec" end,
    format_is_at_least = function() return true end,
  } })
  return tostring(require("luarocks.build.cmake").run(spec, true))
end)()`)
	require.NoError(t, err, "cmake.run")
	assert.Equal(t, "true", got, "what cmake.run returns")

	var defs []string

	for _, arg := range recorded(t, record) {
		if strings.HasPrefix(arg, "-D") {
			defs = append(defs, arg)
		}
	}

	assert.ElementsMatch(t, []string{
		"-DCMAKE_INSTALL_PREFIX=" + prefix,
		"-DLUADIR=" + luadir,
		"-DOWN=own " + os.Getenv("USER"),
	}, defs, "the -D values CMake got")

	for _, marker := range []string{"dollar-ran", "backtick-ran"} {
		assert.NoFileExists(t, filepath.Join(cfg.WorkingDir, marker), "a command a path holds ran")
	}
}
