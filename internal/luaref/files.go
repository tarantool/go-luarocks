package luaref

import (
	"errors"
	"maps"
	"path"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// WritableFiles turns the read-only in-memory files of AddFile into a small
// writable file system rooted at the absolute directory cwd, for the
// reference functions that write, rename and remove files, such as
// luarocks.tools.patch. From then on:
//
//   - a relative name is resolved under cwd, and every name is cleaned, so
//     "a/../b" and "./b" are the file b;
//   - the directories are cwd, its parents, and the parents of every file
//     added before the call; no function creates one;
//   - io.open opens a file for reading ("r", "rb"), creating or truncating
//     it ("w", "wb", "w+", "wb+"), which needs its directory, or a directory
//     for reading, which then reads nothing, as on Linux; what is written
//     lands in the file at once, closed or not;
//   - a read takes a count, "*a" or "*l", as the real one does;
//   - os.rename moves a file onto a name that is not a directory, and
//     os.remove removes a file or an empty directory;
//   - io.stderr:write collects what is written, for Stderr;
//   - luarocks.fs gets absolute_name and current_dir, which resolve against
//     cwd, on top of its stub.
//
// Files returns the files.
func (vm *VM) WritableFiles(cwd string) error {
	cwd = path.Clean(cwd)
	vm.cwd = cwd
	vm.dirs = map[string]bool{}

	for d := cwd; ; d = path.Dir(d) {
		vm.dirs[d] = true
		if d == "/" || d == "." {
			break
		}
	}

	files := map[string]string{}

	for name, content := range vm.files {
		full := vm.resolve(name)
		files[full] = content

		for d := path.Dir(full); !vm.dirs[d]; d = path.Dir(d) {
			vm.dirs[d] = true
		}
	}

	vm.files = files

	L := vm.L

	ioTbl, ok := L.GetGlobal("io").(*lua.LTable)
	if !ok {
		return errors.New("luaref: io table missing")
	}

	osTbl, ok := L.GetGlobal("os").(*lua.LTable)
	if !ok {
		return errors.New("luaref: os table missing")
	}

	L.SetField(ioTbl, "open", L.NewFunction(vm.memOpen))
	L.SetField(osTbl, "rename", L.NewFunction(vm.memRename))
	L.SetField(osTbl, "remove", L.NewFunction(vm.memRemove))

	stderr := L.NewTable()
	stderr.RawSetString("write", L.NewFunction(func(L *lua.LState) int {
		for i := 2; i <= L.GetTop(); i++ {
			vm.fileStderr.WriteString(L.CheckString(i))
		}

		L.Push(L.Get(1))

		return 1
	}))
	L.SetField(ioTbl, "stderr", stderr)

	fs, err := vm.Require("luarocks.fs")
	if err != nil {
		return err
	}

	fs.RawSetString("absolute_name", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(vm.resolve(L.CheckString(1))))

		return 1
	}))
	fs.RawSetString("current_dir", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(vm.cwd))

		return 1
	}))

	return nil
}

// Files returns a copy of the files of the writable file system, keyed by
// their absolute names.
func (vm *VM) Files() map[string]string {
	return maps.Clone(vm.files)
}

// Stderr returns what was written to io.stderr since WritableFiles.
func (vm *VM) Stderr() string {
	return vm.fileStderr.String()
}

// resolve is name, relative to the current dir or absolute, cleaned.
func (vm *VM) resolve(name string) string {
	if !strings.HasPrefix(name, "/") {
		name = vm.cwd + "/" + name
	}

	return path.Clean(name)
}

// pushIOError pushes the nil, message, errno triple of a failed io call.
func pushIOError(ls *lua.LState, name, msg string, errno int) int {
	ls.Push(lua.LNil)
	ls.Push(lua.LString(name + ": " + msg))
	ls.Push(lua.LNumber(errno))

	return 3 //nolint:mnd // nil, message, errno.
}

// The errnos the writable file system reports.
const (
	eisdir    = 21
	enotempty = 39
)

// memOpen is io.open over the writable file system.
func (vm *VM) memOpen(ls *lua.LState) int {
	name := ls.CheckString(1)
	mode := ls.OptString(secondArg, "r")
	full := vm.resolve(name)

	switch strings.ReplaceAll(mode, "b", "") {
	case "r":
		if vm.dirs[full] {
			return vm.pushHandle(ls, full, false, true)
		}

		if _, ok := vm.files[full]; !ok {
			return pushIOError(ls, name, "No such file or directory", enoent)
		}

		return vm.pushHandle(ls, full, false, false)
	case "w", "w+":
		if vm.dirs[full] {
			return pushIOError(ls, name, "Is a directory", eisdir)
		}

		if !vm.dirs[path.Dir(full)] {
			return pushIOError(ls, name, "No such file or directory", enoent)
		}

		vm.files[full] = ""

		return vm.pushHandle(ls, full, true, false)
	default:
		ls.RaiseError("luaref: io.open mode %q is not available", mode)

		return 0
	}
}

// pushHandle pushes a file handle on full. A handle on a directory reads
// nothing.
func (vm *VM) pushHandle(ls *lua.LState, full string, write, dir bool) int {
	pos := 0
	fd := ls.NewTable()

	fd.RawSetString("read", ls.NewFunction(func(ls *lua.LState) int {
		if dir || write {
			ls.Push(lua.LNil)
			ls.Push(lua.LString("Is a directory"))

			return 2 //nolint:mnd // nil, message.
		}

		content := vm.files[full]
		rest := content[min(pos, len(content)):]

		switch f := ls.Get(secondArg).(type) {
		case lua.LNumber:
			if rest == "" {
				ls.Push(lua.LNil)

				return 1
			}

			n := min(int(f), len(rest))
			pos += n

			ls.Push(lua.LString(rest[:n]))
		default:
			if lua.LVAsString(f) == "*l" || lua.LVAsString(f) == "l" {
				if rest == "" {
					ls.Push(lua.LNil)

					return 1
				}

				line, _, _ := strings.Cut(rest, "\n")
				pos += min(len(line)+1, len(rest))

				ls.Push(lua.LString(line))

				return 1
			}

			pos = len(content)

			ls.Push(lua.LString(rest))
		}

		return 1
	}))
	fd.RawSetString("write", ls.NewFunction(func(ls *lua.LState) int {
		if !write {
			ls.Push(lua.LNil)
			ls.Push(lua.LString("Bad file descriptor"))

			return 2 //nolint:mnd // nil, message.
		}

		for i := 2; i <= ls.GetTop(); i++ {
			vm.files[full] += ls.CheckString(i)
		}

		ls.Push(ls.Get(1))

		return 1
	}))
	fd.RawSetString("close", ls.NewFunction(func(ls *lua.LState) int {
		ls.Push(lua.LTrue)

		return 1
	}))
	ls.Push(fd)

	return 1
}

// memRename is os.rename over the writable file system, for a file.
func (vm *VM) memRename(ls *lua.LState) int {
	from, to := vm.resolve(ls.CheckString(1)), vm.resolve(ls.CheckString(secondArg))

	content, ok := vm.files[from]

	switch {
	case !ok:
		return pushIOError(ls, ls.CheckString(1), "No such file or directory", enoent)
	case vm.dirs[to]:
		return pushIOError(ls, ls.CheckString(secondArg), "Is a directory", eisdir)
	case !vm.dirs[path.Dir(to)]:
		return pushIOError(ls, ls.CheckString(secondArg), "No such file or directory", enoent)
	}

	delete(vm.files, from)
	vm.files[to] = content

	ls.Push(lua.LTrue)

	return 1
}

// memRemove is os.remove over the writable file system.
func (vm *VM) memRemove(ls *lua.LState) int {
	name := ls.CheckString(1)
	full := vm.resolve(name)

	if _, ok := vm.files[full]; ok {
		delete(vm.files, full)
		ls.Push(lua.LTrue)

		return 1
	}

	if !vm.dirs[full] {
		return pushIOError(ls, name, "No such file or directory", enoent)
	}

	for p := range vm.files {
		if strings.HasPrefix(p, full+"/") {
			return pushIOError(ls, name, "Directory not empty", enotempty)
		}
	}

	for d := range vm.dirs {
		if strings.HasPrefix(d, full+"/") {
			return pushIOError(ls, name, "Directory not empty", enotempty)
		}
	}

	delete(vm.dirs, full)
	ls.Push(lua.LTrue)

	return 1
}
