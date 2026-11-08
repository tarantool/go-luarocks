// Package luaref runs modules of the vendored upstream LuaRocks in a bare
// gopher-lua state, as the reference side of the differential tests that
// compare the pure-Go implementation against upstream. It is imported only by
// tests.
//
// A VM preloads every module under src/src/luarocks/ of the embedded
// luarocksembed.FS (compiled once per process, on first require) and nothing
// else: package.path is empty, so require never reaches the disk. A few
// modules and VM primitives are replaced, all of them listed here:
//
//   - luarocks.core.cfg is a stub, because the real module detects the host
//     (uname, environment, config files) at load time. It carries only the
//     fields the pure functions under test read: arch, lua_version,
//     accept_unknown_fields, an empty variables table, a root_dir and
//     rocks_subdir to derive install paths from, a cache that says the
//     LuaJIT version was already probed (so util.get_rocks_provided does not
//     run an interpreter) and each_platform, which yields the platforms
//     passed to New in order — upstream's default least-specific-first walk.
//   - luarocks.fs is a stub that raises on any use: persist.lua requires it at
//     load time but none of the functions under test call it, except the
//     ones MakeManifest adds over its in-memory Repo (see there).
//   - io.open reads only the in-memory files registered with AddFile (and,
//     during MakeManifest, those of its Repo); every other open fails as a
//     missing file would. It writes only during MakeManifest, into memory.
//   - luarocks.util.printerr records what it prints, for MakeManifest,
//     instead of writing to stderr. os.getenv finds no variable.
//     io.popen, os.execute, os.remove, os.rename, os.exit, os.tmpname,
//     loadfile and dofile raise. The reference never touches the host.
//   - util.warning (luarocks.core.util) records the warning instead of
//     printing it; Warnings returns the record. vers.parse_version reports an
//     unparsable version only this way.
//   - tostring of a number and string.format("%q", s) follow LuaJIT, the VM
//     Tarantool runs LuaRocks on, rather than gopher-lua, which formats both
//     with Go's fmt verbs: a number prints as C's "%.14g" ("nan", "inf" and
//     "-inf" for the special values) and %q escapes '"', '\\' and newline
//     with a backslash and every other control byte as a decimal escape,
//     three digits wide when a digit follows, leaving bytes 0x80-0xFF as they
//     are (lj_strfmt.c). Every other format string goes to gopher-lua's
//     string.format unchanged.
//   - string.lower and string.upper map ASCII letters only, byte by byte, as
//     LuaJIT does in the C locale; gopher-lua's are Go's Unicode-aware
//     strings.ToLower and strings.ToUpper.
//   - tonumber of a string of decimal digits too long for an int64 returns the
//     nearest number, as LuaJIT does, instead of gopher-lua's nil;
//     vers.parse_version runs every numeric token through tonumber.
package luaref

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	luarocksembed "github.com/tarantool/go-luarocks/internal/luarocks"
	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// modulesRoot is the embed path of the vendored upstream Lua modules.
const modulesRoot = "src/src/luarocks"

const (
	// callTimeout bounds every call into the VM, so a reference function that
	// does not return fails the test instead of hanging it.
	callTimeout = 10 * time.Second

	// secondArg is the stack index of a Lua function's second argument.
	secondArg = 2

	// enoent is the errno io.open reports for a missing file.
	enoent = 2

	// numberPrecision is the %g precision of LuaJIT's number format, "%.14g".
	numberPrecision = 14
)

// DefaultPlatforms is the platform list New uses when none is given: what
// upstream cfg.each_platform yields on Linux.
var DefaultPlatforms = []string{"unix", "linux"}

// modulePaths maps every upstream module name to its embed path; protos caches
// the compiled modules. Both are shared by all VMs: a compiled proto is
// read-only and binds to a VM only through NewFunctionFromProto.
var (
	pathsOnce   sync.Once
	modulePaths map[string]string
	errPaths    error

	protosMu sync.Mutex
	protos   = map[string]*lua.FunctionProto{}
)

func loadModulePaths() (map[string]string, error) {
	pathsOnce.Do(func() {
		modulePaths = map[string]string{}
		errPaths = fs.WalkDir(luarocksembed.FS, modulesRoot, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() || !strings.HasSuffix(p, ".lua") {
				return nil
			}

			rel := strings.TrimSuffix(strings.TrimPrefix(p, modulesRoot+"/"), ".lua")
			modulePaths["luarocks."+strings.ReplaceAll(rel, "/", ".")] = p

			return nil
		})
	})

	return modulePaths, errPaths
}

func compiled(name, path string) (*lua.FunctionProto, error) {
	protosMu.Lock()
	defer protosMu.Unlock()

	if p, ok := protos[name]; ok {
		return p, nil
	}

	src, err := luarocksembed.FS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("luaref: read %s: %w", path, err)
	}

	chunk, err := parse.Parse(bytes.NewReader(src), path)
	if err != nil {
		return nil, fmt.Errorf("luaref: parse %s: %w", path, err)
	}

	p, err := lua.Compile(chunk, path)
	if err != nil {
		return nil, fmt.Errorf("luaref: compile %s: %w", path, err)
	}

	protos[name] = p

	return p, nil
}

// VM is a gopher-lua state with the upstream modules preloaded. It is not
// safe for concurrent use.
type VM struct {
	// L is the underlying state, for conversions and direct calls.
	L        *lua.LState
	files    map[string]string
	warnings []string

	// repo, written and stderr hold the state of a MakeManifest call: the
	// directory it serves, the files it wrote and what it printed.
	repo    *Repo
	written map[string]string
	stderr  []string
}

// New boots a VM whose stub cfg.each_platform yields platforms, or
// DefaultPlatforms when none are given. The caller must Close it.
func New(platforms ...string) (*VM, error) {
	paths, err := loadModulePaths()
	if err != nil {
		return nil, fmt.Errorf("luaref: list embedded modules: %w", err)
	}

	if len(platforms) == 0 {
		platforms = DefaultPlatforms
	}

	vm := &VM{L: lua.NewState(), files: map[string]string{}}

	if err := vm.install(paths, platforms); err != nil {
		vm.Close()

		return nil, err
	}

	return vm, nil
}

// Close releases the state.
func (vm *VM) Close() {
	vm.L.Close()
}

// AddFile registers an in-memory file that io.open (and so
// persist.load_into_table) can read.
func (vm *VM) AddFile(name, content string) {
	vm.files[name] = content
}

// Warnings returns the messages passed to util.warning since the last
// ResetWarnings.
func (vm *VM) Warnings() []string {
	return vm.warnings
}

// ResetWarnings forgets the recorded warnings.
func (vm *VM) ResetWarnings() {
	vm.warnings = nil
}

// Require loads an upstream module (or a stub) and returns its table.
func (vm *VM) Require(name string) (*lua.LTable, error) {
	res, err := vm.CallValue(vm.L.GetGlobal("require"), lua.LString(name))
	if err != nil {
		return nil, err
	}

	t, ok := res[0].(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("luaref: module %s is a %s, not a table", name, res[0].Type())
	}

	return t, nil
}

// Function returns module[name], which must be a function.
func (vm *VM) Function(module, name string) (*lua.LFunction, error) {
	m, err := vm.Require(module)
	if err != nil {
		return nil, err
	}

	fn, ok := m.RawGetString(name).(*lua.LFunction)
	if !ok {
		return nil, fmt.Errorf("luaref: %s.%s is not a function", module, name)
	}

	return fn, nil
}

// Call calls module[function](args...) and returns every result. A Lua error
// becomes the returned error.
func (vm *VM) Call(module, function string, args ...lua.LValue) ([]lua.LValue, error) {
	fn, err := vm.Function(module, function)
	if err != nil {
		return nil, err
	}

	return vm.CallValue(fn, args...)
}

// CallValue calls fn(args...) and returns every result, at least one (nil
// when fn returns nothing).
func (vm *VM) CallValue(fn lua.LValue, args ...lua.LValue) ([]lua.LValue, error) {
	ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
	defer cancel()

	vm.L.SetContext(ctx)
	defer vm.L.RemoveContext()

	top := vm.L.GetTop()

	if err := vm.L.CallByParam(lua.P{Fn: fn, NRet: lua.MultRet, Protect: true}, args...); err != nil {
		vm.L.SetTop(top)

		return nil, err
	}

	n := vm.L.GetTop() - top
	res := make([]lua.LValue, 0, max(n, 1))

	for i := top + 1; i <= top+n; i++ {
		res = append(res, vm.L.Get(i))
	}

	vm.L.SetTop(top)

	if len(res) == 0 {
		res = append(res, lua.LNil)
	}

	return res, nil
}

// Upvalue returns the upvalue called name of the Lua function fn. It reaches
// the module-local helpers upstream does not export, such as the
// parse_constraints closure of queries.from_dep_string.
//
//nolint:ireturn // An upvalue can hold any Lua value.
func Upvalue(fn *lua.LFunction, name string) (lua.LValue, error) {
	if fn.IsG || fn.Proto == nil {
		return nil, errors.New("luaref: not a Lua function")
	}

	for i, n := range fn.Proto.DbgUpvalues {
		if n == name && i < len(fn.Upvalues) {
			return fn.Upvalues[i].Value(), nil
		}
	}

	return nil, fmt.Errorf("luaref: no upvalue %q", name)
}

// install sets up the preload table, the stubs and the VM shims.
func (vm *VM) install(paths map[string]string, platforms []string) error {
	L := vm.L

	pkg, ok := L.GetGlobal("package").(*lua.LTable)
	if !ok {
		return errors.New("luaref: package table missing")
	}

	L.SetField(pkg, "path", lua.LString(""))
	L.SetField(pkg, "cpath", lua.LString(""))

	preload, ok := L.GetField(pkg, "preload").(*lua.LTable)
	if !ok {
		return errors.New("luaref: package.preload missing")
	}

	for name, path := range paths {
		L.SetField(preload, name, L.NewFunction(func(L *lua.LState) int {
			p, err := compiled(name, path)
			if err != nil {
				L.RaiseError("%s", err.Error())
			}

			L.Push(L.NewFunctionFromProto(p))
			L.Push(lua.LString(name))
			L.Call(1, 1)

			return 1
		}))
	}

	L.SetField(preload, "luarocks.core.cfg", L.NewFunction(func(L *lua.LState) int {
		L.Push(vm.cfgStub(platforms))

		return 1
	}))
	L.SetField(preload, "luarocks.fs", L.NewFunction(func(L *lua.LState) int {
		L.Push(loudStub(L, "luarocks.fs"))

		return 1
	}))

	if err := vm.installShims(); err != nil {
		return err
	}

	util, err := vm.Require("luarocks.core.util")
	if err != nil {
		return err
	}

	util.RawSetString("warning", L.NewFunction(func(L *lua.LState) int {
		vm.warnings = append(vm.warnings, L.OptString(1, ""))

		return 0
	}))

	return nil
}

// cfgStub builds the luarocks.core.cfg replacement described in the package
// comment.
func (vm *VM) cfgStub(platforms []string) *lua.LTable {
	L := vm.L
	cfg := L.NewTable()
	cfg.RawSetString("arch", lua.LString("linux-x86_64"))
	cfg.RawSetString("lua_version", lua.LString("5.1"))
	cfg.RawSetString("accept_unknown_fields", lua.LFalse)
	cfg.RawSetString("variables", L.NewTable())
	cfg.RawSetString("root_dir", lua.LString("/luaref-tree"))
	cfg.RawSetString("rocks_subdir", lua.LString("lib/luarocks/rocks"))

	cache := L.NewTable()
	cache.RawSetString("luajit_version_checked", lua.LTrue)
	cfg.RawSetString("cache", cache)

	cfg.RawSetString("each_platform", L.NewFunction(func(L *lua.LState) int {
		i := 0

		L.Push(L.NewFunction(func(L *lua.LState) int {
			if i >= len(platforms) {
				L.Push(lua.LNil)

				return 1
			}

			L.Push(lua.LString(platforms[i]))
			i++

			return 1
		}))

		return 1
	}))

	return cfg
}

// loudStub returns a table that raises on any field read.
func loudStub(ls *lua.LState, name string) *lua.LTable {
	t := ls.NewTable()
	mt := ls.NewTable()
	mt.RawSetString("__index", ls.NewFunction(func(L *lua.LState) int {
		L.RaiseError("luaref stub: %s.%s is not available", name, L.OptString(secondArg, "?"))

		return 0
	}))
	ls.SetMetatable(t, mt)

	return t
}

// installShims replaces the VM primitives listed in the package comment.
func (vm *VM) installShims() error {
	L := vm.L

	ioTbl, ok := L.GetGlobal("io").(*lua.LTable)
	if !ok {
		return errors.New("luaref: io table missing")
	}

	osTbl, ok := L.GetGlobal("os").(*lua.LTable)
	if !ok {
		return errors.New("luaref: os table missing")
	}

	strTbl, ok := L.GetGlobal("string").(*lua.LTable)
	if !ok {
		return errors.New("luaref: string table missing")
	}

	deny := func(name string) *lua.LFunction {
		return L.NewFunction(func(L *lua.LState) int {
			L.RaiseError("luaref: %s is not available", name)

			return 0
		})
	}

	L.SetField(ioTbl, "popen", deny("io.popen"))
	L.SetField(ioTbl, "open", L.NewFunction(vm.ioOpen))

	for _, name := range []string{"execute", "remove", "rename", "exit", "tmpname"} {
		L.SetField(osTbl, name, deny("os."+name))
	}

	L.SetField(osTbl, "getenv", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LNil)

		return 1
	}))

	L.SetGlobal("loadfile", deny("loadfile"))
	L.SetGlobal("dofile", deny("dofile"))

	origToString := L.GetGlobal("tostring")
	L.SetGlobal("tostring", L.NewFunction(func(L *lua.LState) int {
		if n, ok := L.Get(1).(lua.LNumber); ok {
			L.Push(lua.LString(FormatNumber(float64(n))))

			return 1
		}

		L.Push(origToString)
		L.Push(L.Get(1))
		L.Call(1, 1)

		return 1
	}))

	origToNumber := L.GetGlobal("tonumber")
	L.SetGlobal("tonumber", L.NewFunction(func(L *lua.LState) int {
		top := L.GetTop()
		L.Push(origToNumber)

		for i := 1; i <= top; i++ {
			L.Push(L.Get(i))
		}

		L.Call(top, 1)

		s, isString := L.Get(1).(lua.LString)
		if isString && top == 1 && L.Get(-1) == lua.LNil && isDigits(string(s)) {
			// ParseFloat reports ErrRange with ±Inf for an overflow, which is
			// what strtod returns too.
			f, _ := strconv.ParseFloat(string(s), 64)

			L.Pop(1)
			L.Push(lua.LNumber(f))
		}

		return 1
	}))

	for name, conv := range map[string]func(byte) byte{"lower": asciiLower, "upper": asciiUpper} {
		L.SetField(strTbl, name, L.NewFunction(func(L *lua.LState) int {
			s := []byte(L.CheckString(1))
			for i := range s {
				s[i] = conv(s[i])
			}

			L.Push(lua.LString(s))

			return 1
		}))
	}

	origFormat := L.GetField(strTbl, "format")
	L.SetField(strTbl, "format", L.NewFunction(func(L *lua.LState) int {
		if L.CheckString(1) == "%q" {
			L.Push(lua.LString(Quote(L.CheckString(secondArg))))

			return 1
		}

		top := L.GetTop()
		L.Push(origFormat)

		for i := 1; i <= top; i++ {
			L.Push(L.Get(i))
		}

		L.Call(top, 1)

		return 1
	}))

	return nil
}

// ioOpen serves the in-memory files registered with AddFile, read-only.
func (vm *VM) ioOpen(ls *lua.LState) int {
	name := ls.CheckString(1)
	mode := ls.OptString(secondArg, "r")

	if mode == "w" || mode == "wb" {
		if _, _, inRepo := repoPath(name); vm.repo != nil && inRepo {
			return vm.ioCreate(ls, name)
		}
	}

	content, ok := vm.files[name]
	if !ok {
		content, ok = vm.repoFile(name)
	}

	if !ok || (mode != "r" && mode != "rb") {
		ls.Push(lua.LNil)
		ls.Push(lua.LString(name + ": No such file or directory"))
		ls.Push(lua.LNumber(enoent))

		return 3 //nolint:mnd // nil, message, errno.
	}

	read := false
	fd := ls.NewTable()
	fd.RawSetString("read", ls.NewFunction(func(L *lua.LState) int {
		if read {
			L.Push(lua.LString(""))

			return 1
		}

		read = true

		L.Push(lua.LString(content))

		return 1
	}))
	fd.RawSetString("close", ls.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LTrue)

		return 1
	}))
	ls.Push(fd)

	return 1
}

// ioCreate opens name for writing into vm.written; close stores what was
// written.
func (vm *VM) ioCreate(ls *lua.LState, name string) int {
	var buf strings.Builder

	fd := ls.NewTable()
	fd.RawSetString("write", ls.NewFunction(func(L *lua.LState) int {
		for i := 2; i <= L.GetTop(); i++ {
			buf.WriteString(L.CheckString(i))
		}

		L.Push(L.Get(1))

		return 1
	}))
	fd.RawSetString("close", ls.NewFunction(func(L *lua.LState) int {
		if vm.written != nil {
			vm.written[name] = buf.String()
		}

		L.Push(lua.LTrue)

		return 1
	}))
	ls.Push(fd)

	return 1
}

func asciiLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}

	return c
}

func asciiUpper(c byte) byte {
	if c >= 'a' && c <= 'z' {
		return c - ('a' - 'A')
	}

	return c
}

// isDigits reports whether s is a non-empty run of decimal digits.
func isDigits(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}

// FormatNumber renders f the way LuaJIT's tostring does: C's "%.14g", with
// "nan", "inf" and "-inf" for the special values.
func FormatNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}

	// strconv's 'g' with an explicit precision picks %e exactly when C does
	// (exponent < -4 or >= precision), drops trailing zeros as C's %g does and
	// writes at least two exponent digits, as C does.
	return strconv.FormatFloat(f, 'g', numberPrecision, 64)
}

// Quote renders s the way LuaJIT's string.format("%q", s) does
// (lj_strfmt.c strfmt_putquotedlen).
func Quote(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for i := range len(s) {
		c := s[i]

		switch {
		case c == '"' || c == '\\' || c == '\n':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			b.WriteByte('\\')

			if i+1 < len(s) && s[i+1] >= '0' && s[i+1] <= '9' {
				fmt.Fprintf(&b, "%03d", c)
			} else {
				b.WriteString(strconv.Itoa(int(c)))
			}
		default:
			b.WriteByte(c)
		}
	}

	b.WriteByte('"')

	return b.String()
}
