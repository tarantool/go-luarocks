package luaref

import (
	"errors"
	"fmt"

	lua "github.com/yuin/gopher-lua"
)

// Version is an upstream version table (luarocks.core.vers) in Go form.
type Version struct {
	// String is the version's string field: the input without its leading
	// whitespace.
	String string
	// Components is the array part, in order.
	Components []float64
	// Revision is the revision field; HasRevision tells whether it is set.
	Revision    float64
	HasRevision bool
	// Warned reports that parse_version warned it could not parse the input.
	// It still returns a version: the parsed prefix with a 0 appended.
	Warned bool
}

// Constraint is one entry of a constraints list (luarocks.queries).
type Constraint struct {
	Op        string
	Version   Version
	NoUpgrade bool
}

// Query is the result of queries.from_dep_string.
type Query struct {
	Name        string
	Namespace   string
	Constraints []Constraint
}

// resetVersionCache empties parse_version's memo, so a repeated input warns
// again and the result does not depend on what the VM parsed before.
func (vm *VM) resetVersionCache() error {
	fn, err := vm.Function("luarocks.core.vers", "parse_version")
	if err != nil {
		return err
	}

	cache, err := Upvalue(fn, "version_cache")
	if err != nil {
		return err
	}

	t, ok := cache.(*lua.LTable)
	if !ok {
		return errors.New("luaref: version_cache is not a table")
	}

	var keys []lua.LValue

	t.ForEach(func(k, _ lua.LValue) { keys = append(keys, k) })

	for _, k := range keys {
		t.RawSet(k, lua.LNil)
	}

	return nil
}

// ParseVersion runs vers.parse_version(s).
func (vm *VM) ParseVersion(s string) (Version, error) {
	vm.ResetWarnings()

	if err := vm.resetVersionCache(); err != nil {
		return Version{}, err
	}

	res, err := vm.Call("luarocks.core.vers", "parse_version", lua.LString(s))
	if err != nil {
		return Version{}, err
	}

	v, err := VersionFromLua(res[0])
	if err != nil {
		return Version{}, err
	}

	v.Warned = len(vm.Warnings()) > 0

	return v, nil
}

// VersionFromLua converts an upstream version table.
func VersionFromLua(lv lua.LValue) (Version, error) {
	t, ok := lv.(*lua.LTable)
	if !ok {
		return Version{}, fmt.Errorf("luaref: version is a %s, not a table", lv.Type())
	}

	var v Version

	if s, ok := t.RawGetString("string").(lua.LString); ok {
		v.String = string(s)
	}

	if r, ok := t.RawGetString("revision").(lua.LNumber); ok {
		v.Revision = float64(r)
		v.HasRevision = true
	}

	for i := 1; i <= t.Len(); i++ {
		n, ok := t.RawGetInt(i).(lua.LNumber)
		if !ok {
			return Version{}, fmt.Errorf("luaref: version component %d is a %s", i, t.RawGetInt(i).Type())
		}

		v.Components = append(v.Components, float64(n))
	}

	return v, nil
}

// VersionRelations is how two parsed versions relate under the version
// metatable and vers.compare_versions.
type VersionRelations struct {
	// Less is a < b (__lt), Equal is a == b (__eq), LessEq is a <= b (__le).
	Less, Equal, LessEq bool
	// Greater is vers.compare_versions(a, b), which is a > b on the strings.
	Greater bool
}

// CompareVersions parses a and b and evaluates every relation between them
// with the upstream operators.
func (vm *VM) CompareVersions(a, b string) (VersionRelations, error) {
	if err := vm.resetVersionCache(); err != nil {
		return VersionRelations{}, err
	}

	fn, err := vm.compareChunk()
	if err != nil {
		return VersionRelations{}, err
	}

	res, err := vm.CallValue(fn, lua.LString(a), lua.LString(b))
	if err != nil {
		return VersionRelations{}, err
	}

	if len(res) != 4 { //nolint:mnd // The chunk returns four booleans.
		return VersionRelations{}, fmt.Errorf("luaref: compare returned %d values", len(res))
	}

	return VersionRelations{
		Less:    lua.LVAsBool(res[0]),
		Equal:   lua.LVAsBool(res[1]),
		LessEq:  lua.LVAsBool(res[2]),
		Greater: lua.LVAsBool(res[3]),
	}, nil
}

// compareSrc evaluates the relations with the operators themselves, so the
// metatable dispatch of the VM is what decides, as it does upstream.
const compareSrc = `
local vers = require("luarocks.core.vers")
return function(a, b)
   local va, vb = vers.parse_version(a), vers.parse_version(b)
   return va < vb, va == vb, va <= vb, vers.compare_versions(a, b)
end`

func (vm *VM) compareChunk() (*lua.LFunction, error) {
	return vm.chunk("compare", compareSrc)
}

// chunk compiles a fixed helper source once per VM and returns the function
// it evaluates to.
func (vm *VM) chunk(name, src string) (*lua.LFunction, error) {
	key := "luaref." + name

	if fn, ok := vm.L.GetField(vm.L.Get(lua.RegistryIndex), key).(*lua.LFunction); ok {
		return fn, nil
	}

	loader, err := vm.L.LoadString(src)
	if err != nil {
		return nil, fmt.Errorf("luaref: compile %s helper: %w", name, err)
	}

	res, err := vm.CallValue(loader)
	if err != nil {
		return nil, err
	}

	fn, ok := res[0].(*lua.LFunction)
	if !ok {
		return nil, fmt.Errorf("luaref: %s helper is not a function", name)
	}

	vm.L.SetField(vm.L.Get(lua.RegistryIndex), key, fn)

	return fn, nil
}

// parseConstraintsFn returns the parse_constraints local of luarocks.queries,
// an upvalue of queries.from_dep_string.
//
//nolint:ireturn // An upvalue is a Lua value; it is only passed back to Lua.
func (vm *VM) parseConstraintsFn() (lua.LValue, error) {
	fromDep, err := vm.Function("luarocks.queries", "from_dep_string")
	if err != nil {
		return nil, err
	}

	return Upvalue(fromDep, "parse_constraints")
}

// ParseConstraints runs the parse_constraints function that
// queries.from_dep_string uses (a local of queries.lua). ok is false when
// upstream rejects the input; msg is its error message then.
func (vm *VM) ParseConstraints(s string) (cs []Constraint, ok bool, msg string, err error) {
	if err := vm.resetVersionCache(); err != nil {
		return nil, false, "", err
	}

	pc, err := vm.parseConstraintsFn()
	if err != nil {
		return nil, false, "", err
	}

	res, err := vm.CallValue(pc, lua.LString(s))
	if err != nil {
		return nil, false, "", err
	}

	if res[0] == lua.LNil {
		return nil, false, errMsg(res), nil
	}

	cs, err = constraintsFromLua(res[0])

	return cs, err == nil, "", err
}

// FromDepString runs queries.from_dep_string(s). ok is false when upstream
// rejects the input; msg is its error message then.
func (vm *VM) FromDepString(s string) (q Query, ok bool, msg string, err error) {
	if err := vm.resetVersionCache(); err != nil {
		return Query{}, false, "", err
	}

	res, err := vm.Call("luarocks.queries", "from_dep_string", lua.LString(s))
	if err != nil {
		return Query{}, false, "", err
	}

	t, isTable := res[0].(*lua.LTable)
	if !isTable {
		return Query{}, false, errMsg(res), nil
	}

	q, err = QueryFromLua(t)

	return q, err == nil, "", err
}

// QueryFromLua converts an upstream query table.
func QueryFromLua(t *lua.LTable) (Query, error) {
	var q Query

	if s, ok := t.RawGetString("name").(lua.LString); ok {
		q.Name = string(s)
	}

	if s, ok := t.RawGetString("namespace").(lua.LString); ok {
		q.Namespace = string(s)
	}

	cs, err := constraintsFromLua(t.RawGetString("constraints"))
	if err != nil {
		return Query{}, err
	}

	q.Constraints = cs

	return q, nil
}

func constraintsFromLua(lv lua.LValue) ([]Constraint, error) {
	t, ok := lv.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("luaref: constraints are a %s, not a table", lv.Type())
	}

	cs := []Constraint{}

	for i := 1; i <= t.Len(); i++ {
		ct, ok := t.RawGetInt(i).(*lua.LTable)
		if !ok {
			return nil, fmt.Errorf("luaref: constraint %d is not a table", i)
		}

		v, err := VersionFromLua(ct.RawGetString("version"))
		if err != nil {
			return nil, err
		}

		c := Constraint{Version: v, NoUpgrade: lua.LVAsBool(ct.RawGetString("no_upgrade"))}
		if op, ok := ct.RawGetString("op").(lua.LString); ok {
			c.Op = string(op)
		}

		cs = append(cs, c)
	}

	return cs, nil
}

// matchSrc runs vers.match_constraints on a version and a constraint list it
// parses with the given parse_constraints. It returns nil plus a message when
// the constraints do not parse.
const matchSrc = `
local vers = require("luarocks.core.vers")
return function(parse_constraints, version, constraints)
   local cs, err = parse_constraints(constraints)
   if not cs then return nil, err end
   return vers.match_constraints(vers.parse_version(version), cs)
end`

// MatchConstraints runs vers.match_constraints(vers.parse_version(version),
// parse_constraints(constraints)). ok is false when the constraints do not
// parse.
func (vm *VM) MatchConstraints(version, constraints string) (match, ok bool, err error) {
	vm.ResetWarnings()

	if err := vm.resetVersionCache(); err != nil {
		return false, false, err
	}

	pc, err := vm.parseConstraintsFn()
	if err != nil {
		return false, false, err
	}

	fn, err := vm.chunk("match", matchSrc)
	if err != nil {
		return false, false, err
	}

	res, err := vm.CallValue(fn, pc, lua.LString(version), lua.LString(constraints))
	if err != nil {
		return false, false, err
	}

	if res[0] == lua.LNil {
		return false, false, nil
	}

	return lua.LVAsBool(res[0]), true, nil
}

// errMsg returns the message that follows a nil result, if any.
func errMsg(res []lua.LValue) string {
	if len(res) > 1 {
		return lua.LVAsString(res[1])
	}

	return ""
}
