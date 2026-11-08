package luaref

import (
	"errors"
	"strings"

	lua "github.com/yuin/gopher-lua"
)

// loadRockspecSrc loads a rockspec the way fetch.load_local_rockspec does for
// a file named "rockspec", the one name exempt from its filename checks:
// persist.load_into_table runs the file, and rockspecs.from_persisted_table
// checks the format, type-checks the table against the globals the file read
// and normalizes it.
const loadRockspecSrc = `
local persist = require("luarocks.persist")
local rockspecs = require("luarocks.rockspecs")
return function(filename)
   local tbl, globals = persist.load_into_table(filename)
   if not tbl then return nil, globals end
   return rockspecs.from_persisted_table(filename, tbl, globals)
end`

// LoadRockspec loads src as a rockspec file with upstream (see
// loadRockspecSrc) and returns the normalized rockspec table. ok is false
// when upstream rejects it; msg is the error then. A Lua error raised on the
// way (upstream asserts on some malformed fields) is a rejection too.
func (vm *VM) LoadRockspec(src string) (spec *lua.LTable, ok bool, msg string, err error) {
	const name = "rockspec"

	if err := vm.resetVersionCache(); err != nil {
		return nil, false, "", err
	}

	fn, err := vm.chunk("rockspec", loadRockspecSrc)
	if err != nil {
		return nil, false, "", err
	}

	vm.AddFile(name, src)
	defer delete(vm.files, name)

	res, err := vm.CallValue(fn, lua.LString(name))
	if err != nil {
		// A failing stub or the call deadline is a harness problem, not an
		// upstream verdict.
		var apiErr *lua.ApiError
		if errors.As(err, &apiErr) && apiErr.Type == lua.ApiErrorRun &&
			!strings.Contains(err.Error(), "luaref") && !strings.Contains(err.Error(), "context") {
			return nil, false, err.Error(), nil
		}

		return nil, false, "", err
	}

	t, isTable := res[0].(*lua.LTable)
	if !isTable {
		return nil, false, errMsg(res), nil
	}

	return t, true, "", nil
}
