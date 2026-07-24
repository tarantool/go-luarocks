package rockspec

import (
	"fmt"
	"os"
	"sort"
	"strings"

	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// Eval reads `path` as a `.rockspec` file, executes it inside a sandboxed
// gopher-lua VM, and harvests known globals into a *rocks.Rockspec.
//
// The returned spec contains Build.Platforms populated as-declared; the
// caller folds them into Build via MergePlatforms when running for a
// specific OS. Eval does NOT call MergePlatforms itself — keeping the two
// stages distinct lets the same parsed spec drive both the host build and
// e.g. `download` flows that want to inspect platform overlays.
//
// Returns ErrUnsupportedRockspecFeature if build.type is outside the
// {builtin, cmake, make, command, none} set.
func Eval(path string, _ rocks.RockspecConfig) (*rocks.Rockspec, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("rockspec: read %s: %w", path, err)
	}

	L, reads := newSandboxedState()
	defer L.Close()

	// Snapshot the (empty) global table so anything the chunk itself assigns can
	// be diffed against the rockspec schema afterwards.
	builtins := globalNames(L)

	if err := L.DoString(string(src)); err != nil {
		return nil, fmt.Errorf("rockspec: eval %s: %w", path, err)
	}

	// Reject undefined globals the chunk READ (library access, typo'd reads) —
	// the all-nil-env analogue of upstream check_undeclared_globals.
	if err := checkUndeclaredReads(reads); err != nil {
		return nil, fmt.Errorf("rockspec: %s: %w", path, err)
	}

	if err := checkUndeclaredGlobals(L, builtins); err != nil {
		return nil, fmt.Errorf("rockspec: %s: %w", path, err)
	}

	spec := &rocks.Rockspec{RawSource: src}
	if err := harvest(L, spec); err != nil {
		return nil, fmt.Errorf("rockspec: harvest %s: %w", path, err)
	}

	return spec, nil
}

// rockspecFields is the set of top-level globals a rockspec may assign,
// mirroring the declared schema. A global outside this set (and outside the
// sandbox builtins) is an Unknown variable, matching upstream
// check_undeclared_globals (cfg.accept_unknown_fields defaults to false).
var rockspecFields = map[string]bool{
	"rockspec_format":       true,
	"package":               true,
	"version":               true,
	"description":           true,
	"dependencies":          true,
	"build_dependencies":    true,
	"test_dependencies":     true,
	"external_dependencies": true,
	"supported_platforms":   true,
	"source":                true,
	"build":                 true,
	"hooks":                 true,
	"test":                  true,
	"deploy":                true,
}

// globalNames returns the set of string keys currently in the global table.
func globalNames(l *lua.LState) map[string]bool {
	out := map[string]bool{}

	l.G.Global.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok {
			out[string(s)] = true
		}
	})

	return out
}

// checkUndeclaredGlobals rejects any global the chunk assigned that is neither
// a sandbox builtin (present before the chunk ran) nor a recognized rockspec
// field — the Go analogue of upstream's all-nil-env global recording.
func checkUndeclaredGlobals(l *lua.LState, builtins map[string]bool) error {
	var unknown []string

	l.G.Global.ForEach(func(k, _ lua.LValue) {
		s, ok := k.(lua.LString)
		if !ok {
			return
		}

		name := string(s)
		if builtins[name] || rockspecFields[name] {
			return
		}

		unknown = append(unknown, name)
	})

	if len(unknown) > 0 {
		sort.Strings(unknown)

		return fmt.Errorf("Unknown variable: %s", unknown[0]) //nolint:staticcheck // mirrors upstream check_undeclared_globals message
	}

	return nil
}

// newSandboxedState builds a fresh LState reproducing upstream's rockspec
// loading environment (core/persist.lua run_file + load_into_table).
//
// Upstream runs the rockspec chunk with its _ENV set to a fresh table (empty +
// __index recorder), so every GLOBAL — string, os, table, math, require, io,
// setmetatable, … — reads as nil and is recorded, and a rockspec that accesses
// one fails to load. Crucially, though, the host Lua still has its standard
// libraries loaded, so the string/table type METATABLES are set: string-literal
// method calls like ("%s"):format(x) keep working even though the `string`
// GLOBAL is nil (they resolve through the metatable, not _ENV).
//
// We reproduce both halves: open the standard libraries (installs the
// metatables), then clear every global from the environment and install an
// __index recorder. Result: global library access is nil-and-recorded (as
// upstream), while string/table literal methods still work (as upstream).
func newSandboxedState() (*lua.LState, map[string]bool) {
	L := lua.NewState() // opens all standard libs → type metatables are set

	// Clear every global so the rockspec env is empty (the metatables set above
	// survive on the string/table TYPES, independent of these globals).
	var names []string

	L.G.Global.ForEach(func(k, _ lua.LValue) {
		if s, ok := k.(lua.LString); ok {
			names = append(names, string(s))
		}
	})

	for _, name := range names {
		L.SetGlobal(name, lua.LNil)
	}

	reads := map[string]bool{}
	mt := L.NewTable()
	// __index(table, key): the read key is the second argument on the Lua stack.
	const indexKeyArg = 2

	mt.RawSetString("__index", L.NewFunction(func(l *lua.LState) int {
		reads[l.CheckString(indexKeyArg)] = true
		l.Push(lua.LNil)

		return 1
	}))
	L.SetMetatable(L.G.Global, mt)

	return L, reads
}

// checkUndeclaredReads rejects any global the rockspec chunk READ that is not a
// recognized rockspec field, mirroring upstream check_undeclared_globals over
// the set recorded by the load environment's __index. This is what makes a
// rockspec calling string/os/etc. fail: those names are read (returning nil)
// and reported as Unknown variable.
func checkUndeclaredReads(reads map[string]bool) error {
	var unknown []string

	for name := range reads {
		if rockspecFields[name] {
			continue
		}

		unknown = append(unknown, name)
	}

	if len(unknown) > 0 {
		sort.Strings(unknown)

		return fmt.Errorf("Unknown variable: %s", unknown[0]) //nolint:staticcheck // mirrors upstream check_undeclared_globals message
	}

	return nil
}

// ---- harvesting ----

func harvest(l *lua.LState, spec *rocks.Rockspec) error {
	// A present-but-wrong-typed field is a hard error, mirroring upstream
	// type_check_item's Type mismatch (a silently-dropped field would change an
	// upstream reject into a Go accept). Scalars must be strings; the structured
	// fields must be tables.
	pkg, err := stringGlobal(l, "package")
	if err != nil {
		return err
	}
	// Upstream rockspecs.from_persisted_table computes rockspec.name =
	// rockspec.package:lower() and drives ALL identity (install dir, manifest
	// repository key, filename consistency) off that lowercased name. We fold
	// the two into a single lowercased Package so every downstream consumer
	// (tree.InstallDir, the manifest key) gets the upstream-compatible layout.
	spec.Package = strings.ToLower(pkg)

	if spec.RockspecFormat, err = stringGlobal(l, "rockspec_format"); err != nil {
		return err
	}

	if spec.Version, err = stringGlobal(l, "version"); err != nil {
		return err
	}

	if tbl, err := tableGlobal(l, "description"); err != nil {
		return err
	} else if tbl != nil {
		spec.Description = harvestDescription(tbl)
	}

	if tbl, err := tableGlobal(l, "source"); err != nil {
		return err
	} else if tbl != nil {
		spec.Source = harvestSource(tbl)
	}

	for _, dep := range []struct {
		name  string
		dst   *[]rocks.Dep
		plats *map[string][]rocks.Dep
	}{
		{"dependencies", &spec.Dependencies, &spec.DependenciesPlatforms},
		{"build_dependencies", &spec.BuildDependencies, &spec.BuildDependenciesPlatforms},
		{"test_dependencies", &spec.TestDependencies, &spec.TestDependenciesPlatforms},
	} {
		tbl, err := tableGlobal(l, dep.name)
		if err != nil {
			return err
		}

		if tbl == nil {
			continue
		}

		ds, err := harvestDeps(dep.name, tbl)
		if err != nil {
			return err
		}

		*dep.dst = ds

		plats, err := harvestDepPlatforms(dep.name, tbl)
		if err != nil {
			return err
		}

		if len(plats) > 0 {
			*dep.plats = plats
		}
	}

	if tbl, err := tableGlobal(l, "external_dependencies"); err != nil {
		return err
	} else if tbl != nil {
		spec.ExternalDependencies = harvestExternalDeps(tbl)
		spec.ExternalDependenciesPlatforms = harvestExternalDepPlatforms(tbl)
	}

	if tbl, err := tableGlobal(l, "supported_platforms"); err != nil {
		return err
	} else if tbl != nil {
		spec.SupportedPlatforms = harvestStringArray(tbl)
	}

	if tbl, err := tableGlobal(l, "build"); err != nil {
		return err
	} else if tbl != nil {
		b, err := harvestBuild(tbl)
		if err != nil {
			return err
		}

		spec.Build = b
		spec.HasBuild = true
	}

	if tbl, err := tableGlobal(l, "deploy"); err != nil {
		return err
	} else if tbl != nil {
		spec.Deploy = harvestDeploy(tbl)
	}

	return nil
}

// harvestDeploy reads the top-level `deploy = {...}` table. wrap_bin_scripts
// is tri-state: absent leaves the pointer nil (upstream default: wrap).
func harvestDeploy(tbl *lua.LTable) rocks.Deploy {
	var d rocks.Deploy

	if v, ok := tbl.RawGetString("wrap_bin_scripts").(lua.LBool); ok {
		b := bool(v)
		d.WrapBinScripts = &b
	}

	return d
}

func optString(v lua.LValue) string {
	if s, ok := v.(lua.LString); ok {
		return string(s)
	}

	return ""
}

// optBool returns a tri-state pointer for a Lua boolean field: nil when the
// field is absent (or not a boolean), else a pointer to its value. Mirrors
// Lua's nil→default semantics for build_pass / install_pass.
func optBool(v lua.LValue) *bool {
	b, ok := v.(lua.LBool)
	if !ok {
		return nil
	}

	out := bool(b)

	return &out
}

// stringGlobal reads a string-typed global. An absent global (LNil) yields "".
// A present global of the wrong Lua type is a Type mismatch error.
func stringGlobal(l *lua.LState, name string) (string, error) {
	v := l.GetGlobal(name)
	if v == lua.LNil {
		return "", nil
	}

	s, ok := v.(lua.LString)
	if !ok {
		return "", fmt.Errorf("rockspec: Type mismatch on field %s: expected a string, got %s", name, v.Type().String())
	}

	return string(s), nil
}

// tableGlobal reads a table-typed global. An absent global (LNil) yields nil.
// A present global of the wrong Lua type is a Type mismatch error.
func tableGlobal(l *lua.LState, name string) (*lua.LTable, error) {
	v := l.GetGlobal(name)
	if v == lua.LNil {
		return nil, nil //nolint:nilnil // absent optional global: nil table, no error
	}

	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil, fmt.Errorf("rockspec: Type mismatch on field %s: expected a table, got %s", name, v.Type().String())
	}

	return tbl, nil
}

func harvestDescription(tbl *lua.LTable) rocks.Description {
	return rocks.Description{
		Summary:    optString(tbl.RawGetString("summary")),
		Detailed:   optString(tbl.RawGetString("detailed")),
		License:    optString(tbl.RawGetString("license")),
		Homepage:   optString(tbl.RawGetString("homepage")),
		IssuesURL:  optString(tbl.RawGetString("issues_url")),
		Maintainer: optString(tbl.RawGetString("maintainer")),
		Labels:     stringArray(tbl.RawGetString("labels")),
	}
}

func harvestSource(tbl *lua.LTable) rocks.Source {
	s := rocks.Source{
		URL:    optString(tbl.RawGetString("url")),
		Tag:    optString(tbl.RawGetString("tag")),
		Branch: optString(tbl.RawGetString("branch")),
		MD5:    optString(tbl.RawGetString("md5")),
		File:   optString(tbl.RawGetString("file")),
		Dir:    optString(tbl.RawGetString("dir")),
		Module: optString(tbl.RawGetString("module")),
	}

	// Per-platform source overrides (source.platforms.<name> = { url = ... }).
	if plats, ok := tbl.RawGetString("platforms").(*lua.LTable); ok {
		out := map[string]rocks.Source{}

		plats.ForEach(func(k, v lua.LValue) {
			name, nok := k.(lua.LString)

			sub, sok := v.(*lua.LTable)
			if nok && sok {
				out[string(name)] = harvestSource(sub)
			}
		})

		if len(out) > 0 {
			s.Platforms = out
		}
	}

	return s
}

// harvestDeps walks the array part of a dependencies-table and parses each
// dependency string into Name + raw constraint operators.
//
// Constraint parsing here is intentionally minimal: it splits on commas and
// then on whitespace to extract op+version pairs, leaving Version.Components
// empty. The version-string parser in deps/version.go refines this further.
func harvestDeps(field string, tbl *lua.LTable) ([]rocks.Dep, error) {
	if tbl == nil {
		return nil, nil
	}

	var (
		deps []rocks.Dep
		ferr error
	)

	tbl.ForEach(func(k, v lua.LValue) {
		if ferr != nil {
			return
		}

		if _, ok := k.(lua.LNumber); !ok {
			return
		}

		// A non-string array entry is a Type mismatch (upstream
		// type_check_item); a string that fails to parse (empty name / leading
		// operator / bad constraint) is a Parse error (convert_dependencies).
		// Both abort the load rather than being silently dropped.
		s, ok := v.(lua.LString)
		if !ok {
			ferr = fmt.Errorf("rockspec: Type mismatch on field %s: expected a string, got %s", field, v.Type().String())

			return
		}

		d, ok := parseDepString(string(s))
		if !ok {
			ferr = fmt.Errorf("rockspec: Parse error processing dependency '%s'", string(s))

			return
		}

		deps = append(deps, d)
	})

	return deps, ferr
}

// harvestDepPlatforms reads the per-platform overlays from a dependency table's
// `platforms` sub-table (e.g. dependencies.platforms.unix = { "posix" }).
func harvestDepPlatforms(field string, tbl *lua.LTable) (map[string][]rocks.Dep, error) {
	plats, ok := tbl.RawGetString("platforms").(*lua.LTable)
	if !ok {
		return nil, nil //nolint:nilnil // no platforms overlay: nil map, no error
	}

	out := map[string][]rocks.Dep{}

	var ferr error

	plats.ForEach(func(k, v lua.LValue) {
		if ferr != nil {
			return
		}

		name, nok := k.(lua.LString)

		sub, sok := v.(*lua.LTable)
		if !nok || !sok {
			return
		}

		ds, err := harvestDeps(field, sub)
		if err != nil {
			ferr = err

			return
		}

		out[string(name)] = ds
	})

	return out, ferr
}

// harvestExternalDepPlatforms reads per-platform external_dependencies overlays.
func harvestExternalDepPlatforms(tbl *lua.LTable) map[string]map[string]rocks.ExternalDep {
	plats, ok := tbl.RawGetString("platforms").(*lua.LTable)
	if !ok {
		return nil
	}

	out := map[string]map[string]rocks.ExternalDep{}

	plats.ForEach(func(k, v lua.LValue) {
		name, nok := k.(lua.LString)

		sub, sok := v.(*lua.LTable)
		if nok && sok {
			out[string(name)] = harvestExternalDeps(sub)
		}
	})

	if len(out) == 0 {
		return nil
	}

	return out
}

// parseDepString returns the Dep for a string like "foo >= 1.0, < 2.0".
//
// On a malformed input (no leading identifier) it returns ok=false rather
// than fabricating a name — fail loud, but ForEach is best-effort over
// table data so the caller drops the entry. (Validation surfaces missing
// dependencies via Validate later.)
func parseDepString(s string) (rocks.Dep, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return rocks.Dep{}, false
	}

	// Split the name off at the first non-identifier character, matching
	// upstream from_dep_string's [a-zA-Z0-9._-] name class (queries.lua:158) —
	// NOT at whitespace. This lets a whitespace-free "penlight>=1.5.4" split
	// into name="penlight", rest=">=1.5.4" (glr-0m2).
	i := 0
	for i < len(s) && isDepNameChar(s[i]) {
		i++
	}

	name := s[:i]
	rest := strings.TrimSpace(s[i:])

	if name == "" || !isValidDepName(name) {
		// A leading operator like ">=" yields no name and is a parse error, not
		// a dependency named ">=".
		return rocks.Dep{}, false
	}

	// Split a namespaced identifier "user/rock" the way util.split_namespace
	// does (^([^/]+)/([^/]+)$): the namespace is the first half, the rock name
	// the second. A bare name keeps Namespace empty.
	namespace := ""

	if i := strings.IndexByte(name, '/'); i >= 0 {
		ns, rock := name[:i], name[i+1:]
		if ns != "" && rock != "" && !strings.Contains(rock, "/") {
			namespace = ns
			name = rock
		}
	}

	dep := rocks.Dep{Name: name, Namespace: namespace}

	if rest != "" {
		// Delegate to the shared deps parser, which consumes constraints
		// separated by commas, whitespace, or both (glr-9wa). An unrecognized
		// operator or malformed version fails the whole dependency, mirroring
		// queries.parse_constraint (glr-vkx).
		cs, err := deps.ParseConstraints(rest)
		if err != nil {
			return rocks.Dep{}, false
		}

		dep.Constraints = cs
	}

	return dep, true
}

// isDepNameChar reports whether b is a dependency-name character
// ([A-Za-z0-9._/-]), the class upstream from_dep_string splits the name on.
func isDepNameChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '.', b == '_', b == '-', b == '/':
		return true
	default:
		return false
	}
}

// isValidDepName reports whether s is a valid dependency name: only
// [A-Za-z0-9._/-] characters and at least one alphanumeric, matching upstream's
// from_dep_string name capture (queries.lua:158).
func isValidDepName(s string) bool {
	hasAlnum := false

	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			hasAlnum = true
		case r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}

	return hasAlnum
}

func harvestExternalDeps(tbl *lua.LTable) map[string]rocks.ExternalDep {
	if tbl == nil {
		return nil
	}

	out := map[string]rocks.ExternalDep{}

	tbl.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok || string(name) == "platforms" { // platforms is the overlay key, not an entry
			return
		}

		sub, ok := v.(*lua.LTable)
		if !ok {
			return
		}

		out[string(name)] = rocks.ExternalDep{
			Header:  optString(sub.RawGetString("header")),
			Library: optString(sub.RawGetString("library")),
		}
	})

	if len(out) == 0 {
		return nil
	}

	return out
}

func harvestStringArray(tbl *lua.LTable) []string {
	return stringArray(tbl)
}

func stringArray(v lua.LValue) []string {
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil
	}

	out := make([]string, 0, tbl.Len())
	for i := 1; i <= tbl.Len(); i++ {
		if s, ok := tbl.RawGetInt(i).(lua.LString); ok {
			out = append(out, string(s))
		}
	}

	return out
}

func stringMap(v lua.LValue) map[string]string {
	tbl, ok := v.(*lua.LTable)
	if !ok {
		return nil
	}

	out := map[string]string{}

	tbl.ForEach(func(k, vv lua.LValue) {
		ks, kok := k.(lua.LString)

		vs, vok := vv.(lua.LString)
		if !kok || !vok {
			return
		}

		out[string(ks)] = string(vs)
	})

	if len(out) == 0 {
		return nil
	}

	return out
}

func harvestBuild(tbl *lua.LTable) (rocks.Build, error) {
	b := rocks.Build{
		Type:             optString(tbl.RawGetString("type")),
		Makefile:         optString(tbl.RawGetString("makefile")),
		BuildTarget:      optString(tbl.RawGetString("build_target")),
		InstallTarget:    optString(tbl.RawGetString("install_target")),
		BuildCommand:     optString(tbl.RawGetString("build_command")),
		InstallCommand:   optString(tbl.RawGetString("install_command")),
		Variables:        stringMap(tbl.RawGetString("variables")),
		BuildVariables:   stringMap(tbl.RawGetString("build_variables")),
		InstallVariables: stringMap(tbl.RawGetString("install_variables")),
		CopyDirectories:  stringArray(tbl.RawGetString("copy_directories")),
		BuildPass:        optBool(tbl.RawGetString("build_pass")),
		InstallPass:      optBool(tbl.RawGetString("install_pass")),
	}

	if !allowedBuildTypes[b.Type] {
		return rocks.Build{}, fmt.Errorf("%w: build.type=%q",
			rocks.ErrUnsupportedRockspecFeature, b.Type)
	}

	if modsTbl, ok := tbl.RawGetString("modules").(*lua.LTable); ok {
		b.Modules = harvestModules(modsTbl)
	}

	if installTbl, ok := tbl.RawGetString("install").(*lua.LTable); ok {
		b.Install = harvestBuildInstall(installTbl)
	}

	if platsTbl, ok := tbl.RawGetString("platforms").(*lua.LTable); ok {
		plats, err := harvestPlatforms(platsTbl)
		if err != nil {
			return rocks.Build{}, err
		}

		if len(plats) > 0 {
			b.Platforms = plats
		}
	}

	return b, nil
}

// harvestPlatforms recurses into a build.platforms table, returning one
// rocks.Build per named platform overlay.
func harvestPlatforms(platsTbl *lua.LTable) (map[string]rocks.Build, error) {
	plats := map[string]rocks.Build{}

	var harvestErr error

	platsTbl.ForEach(func(k, v lua.LValue) {
		if harvestErr != nil {
			return
		}

		name, ok := k.(lua.LString)
		if !ok {
			return
		}

		sub, ok := v.(*lua.LTable)
		if !ok {
			return
		}

		pb, err := harvestBuild(sub)
		if err != nil {
			harvestErr = err

			return
		}

		plats[string(name)] = pb
	})

	if harvestErr != nil {
		return nil, harvestErr
	}

	return plats, nil
}

func harvestModules(tbl *lua.LTable) map[string]rocks.Module {
	out := map[string]rocks.Module{}

	tbl.ForEach(func(k, v lua.LValue) {
		name, ok := k.(lua.LString)
		if !ok {
			return
		}

		switch vv := v.(type) {
		case lua.LString:
			out[string(name)] = rocks.Module{Path: string(vv)}
		case *lua.LTable:
			m := rocks.Module{}
			// String shorthand for sources: `{"src/foo.c"}` (array form).
			if arr := stringArray(vv); len(arr) > 0 && vv.RawGetString("sources") == lua.LNil {
				m.Sources = arr
			}

			if s := stringArray(vv.RawGetString("sources")); len(s) > 0 {
				m.Sources = s
			}

			m.Incdirs = stringArray(vv.RawGetString("incdirs"))
			m.Libdirs = stringArray(vv.RawGetString("libdirs"))
			m.Libraries = stringArray(vv.RawGetString("libraries"))
			m.Defines = stringArray(vv.RawGetString("defines"))
			out[string(name)] = m
		}
	})

	if len(out) == 0 {
		return nil
	}

	return out
}

func harvestBuildInstall(tbl *lua.LTable) rocks.BuildInstall {
	return rocks.BuildInstall{
		Lua:  stringMap(tbl.RawGetString("lua")),
		Lib:  stringMap(tbl.RawGetString("lib")),
		Bin:  stringMap(tbl.RawGetString("bin")),
		Conf: stringMap(tbl.RawGetString("conf")),
	}
}
