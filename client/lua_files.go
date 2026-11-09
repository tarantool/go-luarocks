package client

import (
	"errors"
	"os"

	lua "github.com/yuin/gopher-lua"
)

// installFileOpeners wraps io.open and io.output in L.
//
// Normalize io.open mode strings. LuaRocks' luarocks/fs/lua.lua opens files
// with PUC-Lua-5.1 mode strings of the form "<mode>+b" (e.g. "r+b", "w+b",
// "a+b") for fs.is_writable / fs.copy_binary. gopher-lua's io.open accepts
// only the "<mode>b+" ordering ("rb+", "wb+", "ab+") and raises "invalid
// option" otherwise, crashing the command (exit 99). Wrap the original
// io.open to rewrite the "+b" tail to "b+" before delegating; every other
// mode passes through untouched. This is a VM-compatibility shim, not a
// behavior change — the resulting file is opened in the identical mode.
//
// A file io.open or io.output(path) creates gets the mode C's fopen gives it
// (callCreating).
func installFileOpeners(L *lua.LState, ioTbl *lua.LTable) error { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	origOpen, ok := L.GetField(ioTbl, "open").(*lua.LFunction)
	if !ok {
		return errors.New("luaEngine: io.open missing from Lua environment")
	}

	origOutput, ok := L.GetField(ioTbl, "output").(*lua.LFunction)
	if !ok {
		return errors.New("luaEngine: io.output missing from Lua environment")
	}

	L.SetField(ioTbl, "open", L.NewFunction(func(L *lua.LState) int {
		// Collect every argument so we can re-dispatch verbatim, rewriting
		// only the mode (arg 2) when it uses the unsupported "+b" ordering.
		top := L.GetTop()

		args := make([]lua.LValue, top)

		for i := 1; i <= top; i++ {
			args[i-1] = L.Get(i)
		}

		if top >= ioOpenMinArgsForMode {
			if ls, ok := args[1].(lua.LString); ok {
				mode := normalizeOpenMode(string(ls))
				args[1] = lua.LString(mode)

				if path, ok := args[0].(lua.LString); ok && luaCreatingOpenModes[mode] {
					return callCreating(L, origOpen, string(path), args)
				}
			}
		}

		L.Push(origOpen)

		for _, a := range args {
			L.Push(a)
		}
		// io.open returns (file) on success or (nil, errmsg, errno) on failure;
		// propagate all of them so LuaRocks' error handling is unchanged.
		L.Call(len(args), lua.MultRet)

		return L.GetTop() - top
	}))

	L.SetField(ioTbl, "output", L.NewFunction(func(L *lua.LState) int {
		path, ok := L.Get(1).(lua.LString)
		if !ok {
			return callThrough(L, origOutput)
		}

		args := make([]lua.LValue, L.GetTop())
		for i := range args {
			args[i] = L.Get(i + 1)
		}

		return callCreating(L, origOutput, string(path), args)
	}))

	return nil
}

// luaCreatedFileMode is the mode C's fopen, and so upstream LuaRocks, creates
// a file with, which the umask narrows. gopher-lua's io.open and io.output
// create theirs 0600 whatever the umask, so a tree the lua backend wrote had
// a manifest and rock_manifests no other user could read.
const luaCreatedFileMode = 0o666

// luaCreatingOpenModes are the io.open modes, as normalizeOpenMode leaves
// them, that create a missing file.
var luaCreatingOpenModes = map[string]bool{
	"w": true, "wb": true, "w+": true, "wb+": true,
	"a": true, "ab": true, "a+": true, "ab+": true,
}

// callCreating calls fn — gopher-lua's io.open or io.output — with args, the
// first of which, path, names a file the call creates when it is missing, and
// returns its results; a file the call creates gets luaCreatedFileMode less
// the umask. The file is created empty first, when nothing is at path, for fn
// to open. Should fn then fail on it, as it may where the umask leaves the
// file unwritable, the file is removed and fn called again, so the call ends
// exactly as it would have.
func callCreating(L *lua.LState, fn *lua.LFunction, path string, args []lua.LValue) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	top := L.GetTop()

	call := func(protected bool) error {
		L.Push(fn)

		for _, a := range args {
			L.Push(a)
		}

		if !protected {
			L.Call(len(args), lua.MultRet)

			return nil
		}

		return L.PCall(len(args), lua.MultRet, nil)
	}

	if !createByUmask(path) {
		_ = call(false)

		return L.GetTop() - top
	}

	// io.open fails with nil and a message, io.output raises.
	if err := call(true); err == nil && L.Get(top+1) != lua.LNil {
		return L.GetTop() - top
	}

	L.SetTop(top)

	_ = os.Remove(path)
	_ = call(false)

	return L.GetTop() - top
}

// luaTempNames records the names os.tmpname gives out in one VM, so that
// luaVM.Close removes the files LuaRocks leaves at them. util.popen_read
// (core/util.lua), which cfg.init runs, has os.execute write the output of its
// command into such a file and removes the file once it has read it, but a
// cancellation, which os.execute raises once the command is killed and the VM
// at its next instruction, skips the removal, so a cancelled call left the file
// in TMPDIR; and upload/api.lua returns without removing its file when the
// download into it fails. Only the goroutine running the VM, and then the one
// closing it, touch it.
type luaTempNames struct {
	names []string
}

// install wraps os.tmpname in L so that it records every name it returns.
func (n *luaTempNames) install(L *lua.LState, osTbl *lua.LTable) error { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	orig, ok := L.GetField(osTbl, "tmpname").(*lua.LFunction)
	if !ok {
		return errors.New("luaEngine: os.tmpname missing from Lua environment")
	}

	L.SetField(osTbl, "tmpname", L.NewFunction(func(L *lua.LState) int {
		L.Push(orig)
		L.Call(0, 1)

		if name, ok := L.Get(-1).(lua.LString); ok {
			n.names = append(n.names, string(name))
		}

		return 1
	}))

	return nil
}

// remove removes what is left at every recorded name: the file a command
// wrote there, or an empty directory. A name nothing is at any more, as
// LuaRocks removed its file, is passed over.
func (n *luaTempNames) remove() {
	for _, name := range n.names {
		_ = os.Remove(name)
	}

	n.names = nil
}

// createByUmask creates path as an empty file of mode luaCreatedFileMode less
// the umask when nothing is there, and reports whether it did.
func createByUmask(path string) bool {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, luaCreatedFileMode)
	if err != nil {
		return false
	}

	_ = f.Close()

	return true
}
