package rockspec_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

func tdata(name string) string {
	return filepath.Join("testdata", name)
}

func TestParseDepString_Namespaced(t *testing.T) {
	t.Parallel()

	// glr-eqd: "user/rock" splits into Name=rock, Namespace=user.
	d, ok := rockspec.ParseDepStringForTest("myuser/mymodule >= 1.0")
	require.True(t, ok)
	assert.Equal(t, "mymodule", d.Name)
	assert.Equal(t, "myuser", d.Namespace)
	require.Len(t, d.Constraints, 1)
	assert.Equal(t, ">=", d.Constraints[0].Op)

	// Bare name keeps Namespace empty.
	d, ok = rockspec.ParseDepStringForTest("checks")
	require.True(t, ok)
	assert.Equal(t, "checks", d.Name)
	assert.Empty(t, d.Namespace)

	// glr-0m2: a whitespace-free dependency splits at the identifier boundary.
	d, ok = rockspec.ParseDepStringForTest("penlight>=1.5.4")
	require.True(t, ok)
	assert.Equal(t, "penlight", d.Name)
	require.Len(t, d.Constraints, 1)
	assert.Equal(t, ">=", d.Constraints[0].Op)
	assert.Equal(t, "1.5.4", d.Constraints[0].Version.Raw)

	// glr-9wa: constraints separated by whitespace (no comma) parse as two.
	d, ok = rockspec.ParseDepStringForTest("lua >= 5.1 < 5.4")
	require.True(t, ok)
	require.Len(t, d.Constraints, 2)
	assert.Equal(t, ">=", d.Constraints[0].Op)
	assert.Equal(t, "<", d.Constraints[1].Op)
}

func TestEval_PackageNameLowercased(t *testing.T) {
	t.Parallel()

	// glr-9bs: upstream drives all identity off package:lower(), so a
	// mixed-case package name must be normalized to lowercase.
	dir := t.TempDir()
	path := filepath.Join(dir, "LuaFoo-1.0-1.rockspec")
	content := `package = "LuaFoo"
version = "1.0-1"
source = { url = "https://example.com/luafoo-1.0.tar.gz" }
build = { type = "builtin" }
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "luafoo", spec.Package, "Package should be lowercased")

	// Only ASCII letters are lowered, as Lua's string.lower does; other
	// bytes, UTF-8 or not, stay as they are.
	spec, err = rockspec.Eval(writeTempRockspec(t, "package = \"\xc3\x84Bc\xff\"\n"+`version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "builtin" }
`), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "\xc3\x84bc\xff", spec.Package)
}

func TestEval_DeployWrapBinScripts(t *testing.T) {
	t.Parallel()

	base := `package = "foo"
version = "1.0-1"
source = { url = "https://example.com/foo-1.0.tar.gz" }
build = { type = "builtin" }
`
	// Absent deploy table → tri-state pointer stays nil (default: wrap).
	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "foo-1.0-1.rockspec")
		require.NoError(t, os.WriteFile(path, []byte(base), 0o644))

		spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
		require.NoError(t, err)
		assert.Nil(t, spec.Deploy.WrapBinScripts, "absent deploy.wrap_bin_scripts should stay nil")
	})

	// Explicit false → non-nil pointer to false (opt out of wrapping).
	t.Run("false", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, "foo-1.0-1.rockspec")
		require.NoError(t, os.WriteFile(path, []byte(base+"deploy = { wrap_bin_scripts = false }\n"), 0o644))

		spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
		require.NoError(t, err)
		require.NotNil(t, spec.Deploy.WrapBinScripts, "explicit deploy.wrap_bin_scripts should be harvested")
		assert.False(t, *spec.Deploy.WrapBinScripts, "wrap_bin_scripts = false")
	})
}

func TestEval_PureLuaBuiltin(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("pure_lua_builtin.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "purelua", spec.Package, "Package")
	assert.Equal(t, "1.2-1", spec.Version, "Version")
	assert.Equal(t, rockspec.SupportedRockspecFormat, spec.RockspecFormat, "RockspecFormat")
	assert.Equal(t, "https://example.com/purelua-1.2.tar.gz", spec.Source.URL, "Source.URL")
	assert.Equal(t, "0123456789abcdef0123456789abcdef", spec.Source.MD5, "Source.MD5")
	assert.Equal(t, "purelua-1.2", spec.Source.Dir, "Source.Dir")
	assert.Equal(t, "Pure-Lua test rock", spec.Description.Summary, "Description.Summary")
	assert.Equal(t, "MIT", spec.Description.License, "Description.License")
	assert.Equal(t, "https://example.com/purelua/issues", spec.Description.IssuesURL, "Description.IssuesURL")
	assert.Equal(t, []string{"test", "pure-lua"}, spec.Description.Labels, "Description.Labels")
	require.Len(t, spec.Dependencies, 2, "Dependencies len")
	assert.Equal(t, "lua", spec.Dependencies[0].Name, "Dependencies[0].Name")
	assert.Equal(t, "checks", spec.Dependencies[1].Name, "Dependencies[1].Name")
	assert.GreaterOrEqual(t, len(spec.Dependencies[1].Constraints), 2, "Dependencies[1].Constraints len")
	assert.Equal(t, []string{"unix", "macosx"}, spec.SupportedPlatforms, "SupportedPlatforms")
	assert.Equal(t, "builtin", spec.Build.Type, "Build.Type")
	assert.Equal(t, "src/purelua.lua", spec.Build.Modules["purelua"].Path, "Build.Modules[purelua].Path")
	assert.Equal(t, "src/purelua/inner.lua", spec.Build.Modules["purelua.inner"].Path, "Build.Modules[purelua.inner].Path")
	assert.Equal(t, []string{"doc", "tests"}, spec.Build.CopyDirectories, "Build.CopyDirectories")
	assert.Equal(t, "extras/extras.lua", spec.Build.Install.Lua["purelua.extras"], "Build.Install.Lua[purelua.extras]")
	assert.Equal(t, "bin/purelua-cli", spec.Build.Install.Bin["purelua-cli"], "Build.Install.Bin[purelua-cli]")
}

func TestEval_CBuiltin(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("c_builtin.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "cbuiltin", spec.Package, "Package")
	mod := spec.Build.Modules["cbuiltin.core"]
	assert.Equal(t, []string{"src/core.c", "src/aux.c"}, mod.Sources, "Sources")
	assert.Equal(t, []string{"$(TARANTOOL_INCDIR)"}, mod.Incdirs, "Incdirs")
	assert.Equal(t, []string{"$(FOO_LIBDIR)"}, mod.Libdirs, "Libdirs")
	assert.Equal(t, []string{"foo"}, mod.Libraries, "Libraries")
	assert.Equal(t, []string{"CBUILTIN_DEBUG=1", "_GNU_SOURCE"}, mod.Defines, "Defines")
	assert.Equal(t, "tarantool/module.h", spec.ExternalDependencies["TARANTOOL"].Header, "ExternalDependencies[TARANTOOL].Header")
	assert.Equal(t, "foo", spec.ExternalDependencies["FOO"].Library, "ExternalDependencies[FOO].Library")
}

func TestEval_CMake(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("cmake.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "cmake", spec.Build.Type, "Build.Type")
	assert.Equal(t, "Release", spec.Build.Variables["CMAKE_BUILD_TYPE"], "Variables[CMAKE_BUILD_TYPE]")
	assert.Equal(t, "v2.0", spec.Source.Tag, "Source.Tag")
}

func TestEval_Make(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("make.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "make", spec.Build.Type, "Build.Type")
	assert.Equal(t, "all", spec.Build.BuildTarget, "BuildTarget")
	assert.Equal(t, "install", spec.Build.InstallTarget, "InstallTarget")
	assert.Equal(t, "$(CC)", spec.Build.BuildVariables["CC"], "BuildVariables[CC]")
	assert.Equal(t, "$(PREFIX)", spec.Build.InstallVariables["PREFIX"], "InstallVariables[PREFIX]")
}

func TestEval_Command(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("command.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "command", spec.Build.Type, "Build.Type")
	assert.True(t, strings.HasPrefix(spec.Build.BuildCommand, "./configure"), "BuildCommand = %q", spec.Build.BuildCommand)
	assert.Equal(t, "make install", spec.Build.InstallCommand, "InstallCommand")
}

func TestEval_PlatformsHarvested(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("platforms.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	require.Len(t, spec.Build.Platforms, 3, "Build.Platforms len")
	assert.Equal(t, "src/linux.lua", spec.Build.Platforms["linux"].Modules["platformer.linux"].Path, "linux module path")
	assert.Equal(t, "src/macosx.lua", spec.Build.Platforms["macosx"].Modules["platformer.macosx"].Path, "macosx module path")
	assert.Equal(t, "src/posix.lua", spec.Build.Platforms["unix"].Modules["platformer.posix"].Path, "unix module path")
	// Eval does NOT auto-merge platforms; only the top-level module is present.
	assert.NotContains(t, spec.Build.Modules, "platformer.linux", "Eval should not auto-merge platforms; linux module should not yet be folded in")
}

// TestEval_PatchesAndExtraFiles — build.patches and build.extra_files were
// not read at all, so the native backend built a rock that needs its patches
// from unpatched sources. Both are read, their platform overrides merged by
// name.
func TestEval_PatchesAndExtraFiles(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "file:///p.tar.gz" }
build = {
   type = "none",
   patches = { ["fix.diff"] = "--- a/x\n+++ b/x\n" },
   extra_files = { ["extra.lua"] = "return 1\n", ["other.lua"] = "return 2\n" },
   platforms = {
      linux = {
         patches = { ["linux.diff"] = "linux" },
         extra_files = { ["other.lua"] = "return 3\n" },
      },
   },
}
`), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"fix.diff": "--- a/x\n+++ b/x\n"}, spec.Build.Patches)
	assert.Equal(t, map[string]string{"extra.lua": "return 1\n", "other.lua": "return 2\n"}, spec.Build.ExtraFiles)

	rockspec.MergePlatforms(spec, []string{"unix", "linux"})
	assert.Equal(t, map[string]string{"fix.diff": "--- a/x\n+++ b/x\n", "linux.diff": "linux"}, spec.Build.Patches)
	assert.Equal(t, map[string]string{"extra.lua": "return 1\n", "other.lua": "return 3\n"}, spec.Build.ExtraFiles)
}

// glr-ak3: os.getenv is unreachable in the all-nil rockspec environment — a
// rockspec calling it fails to load, matching upstream (which loads rockspecs
// via persist.load_into_table with no host env access).
func TestEval_Getenv_Rejected(t *testing.T) {
	t.Parallel()

	_, err := rockspec.Eval(tdata("getenv.rockspec"), rocks.RockspecConfig{})
	require.Error(t, err, "os.getenv in a rockspec must fail to load")
}

func TestEval_FileNotFound(t *testing.T) {
	t.Parallel()

	_, err := rockspec.Eval(tdata("nope-does-not-exist.rockspec"), rocks.RockspecConfig{})
	require.Error(t, err, "expected error for missing file")
}

// Sandbox-escape tests.
type sandboxCase struct {
	name string
	src  string
}

func TestEval_Sandbox_BlocksForbiddenGlobals(t *testing.T) {
	t.Parallel()

	// Each case writes a minimal rockspec that tries to reach a forbidden API.
	// Either the eval errors, or the global is nil (so indexing fails). We
	// assert eval returns an error containing a recognisable substring.
	cases := []sandboxCase{
		{"io", `io.open("/etc/passwd", "r")`},
		{"package", `local _ = package.loaded`},
		{"require", `require("os")`},
		{"os.execute", `os.execute("echo hi")`},
		{"os.exit", `os.exit(0)`},
		{"os.remove", `os.remove("/tmp/x")`},
		{"os.rename", `os.rename("a", "b")`},
		{"loadfile", `loadfile("/etc/passwd")`},
		{"dofile", `dofile("/etc/passwd")`},
		{"debug", `local _ = debug.traceback`},
		{"load", `load("return 1")()`},
		{"loadstring", `loadstring("return 1")()`},
		{"setfenv", `setfenv(0, {})`},
		{"getfenv", `local _ = getfenv(0)`},
		{"rawset", `rawset(_G, "x", 1)`},
		{"rawget", `local _ = rawget(_G, "package")`},
		{"rawequal", `local _ = rawequal(1, 1)`},
		{"rawlen", `local _ = rawlen({1,2,3})`},
		{"setmetatable", `setmetatable({}, {})`},
		{"getmetatable", `local _ = getmetatable("")`},
		{"collectgarbage", `collectgarbage()`},
	}
	footer := `
package = "x"
version = "0.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "none" }
`

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// Probe BEFORE the header so the rockspec's own `package = "x"`
			// assignment can't shadow the package library we're testing.
			path := writeTempRockspec(t, c.src+"\n"+footer)
			_, err := rockspec.Eval(path, rocks.RockspecConfig{})
			require.Error(t, err, "expected error attempting %s, got nil", c.name)
		})
	}
}

// glr-ak3: standard libraries are NOT reachable from a rockspec (all-nil env).
// A rockspec calling string/table/math fails to load, matching upstream.
func TestEval_Sandbox_LibsUnreachable(t *testing.T) {
	t.Parallel()

	for _, src := range []string{
		`local _ = string.upper("ok")`,
		`local _ = table.concat({"a"}, "-")`,
		`local _ = math.floor(3.7)`,
		`local _ = os.time()`,
		`local _ = os.date("%Y")`,
		`local _ = tostring(1)`,
	} {
		path := writeTempRockspec(t, src+`
package = "x"
version = "0.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "none" }
`)
		_, err := rockspec.Eval(path, rocks.RockspecConfig{})
		require.Error(t, err, "library access %q must fail to load", src)
	}
}

// glr-ak3: string-LITERAL methods still work (they resolve through the string
// metatable, not the environment) — exactly as upstream, where the host stdlib
// is loaded even though rockspec globals are nil. This is why a real rock like
// `say`, whose rockspec uses ("%s"):format(...), still loads.
func TestEval_Sandbox_StringLiteralMethodsWork(t *testing.T) {
	t.Parallel()

	path := writeTempRockspec(t, `
package = "x"
version = "0.0-1"
source = { url = ("https://example.com/%s.tar.gz"):format("x-1.0") }
build = { type = "none" }
`)
	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, "https://example.com/x-1.0.tar.gz", spec.Source.URL, "string-literal :format must work")
}

func TestEval_UnsupportedBuildType(t *testing.T) {
	t.Parallel()

	path := writeTempRockspec(t, `
package = "x"
version = "0.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "rustup" }
`)
	_, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.Error(t, err, "expected ErrUnsupportedRockspecFeature")
	require.ErrorIs(t, err, rocks.ErrUnsupportedRockspecFeature)
}

func TestValidate_RequiresPackageVersionSourceURL(t *testing.T) {
	t.Parallel()

	require.Error(t, rockspec.Validate(&rocks.Rockspec{}), "expected error for empty rockspec")

	spec := &rocks.Rockspec{Package: "p"}
	require.Error(t, rockspec.Validate(spec), "expected error for missing version")
	spec.Version = "1.0"
	require.Error(t, rockspec.Validate(spec), "glr-pgb: bare 1.0 (no -N revision) must be rejected")
	spec.Version = "1.0-1"
	require.Error(t, rockspec.Validate(spec), "expected error for missing source.url")
	spec.Source.URL = "https://example.com/x.tar.gz"
	// A pre-3.0 rockspec (no rockspec_format) with no build table is rejected
	// upstream (glr-8ao); give it a concrete build type to isolate the
	// presence checks above.
	spec.Build.Type = "builtin"
	spec.HasBuild = true
	require.NoError(t, rockspec.Validate(spec))
}

func TestValidate_BuildTypeGatedByFormat(t *testing.T) {
	t.Parallel()

	base := func() *rocks.Rockspec {
		return &rocks.Rockspec{
			Package: "p", Version: "1.0-1",
			Source: rocks.Source{URL: "https://example.com/x.tar.gz"},
		}
	}

	// glr-8ao: pre-3.0 with no build table → "build table not specified".
	require.ErrorContains(t, rockspec.Validate(base()), "build table not specified")

	// pre-3.0 with a build table but no type → "build type not specified".
	s := base()
	s.HasBuild = true
	require.ErrorContains(t, rockspec.Validate(s), "build type not specified")

	// format >= 3.0 with no build table → defaults to builtin, accepted.
	s = base()
	s.RockspecFormat = rockspec.SupportedRockspecFormat
	require.NoError(t, rockspec.Validate(s))
}

func TestValidate_RejectsBadBuildType(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{
		Package: "p", Version: "1.0-1",
		Source: rocks.Source{URL: "https://example.com/x.tar.gz"},
		Build:  rocks.Build{Type: "wasm"},
	}
	err := rockspec.Validate(spec)
	require.ErrorIs(t, err, rocks.ErrUnsupportedRockspecFeature)
}

func TestEval_TypeMismatchAndUnknownGlobals(t *testing.T) {
	t.Parallel()

	base := `package = "foo"
version = "1.0-1"
source = { url = "https://example.com/foo-1.0.tar.gz" }
build = { type = "builtin" }
`

	cases := []struct {
		name    string
		extra   string
		wantErr string
	}{
		{ // glr-q32: bare-string dependencies (wrong Lua type for the field).
			name:    "dependencies wrong type",
			extra:   `dependencies = "luasocket"`,
			wantErr: "Type mismatch on field dependencies: expected a table",
		},
		{ // glr-q32: numeric element inside the dependencies array.
			name:    "dependency element wrong type",
			extra:   `dependencies = { 1234 }`,
			wantErr: "Type mismatch on field dependencies: expected a string, got number",
		},
		{ // glr-vkx: leading operator, no name; the schema pattern rejects it.
			name:    "dependency without name",
			extra:   `dependencies = { ">= 1.0" }`,
			wantErr: "invalid value '>= 1.0' does not match a valid dependency string",
		},
		{ // The schema pattern accepts it, from_dep_string does not.
			name:    "dependency parse error",
			extra:   `dependencies = { "foo !! 1.0" }`,
			wantErr: "Parse error processing dependency 'foo !! 1.0'",
		},
		{ // A namespace needs format 3.0.
			name:    "namespaced dependency before 3.0",
			extra:   `dependencies = { "user/bar" }`,
			wantErr: "invalid value 'user/bar' does not match a valid dependency string",
		},
		{ // glr-zvy: typo'd global.
			name:    "unknown global",
			extra:   `depemdencies = { "foo" }`,
			wantErr: "Unknown variable: depemdencies",
		},
		{ // glr-zvy: unknown scalar field.
			name:    "unknown field licence",
			extra:   `licence = "MIT"`,
			wantErr: "Unknown variable: licence",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, "foo-1.0-1.rockspec")
			require.NoError(t, os.WriteFile(path, []byte(base+c.extra+"\n"), 0o644))

			_, err := rockspec.Eval(path, rocks.RockspecConfig{})
			require.ErrorContains(t, err, c.wantErr)
		})
	}
}

func TestEval_WellFormedDepsAndKnownFieldsAccepted(t *testing.T) {
	t.Parallel()

	// A control: valid deps and every recognized top-level field load cleanly.
	dir := t.TempDir()
	path := filepath.Join(dir, "foo-3.0-1.rockspec")
	content := `rockspec_format = "3.0"
package = "foo"
version = "1.0-1"
description = { summary = "s", labels = { "x" }, issues_url = "u" }
source = { url = "https://example.com/foo-1.0.tar.gz" }
dependencies = { "lua >= 5.1", "checks >= 3.0, < 4.0" }
build_dependencies = { "cmake" }
test_dependencies = { "luatest" }
external_dependencies = { FOO = { library = "foo" } }
supported_platforms = { "unix" }
build = { type = "builtin" }
deploy = { wrap_bin_scripts = false }
`
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.NoError(t, err)
	require.Len(t, spec.Dependencies, 2)
	assert.Equal(t, "lua", spec.Dependencies[0].Name)
	assert.Equal(t, "checks", spec.Dependencies[1].Name)
}

func TestValidate_RejectsFutureRockspecFormat(t *testing.T) {
	t.Parallel()

	// glr-8ov: a format newer than the supported ceiling (3.0) is rejected.
	spec := &rocks.Rockspec{
		Package: "p", Version: "1.0-1",
		Source:         rocks.Source{URL: "https://example.com/x.tar.gz"},
		Build:          rocks.Build{Type: "builtin"},
		RockspecFormat: "4.0",
	}
	require.ErrorContains(t, rockspec.Validate(spec), "Rockspec format 4.0 is not supported")

	// 3.0 itself is accepted.
	spec.RockspecFormat = rockspec.SupportedRockspecFormat
	require.NoError(t, rockspec.Validate(spec))
}

// TestEval_PlatformTableHoldingItself checks that a build or source table
// listed among its own platform overlays is read one level deep, as
// MergePlatforms applies it, rather than without end.
func TestEval_PlatformTableHoldingItself(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
local s = { url = "https://example.com/x.tar.gz" }
s.platforms = { unix = s }
source = s
local b = { type = "builtin", modules = { m = "m.lua" } }
b.platforms = { unix = b, linux = { platforms = { linux = b } } }
build = b
`), rocks.RockspecConfig{})
	require.NoError(t, err)

	assert.Equal(t, "builtin", spec.Build.Platforms["unix"].Type)
	assert.Nil(t, spec.Build.Platforms["unix"].Platforms)
	assert.Nil(t, spec.Build.Platforms["linux"].Platforms)
	assert.Equal(t, "https://example.com/x.tar.gz", spec.Source.Platforms["unix"].URL)
	assert.Nil(t, spec.Source.Platforms["unix"].Platforms)
}

// TestEval_ModuleListsGivenAsStrings checks that a module's sources and flag
// lists may be a single string, and that the array part of a module table
// takes precedence over its sources field, as in the builtin backend.
func TestEval_ModuleListsGivenAsStrings(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = {
   type = "builtin",
   modules = {
      a = { sources = "a.c", incdirs = "$(FOO_INCDIR)", libdirs = "lib",
            libraries = "foo", defines = "X=1" },
      b = { "b1.c", "b2.c", sources = { "ignored.c" } },
      c = { sources = {} },
   },
}
`), rocks.RockspecConfig{})
	require.NoError(t, err)

	assert.Equal(t, rocks.Module{
		Sources: []string{"a.c"}, Incdirs: []string{"$(FOO_INCDIR)"}, Libdirs: []string{"lib"},
		Libraries: []string{"foo"}, Defines: []string{"X=1"},
	}, spec.Build.Modules["a"])
	assert.Equal(t, []string{"b1.c", "b2.c"}, spec.Build.Modules["b"].Sources)
	assert.Nil(t, spec.Build.Modules["c"].Sources)
}

// TestEval_InstallArrayPart checks that the array entries of build.install,
// which upstream installs under their base name right in the group's
// directory, are kept under the key naming that destination.
func TestEval_InstallArrayPart(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = {
   type = "builtin",
   install = {
      bin = { "bin/x", "scripts/y.sh", z = "bin/z" },
      conf = { "etc/a.conf" },
      lua = { "src/foo.lua", "src/data.json", ["bar.baz"] = "src/baz.lua" },
      lib = { "build/core.so" },
   },
}
`), rocks.RockspecConfig{})
	require.NoError(t, err)

	assert.Equal(t, rocks.BuildInstall{
		Bin:  map[string]string{"x": "bin/x", "y.sh": "scripts/y.sh", "z": "bin/z"},
		Conf: map[string]string{"a.conf": "etc/a.conf"},
		Lua:  map[string]string{"foo": "src/foo.lua", "[2]": "src/data.json", "bar.baz": "src/baz.lua"},
		Lib:  map[string]string{"[1]": "build/core.so"},
	}, spec.Build.Install)

	// A string key naming the same destination wins, as upstream copies it
	// last.
	spec, err = rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "builtin", install = { bin = { "a/x", x = "b/x" } } }
`), rocks.RockspecConfig{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"x": "b/x"}, spec.Build.Install.Bin)

	for _, install := range []string{
		`lua = { "src/foo.bar.lua" }`,
		`lua = { "src/.lua" }`,
		`lib = { "FOO.LUA" }`,
		`bin = { "x/.." }`,
		`conf = { "" }`,
	} {
		_, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "builtin", install = { `+install+` } }
`), rocks.RockspecConfig{})
		require.ErrorContains(t, err, "build.install.", "install = { %s }", install)
	}
}

// TestEval_InstallArrayPartLandsWhereUpstreamPutsIt builds and deploys a rock
// whose build.install has array entries, and checks that each file lands
// where upstream's install_to copies it: under its base name, right in the
// directory of its group.
func TestEval_InstallArrayPartLandsWhereUpstreamPutsIt(t *testing.T) {
	t.Parallel()

	src := t.TempDir()

	for _, f := range []string{"m.lua", "bin/x", "etc/a.conf", "src/foo.lua", "src/data.json", "build/core.so"} {
		require.NoError(t, os.MkdirAll(filepath.Join(src, filepath.Dir(f)), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(src, f), []byte("-- "+f+"\n"), 0o600))
	}

	spec, err := rockspec.Eval(writeTempRockspec(t, `package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = {
   type = "builtin",
   modules = { m = "m.lua" },
   install = {
      bin = { "bin/x" },
      conf = { "etc/a.conf" },
      lua = { "src/foo.lua", "src/data.json" },
      lib = { "build/core.so" },
   },
}
`), rocks.RockspecConfig{})
	require.NoError(t, err)

	rockspec.MergePlatforms(spec, rockspec.RuntimePlatforms())
	require.NoError(t, rockspec.Validate(spec))

	stage := t.TempDir()
	require.NoError(t, build.RunBackend(context.Background(), spec, src, stage, rocks.Config{}))

	for _, f := range []string{"bin/x", "conf/a.conf", "lua/foo.lua", "lua/data.json", "lib/core.so"} {
		assert.FileExists(t, filepath.Join(stage, f), "builtin backend")
	}

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	_, err = tr.Deploy(spec, src, stage)
	require.NoError(t, err)

	for _, f := range []string{
		filepath.Join(tr.BinDir(), "x"),
		filepath.Join(tr.ConfDir("p", "1.0-1"), "a.conf"),
		filepath.Join(tr.DeployLuaDir(), "foo.lua"),
		filepath.Join(tr.DeployLuaDir(), "data.json"),
		filepath.Join(tr.DeployLibDir(), "core.so"),
	} {
		assert.FileExists(t, f, "tree.Deploy")
	}
}

// TestValidate_RejectsPackageEscapingRocksDir checks that a package name
// must stay one directory under the rocks dir.
func TestValidate_RejectsPackageEscapingRocksDir(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{
		Version: "1.0-1",
		Source:  rocks.Source{URL: "https://example.com/x.tar.gz"},
		Build:   rocks.Build{Type: "builtin"},
	}

	for _, p := range []string{"../evil", "a/b", "/abs", "a/", `a\b`, `..\x`, ".", ".."} {
		spec.Package = p
		require.ErrorIs(t, rockspec.Validate(spec), rockspec.ErrPackageNotPathComponent, "package %q", p)
	}

	for _, p := range []string{"foo", "foo.bar", "..a", "a..b", "...", "-x"} {
		spec.Package = p
		require.NoError(t, rockspec.Validate(spec), "package %q", p)
	}
}

// TestValidate_RejectsUnknownRockspecFormat checks that a format no newer
// than 3.0 must still be one upstream declares a schema for, spelled exactly.
func TestValidate_RejectsUnknownRockspecFormat(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{
		Package: "p", Version: "1.0-1",
		Source: rocks.Source{URL: "https://example.com/x.tar.gz"},
		Build:  rocks.Build{Type: "builtin"},
	}

	for _, f := range []string{"2.0", "0", "3", "1", "1.0.0", "3.0-0", " 3.0"} {
		spec.RockspecFormat = f
		require.ErrorContains(t, rockspec.Validate(spec), "unknown rockspec format", "format %q", f)
	}

	for _, f := range []string{"1.0", "1.1", "3.0"} {
		spec.RockspecFormat = f
		require.NoError(t, rockspec.Validate(spec), "format %q", f)
	}

	// Written as "", the format is not absent but unknown.
	_, err := rockspec.Eval(writeTempRockspec(t, `rockspec_format = ""
package = "p"
version = "1.0-1"
source = { url = "https://example.com/x.tar.gz" }
build = { type = "builtin" }
`), rocks.RockspecConfig{})
	require.ErrorContains(t, err, "unknown rockspec format")
}

func TestValidate_GatesNewFieldsByFormat(t *testing.T) {
	t.Parallel()

	// glr-vto: 3.0-only fields must be rejected under a pre-3.0 format.
	base := func() *rocks.Rockspec {
		return &rocks.Rockspec{
			Package: "p", Version: "1.0-1",
			Source: rocks.Source{URL: "https://example.com/x.tar.gz"},
			Build:  rocks.Build{Type: "builtin"},
		}
	}

	s := base()
	s.BuildDependencies = []rocks.Dep{{Name: "cmake"}}
	require.ErrorContains(t, rockspec.Validate(s), "build_dependencies is not supported in rockspec format 1.0 (requires version 3.0)")

	s = base()
	s.TestDependencies = []rocks.Dep{{Name: "luatest"}}
	require.ErrorContains(t, rockspec.Validate(s), "test_dependencies is not supported in rockspec format 1.0")

	s = base()
	s.Description.Labels = []string{"web"}
	require.ErrorContains(t, rockspec.Validate(s), "labels is not supported in rockspec format 1.0")

	s = base()
	s.Description.IssuesURL = "https://example.com/issues"
	require.ErrorContains(t, rockspec.Validate(s), "issues_url is not supported in rockspec format 1.0")

	// deploy is a 1.1 field.
	wrap := false
	s = base()
	s.Deploy.WrapBinScripts = &wrap
	require.ErrorContains(t, rockspec.Validate(s), "deploy is not supported in rockspec format 1.0 (requires version 1.1)")

	// All accepted once the format is bumped to 3.0.
	s = base()
	s.RockspecFormat = rockspec.SupportedRockspecFormat
	s.BuildDependencies = []rocks.Dep{{Name: "cmake"}}
	s.TestDependencies = []rocks.Dep{{Name: "luatest"}}
	s.Description.Labels = []string{"web"}
	s.Description.IssuesURL = "https://example.com/issues"
	s.Deploy.WrapBinScripts = &wrap
	require.NoError(t, rockspec.Validate(s))
}

func TestRuntimePlatforms(t *testing.T) {
	t.Parallel()

	got := rockspec.RuntimePlatforms()
	require.NotEmpty(t, got, "RuntimePlatforms() empty")
	assert.Equal(t, "unix", got[0], "RuntimePlatforms()[0]")

	// Cover every OS via the injectable helper (mirrors cfg.each_platform).
	cases := map[string][]string{
		"linux":     {"unix", "linux"},
		"darwin":    {"unix", "bsd", "macosx"},  // glr-9gl: +bsd, -macos
		"freebsd":   {"unix", "bsd", "freebsd"}, // glr-h8f
		"openbsd":   {"unix", "bsd", "openbsd"},
		"netbsd":    {"unix", "bsd", "netbsd"},
		"dragonfly": {"unix", "bsd", "dragonfly"},
		"plan9":     {"unix"},
	}
	for goos, want := range cases {
		assert.Equal(t, want, rockspec.RuntimePlatformsForTest(goos), "RuntimePlatformsFor(%q)", goos)
	}

	// The darwin set must never include the unreachable "macos" flag.
	assert.NotContains(t, rockspec.RuntimePlatformsForTest("darwin"), "macos")
}

func TestMergePlatforms_ScalarAndMap(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"base": {Path: "src/base.lua"},
			},
			Variables: map[string]string{"X": "1"},
			Platforms: map[string]rocks.Build{
				"unix": {
					Modules: map[string]rocks.Module{
						"posix": {Path: "src/posix.lua"},
					},
				},
				"linux": {
					Type: "make", // scalar overwrite
					Modules: map[string]rocks.Module{
						"linuxonly": {Path: "src/linuxonly.lua"},
					},
					Variables: map[string]string{"Y": "2"},
				},
			},
		},
	}
	rockspec.MergePlatforms(spec, []string{"unix", "linux"})
	assert.Equal(t, "make", spec.Build.Type, "Build.Type want make (linux scalar overwrite)")
	assert.Contains(t, spec.Build.Modules, "base", "base module lost")
	assert.Contains(t, spec.Build.Modules, "posix", "posix module not merged from unix")
	assert.Contains(t, spec.Build.Modules, "linuxonly", "linuxonly module not merged from linux")
	assert.Equal(t, "1", spec.Build.Variables["X"], "Variables not merged")
	assert.Equal(t, "2", spec.Build.Variables["Y"], "Variables not merged")
	assert.Nil(t, spec.Build.Platforms, "Build.Platforms should be nil after merge")
}

func TestMergePlatforms_CopyDirectoriesIndexMerge(t *testing.T) {
	t.Parallel()

	// glr-0j3: a platform copy_directories override merges index-wise (like any
	// table under deep_merge), replacing base[i] and keeping longer base tails —
	// NOT concatenating. base {doc,samples} + override {extra} → {extra,samples}.
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:            "builtin",
			CopyDirectories: []string{"doc", "samples"},
			Platforms: map[string]rocks.Build{
				"unix": {CopyDirectories: []string{"extra"}},
			},
		},
	}
	rockspec.MergePlatforms(spec, []string{"unix"})
	assert.Equal(t, []string{"extra", "samples"}, spec.Build.CopyDirectories)
}

func TestMergePlatforms_ModuleFieldDeepMerge(t *testing.T) {
	t.Parallel()

	// glr-18s: a per-platform override that restates only some module fields
	// must deep-merge with the base entry, not replace it wholesale.
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Sources: []string{"a.c", "b.c"}},
			},
			Platforms: map[string]rocks.Build{
				"linux": {
					Modules: map[string]rocks.Module{
						"foo": {Libraries: []string{"m"}},
					},
				},
			},
		},
	}
	rockspec.MergePlatforms(spec, []string{"unix", "linux"})

	foo := spec.Build.Modules["foo"]
	assert.Equal(t, []string{"a.c", "b.c"}, foo.Sources, "base Sources must survive the override")
	assert.Equal(t, []string{"m"}, foo.Libraries, "override Libraries must be applied")
}

func TestMergePlatforms_Precedence(t *testing.T) {
	t.Parallel()

	// linux applied after unix → linux value wins on shared keys.
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Path: "src/foo-base.lua"},
			},
			Platforms: map[string]rocks.Build{
				"unix":  {Modules: map[string]rocks.Module{"foo": {Path: "src/foo-unix.lua"}}},
				"linux": {Modules: map[string]rocks.Module{"foo": {Path: "src/foo-linux.lua"}}},
			},
		},
	}
	rockspec.MergePlatforms(spec, []string{"unix", "linux"})
	assert.Equal(t, "src/foo-linux.lua", spec.Build.Modules["foo"].Path, "foo path want src/foo-linux.lua (linux is more specific)")
}

func TestEval_PlatformsThenMerge_Linux(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.Eval(tdata("platforms.rockspec"), rocks.RockspecConfig{})
	require.NoError(t, err)
	rockspec.MergePlatforms(spec, []string{"unix", "linux"})

	keys := make([]string, 0, len(spec.Build.Modules))
	for k := range spec.Build.Modules {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	want := []string{"platformer", "platformer.linux", "platformer.posix"}
	assert.Equal(t, want, keys, "merged modules")
}

// TestMergePlatforms_SourceAndDeps — glr-xuh: platform overrides apply to
// source, dependencies and external_dependencies, not just build.
func TestMergePlatforms_SourceAndDeps(t *testing.T) {
	t.Parallel()

	path := writeTempRockspec(t, `
package = "x"
version = "1.0-1"
source = {
   url = "https://example.com/generic.tar.gz",
   platforms = { linux = { url = "https://example.com/linux.tar.gz" } },
}
dependencies = {
   "lua >= 5.1",
   platforms = { unix = { "posix >= 1.0" } },
}
external_dependencies = {
   platforms = { linux = { LIBFOO = { library = "foo" } } },
}
build = { type = "none" }
`)
	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.NoError(t, err)

	// Before merge: overlays are captured, base is generic.
	assert.Equal(t, "https://example.com/generic.tar.gz", spec.Source.URL)
	require.Contains(t, spec.Source.Platforms, "linux")

	rockspec.MergePlatforms(spec, []string{"unix", "linux"})

	// source.url overridden on linux (glr-xuh failing case).
	assert.Equal(t, "https://example.com/linux.tar.gz", spec.Source.URL, "source.url must take the linux override")
	// dependencies index-merged: entry 0 replaced by the unix override.
	require.NotEmpty(t, spec.Dependencies)
	assert.Equal(t, "posix", spec.Dependencies[0].Name, "dependencies[0] must take the unix override")
	// external_dependencies gains the linux entry.
	assert.Equal(t, "foo", spec.ExternalDependencies["LIBFOO"].Library, "external dep from linux override")
	// Overlay data cleared after merge.
	assert.Nil(t, spec.Source.Platforms)
	assert.Nil(t, spec.DependenciesPlatforms)
}

// ---- helpers ----

func writeTempRockspec(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "tmp-0.0-1.rockspec")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600), "write")

	return p
}
