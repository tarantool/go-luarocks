package tree_test

import (
	"crypto/md5"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
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

// TestDeploy_ShebangLuaBinWrapped — a Lua script that starts with a "#!"
// line is Lua to upstream's fs.is_lua (loadfile skips that line), so upstream
// installs it under the per-rock bin dir and deploys a launcher to <tree>/bin.
// It used to fail the parse and be copied verbatim to <tree>/bin.
func TestDeploy_ShebangLuaBinWrapped(t *testing.T) {
	t.Parallel()

	src := t.TempDir()

	const body = "#!/usr/bin/env tarantool\nprint('hi')\n"
	writeFile(t, filepath.Join(src, "bin", "tool"), body)

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	tr.BinWrap = &tree.BinWrap{Interpreter: "/opt/tarantool/bin/tarantool", Sysconfdir: "/etc/luarocks"}

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Install: rocks.BuildInstall{
				Bin: map[string]string{"tool": "bin/tool"},
			},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.NoError(t, err)

	realScript := filepath.Join(tr.InstallDir(demoPkg, "1.0.0-1"), "bin", "tool")
	got, err := os.ReadFile(realScript) //nolint:gosec // test temp path
	require.NoError(t, err, "the real script must land in the per-rock bin dir")
	assert.Equal(t, body, string(got), "the real script must be installed verbatim")

	pub, err := os.ReadFile(filepath.Join(tr.BinDir(), "tool"))
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(pub),
		"#!/bin/sh\n\nLUAROCKS_SYSCONFDIR='/etc/luarocks' exec '/opt/tarantool/bin/tarantool' -e "),
		"<tree>/bin/tool must be the launcher, got:\n%s", pub)
	assert.True(t, strings.HasSuffix(string(pub), " '"+realScript+"' \"$@\"\n"),
		"the launcher must exec the real script, got:\n%s", pub)
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
	writeFile(t, filepath.Join(src, "demo.lua"), "return 1")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	raw := []byte("package = \"demo\"\nversion = \"1.0.0-1\"\n")
	spec := &rocks.Rockspec{
		Package:   demoPkg,
		Version:   "1.0.0-1",
		RawSource: raw,
		Build:     rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "demo.lua"}}},
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
	writeFile(t, filepath.Join(src, "demo.lua"), "return 1")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "demo.lua"}}},
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

	// buildDir is a staging area kept on purpose: its files are copied, not
	// moved, so the originals must survive the deploy.
	assert.FileExists(t, filepath.Join(buildDir, "lua", "foo", "bar.lua"),
		"buildDir source must be copied, not moved")
	assert.FileExists(t, filepath.Join(buildDir, "lib", "foo", "baz.so"),
		"buildDir source must be copied, not moved")
}

// TestDeploy_CMakeInstallDirSubtree — TNTP-9962: a cmake rock installs into
// $(LUADIR)/$(LIBDIR), which build/vars.go points at <installDir>/lua|lib. The
// relocation stage must run for that build type too, move the files to the
// shared deploy dirs and leave nothing behind under the per-rock install dir.
func TestDeploy_CMakeInstallDirSubtree(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	buildDir := t.TempDir()

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	installDir := tr.InstallDir(demoPkg, "1.0.0-1")
	staleLua := filepath.Join(installDir, "lua", demoPkg, "init.lua")
	staleSo := filepath.Join(installDir, "lib", demoPkg, "native.so")

	writeFile(t, staleLua, "return {}")
	writeFile(t, staleSo, "ELF-ish")

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Type: "cmake"},
	}
	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	wantLua := filepath.Join(tr.DeployLuaDir(), demoPkg, "init.lua")
	require.FileExists(t, wantLua, "cmake-installed module must reach the deploy dir")
	assert.Contains(t, rm.Lua, demoPkg+"/init.lua", "rock_manifest.lua missing the module")

	wantSo := filepath.Join(tr.DeployLibDir(), demoPkg, "native.so")
	require.FileExists(t, wantSo, "cmake-installed lib must reach the deploy dir")
	assert.Contains(t, rm.Lib, demoPkg+"/native.so", "rock_manifest.lib missing the lib")

	st, err := os.Stat(wantSo)
	require.NoError(t, err, "stat %q", wantSo)
	assert.NotZero(t, st.Mode().Perm()&0o100, "deployed .so should be executable")

	// installDir files are moved, not copied: a .lua left under
	// share/tarantool/rocks/<name>/<ver>/lua/ is on no loader path.
	assert.NoFileExists(t, staleLua, "installDir source must be moved away")
	assert.NoFileExists(t, staleSo, "installDir source must be moved away")
	assert.NoDirExists(t, filepath.Join(installDir, "lua"), "emptied lua dir must be pruned")
	assert.NoDirExists(t, filepath.Join(installDir, "lib"), "emptied lib dir must be pruned")
}

// TestDeploy_DefaultCopyDirectories pins upstream install_files' defaults
// (build.lua:299-321): with copy_directories unset a doc/ dir is copied, and
// when no directory is copied the README, LICENSE, COPYING and *.md files of
// the source root go to the per-rock doc dir.
func TestDeploy_DefaultCopyDirectories(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		files    []string
		copyDirs []string
		wantDoc  []string
	}{
		{
			name:    "doc dir copied by default",
			files:   []string{"doc/index.md", "README.md"},
			wantDoc: []string{"index.md"},
		},
		{
			name:    "default docs without a doc dir",
			files:   []string{"README.md", "LICENSE", "copying.txt", "CHANGES.md", "notes.txt", "src/readme.lua"},
			wantDoc: []string{"CHANGES.md", "LICENSE", "README.md", "copying.txt"},
		},
		{
			name:     "explicit empty list still gets default docs",
			files:    []string{"doc/index.md", "README"},
			copyDirs: []string{},
			wantDoc:  []string{"README"},
		},
		{
			name:     "an explicit directory suppresses default docs",
			files:    []string{"samples/a.lua", "README.md"},
			copyDirs: []string{"samples"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			src := t.TempDir()
			for _, f := range c.files {
				writeFile(t, filepath.Join(src, filepath.FromSlash(f)), f)
			}

			tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
			require.NoError(t, err)

			spec := &rocks.Rockspec{Package: demoPkg, Version: "1.0.0-1", Build: rocks.Build{CopyDirectories: c.copyDirs}}
			rm, err := tr.Deploy(spec, src, src)
			require.NoError(t, err)

			var gotDoc []string
			for k := range rm.Doc {
				gotDoc = append(gotDoc, k)
			}

			assert.ElementsMatch(t, c.wantDoc, gotDoc, "rock_manifest.doc")

			for _, f := range c.wantDoc {
				assert.FileExists(t, filepath.Join(tr.DocDir(demoPkg, "1.0.0-1"), f))
			}
		})
	}
}

// TestDeploy_RockManifestListsInstallDir: the rock_manifest lists every file
// the per-rock install dir holds, as upstream make_rock_manifest walks it — a
// non-doc copy_directories entry, and conf/doc files a backend installed into
// $(CONFDIR)/$(DOCDIR) — but not the build/ staging dir the native backends
// keep there.
func TestDeploy_RockManifestListsInstallDir(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	writeFile(t, filepath.Join(src, "samples", "demo.lua"), "demo")
	writeFile(t, filepath.Join(src, "samples", "nested", "more.txt"), "more")

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	installDir := tr.InstallDir(demoPkg, "1.0.0-1")
	buildDir := filepath.Join(installDir, "build")

	writeFile(t, filepath.Join(installDir, "conf", "demo.cfg"), "cfg")
	writeFile(t, filepath.Join(installDir, "doc", "manual.txt"), "manual")
	writeFile(t, filepath.Join(installDir, "share", "extra.dat"), "extra")
	writeFile(t, filepath.Join(buildDir, "staged.txt"), "staging")

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build:   rocks.Build{Type: "make", CopyDirectories: []string{"samples"}},
	}
	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	assert.Equal(t, map[string]string{"demo.cfg": md5Hex("cfg")}, rm.Conf, "rock_manifest.conf")
	assert.Equal(t, map[string]string{"manual.txt": md5Hex("manual")}, rm.Doc, "rock_manifest.doc")
	assert.Equal(t, map[string]string{
		"samples/demo.lua":        md5Hex("demo"),
		"samples/nested/more.txt": md5Hex("more"),
		"share/extra.dat":         md5Hex("extra"),
	}, rm.Other, "every other file of the install dir, the build/ staging dir aside")
}

// TestDeploy_BackendInstalledBin: a make, cmake or command rock that installs
// a command into $(BINDIR) — <buildDir>/bin for make, <installDir>/bin for the
// others — gets it deployed into <tree>/bin and listed in rock_manifest.bin, as
// upstream deploys the rock_manifest's bin section (repos.lua:468-486): a Lua
// script through a wrapper whose real script stays in the per-rock bin dir, a
// shell script verbatim.
func TestDeploy_BackendInstalledBin(t *testing.T) {
	t.Parallel()

	const (
		luaCLI = "print('cli')\n"
		shTool = "#!/bin/sh\necho tool\n"
	)

	for _, root := range []string{"buildDir", "installDir"} {
		t.Run(root, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			buildDir := t.TempDir()

			tr, err := tree.Open(rocks.Config{Tree: dir})
			require.NoError(t, err)

			tr.BinWrap = &tree.BinWrap{Interpreter: "/opt/tt/bin/tarantool", Sysconfdir: "/etc/luarocks"}

			installDir := tr.InstallDir(demoPkg, "1.0.0-1")

			binDir := filepath.Join(installDir, "bin")
			if root == "buildDir" {
				binDir = filepath.Join(buildDir, "bin")
			}

			writeFile(t, filepath.Join(binDir, "demo-cli"), luaCLI)
			writeFile(t, filepath.Join(binDir, "tools", "demo-tool"), shTool)

			spec := &rocks.Rockspec{Package: demoPkg, Version: "1.0.0-1", Build: rocks.Build{Type: "make"}}
			rm, err := tr.Deploy(spec, t.TempDir(), buildDir)
			require.NoError(t, err)

			assert.Equal(t, map[string]string{
				"demo-cli":        md5Hex(luaCLI),
				"tools/demo-tool": md5Hex(shTool),
			}, rm.Bin, "rock_manifest.bin")

			realScript := filepath.Join(installDir, "bin", "demo-cli")
			assert.FileExists(t, realScript, "the real script of a wrapped command stays in the per-rock bin dir")

			wrapper, err := os.ReadFile(filepath.Join(tr.BinDir(), "demo-cli"))
			require.NoError(t, err, "the Lua command must be deployed")
			assert.Contains(t, string(wrapper), "'"+realScript+"'", "the wrapper must exec the real script")

			tool, err := os.ReadFile(filepath.Join(tr.BinDir(), "tools", "demo-tool"))
			require.NoError(t, err, "the shell command must be deployed")
			assert.Equal(t, shTool, string(tool), "a non-Lua command is copied verbatim")
		})
	}
}

// TestDeploy_BuiltinWithoutSubtreesUnaffected — the relocation stage now runs
// for every build type, so the common case with no lua/lib subtree at all must
// stay a silent no-op rather than an error or a spurious deployment.
func TestDeploy_BuiltinWithoutSubtreesUnaffected(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeFile(t, filepath.Join(src, "demo.lua"), "return {}")

	tr, err := tree.Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0.0-1",
		Build: rocks.Build{
			Type:    "builtin",
			Modules: map[string]rocks.Module{demoPkg: {Path: "demo.lua"}},
		},
	}
	rm, err := tr.Deploy(spec, src, src)
	require.NoError(t, err)

	assert.Equal(t, []string{demoPkg + ".lua"}, deployedFiles(t, tr.DeployLuaDir()),
		"only the declared module should be deployed")
	assert.Empty(t, deployedFiles(t, tr.DeployLibDir()), "nothing should reach the lib deploy dir")
	assert.Len(t, rm.Lua, 1, "rock_manifest.lua should hold only the declared module")
	assert.Empty(t, rm.Lib, "rock_manifest.lib should stay empty")
}

// TestDeploy_NonBuiltinIgnoresModules: build.modules is read by upstream's
// builtin backend alone, so a make, cmake, command or none rock that lists
// modules deploys only what its build installed (build/make.lua, cmake.lua,
// command.lua never read build.modules). build.install still applies to every
// type (build.lua install_files).
func TestDeploy_NonBuiltinIgnoresModules(t *testing.T) {
	t.Parallel()

	for _, typ := range []string{"make", "cmake", "command", "none"} {
		t.Run(typ, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			src := t.TempDir()
			buildDir := t.TempDir()

			writeFile(t, filepath.Join(src, "src", "listed.lua"), "return 'listed'")
			writeFile(t, filepath.Join(src, "src", "installed.lua"), "return 'installed'")

			tr, err := tree.Open(rocks.Config{Tree: dir})
			require.NoError(t, err)

			spec := &rocks.Rockspec{
				Package: demoPkg,
				Version: "1.0.0-1",
				Build: rocks.Build{
					Type:    typ,
					Modules: map[string]rocks.Module{"demo.listed": {Path: "src/listed.lua"}},
					Install: rocks.BuildInstall{Lua: map[string]string{"demo.installed": "src/installed.lua"}},
				},
			}
			rm, err := tr.Deploy(spec, src, buildDir)
			require.NoError(t, err)

			assert.Equal(t, []string{"demo/installed.lua"}, deployedFiles(t, tr.DeployLuaDir()),
				"a %s rock must not deploy its build.modules", typ)
			assert.Equal(t, map[string]string{"demo/installed.lua": md5Hex("return 'installed'")}, rm.Lua,
				"rock_manifest.lua of a %s rock", typ)
		})
	}
}

// stageBuiltinRock writes the source tree and the build dir the builtin
// backend leaves for a rock with a build.modules .lua entry, a
// build.install.lua entry and a compiled C module: the backend stages the
// modules and the install.lua file under <buildDir>/lua and the .so under
// <buildDir>/lib, the same files Deploy also reads from the source tree. The
// module bodies carry version so an upgrade can tell the two versions apart.
func stageBuiltinRock(t *testing.T, version string) (*rocks.Rockspec, string, string) {
	t.Helper()

	src := t.TempDir()
	buildDir := t.TempDir()

	mod := "return '" + version + "'"
	helper := "return 'helper " + version + "'"

	writeFile(t, filepath.Join(src, "src", "demo.lua"), mod)
	writeFile(t, filepath.Join(src, "src", "helper.lua"), helper)
	writeFile(t, filepath.Join(buildDir, "lua", "demo.lua"), mod)
	writeFile(t, filepath.Join(buildDir, "lua", "demo", "helper.lua"), helper)
	writeFile(t, filepath.Join(buildDir, "lib", "cdemo.so"), "ELF "+version)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: version,
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				demoPkg: {Path: "src/demo.lua"},
				"cdemo": {Path: "cdemo.c"},
			},
			Install: rocks.BuildInstall{
				Lua: map[string]string{"demo.helper": "src/helper.lua"},
			},
		},
	}

	return spec, src, buildDir
}

// TestDeploy_BuiltinStagedFilesDeployOnce — a builtin rock's modules reach
// Deploy twice, from the source tree (build.modules, build.install.lua) and
// from the build dir the builtin backend staged them in. Each must be deployed
// once: the second visit used to find the file the first had just written,
// take it for an untracked one and back it up to <file>~, which upstream never
// deploys (repos.deploy_files moves each rock_manifest entry once).
func TestDeploy_BuiltinStagedFilesDeployOnce(t *testing.T) {
	t.Parallel()

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	spec, src, buildDir := stageBuiltinRock(t, "1.0-1")

	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"demo.lua", "demo/helper.lua"}, deployedFiles(t, tr.DeployLuaDir()),
		"each lua file must be deployed once, with no ~ backup")
	assert.Equal(t, []string{"cdemo.so"}, deployedFiles(t, tr.DeployLibDir()),
		"the C module must be deployed once, with no ~ backup")
	assert.Equal(t, map[string]string{"demo.lua": md5Hex("return '1.0-1'"),
		"demo/helper.lua": md5Hex("return 'helper 1.0-1'")}, rm.Lua, "rock_manifest.lua")
	assert.Equal(t, map[string]string{"cdemo.so": md5Hex("ELF 1.0-1")}, rm.Lib, "rock_manifest.lib")

	// A re-deploy of the installed version (a re-make) overwrites in place.
	tr.Provider = func(string) (string, string, bool) { return demoPkg, "1.0-1", true }

	_, err = tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"demo.lua", "demo/helper.lua"}, deployedFiles(t, tr.DeployLuaDir()),
		"a re-deploy must leave no ~ backup")
	assert.Equal(t, []string{"cdemo.so"}, deployedFiles(t, tr.DeployLibDir()),
		"a re-deploy must leave no ~ backup")
}

// TestDeploy_BuiltinUpgradeKeepsDemotedFile — on an upgrade the new version
// takes the plain path and the old version's file is demoted to its versioned
// name. The second visit of a builtin-staged file used to demote again,
// renaming the NEW version's file over the old version's demoted one.
func TestDeploy_BuiltinUpgradeKeepsDemotedFile(t *testing.T) {
	t.Parallel()

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	oldSpec, oldSrc, oldBuild := stageBuiltinRock(t, "1.0-1")
	_, err = tr.Deploy(oldSpec, oldSrc, oldBuild)
	require.NoError(t, err)

	tr.Provider = func(string) (string, string, bool) { return demoPkg, "1.0-1", true }

	newSpec, newSrc, newBuild := stageBuiltinRock(t, "2.0-1")
	_, err = tr.Deploy(newSpec, newSrc, newBuild)
	require.NoError(t, err)

	for rel, want := range map[string]string{
		"demo.lua":                   "return '2.0-1'",
		"demo/helper.lua":            "return 'helper 2.0-1'",
		"demo_1_0_1-demo.lua":        "return '1.0-1'",
		"demo_1_0_1-demo/helper.lua": "return 'helper 1.0-1'",
	} {
		got, err := os.ReadFile(filepath.Join(tr.DeployLuaDir(), filepath.FromSlash(rel))) //nolint:gosec // test temp path
		if assert.NoError(t, err, "read %s", rel) {
			assert.Equal(t, want, string(got), "content of %s", rel)
		}
	}

	assert.ElementsMatch(t, []string{"cdemo.so", "demo_1_0_1-cdemo.so"}, deployedFiles(t, tr.DeployLibDir()),
		"lib deploy dir after the upgrade")

	got, err := os.ReadFile(filepath.Join(tr.DeployLibDir(), "demo_1_0_1-cdemo.so")) //nolint:gosec // test temp path
	require.NoError(t, err, "read the demoted C module")
	assert.Equal(t, "ELF 1.0-1", string(got), "the demoted C module must be the old version's")
}

// TestDeploy_InitLuaModuleKeepsItsName — upstream builtin deploys a
// build.modules .lua source named init.lua as <module>/init.lua unless the
// module name already ends in ".init" (builtin.lua:298-306), and the builtin
// backend stages it there. Deploy used to put it at <module>.lua as well, so
// the rock deployed the module twice, once at a path upstream never writes.
func TestDeploy_InitLuaModuleKeepsItsName(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	buildDir := t.TempDir()

	for _, rel := range []string{"demo/init.lua", "demo/util.lua", "other/init.lua"} {
		writeFile(t, filepath.Join(src, "src", rel), "return '"+rel+"'")
		writeFile(t, filepath.Join(buildDir, "lua", rel), "return '"+rel+"'")
	}

	tr, err := tree.Open(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: demoPkg,
		Version: "1.0-1",
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				demoPkg:      {Path: "src/demo/init.lua"},
				"demo.util":  {Path: "src/demo/util.lua"},
				"other.init": {Path: "src/other/init.lua"},
			},
		},
	}

	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)

	want := []string{"demo/init.lua", "demo/util.lua", "other/init.lua"}
	assert.ElementsMatch(t, want, deployedFiles(t, tr.DeployLuaDir()), "deployed lua files")

	got := make([]string, 0, len(rm.Lua))
	for rel := range rm.Lua {
		got = append(got, rel)
	}

	assert.ElementsMatch(t, want, got, "rock_manifest.lua")
}

// md5Hex returns the md5 hex digest a rock_manifest records for body.
func md5Hex(body string) string {
	sum := md5.Sum([]byte(body))

	return hex.EncodeToString(sum[:])
}

// deployedFiles returns the slash-separated paths of every regular file under
// root, relative to it.
func deployedFiles(t *testing.T, root string) []string {
	t.Helper()

	var got []string

	require.NoError(t, filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		got = append(got, filepath.ToSlash(rel))

		return nil
	}), "walk %q", root)

	return got
}

// TestDeploy_PromotesHigherVersion — glr-5e9: installing a HIGHER version over
// an existing active one promotes the new version to the plain (active) path
// and demotes the previous provider's file to its versioned name.
func TestDeploy_PromotesHigherVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "demo.lua"), "v2")

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
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "demo.lua"}}},
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
	writeFile(t, filepath.Join(src, "demo.lua"), "v1old")

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
		Build:   rocks.Build{Modules: map[string]rocks.Module{demoPkg: {Path: "demo.lua"}}},
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

// TestModuleIndex — TNTP-9962: the tree manifest's module index is derived from
// what was deployed (the RockManifest), not from the rockspec's build.modules,
// which only the builtin backend fills in.
func TestModuleIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rm   *rocks.RockManifest
		want map[string]string
	}{
		{"nil manifest", nil, map[string]string{}},
		{"empty manifest", &rocks.RockManifest{}, map[string]string{}},
		{
			// The vshard shape: a cmake rock with no build.modules at all.
			name: "deployed lua tree",
			rm: &rocks.RockManifest{Lua: map[string]string{
				"vshard/init.lua":   "d1",
				"vshard/cfg.lua":    "d2",
				"vshard/router.lua": "d3",
			}},
			want: map[string]string{
				"vshard.init":   "vshard/init.lua",
				"vshard.cfg":    "vshard/cfg.lua",
				"vshard.router": "vshard/router.lua",
			},
		},
		{
			name: "lib entries and a top-level module",
			rm: &rocks.RockManifest{
				Lua: map[string]string{"demo.lua": "d1"},
				Lib: map[string]string{"demo/native.so": "d2"},
			},
			want: map[string]string{"demo": "demo.lua", "demo.native": "demo/native.so"},
		},
		{
			// A .so and a .lua providing the same module name: the .so wins,
			// the preference the rockspec-driven index has always encoded.
			name: "so beats lua for the same module",
			rm: &rocks.RockManifest{
				Lua: map[string]string{"demo.lua": "d1"},
				Lib: map[string]string{"demo.so": "d2"},
			},
			want: map[string]string{"demo": "demo.so"},
		},
		{
			// A munged key belongs to an install that lost the plain spot; it
			// must not be reported as a module (its name would be mangled and
			// it is not the active provider).
			name: "munged key skipped",
			rm: &rocks.RockManifest{Lua: map[string]string{
				"demo/init.lua":              "d1",
				"demo_1_0_0_1-other/one.lua": "d2",
			}},
			want: map[string]string{"demo.init": "demo/init.lua"},
		},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, tree.ModuleIndex(c.rm, demoPkg, "1.0.0-1"), c.name)
	}
}

// TestModuleIndex_InvertsMungedPath pins the munged-key skip to the formation
// MungedPath actually produces, rather than to a hand-written prefix.
func TestModuleIndex_InvertsMungedPath(t *testing.T) {
	t.Parallel()

	p := tree.Paths{Tree: "/tmp/r"}

	plain := filepath.Join(p.DeployLuaDir(), "demo", "init.lua")
	munged := tree.MungedPath(p.DeployLuaDir(), plain, demoPkg, "1.0.0-1")

	rel, err := filepath.Rel(p.DeployLuaDir(), munged)
	require.NoError(t, err)

	rm := &rocks.RockManifest{Lua: map[string]string{filepath.ToSlash(rel): "d1"}}
	assert.Empty(t, tree.ModuleIndex(rm, demoPkg, "1.0.0-1"),
		"a MungedPath-formed key must not be indexed as a module")
}

// TestCommandIndex: the tree manifest's per-rock command index is derived from
// the rock_manifest's bin section, keyed and valued by the path under the bin
// dir, as upstream repos.package_commands does — so a build.install.bin entry
// is not valued by its source path.
func TestCommandIndex(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rm   *rocks.RockManifest
		want map[string]string
	}{
		{"nil manifest", nil, map[string]string{}},
		{"no commands", &rocks.RockManifest{Lua: map[string]string{"demo.lua": "d1"}}, map[string]string{}},
		{
			name: "commands with and without a directory",
			rm:   &rocks.RockManifest{Bin: map[string]string{"demo-cli": "d1", "tools/demo": "d2"}},
			want: map[string]string{"demo-cli": "demo-cli", "tools/demo": "tools/demo"},
		},
		{
			name: "munged key skipped",
			rm:   &rocks.RockManifest{Bin: map[string]string{"demo-cli": "d1", "demo_1_0_0_1-old-cli": "d2"}},
			want: map[string]string{"demo-cli": "demo-cli"},
		},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, tree.CommandIndex(c.rm, demoPkg, "1.0.0-1"), c.name)
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750), "mkdir %q", filepath.Dir(p))
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600), "write %q", p)
}
