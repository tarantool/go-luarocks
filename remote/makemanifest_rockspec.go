package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// errUpstreamRaises marks an input upstream's code raises a Lua error on,
// which aborts the whole make_manifest command, rather than rejecting.
var errUpstreamRaises = errors.New("upstream raises a Lua error")

// raisef returns an error wrapping errUpstreamRaises.
func raisef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errUpstreamRaises}, args...)...)
}

// luaVersionFilter is filter_by_lua_version (manif/writer.lua:184-229), with
// the rockspec cache upstream shares between the Lua versions.
type luaVersionFilter struct {
	ctx       context.Context //nolint:containedctx // One MakeManifest call.
	dir       string
	platforms []string
	warn      func(string)
	ls        *lua.LState
	cache     map[string]*quickRockspec
}

// quickRockspec is what the filter keeps of a rockspec loaded as
// fetch.load_local_rockspec(path, true) loads it: the constraints of its
// first "lua" dependency, or a failure upstream reports and moves past.
type quickRockspec struct {
	lua     []rocks.VersionConstraint
	hasLua  bool
	loadErr string
}

func newLuaVersionFilter(ctx context.Context, dir string, platforms []string, warn func(string)) *luaVersionFilter {
	return &luaVersionFilter{
		ctx: ctx, dir: dir, platforms: platforms, warn: warn,
		cache: map[string]*quickRockspec{},
	}
}

func (f *luaVersionFilter) close() {
	if f.ls != nil {
		f.ls.Close()
	}
}

// apply returns idx without the versions whose rockspec rules out Lua
// version lv.
func (f *luaVersionFilter) apply(idx repoIndex, lv string) (repoIndex, error) {
	luaVer := luaParseVersion(lv)
	out := make(repoIndex, len(idx))

	for _, name := range slices.Sorted(maps.Keys(idx)) {
		versions := idx[name]
		kept := map[string][]string{}

		for _, version := range slices.Sorted(maps.Keys(versions)) {
			arches := versions[version]
			drop := false

			for _, a := range arches {
				if a != archRockspec {
					continue
				}

				spec, err := f.load(name, version)
				if err != nil {
					return nil, err
				}

				if spec.loadErr != "" {
					f.warnf("Error loading rockspec for %s %s: %s", name, version, spec.loadErr)

					continue
				}

				if spec.hasLua && !deps.Match(luaVer, spec.lua) {
					drop = true
				}
			}

			if !drop {
				kept[version] = arches
			}
		}

		if len(kept) > 0 {
			out[name] = kept
		}
	}

	return out, nil
}

func (f *luaVersionFilter) warnf(format string, args ...any) {
	if f.warn != nil {
		f.warn(fmt.Sprintf(format, args...))
	}
}

// load returns the rockspec of name and version, loading it on first use.
// Upstream caches only a rockspec that loaded; a failure loads the same way
// each time, so it is cached too.
func (f *luaVersionFilter) load(name, version string) (*quickRockspec, error) {
	base := name + "-" + version + ".rockspec"

	if spec, ok := f.cache[base]; ok {
		return spec, nil
	}

	path := filepath.Join(f.dir, base)

	src, err := readInside(f.dir, path)
	if err != nil {
		if errors.Is(err, rocks.ErrUnsafePath) {
			return nil, fmt.Errorf("rockspec %s: %w", path, err)
		}

		spec := &quickRockspec{loadErr: fmt.Sprintf("Could not load rockspec file %s (%v)", path, err)}
		f.cache[base] = spec

		return spec, nil
	}

	if f.ls == nil {
		f.ls = newRockspecState()
	}

	spec, err := loadQuickRockspec(f.ctx, f.ls, path, base, src, f.platforms)
	if err != nil {
		return nil, fmt.Errorf("rockspec %s: %w", path, err)
	}

	f.cache[base] = spec

	return spec, nil
}

// readInside reads the file at path, which must stay inside dir.
func readInside(dir, path string) ([]byte, error) {
	r, err := safepath.Open(dir, path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = r.Close() }()

	return io.ReadAll(r)
}

// newRockspecState returns the VM rockspecs run in: only the string library
// is opened, for the metatable that gives string values their methods; the
// globals a rockspec sees are its own environment table (runRockspec).
func newRockspecState() *lua.LState {
	ls := lua.NewState(lua.Options{SkipOpenLibs: true})

	ls.Push(ls.NewFunction(lua.OpenString))
	ls.Push(lua.LString(lua.StringLibName))
	ls.Call(1, 0)

	return ls
}

// shebangRe is the line persist.run_file strips first (core/persist.lua).
var shebangRe = regexp.MustCompile(`^#![^\n]*\n`)

// runRockspec mirrors persist.load_into_table (core/persist.lua): it runs src
// with a fresh table as its environment and returns that table, or the
// message of a failure to load or run it.
func runRockspec(ctx context.Context, ls *lua.LState, path string, src []byte) (*lua.LTable, string) {
	src = shebangRe.ReplaceAll(src, nil)

	fn, err := ls.Load(bytes.NewReader(src), path)
	if err != nil {
		return nil, "Error loading file: " + err.Error()
	}

	env := ls.NewTable()
	fn.Env = env

	ls.SetContext(ctx)
	defer ls.RemoveContext()

	ls.Push(fn)

	if err := ls.PCall(0, 0, nil); err != nil {
		return nil, "Error running file: " + err.Error()
	}

	return env, ""
}

// loadQuickRockspec mirrors fetch.load_local_rockspec(path, true)
// (fetch.lua:344-371) and the quick branch of
// rockspecs.from_persisted_table (rockspecs.lua:84-156), as far as the
// filter's outcome goes: whether the rockspec loads, and its "lua"
// dependency. A rejection is returned in loadErr; an input upstream raises
// on is an error wrapping errUpstreamRaises.
func loadQuickRockspec(
	ctx context.Context, ls *lua.LState, path, base string, src []byte, platforms []string,
) (*quickRockspec, error) {
	tbl, msg := runRockspec(ctx, ls, path, src)
	if tbl == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		return &quickRockspec{loadErr: fmt.Sprintf("Could not load rockspec file %s (%s)", path, msg)}, nil
	}

	reject := func(msg string) (*quickRockspec, error) {
		return &quickRockspec{loadErr: path + ": " + msg}, nil
	}

	if f := tbl.RawGetString("rockspec_format"); lua.LVAsBool(f) {
		s, ok := f.(lua.LString)
		if !ok {
			return nil, raisef("rockspec_format is a %s, not a string", f.Type())
		}

		if string(s) != supportedRockspecFormat &&
			deps.Compare(luaParseVersion(string(s)), luaParseVersion(supportedRockspecFormat)) > 0 {
			return reject("Rockspec format " + string(s) + " is not supported, please upgrade LuaRocks.")
		}
	}

	for _, field := range []string{
		"build", "dependencies", "build_dependencies", "test_dependencies",
		"external_dependencies", "source", "hooks", "test",
	} {
		if err := platformOverrides(ls, tbl.RawGetString(field), platforms); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
	}

	pkg, ok := tbl.RawGetString("package").(lua.LString)
	if !ok {
		return nil, raisef("package is a %s, not a string", tbl.RawGetString("package").Type())
	}

	if err := checkQuickSource(tbl.RawGetString("source")); err != nil {
		return nil, err
	}

	var spec quickRockspec

	for _, key := range []string{"dependencies", "build_dependencies", "test_dependencies"} {
		parsed, errMsg, err := convertDependencies(tbl.RawGetString(key))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}

		if errMsg != "" {
			return reject(errMsg)
		}

		if key != "dependencies" {
			continue
		}

		for _, d := range parsed {
			if d.name == "lua" {
				spec.lua, spec.hasLua = d.constraints, true

				break
			}
		}
	}

	version, err := luaConcatString(tbl.RawGetString("version"))
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}

	nameVersion := asciiLower(string(pkg)) + "-" + version
	if base != nameVersion+".rockspec" {
		return reject("Inconsistency between rockspec filename (" + base +
			") and its contents (" + nameVersion + ".rockspec).")
	}

	return &spec, nil
}

// supportedRockspecFormat is type_rockspec.rockspec_format
// (type/rockspec.lua:5), the newest format upstream loads.
const supportedRockspecFormat = "3.0"

// maxMergeDepth bounds util.deep_merge's recursion, which upstream ends with
// a stack overflow on a table that holds itself.
const maxMergeDepth = 200

// platformOverrides mirrors rockspecs.lua platform_overrides: it merges the
// overrides of each platform in turn into tbl and drops tbl.platforms.
func platformOverrides(ls *lua.LState, v lua.LValue, platforms []string) error {
	if !lua.LVAsBool(v) {
		return nil
	}

	tbl, ok := v.(*lua.LTable)
	if !ok {
		return raisef("assertion failed: a %s, not a table", v.Type())
	}

	plats := tbl.RawGetString("platforms")
	if lua.LVAsBool(plats) {
		for _, p := range platforms {
			over, err := luaIndex(plats, lua.LString(p))
			if err != nil {
				return fmt.Errorf("platforms: %w", err)
			}

			if !lua.LVAsBool(over) {
				continue
			}

			src, ok := over.(*lua.LTable)
			if !ok {
				return raisef("platforms.%s: bad argument #1 to 'pairs' (table expected, got %s)", p, over.Type())
			}

			if err := deepMerge(ls, tbl, src, 0); err != nil {
				return err
			}
		}
	}

	tbl.RawSetString("platforms", lua.LNil)

	return nil
}

// deepMerge mirrors util.deep_merge (core/util.lua:120-135).
func deepMerge(ls *lua.LState, dst, src *lua.LTable, depth int) error {
	if depth > maxMergeDepth {
		return raisef("stack overflow merging platform overrides")
	}

	type kv struct{ k, v lua.LValue }

	var pairs []kv

	src.ForEach(func(k, v lua.LValue) { pairs = append(pairs, kv{k, v}) })

	for _, p := range pairs {
		sub, isTable := p.v.(*lua.LTable)
		if !isTable {
			dst.RawSet(p.k, p.v)

			continue
		}

		if dst.RawGet(p.k) == lua.LNil {
			dst.RawSet(p.k, ls.NewTable())
		}

		if d, ok := dst.RawGet(p.k).(*lua.LTable); ok {
			if err := deepMerge(ls, d, sub, depth+1); err != nil {
				return err
			}
		} else {
			dst.RawSet(p.k, sub)
		}
	}

	return nil
}

// luaIndex mirrors v[k] for the values a rockspec holds: a table is indexed
// raw (rockspec code cannot set a metatable), a string through the string
// library, whose functions no key used here names, and anything else
// raises.
//
//nolint:ireturn // An index yields any Lua value.
func luaIndex(v, k lua.LValue) (lua.LValue, error) {
	switch t := v.(type) {
	case *lua.LTable:
		return t.RawGet(k), nil
	case lua.LString:
		return lua.LNil, nil
	default:
		return nil, raisef("attempt to index a %s value", v.Type())
	}
}

// checkQuickSource mirrors what from_persisted_table does with source
// (rockspecs.lua:123-145), as far as it can raise: source.url goes to
// dir.split_url and source.file, or the URL, to dir.deduce_base_dir, which
// both assert a string.
func checkQuickSource(v lua.LValue) error {
	if !lua.LVAsBool(v) {
		return raisef("attempt to index field 'source' (a %s value)", v.Type())
	}

	url, err := luaIndex(v, lua.LString("url"))
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	if _, ok := url.(lua.LString); !ok {
		return raisef("source.url is a %s, not a string", url.Type())
	}

	file, _ := luaIndex(v, lua.LString("file"))
	if lua.LVAsBool(file) {
		if _, ok := file.(lua.LString); !ok {
			return raisef("source.file is a %s, not a string", file.Type())
		}
	}

	return nil
}

// queryDep is a dependency as queries.from_dep_string parses it.
type queryDep struct {
	name        string
	constraints []rocks.VersionConstraint
}

// convertDependencies mirrors rockspecs.lua convert_dependencies for one
// dependency list. errMsg is the rejection upstream returns for a string it
// cannot parse.
func convertDependencies(v lua.LValue) (out []queryDep, errMsg string, err error) {
	if !lua.LVAsBool(v) {
		return nil, "", nil
	}

	var n int

	switch t := v.(type) {
	case *lua.LTable:
		n = t.Len()
	case lua.LString:
		n = len(t)
	default:
		return nil, "", raisef("attempt to get length of a %s value", v.Type())
	}

	for i := 1; i <= n; i++ {
		elem, err := luaIndex(v, lua.LNumber(i))
		if err != nil {
			return nil, "", err
		}

		s, ok := elem.(lua.LString)
		if !ok {
			return nil, "", raisef("dependency %d is a %s, not a string", i, elem.Type())
		}

		d, msg := fromDepString(string(s))
		if msg != "" {
			return nil, "Parse error processing dependency '" + string(s) + "': " + msg, nil
		}

		out = append(out, d)
	}

	return out, "", nil
}

// depStringRe is the pattern of queries.from_dep_string (queries.lua:158).
var depStringRe = regexp.MustCompile(
	`(?s)^[ \t\n\v\f\r]*([a-zA-Z0-9._-]*/?[a-zA-Z0-9][a-zA-Z0-9._-]*)[ \t\n\v\f\r]*([^/]*)`)

// namespaceRe is util.split_namespace's pattern (util.lua:350-356).
var namespaceRe = regexp.MustCompile(`^([^/]+)/([^/]+)$`)

// fromDepString mirrors queries.from_dep_string; msg is its error.
func fromDepString(s string) (queryDep, string) {
	m := depStringRe.FindStringSubmatch(s)
	if m == nil {
		return queryDep{}, "failed to extract dependency name from '" + s + "'"
	}

	cs, msg := luaParseConstraints(m[2])
	if msg != "" {
		return queryDep{}, msg
	}

	name := m[1]
	if ns := namespaceRe.FindStringSubmatch(name); ns != nil {
		name = ns[2]
	}

	return queryDep{name: name, constraints: cs}, ""
}

// constraintOps is the operator table of queries.lua parse_constraint.
var constraintOps = map[string]string{
	"==": "==", "~=": "~=", ">": ">", "<": "<", ">=": ">=", "<=": "<=", "~>": "~>",
	"": "==", "=": "==", "!=": "~=",
}

// constraintRe is parse_constraint's pattern (queries.lua:113).
var constraintRe = regexp.MustCompile(
	`(?s)^(@?)([<>=~!]*)[ \t\n\v\f\r]*([0-9A-Za-z._-]+)[ \t\n\v\f\r,]*(.*)`)

// luaParseConstraints mirrors queries.lua parse_constraints; msg is its
// error.
func luaParseConstraints(input string) ([]rocks.VersionConstraint, string) {
	orig := input

	out := []rocks.VersionConstraint{}

	for input != "" {
		m := constraintRe.FindStringSubmatch(input)
		if m == nil {
			return nil, "Failed to parse constraint '" + orig + "' with error: Encountered bad constraint operator: 'nil' in '" + input + "'"
		}

		op, ok := constraintOps[m[2]]
		if !ok {
			return nil, "Failed to parse constraint '" + orig + "' with error: Encountered bad constraint operator: '" + m[2] + "' in '" + input + "'"
		}

		out = append(out, rocks.VersionConstraint{Op: op, Version: luaParseVersion(m[3])})
		input = m[4]
	}

	return out, ""
}

// Version deltas of core/vers.lua.
//
//nolint:mnd // Upstream's table, copied as it is.
var versionDeltas = map[string]float64{
	"dev": 120000000, "scm": 110000000, "cvs": 100000000,
	"rc": -1000, "pre": -10000, "beta": -100000, "alpha": -1000000,
}

var (
	versionRevisionRe = regexp.MustCompile(`(?s)^(.*)-([0-9]+)$`)
	versionNumberRe   = regexp.MustCompile(`(?s)^([0-9]+)[._-]*(.*)`)
	versionWordRe     = regexp.MustCompile(`(?s)^([A-Za-z]+)[._-]*(.*)`)
)

// versionTokenDivisor is add_token's divisor for a number merged into a slot
// a word opened (core/vers.lua `number/100000`).
const versionTokenDivisor = 100000

// wordByteDivisor scales the first byte of an unknown word (`token:byte() /
// 1000`).
const wordByteDivisor = 1000

// luaParseVersion mirrors vers.parse_version (core/vers.lua:89-135), which
// never fails: an unparsable remainder fills its slot with 0. Its leading
// whitespace is dropped, its trailing kept, as the pattern
// "^%s*(.*)%s*$" does. deps.ParseVersion rejects what this accepts.
func luaParseVersion(s string) rocks.Version {
	v := rocks.Version{Raw: s}
	rest := strings.TrimLeft(s, luaSpaceChars)

	if m := versionRevisionRe.FindStringSubmatch(rest); m != nil {
		rest = m[1]
		v.HasRevision = true

		if n, err := strconv.Atoi(m[2]); err == nil {
			v.Revision = n
		} else {
			v.Revision = math.MaxInt
		}
	}

	i := 0

	for rest != "" {
		if m := versionNumberRe.FindStringSubmatch(rest); m != nil {
			n, _ := strconv.ParseFloat(m[1], 64)

			if i < len(v.Components) {
				v.Components[i] += n / versionTokenDivisor
			} else {
				v.Components = append(v.Components, n)
			}

			i++
			rest = m[2]

			continue
		}

		var slot float64

		m := versionWordRe.FindStringSubmatch(rest)
		if m != nil {
			if d, ok := versionDeltas[m[1]]; ok {
				slot = d
			} else {
				slot = float64(m[1][0]) / wordByteDivisor
			}
		}

		if i < len(v.Components) {
			v.Components[i] = slot
		} else {
			v.Components = append(v.Components, slot)
		}

		if m == nil {
			break
		}

		rest = m[2]
	}

	return v
}

// luaSpaceChars is Lua's %s class in the C locale.
const luaSpaceChars = " \t\n\v\f\r"

// luaConcatString returns v as the .. operator renders it: a string as it
// is, a number as LuaJIT's tostring does. Anything else raises.
func luaConcatString(v lua.LValue) (string, error) {
	switch t := v.(type) {
	case lua.LString:
		return string(t), nil
	case lua.LNumber:
		return luaNumberString(float64(t)), nil
	default:
		return "", raisef("attempt to concatenate a %s value", v.Type())
	}
}

// luaNumberPrecision is the precision of LuaJIT's number format, "%.14g".
const luaNumberPrecision = 14

// luaNumberString renders f as LuaJIT's tostring does.
func luaNumberString(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}

	return strconv.FormatFloat(f, 'g', luaNumberPrecision, 64)
}

// asciiLower mirrors string.lower in the C locale LuaJIT runs in: ASCII
// letters only, byte by byte.
func asciiLower(s string) string {
	b := []byte(s)

	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}

	return string(b)
}
