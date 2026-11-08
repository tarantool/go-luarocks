package rockspec_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/rockspec"
)

// baseSpec is a rockspec Validate accepts, for the path tests to spoil.
func baseSpec() *rocks.Rockspec {
	return &rocks.Rockspec{
		Package: "demo",
		Version: "1.0-1",
		Source:  rocks.Source{URL: "https://example.com/demo-1.0.tar.gz"},
		Build:   rocks.Build{Type: "builtin"},
	}
}

// TestValidate_RejectsPathsLeavingTheirDir: every path-valued field that
// becomes a file system path is refused when it is absolute or reaches above
// its directory, and a field naming a file when it is empty or ".". The error
// names the field.
func TestValidate_RejectsPathsLeavingTheirDir(t *testing.T) {
	t.Parallel()

	bad := []string{"../x", "a/../../x", "..", "/etc/passwd", `\x`, `a\..\x`, ""}

	for _, tc := range []struct {
		field string
		set   func(s *rocks.Rockspec, p string)
		entry bool // "." is refused too
	}{
		{"source.file", func(s *rocks.Rockspec, p string) { s.Source.File = p }, true},
		{"source.dir", func(s *rocks.Rockspec, p string) { s.Source.Dir = p }, false},
		{"build.install.bin key", func(s *rocks.Rockspec, p string) {
			s.Build.Install.Bin = map[string]string{p: "bin/x"}
		}, true},
		{`build.install.bin["x"]`, func(s *rocks.Rockspec, p string) {
			s.Build.Install.Bin = map[string]string{"x": p}
		}, true},
		{"build.install.conf key", func(s *rocks.Rockspec, p string) {
			s.Build.Install.Conf = map[string]string{p: "x.conf"}
		}, true},
		{`build.install.conf["x"]`, func(s *rocks.Rockspec, p string) {
			s.Build.Install.Conf = map[string]string{"x": p}
		}, true},
		{"build.install.lua key", func(s *rocks.Rockspec, p string) {
			s.Build.Install.Lua = map[string]string{p: "x.lua"}
		}, true},
		{`build.install.lib["x"]`, func(s *rocks.Rockspec, p string) {
			s.Build.Install.Lib = map[string]string{"x": p}
		}, true},
		{`build.modules["m"]`, func(s *rocks.Rockspec, p string) {
			s.Build.Modules = map[string]rocks.Module{"m": {Path: p + "/m.lua"}}
		}, false},
		{`build.modules["m"].sources`, func(s *rocks.Rockspec, p string) {
			s.Build.Modules = map[string]rocks.Module{"m": {Sources: []string{p}}}
		}, true},
		{"build.copy_directories", func(s *rocks.Rockspec, p string) { s.Build.CopyDirectories = []string{p} }, false},
		{"build.platforms.linux.install.bin key", func(s *rocks.Rockspec, p string) {
			s.Build.Platforms = map[string]rocks.Build{"linux": {Install: rocks.BuildInstall{Bin: map[string]string{p: "x"}}}}
		}, true},
		{"source.platforms.linux.file", func(s *rocks.Rockspec, p string) {
			s.Source.Platforms = map[string]rocks.Source{"linux": {File: p}}
		}, true},
	} {
		values := bad
		if tc.entry {
			values = append(append([]string(nil), bad...), ".", "./", "a/..")
		}

		for _, p := range values {
			// source.file, source.dir and a module path are unset when empty.
			if p == "" && (tc.field == "source.dir" || tc.field == "source.file" ||
				tc.field == "source.platforms.linux.file") {
				continue
			}

			spec := baseSpec()
			tc.set(spec, p)

			err := rockspec.Validate(spec)
			require.ErrorIs(t, err, rocks.ErrUnsafePath, "%s = %q", tc.field, p)
			assert.Contains(t, err.Error(), tc.field, "%s = %q", tc.field, p)
		}
	}
}

// TestValidate_AcceptsPathsInsideTheirDir: the paths real rockspecs use pass.
func TestValidate_AcceptsPathsInsideTheirDir(t *testing.T) {
	t.Parallel()

	spec := baseSpec()
	spec.Source.File = "demo-1.0.tar.gz"
	spec.Source.Dir = "."
	spec.Build.Modules = map[string]rocks.Module{
		"demo":      {Path: "src/demo.lua"},
		"demo.init": {Path: "./src/demo/init.lua"},
		"demo.c":    {Sources: []string{"src/c.c", "src/..util.c"}},
	}
	spec.Build.Install = rocks.BuildInstall{
		Lua:  map[string]string{"demo.extra": "lua/extra.lua"},
		Bin:  map[string]string{"demo": "bin/demo", "tools/x": "bin/x"},
		Conf: map[string]string{"demo.cfg": "conf/demo.cfg"},
	}
	spec.Build.CopyDirectories = []string{"doc", "samples/", "."}

	require.NoError(t, rockspec.Validate(spec))

	spec.Source.Dir = "demo-1.0"
	require.NoError(t, rockspec.Validate(spec))
}

// TestValidate_VersionIsOneDirectory: the version is a directory of the
// install path too, and its grammar admits no separator.
func TestValidate_VersionIsOneDirectory(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"../1.0-1", "1.0/../../x-1", "..", "/1.0-1"} {
		spec := baseSpec()
		spec.Version = v
		require.Error(t, rockspec.Validate(spec), v)
	}
}

// TestEval_RejectsInstallBinOutsideTree runs a real rockspec through Eval and
// Validate: a "none" rock whose build.install.bin key leaves the tree is
// refused before anything is installed.
func TestEval_RejectsInstallBinOutsideTree(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "demo-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(path, []byte(`package = "demo"
version = "1.0-1"
source = { url = "https://example.com/demo-1.0.tar.gz" }
build = { type = "none", install = { bin = { ["../../x"] = "demo" } } }
`), 0o600))

	spec, err := rockspec.Eval(path, rocks.RockspecConfig{})
	require.NoError(t, err)

	err = rockspec.Validate(spec)
	require.ErrorIs(t, err, rocks.ErrUnsafePath)
	assert.Contains(t, err.Error(), `build.install.bin key "../../x"`)
}
