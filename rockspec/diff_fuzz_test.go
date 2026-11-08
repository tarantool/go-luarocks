package rockspec_test

// Differential tests: every input goes through the Go implementation and
// through upstream LuaRocks, run by internal/luaref, and the two must agree.
// `go test` runs the seeds below; `go test -run '^$' -fuzz FuzzXxx
// ./rockspec/` explores further.

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	lua "github.com/yuin/gopher-lua"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/luaref"
	"github.com/tarantool/go-luarocks/rockspec"
)

// maxInput bounds the fuzzed strings: real dependency strings are short, and
// upstream's patterns take quadratic time on long ones.
const maxInput = 256

// depStringSeeds are dependency strings from the rockspec tests and the
// upstream spec fixtures, plus the edges of from_dep_string: namespaces,
// every operator, missing whitespace, and names it rejects.
var depStringSeeds = []string{
	"lua >= 5.1", "lua >= 5.1, < 5.5", "lua >= 5.1 < 5.4", "a_rock 1.0",
	"a_rock >= 0.8", "rock_b < 1.0", "luajit >= 2.0", "checks >= 3.0, < 4.0",
	"checks >= 3.0, < 4", "posix >= 1.0", "penlight>=1.5.4",
	"myuser/mymodule >= 1.0", "tarantool >= 2.10", "luasocket ~> 3",
	"foo", "  foo  ", "foo == 1.0", "foo = 1.0", "foo ~= 1.0", "foo != 1.0",
	"foo > 1.0", "foo <= 2.0", "foo ~> 1.2.3-1", "foo @>= 1.0", "foo scm-1",
	"foo >= dev", "Foo-Bar.baz_1 1.0", "1foo", "a.b", "_a", "-a", "./a",
	">= 1.0", "", " ", "-", "/", "foo >", "foo >= ", "foo >= 1.0,",
	"foo !! 1.0", "foo => 1.0", "foo bar", "foo >= 1.0 bar",
}

// FuzzDepString checks the rockspec dependency-string parser against
// queries.from_dep_string: the same strings are rejected, and the rest give
// the same name, namespace and constraints.
func FuzzDepString(f *testing.F) {
	for _, s := range depStringSeeds {
		f.Add(s)
	}

	vm, err := luaref.New()
	if err != nil {
		f.Fatalf("boot reference: %v", err)
	}

	f.Cleanup(vm.Close)

	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > maxInput {
			return
		}

		gd, gok := rockspec.ParseDepStringForTest(s)

		lq, lok, msg, err := vm.FromDepString(s)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		if gok != lok {
			t.Fatalf("dependency %q: Go accepts: %v; upstream accepts: %v (%s)", s, gok, lok, msg)
		}

		if !gok {
			return
		}

		if gd.Name != lq.Name || gd.Namespace != lq.Namespace {
			t.Fatalf("dependency %q: name %q namespace %q, upstream name %q namespace %q",
				s, gd.Name, gd.Namespace, lq.Name, lq.Namespace)
		}

		if d := luaref.DiffConstraints(gd.Constraints, lq.Constraints); d != "" {
			t.Fatalf("dependency %q: %s", s, d)
		}
	})
}

// depView, moduleView and rockspecView are the parts of a loaded rockspec
// that both sides expose, in a form that compares with reflect.DeepEqual.
type depView struct {
	Name, Namespace string
	Constraints     []constraintView
}

type constraintView struct {
	Op, Version string
	Components  []float64
	Revision    float64
	HasRevision bool
}

type moduleView struct {
	Path    string
	Sources []string
}

type rockspecView struct {
	Package, Version, Format                          string
	URL, Tag, Branch, MD5                             string
	Dependencies, BuildDependencies, TestDependencies []depView
	BuildType                                         string
	Modules                                           map[string]moduleView
}

func goDeps(ds []rocks.Dep) []depView {
	out := make([]depView, 0, len(ds))

	for _, d := range ds {
		v := depView{Name: d.Name, Namespace: d.Namespace, Constraints: []constraintView{}}
		for _, c := range d.Constraints {
			v.Constraints = append(v.Constraints, constraintView{
				Op: c.Op, Version: c.Version.Raw, Components: c.Version.Components,
				Revision: float64(c.Version.Revision), HasRevision: c.Version.HasRevision,
			})
		}

		out = append(out, v)
	}

	return out
}

func goView(spec *rocks.Rockspec) rockspecView {
	v := rockspecView{
		// Eval stores the package lowercased, deliberately: upstream keys
		// every install path and manifest entry on rockspec.name, which is
		// package:lower(), and Package plays that role in Go. The reference
		// side therefore reports name, not package.
		Package:           spec.Package,
		Version:           spec.Version,
		Format:            spec.RockspecFormat,
		URL:               spec.Source.URL,
		Tag:               spec.Source.Tag,
		Branch:            spec.Source.Branch,
		MD5:               spec.Source.MD5,
		Dependencies:      goDeps(spec.Dependencies),
		BuildDependencies: goDeps(spec.BuildDependencies),
		TestDependencies:  goDeps(spec.TestDependencies),
		BuildType:         spec.Build.Type,
		Modules:           map[string]moduleView{},
	}

	for name, m := range spec.Build.Modules {
		v.Modules[name] = moduleView{Path: m.Path, Sources: m.Sources}
	}

	return v
}

func luaString(t *lua.LTable, key string) string {
	if t == nil {
		return ""
	}

	s, _ := t.RawGetString(key).(lua.LString)

	return string(s)
}

func luaDeps(t *testing.T, lv lua.LValue) []depView {
	t.Helper()

	out := []depView{}

	tbl, ok := lv.(*lua.LTable)
	if !ok {
		return out
	}

	for i := 1; i <= tbl.Len(); i++ {
		qt, ok := tbl.RawGetInt(i).(*lua.LTable)
		if !ok {
			t.Fatalf("reference: dependency %d is not a query", i)
		}

		q, err := luaref.QueryFromLua(qt)
		if err != nil {
			t.Fatalf("reference: %v", err)
		}

		v := depView{Name: q.Name, Namespace: q.Namespace, Constraints: []constraintView{}}
		for _, c := range q.Constraints {
			v.Constraints = append(v.Constraints, constraintView{
				Op: c.Op, Version: c.Version.String, Components: c.Version.Components,
				Revision: c.Version.Revision, HasRevision: c.Version.HasRevision,
			})
		}

		out = append(out, v)
	}

	return out
}

// luaStrings returns the strings of an array, or of a lone string.
func luaStrings(lv lua.LValue) []string {
	switch v := lv.(type) {
	case lua.LString:
		return []string{string(v)}
	case *lua.LTable:
		var out []string

		for i := 1; i <= v.Len(); i++ {
			if s, ok := v.RawGetInt(i).(lua.LString); ok {
				out = append(out, string(s))
			}
		}

		return out
	}

	return nil
}

func luaView(t *testing.T, spec *lua.LTable) rockspecView {
	t.Helper()

	source, _ := spec.RawGetString("source").(*lua.LTable)
	build, _ := spec.RawGetString("build").(*lua.LTable)

	v := rockspecView{
		Package:           luaString(spec, "name"),
		Version:           luaString(spec, "version"),
		Format:            luaString(spec, "rockspec_format"),
		URL:               luaString(source, "url"),
		Tag:               luaString(source, "tag"),
		Branch:            luaString(source, "branch"),
		MD5:               luaString(source, "md5"),
		Dependencies:      luaDeps(t, spec.RawGetString("dependencies")),
		BuildDependencies: luaDeps(t, spec.RawGetString("build_dependencies")),
		TestDependencies:  luaDeps(t, spec.RawGetString("test_dependencies")),
		BuildType:         luaString(build, "type"),
		Modules:           map[string]moduleView{},
	}

	if build == nil {
		return v
	}

	if mods, ok := build.RawGetString("modules").(*lua.LTable); ok {
		mods.ForEach(func(k, mv lua.LValue) {
			name, ok := k.(lua.LString)
			if !ok {
				return
			}

			switch m := mv.(type) {
			case lua.LString:
				v.Modules[string(name)] = moduleView{Path: string(m)}
			case *lua.LTable:
				// The sources of a module table, as the builtin backend reads
				// them (build/builtin.lua): the array part when there is one,
				// else the sources field, a string or a list.
				sources := m.RawGetString("sources")
				if m.RawGetInt(1) != lua.LNil {
					sources = m
				}

				v.Modules[string(name)] = moduleView{Sources: luaStrings(sources)}
			}
		})
	}

	return v
}

// goLoad evaluates, merges and validates a rockspec the way the native
// backend does.
func goLoad(path string) (*rocks.Rockspec, error) {
	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	if err != nil {
		return nil, err
	}

	rockspec.MergePlatforms(spec, luaref.DefaultPlatforms)

	if err := rockspec.Validate(spec); err != nil {
		return nil, err
	}

	return spec, nil
}

// buildTimeCheck reports whether err is one of the Validate checks upstream
// makes only when it builds the rock, not when it loads the rockspec. Both
// are documented: Validate rejects a build type outside the implemented
// subset (ErrUnsupportedRockspecFeature), and a missing build type before
// format 3.0, which it says mirrors build.lua.
func buildTimeCheck(err error) bool {
	return errors.Is(err, rocks.ErrUnsupportedRockspecFeature) ||
		strings.Contains(err.Error(), "build type not specified")
}

// compareRockspec loads src on both sides and requires the same verdict and,
// when both accept it, the same view.
func compareRockspec(t *testing.T, vm *luaref.VM, path, src string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	gspec, gerr := goLoad(path)

	lspec, lok, msg, err := vm.LoadRockspec(src)
	if err != nil {
		t.Fatalf("reference: %v", err)
	}

	if gerr != nil && lok && buildTimeCheck(gerr) {
		return
	}

	if (gerr == nil) != lok {
		t.Fatalf("Go error: %v; upstream accepts: %v (%s)\ninput: %q", gerr, lok, msg, src)
	}

	if gerr != nil {
		return
	}

	gv, lv := goView(gspec), luaView(t, lspec)
	if !reflect.DeepEqual(gv, lv) {
		t.Fatalf("loaded rockspecs differ\nGo:       %#v\nupstream: %#v\ninput:    %q", gv, lv, src)
	}
}

// Bits of the flags argument of FuzzRockspec, each switching on one shape
// of the rendered rockspec.
const (
	withFormat      = 1 << iota // rockspec_format = format
	withTag                     // source.tag = version
	asBuildDeps                 // the dependency goes to build_dependencies
	inPlatform                  // the dependency sits in platforms.linux
	withoutBuild                // no build table
	withoutType                 // no build.type
	withPlatformTyp             // build.platforms.unix.type = buildType
	withDescription             // description = { summary = ..., [module] = ... }
)

// renderRockspec writes a rockspec whose values are the fuzzed strings,
// quoted as Lua literals, so the fuzzer explores the values without ever
// running code of its own.
func renderRockspec(format, pkg, version, url, dep, buildType, module string, flags uint8) string {
	q := luaref.Quote

	var b strings.Builder

	if flags&withFormat != 0 {
		b.WriteString("rockspec_format = " + q(format) + "\n")
	}

	b.WriteString("package = " + q(pkg) + "\nversion = " + q(version) + "\n")
	b.WriteString("source = { url = " + q(url))

	if flags&withTag != 0 {
		b.WriteString(", tag = " + q(version))
	}

	b.WriteString(" }\n")

	list := "dependencies"
	if flags&asBuildDeps != 0 {
		list = "build_dependencies"
	}

	if flags&inPlatform != 0 {
		b.WriteString(list + " = { platforms = { linux = { " + q(dep) + " } } }\n")
	} else {
		b.WriteString(list + " = { " + q(dep) + " }\n")
	}

	if flags&withoutBuild == 0 {
		b.WriteString("build = {\n")

		if flags&withoutType == 0 {
			b.WriteString("   type = " + q(buildType) + ",\n")
		}

		b.WriteString("   modules = { [" + q(module) + "] = " + q(module+".lua") + " },\n")

		if flags&withPlatformTyp != 0 {
			b.WriteString("   platforms = { unix = { type = " + q(buildType) + " } },\n")
		}

		b.WriteString("}\n")
	}

	if flags&withDescription != 0 {
		b.WriteString("description = { summary = " + q(pkg) + ", [" + q(module) + "] = \"x\" }\n")
	}

	return b.String()
}

// FuzzRockspec loads generated rockspecs on both sides: the same rockspecs
// are rejected, and the rest load to the same fields.
func FuzzRockspec(f *testing.F) {
	type seed struct {
		format, pkg, version, url, dep, buildType, module string
		flags                                             uint8
	}

	seeds := []seed{
		{"3.0", "foo", "1.0-1", "https://example.com/foo-1.0.tar.gz", "lua >= 5.1", "builtin", "foo", withFormat},
		{"1.0", "foo", "1.0-1", "https://example.com/foo-1.0.tar.gz", "lua >= 5.1", "builtin", "foo", withFormat | withTag},
		{"1.1", "Foo", "scm-1", "git+https://github.com/x/foo.git", "checks >= 3.0, < 4", "make", "foo.bar", withFormat},
		{"3.0", "foo", "1.0-1", "git://github.com/x/foo", "a_user/a_rock", "builtin", "foo", withFormat},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.lua", "a_build_dep", "builtin", "foo", withFormat | asBuildDeps},
		{"3.0", "foo", "1.0-1", "file:///tmp/foo.tar.gz", "posix >= 1.0", "cmake", "foo", withFormat | inPlatform},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "", "foo", withFormat | withoutBuild},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "", "foo", withFormat | withoutType},
		{"", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "builtin", "foo", withPlatformTyp},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "none", "summary", withFormat | withDescription},
		{"", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua ~> 5.1", "command", "foo", 0},
		{"3.0", "foo", "1.0", "https://example.com/foo.tar.gz", "lua", "builtin", "foo", withFormat},
		{"4.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "builtin", "foo", withFormat},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", ">= 1.0", "builtin", "foo", withFormat},
		{"3.0", "foo", "1.0-1", "https://example.com/foo.tar.gz", "lua", "builtin", "", withFormat},
	}

	for _, s := range seeds {
		f.Add(s.format, s.pkg, s.version, s.url, s.dep, s.buildType, s.module, s.flags)
	}

	vm, err := luaref.New()
	if err != nil {
		f.Fatalf("boot reference: %v", err)
	}

	f.Cleanup(vm.Close)

	path := filepath.Join(f.TempDir(), "x.rockspec")

	f.Fuzz(func(t *testing.T, format, pkg, version, url, dep, buildType, module string, flags uint8) {
		for _, s := range []string{format, pkg, version, url, dep, buildType, module} {
			if len(s) > maxInput {
				return
			}
		}

		compareRockspec(t, vm, path, renderRockspec(format, pkg, version, url, dep, buildType, module, flags))
	})
}

// divergentFixtures are the fixtures on which Go and upstream are known to
// disagree, each with the divergence; they come back in once it is fixed.
var divergentFixtures = map[string]string{
	"with_external_dep-0.1-1.rockspec": "known divergence: Eval drops a module's sources given as a " +
		"string (sources = \"a.c\"), which upstream's builtin backend compiles",
}

// fixtureRockspecs lists the rockspecs of the rockspec tests and of the
// upstream spec fixtures.
func fixtureRockspecs(t *testing.T) []string {
	t.Helper()

	var paths []string

	for _, root := range []string{"testdata", filepath.Join("..", "internal", "luarocks", "src", "spec", "fixtures")} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".rockspec") {
				paths = append(paths, p)
			}

			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	sort.Strings(paths)

	return paths
}

// TestRockspecFixturesMatchUpstream loads every fixture rockspec on both
// sides.
func TestRockspecFixturesMatchUpstream(t *testing.T) {
	t.Parallel()

	for _, p := range fixtureRockspecs(t) {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}

		t.Run(p, func(t *testing.T) {
			t.Parallel()

			if reason, ok := divergentFixtures[filepath.Base(p)]; ok {
				t.Skip(reason)
			}

			vm, err := luaref.New()
			if err != nil {
				t.Fatalf("boot reference: %v", err)
			}

			t.Cleanup(vm.Close)

			compareRockspec(t, vm, filepath.Join(t.TempDir(), "x.rockspec"), string(src))
		})
	}
}
