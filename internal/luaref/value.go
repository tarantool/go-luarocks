package luaref

import (
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// maxDepth bounds the nesting FromLua and ToLua follow, so a cyclic value
// fails instead of recursing forever. It is well above the 100 nested tables
// manif.Parse accepts, so the reference can judge every text Parse reads.
const maxDepth = 256

// FromLua converts a Lua data value into plain Go: nil, bool, float64 (every
// Lua number), string, or map[any]any for a table, whose keys are bool,
// float64 or string. Functions, userdata, threads and table keys are errors:
// none of them is data.
func FromLua(lv lua.LValue) (any, error) {
	return fromLua(lv, 0)
}

func fromLua(lv lua.LValue, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("luaref: value nested deeper than %d", maxDepth)
	}

	switch v := lv.(type) {
	case *lua.LNilType:
		return nil, nil //nolint:nilnil // Lua nil is the Go nil value, not a failure.
	case lua.LBool:
		return bool(v), nil
	case lua.LNumber:
		return float64(v), nil
	case lua.LString:
		return string(v), nil
	case *lua.LTable:
		out := map[any]any{}

		var ferr error

		v.ForEach(func(k, val lua.LValue) {
			if ferr != nil {
				return
			}

			switch k.(type) {
			case lua.LBool, lua.LNumber, lua.LString:
			default:
				ferr = fmt.Errorf("luaref: table key of type %s", k.Type())

				return
			}

			gk, err := fromLua(k, depth+1)
			if err != nil {
				ferr = err

				return
			}

			gv, err := fromLua(val, depth+1)
			if err != nil {
				ferr = err

				return
			}

			out[gk] = gv
		})

		if ferr != nil {
			return nil, ferr
		}

		return out, nil
	default:
		return nil, fmt.Errorf("luaref: %s is not data", lv.Type())
	}
}

// ToLua converts plain Go data into a Lua value of this VM: nil, bool,
// string, int, int64, float64, []any (an array from index 1),
// map[string]any, and map[any]any with bool, string, int, int64 or float64
// keys.
//
//nolint:ireturn // A Lua value is an interface; the caller hands it back to Lua.
func (vm *VM) ToLua(v any) (lua.LValue, error) {
	return vm.toLua(v, 0)
}

//nolint:ireturn // See ToLua.
func (vm *VM) toLua(v any, depth int) (lua.LValue, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("luaref: value nested deeper than %d", maxDepth)
	}

	switch x := v.(type) {
	case nil:
		return lua.LNil, nil
	case bool:
		return lua.LBool(x), nil
	case string:
		return lua.LString(x), nil
	case int:
		return lua.LNumber(x), nil
	case int64:
		return lua.LNumber(x), nil
	case float64:
		return lua.LNumber(x), nil
	case []any:
		t := vm.L.NewTable()

		for i, e := range x {
			lv, err := vm.toLua(e, depth+1)
			if err != nil {
				return nil, err
			}

			t.RawSetInt(i+1, lv)
		}

		return t, nil
	case map[string]any:
		t := vm.L.NewTable()

		for k, e := range x {
			lv, err := vm.toLua(e, depth+1)
			if err != nil {
				return nil, err
			}

			t.RawSetString(k, lv)
		}

		return t, nil
	case map[any]any:
		t := vm.L.NewTable()

		for k, e := range x {
			switch k.(type) {
			case bool, string, int, int64, float64:
			default:
				return nil, fmt.Errorf("luaref: unsupported table key type %T", k)
			}

			lk, err := vm.toLua(k, depth+1)
			if err != nil {
				return nil, err
			}

			lv, err := vm.toLua(e, depth+1)
			if err != nil {
				return nil, err
			}

			t.RawSet(lk, lv)
		}

		return t, nil
	default:
		return nil, fmt.Errorf("luaref: unsupported Go type %T", v)
	}
}

// LoadIntoTable runs persist.load_into_table on src, served as an in-memory
// file. ok is false when upstream fails to load or run it; msg is the error
// then.
func (vm *VM) LoadIntoTable(src string) (tbl *lua.LTable, ok bool, msg string, err error) {
	const name = "luaref-input"

	vm.AddFile(name, src)
	defer delete(vm.files, name)

	res, err := vm.Call("luarocks.persist", "load_into_table", lua.LString(name))
	if err != nil {
		return nil, false, "", err
	}

	t, isTable := res[0].(*lua.LTable)
	if !isTable {
		return nil, false, errMsg(res), nil
	}

	return t, true, "", nil
}

// SaveFromTableToString runs persist.save_from_table_to_string(tbl). ok is
// false when upstream refuses the table; msg is its error then.
func (vm *VM) SaveFromTableToString(tbl lua.LValue) (out string, ok bool, msg string, err error) {
	res, err := vm.Call("luarocks.persist", "save_from_table_to_string", tbl)
	if err != nil {
		return "", false, "", err
	}

	s, isString := res[0].(lua.LString)
	if !isString {
		return "", false, errMsg(res), nil
	}

	return string(s), true, "", nil
}
