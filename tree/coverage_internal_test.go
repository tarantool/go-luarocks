package tree

// White-box tests exercising unexported helpers directly, plus Deploy/Open/
// DeleteVersion error paths that are awkward to trigger from outside the
// package. Kept separate from the black-box tree_test.go suite so the
// exported-API test style there stays untouched.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
)

// testPkg and testVer are the rock name/version reused across these tests
// as package, module, and file basename.
const (
	testPkg = "demo"
	testVer = "1.0-1"
)

// writeInternal mirrors the tree_test.go writeFile helper (unexported test
// package, so it can't be reused directly).
func writeInternal(t *testing.T, p, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(p), dirPerm), "mkdir %q", filepath.Dir(p))
	require.NoError(t, os.WriteFile(p, []byte(body), filePerm), "write %q", p)
}

// readOnlyDir makes the directory path read-only (0o500) and records the
// change to be undone (writable again) before t.TempDir's own cleanup tries
// to remove the tree; t.Cleanup runs LIFO, so registering this after
// t.TempDir() makes it run first.
func readOnlyDir(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o500))
	t.Cleanup(func() { _ = os.Chmod(path, dirPerm) })
}

// TestWrapsCommands: a rock's commands are wrapped unless its rockspec sets
// deploy.wrap_bin_scripts = false.
func TestWrapsCommands(t *testing.T) {
	t.Parallel()

	f := false
	tr := true

	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"absent defaults to wrap", nil, true},
		{"explicit false opts out", &f, false},
		{"explicit true wraps", &tr, true},
	}
	for _, c := range cases {
		spec := &rocks.Rockspec{Deploy: rocks.Deploy{WrapBinScripts: c.val}}
		require.Equal(t, c.want, wrapsCommands(spec), c.name)
	}
}

func TestVersionGreater(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		a, b string
		want bool
	}{
		{"greater", "2.0", "1.0", true},
		{"lesser", "1.0", "2.0", false},
		{"equal", "1.0", "1.0", false},
		{"invalid a", "", "1.0", false},
		{"invalid b", "1.0", "", false},
		{"both invalid", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, c.want, versionGreater(c.a, c.b), c.name)
		})
	}
}

// providersOf returns a Providers lookup answering lists for module items
// (and nothing for commands) from mods.
func providersOf(mods map[string][]string) func(ItemKind, string) []string {
	return func(kind ItemKind, item string) []string {
		if kind != ModuleItem {
			return nil
		}

		return mods[item]
	}
}

func TestCurrentProvider(t *testing.T) {
	t.Parallel()

	t.Run("nil providers", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{}
		name, ver, ok := tr.currentProvider(deployLua, "demo.lua")
		assert.False(t, ok)
		assert.Empty(t, name)
		assert.Empty(t, ver)
	})

	t.Run("direct hit", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Providers: providersOf(map[string][]string{testPkg: {testPkg + "/" + testVer, "other/1.0-1"}})}
		name, ver, ok := tr.currentProvider(deployLua, "demo.lua")
		assert.True(t, ok)
		assert.Equal(t, testPkg, name)
		assert.Equal(t, testVer, ver)
	})

	t.Run("init fallback hit", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Providers: providersOf(map[string][]string{testPkg: {testPkg + "/" + testVer}})}
		name, ver, ok := tr.currentProvider(deployLua, "demo/init.lua")
		assert.True(t, ok)
		assert.Equal(t, testPkg, name)
		assert.Equal(t, testVer, ver)
	})

	t.Run("init fallback is for lua files only", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Providers: providersOf(map[string][]string{testPkg: {testPkg + "/" + testVer}})}
		_, _, ok := tr.currentProvider(deployLib, "demo/init.so")
		assert.False(t, ok, "upstream checks the legacy .init name for deploy_type lua alone")
	})

	t.Run("init fallback miss", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Providers: providersOf(nil)}
		_, _, ok := tr.currentProvider(deployLua, "demo/init.lua")
		assert.False(t, ok)
	})

	t.Run("commands are looked up as commands", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Providers: func(kind ItemKind, item string) []string {
			if kind == CommandItem && item == "tools/demo" {
				return []string{"cli/3.0-1"}
			}

			return nil
		}}
		name, ver, ok := tr.currentProvider(deployBin, "tools/demo")
		assert.True(t, ok)
		assert.Equal(t, "cli", name)
		assert.Equal(t, "3.0-1", ver)
	})
}

func TestCounterpartExts(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "a/b.so", withSoExt("a/b.lua"))
	assert.Equal(t, "a/b.txt", withSoExt("a/b.txt"), "another extension is left alone")
	assert.Equal(t, "a/b.lua", withLuaExt("a/b.so"))
	assert.Equal(t, "a/b", withLuaExt("a/b"), "no extension: left alone")
	assert.Equal(t, "a.lua", withLuaExt("a.b/c"), "Lua's %.[^.]+$ crosses the slash")
}

// TestResolveSpot_UntrackedFileBackedUp: a plain-path file that exists but has
// no recorded provider is renamed to a "~" backup rather than clobbered.
func TestResolveSpot_UntrackedFileBackedUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{Paths: Paths{Tree: dir}}
	plain := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	writeInternal(t, plain, "untracked")

	got, err := tr.resolveSpot(deployLua, "demo.lua", testPkg, testVer, nil)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	backup := plain + "~"
	require.FileExists(t, backup)
	b, err := os.ReadFile(backup) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "untracked", string(b))
}

// TestResolveSpot_BackupKeepsEarlierBackups is the regression test for the
// clobbered backup: upstream backup_existing appends "~" until the name is
// free (repos.lua:276-279), so a backup left by an earlier deploy survives. A
// single os.Rename to <target>~ overwrote it.
func TestResolveSpot_BackupKeepsEarlierBackups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{Paths: Paths{Tree: dir}}
	plain := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	writeInternal(t, plain, "untracked")
	writeInternal(t, plain+"~", "first backup")
	writeInternal(t, plain+"~~", "second backup")

	_, err := tr.resolveSpot(deployLua, "demo.lua", testPkg, testVer, nil)
	require.NoError(t, err)

	for p, want := range map[string]string{
		plain + "~":   "first backup",
		plain + "~~":  "second backup",
		plain + "~~~": "untracked",
	} {
		b, err := os.ReadFile(p) //nolint:gosec // test temp path
		require.NoError(t, err, "read %s", p)
		assert.Equal(t, want, string(b), p)
	}
}

// TestResolveSpot_VersionedTargetBackedUp: upstream backs up whatever sits at
// the target whenever its provider is another rock or version
// (repos.lua:480-484), including a versioned target. Writing the versioned
// name over an existing file lost it.
func TestResolveSpot_VersionedTargetBackedUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths:     Paths{Tree: dir},
		Providers: providersOf(map[string][]string{testPkg: {testPkg + "/2.0-1"}}),
	}
	versioned := MungedPath(tr.DeployLuaDir(), filepath.Join(tr.DeployLuaDir(), "demo.lua"), testPkg, testVer)
	writeInternal(t, versioned, "stale")

	got, err := tr.resolveSpot(deployLua, "demo.lua", testPkg, testVer, nil)
	require.NoError(t, err)
	assert.Equal(t, versioned, got, "an older version takes the versioned name")
	assert.FileExists(t, versioned+"~", "the file already there is backed up")
}

// TestResolveSpot_ReinstallSameVersion: when the current provider is the very
// name and version being deployed, the file at the target is removed for the
// caller to write afresh — no backup (repos.lua:270-274), and the plain path is
// kept.
func TestResolveSpot_ReinstallSameVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths:     Paths{Tree: dir},
		Providers: providersOf(map[string][]string{testPkg: {testPkg + "/" + testVer}}),
	}
	plain := filepath.Join(tr.DeployLuaDir(), "demo.lua")
	writeInternal(t, plain, "old bytes")

	got, err := tr.resolveSpot(deployLua, "demo.lua", testPkg, testVer, map[string]bool{})
	require.NoError(t, err)
	assert.Equal(t, plain, got)
	assert.NoFileExists(t, plain+"~")
}

// TestResolveSpot_DemotesTheModulesOtherForm: when the new rock takes the plain
// path of a module, the current provider's file of the other form — the .so of
// a .lua, the .lua of a .so — is demoted with it (repos.lua:495-500, 514-519),
// except a file this Deploy already wrote.
func TestResolveSpot_DemotesTheModulesOtherForm(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		dt, rel    string
		otherDT    string
		otherRel   string
		otherIsNew bool
	}{
		{"lua demotes the so", deployLua, "pair/impl.lua", deployLib, "pair/impl.so", false},
		{"so demotes the lua", deployLib, "pair/impl.so", deployLua, "pair/impl.lua", false},
		{"the new rock's own file stays", deployLib, "pair/impl.so", deployLua, "pair/impl.lua", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tr := &Tree{
				Paths:     Paths{Tree: t.TempDir()},
				Providers: providersOf(map[string][]string{"pair.impl": {"pair/1.0-1"}}),
			}
			other := filepath.Join(tr.deployDirOf(tc.otherDT), tc.otherRel)
			writeInternal(t, other, "1.0")

			written := map[string]bool{}
			if tc.otherIsNew {
				written[other] = true
			}

			_, err := tr.resolveSpot(tc.dt, tc.rel, "pair", "2.0-1", written)
			require.NoError(t, err)

			demoted := MungedPath(tr.deployDirOf(tc.otherDT), other, "pair", "1.0-1")
			if tc.otherIsNew {
				assert.FileExists(t, other)
				assert.NoFileExists(t, demoted)
			} else {
				assert.NoFileExists(t, other)
				assert.FileExists(t, demoted)
			}
		})
	}
}

// TestResolveSpot_DemoteMkdirError covers the demote branch's MkdirAll
// failure: the new file's plain target lives in a subdirectory whose munged
// sibling directory must be freshly created, but the parent deploy dir is
// read-only.
func TestResolveSpot_DemoteMkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths:     Paths{Tree: dir},
		Providers: providersOf(map[string][]string{"foo.demo": {"other/" + testVer}}),
	}
	deployDir := tr.DeployLuaDir()
	writeInternal(t, filepath.Join(deployDir, "foo", "demo.lua"), "old bytes")

	// deployDir must stay read-only until after resolveSpot runs; register the
	// restore before chmod so cleanup can remove the tree afterward.
	readOnlyDir(t, deployDir)

	_, err := tr.resolveSpot(deployLua, "foo/demo.lua", testPkg, "2.0-1", nil)
	require.Error(t, err)
}

// TestResolveSpot_DemoteRenameError covers the demote branch's os.Rename
// failure: the munged destination's directory cannot be created because a
// plain file occupies its path.
func TestResolveSpot_DemoteRenameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths:     Paths{Tree: dir},
		Providers: providersOf(map[string][]string{"foo.demo": {"other/" + testVer}}),
	}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "foo", "demo.lua")
	writeInternal(t, plain, "old bytes")

	demoted := MungedPath(deployDir, plain, "other", testVer)
	writeInternal(t, filepath.Dir(demoted), "a file where the directory goes")

	_, err := tr.resolveSpot(deployLua, "foo/demo.lua", testPkg, "2.0-1", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demote")
}

// TestResolveSpot_BackupRenameError covers the backup branch's os.Rename
// failure: the deploy dir is read-only, so the untracked file cannot be moved.
func TestResolveSpot_BackupRenameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{Paths: Paths{Tree: dir}}
	deployDir := tr.DeployLuaDir()
	writeInternal(t, filepath.Join(deployDir, "demo.lua"), "untracked")

	readOnlyDir(t, deployDir)

	_, err := tr.resolveSpot(deployLua, "demo.lua", testPkg, testVer, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup")
}

func TestIsLua_MissingFile(t *testing.T) {
	t.Parallel()

	assert.False(t, isLua(filepath.Join(t.TempDir(), "nope.lua")))
}

// TestIsLua_Shebang — upstream fs.is_lua asks the interpreter's loadfile,
// which skips a first line starting with '#', so a "#!" Lua script is Lua
// (and gets wrapped) while a "#!" shell script still is not.
func TestIsLua_Shebang(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"lua script", "#!/usr/bin/env tarantool\nprint('hi')\n", true},
		{"shebang only", "#!/usr/bin/env tarantool", true},
		{"shell script", "#!/bin/sh\necho hi\n", false},
		{"no shebang", "print('hi')\n", true},
		{"second shebang", "#!/usr/bin/env tarantool\n#!/usr/bin/env tarantool\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := filepath.Join(t.TempDir(), "script")
			require.NoError(t, os.WriteFile(p, []byte(tc.body), 0o600))
			assert.Equal(t, tc.want, isLua(p))
		})
	}
}

func TestDeployDirOf(t *testing.T) {
	t.Parallel()

	tr := &Tree{Paths: Paths{Tree: "/opt/tt"}}
	assert.Equal(t, tr.DeployLuaDir(), tr.deployDirOf(deployLua))
	assert.Equal(t, tr.DeployLibDir(), tr.deployDirOf(deployLib))
	assert.Equal(t, tr.BinDir(), tr.deployDirOf(deployBin))
}

func TestLuaQuote(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", `"hello"`},
		{"quote", `he"llo`, `"he\"llo"`},
		{"backslash", `he\llo`, `"he\\llo"`},
		{"newline", "he\nllo", `"he\nllo"`},
		{"carriage return", "he\rllo", `"he\rllo"`},
		{"nul", "he\x00llo", `"he\0llo"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, c.want, luaQuote(c.in), c.name)
		})
	}
}

func TestModuleToPathDir(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "a/b/", moduleToPathDir("a.b.c"))
	assert.Empty(t, moduleToPathDir("foo"))
}

func TestModuleLastSegment(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "helper", moduleLastSegment("withinstall.helper"))
	assert.Equal(t, "foo", moduleLastSegment("foo"))
}

func TestInstallDest(t *testing.T) {
	t.Parallel()

	assert.Equal(t, filepath.Join("/dir", "bin", "tool"), installDest("/dir", "bin/tool", "src/tool", false))

	// Module-path form: dotted key, .lua source renamed to the last segment.
	got := installDest("/dir", "a.b.helper", "src/helper_impl.lua", true)
	assert.Equal(t, filepath.Join("/dir", "a", "b", "helper.lua"), got)

	// Module-path form, non-.lua source keeps its basename.
	got = installDest("/dir", "a.b.native", "src/native.so", true)
	assert.Equal(t, filepath.Join("/dir", "a", "b", "native.so"), got)

	// Module-path form, no dot in the key: subdir is empty.
	got = installDest("/dir", "top", "src/top.lua", true)
	assert.Equal(t, filepath.Join("/dir", "top.lua"), got)
}

func TestResolveModule(t *testing.T) {
	t.Parallel()

	p := Paths{Tree: "/opt/tt"}

	t.Run("lua path", func(t *testing.T) {
		t.Parallel()

		srcRoot, src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.lua"}, p)
		require.NoError(t, err)
		assert.Equal(t, "/src", srcRoot)
		assert.Equal(t, filepath.Join("/src", "foo/bar.lua"), src)
		assert.Equal(t, filepath.Join(p.DeployLuaDir(), "foo", "bar.lua"), dst)
		assert.Equal(t, kindLua, kind)
	})

	t.Run("dylib path", func(t *testing.T) {
		t.Parallel()

		srcRoot, src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.dylib"}, p)
		require.NoError(t, err)
		assert.Equal(t, "/src", srcRoot)
		assert.Equal(t, filepath.Join("/src", "foo/bar.dylib"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("c source path", func(t *testing.T) {
		t.Parallel()

		srcRoot, src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.c"}, p)
		require.NoError(t, err)
		assert.Equal(t, "/build", srcRoot)
		assert.Equal(t, filepath.Join("/build", "lib", "foo", "bar.so"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("table form sources", func(t *testing.T) {
		t.Parallel()

		srcRoot, src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Sources: []string{"a.c"}}, p)
		require.NoError(t, err)
		assert.Equal(t, "/build", srcRoot)
		assert.Equal(t, filepath.Join("/build", "lib", "foo", "bar.so"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("unsupported extension", func(t *testing.T) {
		t.Parallel()

		_, _, _, _, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.xyz"}, p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported source extension")
	})

	t.Run("neither path nor sources", func(t *testing.T) {
		t.Parallel()

		_, _, _, _, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{}, p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "neither Path nor Sources set")
	})
}

func TestCopyFile_Errors(t *testing.T) {
	t.Parallel()

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		_, err := copyFile(dir, filepath.Join(dir, "nope"), dir, filepath.Join(dir, "out"), filePerm)
		require.Error(t, err)
	})

	t.Run("mkdir blocked by file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src.txt")
		writeInternal(t, src, "hi")

		blocker := filepath.Join(dir, "blocker")
		writeInternal(t, blocker, "not a dir")

		_, err := copyFile(dir, src, dir, filepath.Join(blocker, "sub", "out"), filePerm)
		require.Error(t, err)
	})

	t.Run("dst is directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src.txt")
		writeInternal(t, src, "hi")

		dstDir := filepath.Join(dir, "out")
		require.NoError(t, os.MkdirAll(dstDir, dirPerm))

		_, err := copyFile(dir, src, dir, dstDir, filePerm)
		require.Error(t, err)
	})

	t.Run("src is directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		srcDir := filepath.Join(dir, "srcdir")
		require.NoError(t, os.MkdirAll(srcDir, dirPerm))

		_, err := copyFile(dir, srcDir, dir, filepath.Join(dir, "out"), filePerm)
		require.Error(t, err)
	})
}

func TestCopyTree_Errors(t *testing.T) {
	t.Parallel()

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		err := copyTree(dir, filepath.Join(dir, "nope"), dir, filepath.Join(dir, "out"))
		require.Error(t, err)
	})

	t.Run("source is a file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "notadir")
		writeInternal(t, src, "hi")

		err := copyTree(dir, src, dir, filepath.Join(dir, "out"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})
}

// TestUpdate_MkdirError: a tree whose root is a file opens, and fails the
// first Update, which creates its rocks dir.
func TestUpdate_MkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	writeInternal(t, blocked, "not a dir")

	tr, err := Open(rocks.Config{Tree: blocked})
	require.NoError(t, err)

	err = tr.Update(context.Background(), func(*Tx) error { return nil })
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tree.Update: mkdir")
}

// TestDeploy_NilAndInvalidSpec covers Deploy's own guard-clause errors.
func TestDeploy_NilAndInvalidSpec(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	_, err = tr.Deploy(nil, "", "")
	require.Error(t, err)

	_, err = tr.Deploy(&rocks.Rockspec{}, "", "")
	require.Error(t, err)

	_, err = tr.Deploy(&rocks.Rockspec{Package: testPkg}, "", "")
	require.Error(t, err, "missing Version should still error")
}

// TestDeploy_RockspecMkdirError forces deployRockspec's install-dir mkdir to
// fail: the rock's name directory already exists as a plain file.
func TestDeploy_RockspecMkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	writeInternal(t, filepath.Join(tr.RocksDir(), testPkg), "blocking file")

	spec := &rocks.Rockspec{
		Package:   testPkg,
		Version:   testVer,
		RawSource: []byte("package=\"demo\""),
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir install dir")
}

// TestDeploy_RockspecWriteError forces deployRockspec's WriteFile to fail:
// the destination rockspec path already exists as a directory.
func TestDeploy_RockspecWriteError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package:   testPkg,
		Version:   testVer,
		RawSource: []byte("package=\"demo\""),
	}
	dst := filepath.Join(tr.InstallDir(testPkg, testVer), "demo-1.0-1.rockspec")
	require.NoError(t, os.MkdirAll(dst, dirPerm))

	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write rockspec")
}

// TestDeploy_ModuleResolveError propagates resolveModule's unsupported-
// extension error out through deployModules and Deploy.
func TestDeploy_ModuleResolveError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{Modules: map[string]rocks.Module{testPkg: {Path: "demo.xyz"}}},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported source extension")
}

// TestDeploy_ModuleCopyError propagates copyFile's missing-source error out
// through deployModules and Deploy.
func TestDeploy_ModuleCopyError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir() // demo.lua deliberately absent.

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{Modules: map[string]rocks.Module{testPkg: {Path: "demo.lua"}}},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "module \"demo\"")
}

// TestDeploy_InstallModulePathsCopyError propagates copyFile's missing-
// source error through deployInstallModulePaths and Deploy.
func TestDeploy_InstallModulePathsCopyError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir() // extra.lua deliberately absent.

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Lua: map[string]string{"extra": "extra.lua"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install \"extra\"")
}

// TestDeploy_InstallConfCopyError propagates copyFile's missing-source error
// through deployInstallConf and Deploy.
func TestDeploy_InstallConfCopyError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir() // demo.cfg deliberately absent.

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Conf: map[string]string{"demo.cfg": "demo.cfg"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install conf")
}

// TestDeploy_InstallBinCopyError propagates copyFile's missing-source error
// through the verbatim branch of deployInstallBin.
func TestDeploy_InstallBinCopyError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir() // scripts/tool deliberately absent.

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Bin: map[string]string{"tool": "scripts/tool"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "install bin")
}

// TestDeploy_InstallBinWrappedResolveSpotError forces the wrapped-bin
// branch's resolveSpot call to fail: the wrapper's plain target is an
// untracked file in a read-only bin dir, so the backup rename cannot succeed.
func TestDeploy_InstallBinWrappedResolveSpotError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeInternal(t, filepath.Join(src, "scripts", testPkg), "print('hi')\n")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	tr.BinWrap = &BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	// The wrapper's plain target is an untracked existing file, so resolveSpot
	// attempts to back it up to "demo~" — in a bin dir it cannot write.
	writeInternal(t, filepath.Join(tr.BinDir(), testPkg), "untracked launcher")
	readOnlyDir(t, tr.BinDir())

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Bin: map[string]string{testPkg: "scripts/demo"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup")
}

// TestDeploy_InstallBinWrapperMkdirError forces the wrapped-bin branch's
// wrapper write to fail: BinDir is read-only so the new subdirectory the
// wrapper needs cannot be created.
func TestDeploy_InstallBinWrapperMkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeInternal(t, filepath.Join(src, "scripts", testPkg), "print('hi')\n")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	tr.BinWrap = &BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	require.NoError(t, os.MkdirAll(tr.BinDir(), dirPerm))
	readOnlyDir(t, tr.BinDir())

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Bin: map[string]string{"sub/demo": "scripts/demo"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write bin wrapper")
}

// TestDeploy_CopyDirectoriesError propagates copyTree's stat error through
// deployCopyDirectories and Deploy.
func TestDeploy_CopyDirectoriesError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir() // "doc" deliberately absent.

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{CopyDirectories: []string{"doc"}},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "copy_directories")
}

// TestDeploy_InstalledSubtreeError propagates deployInstalledSubtree's error
// (via a resolveSpot backup-rename failure) through deployInstalledSubtrees and
// Deploy.
func TestDeploy_InstalledSubtreeError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	buildDir := t.TempDir()

	writeInternal(t, filepath.Join(buildDir, "lua", "demo.lua"), "return 1")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	// The deployed target is an untracked existing file, so resolveSpot
	// attempts to back it up to "demo.lua~" — in a deploy dir it cannot write.
	writeInternal(t, filepath.Join(tr.DeployLuaDir(), "demo.lua"), "untracked")
	readOnlyDir(t, tr.DeployLuaDir())

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{Type: "make"},
	}
	_, err = tr.Deploy(spec, src, buildDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan installed lua subtree")
}

// TestDeploy_InstalledSubtreeMissingIsNoop covers deployInstalledSubtree's
// no-subtree short-circuit: a make build that only populates buildDir/lua
// leaves buildDir/lib entirely absent, which must not be an error.
func TestDeploy_InstalledSubtreeMissingIsNoop(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	buildDir := t.TempDir()

	writeInternal(t, filepath.Join(buildDir, "lua", "demo.lua"), "return 1")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{Type: "make"},
	}
	rm, err := tr.Deploy(spec, src, buildDir)
	require.NoError(t, err)
	assert.Contains(t, rm.Lua, "demo.lua")
	assert.Empty(t, rm.Lib, "no lib subtree was installed")
}

// TestDeleteVersion_MissingManifestBestEffort covers DeleteVersion's
// best-effort manifest read: a missing rock_manifest must not fail the
// call, and the install dir is still removed.
func TestDeleteVersion_MissingManifestBestEffort(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	installDir := tr.InstallDir(testPkg, testVer)
	writeInternal(t, filepath.Join(installDir, "leftover"), "x")

	require.NoError(t, tr.DeleteVersion(testPkg, testVer))
	assert.NoDirExists(t, installDir)
}

// TestDeleteVersion_KeepsNameDirWithRemainingVersion covers the prune
// branch's "not empty" path: deleting one version must not remove the
// rock's name directory while a sibling version remains installed.
func TestDeleteVersion_KeepsNameDirWithRemainingVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	writeInternal(t, filepath.Join(tr.InstallDir(testPkg, testVer), "leftover"), "x")
	writeInternal(t, filepath.Join(tr.InstallDir(testPkg, "2.0-1"), "leftover"), "x")

	require.NoError(t, tr.DeleteVersion(testPkg, testVer))

	nameDir := filepath.Join(tr.RocksDir(), testPkg)
	assert.DirExists(t, nameDir, "name dir must survive while a sibling version remains")
	assert.DirExists(t, tr.InstallDir(testPkg, "2.0-1"))
}
