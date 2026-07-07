package tree_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// demoPkg is the rock name reused across these tests as package, module, bin,
// and file basename.
const demoPkg = "demo"

func TestOpen_CreatesLayout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	for _, sub := range []string{tr.RocksDir(), tr.DeployLuaDir(), tr.DeployLibDir(), tr.BinDir()} {
		st, err := os.Stat(sub)
		if assert.NoError(t, err, "expected dir %q to exist", sub) {
			assert.True(t, st.IsDir(), "expected dir %q to exist", sub)
		}
	}

	assert.NotNil(t, tr.Store, "Open: Store should default to manif.FileStore, got nil")
}

func TestOpen_EmptyTree(t *testing.T) {
	t.Parallel()

	_, err := tree.Open(rocks.Config{})
	require.Error(t, err, "Open: expected error for empty Tree")
}

func TestDeploy_StringFormModules(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeFile(t, filepath.Join(src, "src", "foo", "bar.lua"), "-- foo.bar")
	writeFile(t, filepath.Join(src, "build", "foo", "baz.so"), "ELF-ish")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Modules: map[string]rocks.Module{
				"foo.bar": {Path: "src/foo/bar.lua"},
				"foo.baz": {Path: "build/foo/baz.so"},
			},
		},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	wantLua := filepath.Join(tr.DeployLuaDir(), "foo", "bar.lua")
	_, err = os.Stat(wantLua)
	require.NoError(t, err, "expected %q", wantLua)

	wantSo := filepath.Join(tr.DeployLibDir(), "foo", "baz.so")
	soSt, err := os.Stat(wantSo)
	require.NoError(t, err, "expected %q", wantSo)
	// Upstream deploys .so modules with perms="exec" (0755).
	assert.NotZero(t, soSt.Mode().Perm()&0o100, "deployed .so %q should be executable; got %v", wantSo, soSt.Mode())
	assert.Contains(t, rm.Lua, "foo/bar.lua", "rock_manifest.lua missing foo/bar.lua")
	assert.Contains(t, rm.Lib, "foo/baz.so", "rock_manifest.lib missing foo/baz.so")
}

func TestDeploy_InstallAndCopyDirs(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeFile(t, filepath.Join(src, "scripts", demoPkg), "#!/bin/sh\necho hi")
	writeFile(t, filepath.Join(src, "config", "demo.cfg"), "key=value")
	writeFile(t, filepath.Join(src, "extra.lua"), "return {}")
	writeFile(t, filepath.Join(src, "doc", "README"), "readme body")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Install: rocks.BuildInstall{
				Lua:  map[string]string{"extra.lua": "extra.lua"},
				Bin:  map[string]string{demoPkg: "scripts/demo"},
				Conf: map[string]string{"demo.cfg": "config/demo.cfg"},
			},
			CopyDirectories: []string{"doc"},
		},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	// Bin should be executable (0755).
	bin := filepath.Join(tr.BinDir(), demoPkg)
	st, err := os.Stat(bin)
	require.NoError(t, err, "stat %q", bin)
	assert.NotZero(t, st.Mode().Perm()&0o100, "bin %q should be executable; got mode %v", bin, st.Mode())

	conf := filepath.Join(tr.ConfDir(demoPkg, "1.0.0-1"), "demo.cfg")
	_, err = os.Stat(conf)
	require.NoError(t, err, "expected conf %q", conf)

	doc := filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "doc", "README")
	_, err = os.Stat(doc)
	require.NoError(t, err, "expected doc %q", doc)

	assert.Contains(t, rm.Bin, demoPkg, "rm.Bin missing demo")
	assert.Contains(t, rm.Conf, "demo.cfg", "rm.Conf missing")
	assert.Contains(t, rm.Doc, "README", "rm.Doc missing README")
}

func TestDeploy_WrappedBinManifestKey(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	// A valid Lua script — is_lua passes, so this takes the wrap branch.
	writeFile(t, filepath.Join(src, "scripts", demoPkg), "print('hi')\n")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)
	// Selecting the wrapped branch (as the native engine always does).
	tr.BinWrap = &tree.BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Install: rocks.BuildInstall{
				Bin: map[string]string{demoPkg: "scripts/demo"},
			},
		},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	// Upstream keys rock_manifest.bin relative to the bin subtree: bin = { demo = md5 },
	// not the doubly-nested bin = { bin = { demo = md5 } } a "bin/demo" key would produce.
	assert.Contains(t, rm.Bin, demoPkg, "rm.Bin should key by subtree-relative name")
	assert.NotContains(t, rm.Bin, "bin/demo", "rm.Bin must not carry a spurious bin/ prefix")

	// The public <tree>/bin/demo is a generated /bin/sh launcher; the real
	// script lives under the per-rock install dir.
	realScript := filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "bin", demoPkg)
	require.FileExists(t, realScript, "real script should land in per-rock bin dir")

	pub, err := os.ReadFile(filepath.Join(tr.BinDir(), demoPkg))
	require.NoError(t, err)
	assert.Contains(t, string(pub), "/usr/bin/lua", "public bin entry should be an interpreter launcher")
}

func TestDeploy_NonLuaBinCopiedVerbatim(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	// A shell script is not valid Lua: upstream install_binary's fs.is_lua gate
	// fails, so it is copied verbatim rather than wrapped, even under BinWrap.
	const body = "#!/bin/sh\necho hi\n"
	writeFile(t, filepath.Join(src, "scripts", "tool"), body)

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	tr.BinWrap = &tree.BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Install: rocks.BuildInstall{
				Bin: map[string]string{"tool": "scripts/tool"},
			},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(tr.BinDir(), "tool"))
	require.NoError(t, err)
	assert.Equal(t, body, string(got), "non-Lua bin must be copied verbatim, not wrapped")
	// The verbatim path does not create a per-rock bin copy.
	assert.NoFileExists(t, filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "bin", "tool"))
}

func TestDeploy_WritesRockspecIntoInstallDir(t *testing.T) {
	t.Parallel()

	// glr-b5y: Deploy copies the rockspec verbatim into the install dir as
	// <name>-<version>.rockspec and records it under that versioned key.
	dir := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "init.lua"), "return 1")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	raw := []byte("package = \"demo\"\nversion = \"1.0.0-1\"\n")
	spec := &rocks.Rockspec{
		Package:   demoPkg,
		Version:   "1.0.0-1",
		RawSource: raw,
		Build:     rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "init.lua"}}},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	dst := filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "demo-1.0.0-1.rockspec")
	got, err := os.ReadFile(dst) //nolint:gosec // test temp path
	require.NoError(t, err, "rockspec should be copied into the install dir")
	assert.Equal(t, raw, got, "rockspec bytes must be verbatim")
	assert.Equal(t, "demo-1.0.0-1.rockspec", rm.RockspecFile, "rock_manifest rockspec filename")
	assert.NotEmpty(t, rm.Rockspec, "rock_manifest rockspec md5")
}

func TestDeleteVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "init.lua"), "return 1")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "init.lua"}}},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)
	// Persist the rock_manifest so DeleteVersion can find the deployed files.
	require.NoError(t, tr.Store.WriteRock(filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "rock_manifest"), rm))

	deployed := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	require.FileExists(t, deployed)

	require.NoError(t, tr.DeleteVersion(demoPkg, "1.0.0-1"))

	assert.NoFileExists(t, deployed, "deployed module must be removed")
	assert.NoDirExists(t, tr.InstallDir(demoPkg, "1.0.0-1"), "install dir must be removed")
	assert.NoDirExists(t, filepath.Join(tr.RocksDir(), demoPkg), "empty name dir must be pruned")
}

func TestDeploy_MakeInstalledSubtree(t *testing.T) {
	t.Parallel()

	// A make backend installs into buildDir/lua and buildDir/lib (no
	// build.modules list); Deploy must scan those subtrees and relocate the
	// files to the shared deploy dirs, recording rock_manifest entries.
	dir := t.TempDir()
	src := t.TempDir()
	buildDir := t.TempDir()

	writeFile(t, filepath.Join(buildDir, "lua", "foo", "bar.lua"), "return 1")
	writeFile(t, filepath.Join(buildDir, "lib", "foo", "baz.so"), "ELF-ish")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Type: "make"},
	}
	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	wantLua := filepath.Join(tr.DeployLuaDir(), "foo", "bar.lua")
	_, err = os.Stat(wantLua)
	require.NoError(t, err, "expected deployed %q", wantLua)

	wantSo := filepath.Join(tr.DeployLibDir(), "foo", "baz.so")
	st, err := os.Stat(wantSo)
	require.NoError(t, err, "expected deployed %q", wantSo)
	assert.NotZero(t, st.Mode().Perm()&0o100, "deployed .so should be executable")

	assert.Contains(t, rm.Lua, "foo/bar.lua", "rock_manifest.lua missing make-installed module")
	assert.Contains(t, rm.Lib, "foo/baz.so", "rock_manifest.lib missing make-installed lib")
}

// TestDeploy_PromotesHigherVersion — glr-5e9: installing a HIGHER version over
// an existing active one promotes the new version to the plain (active) path
// and demotes the previous provider's file to its versioned name.
func TestDeploy_PromotesHigherVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "init.lua"), "v2")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)
	// Pre-existing v1 at the active path, tracked as demo/1.0.0-1.
	active := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	writeFile(t, active, "v1")

	tr.Provider = func(item string) (string, string, bool) {
		if item == demoPkg {
			return demoPkg, "1.0.0-1", true
		}

		return "", "", false
	}

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "2.0.0-1",
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "init.lua"}}},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	// The newer version now occupies the plain active path.
	b, _ := os.ReadFile(active) //nolint:gosec // G304: test-controlled temp path.
	assert.Equal(t, "v2", string(b), "higher version must win the active path")
	assert.Contains(t, rm.Lua, "demo.lua", "rm.Lua should key the new file at the plain path")

	// The previous provider (1.0.0-1) is demoted to its versioned name.
	demoted := tree.MungedPath(tr.DeployLuaDir(), active, demoPkg, "1.0.0-1")
	assert.Equal(t, "demo_1_0_0_1-demo.lua", filepath.Base(demoted), "demoted name")
	b, _ = os.ReadFile(demoted) //nolint:gosec // G304: test-controlled temp path.
	assert.Equal(t, "v1", string(b), "old version preserved under its versioned name")
}

// TestDeploy_LowerVersionTakesVersionedName — glr-5e9: installing a LOWER
// version when a higher one is active leaves the active path untouched and
// writes the new file under its own versioned name.
func TestDeploy_LowerVersionTakesVersionedName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "init.lua"), "v1old")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	active := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	writeFile(t, active, "v2")

	tr.Provider = func(item string) (string, string, bool) {
		if item == demoPkg {
			return demoPkg, "2.0.0-1", true
		}

		return "", "", false
	}

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "init.lua"}}},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	// The active (higher) version is untouched.
	b, _ := os.ReadFile(active) //nolint:gosec // G304: test-controlled temp path.
	assert.Equal(t, "v2", string(b), "active higher version must be preserved")

	// The new lower version lands under its versioned name.
	versioned := tree.MungedPath(tr.DeployLuaDir(), active, demoPkg, "1.0.0-1")
	b, _ = os.ReadFile(versioned) //nolint:gosec // G304: test-controlled temp path.
	assert.Equal(t, "v1old", string(b), "lower version stored under its versioned name")
	assert.Contains(t, rm.Lua, "demo_1_0_0_1-demo.lua", "rm.Lua should key the versioned entry")
}

func TestWhich(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)
	writeFile(t, filepath.Join(tr.DeployLuaDir(), "foo", "bar.lua"), "")
	writeFile(t, filepath.Join(tr.DeployLuaDir(), "pkg", "init.lua"), "")
	writeFile(t, filepath.Join(tr.DeployLibDir(), "nat.so"), "")

	cases := []struct {
		name   string
		module string
		want   string
		ok     bool
	}{
		{"plain lua", "foo.bar", filepath.Join(tr.DeployLuaDir(), "foo", "bar.lua"), true},
		{"init lua", "pkg", filepath.Join(tr.DeployLuaDir(), "pkg", "init.lua"), true},
		{"so", "nat", filepath.Join(tr.DeployLibDir(), "nat.so"), true},
		{"miss", "absent", "", false},
	}
	for _, c := range cases {
		got, ok := tr.Which(c.module)
		assert.Equal(t, c.want, got, c.name)
		assert.Equal(t, c.ok, ok, c.name)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750), "mkdir %q", filepath.Dir(p))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600), "write %q", p)
}
