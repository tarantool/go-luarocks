package tree

// White-box tests exercising unexported helpers directly, plus Deploy/Open/
// DeleteVersion error paths that are awkward to trigger from outside the
// package. Kept separate from the black-box tree_test.go suite so the
// exported-API test style there stays untouched.

import (
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

// chmodCleanup records a permission change that must be undone (writable
// again) before t.TempDir's own cleanup tries to remove the tree; t.Cleanup
// runs LIFO, so registering this after t.TempDir() makes it run first.
func chmodCleanup(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	require.NoError(t, os.Chmod(path, mode))
	t.Cleanup(func() { _ = os.Chmod(path, dirPerm) })
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

func TestCurrentProvider(t *testing.T) {
	t.Parallel()

	t.Run("nil provider", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{}
		name, ver, ok := tr.currentProvider(testPkg)
		assert.False(t, ok)
		assert.Empty(t, name)
		assert.Empty(t, ver)
	})

	t.Run("direct hit", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Provider: func(item string) (string, string, bool) {
			if item == testPkg {
				return testPkg, testVer, true
			}

			return "", "", false
		}}
		name, ver, ok := tr.currentProvider(testPkg)
		assert.True(t, ok)
		assert.Equal(t, testPkg, name)
		assert.Equal(t, testVer, ver)
	})

	t.Run("init fallback hit", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Provider: func(item string) (string, string, bool) {
			if item == testPkg {
				return testPkg, testVer, true
			}

			return "", "", false
		}}
		name, ver, ok := tr.currentProvider("demo.init")
		assert.True(t, ok)
		assert.Equal(t, testPkg, name)
		assert.Equal(t, testVer, ver)
	})

	t.Run("init fallback miss", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Provider: func(string) (string, string, bool) {
			return "", "", false
		}}
		_, _, ok := tr.currentProvider("demo.init")
		assert.False(t, ok)
	})

	t.Run("no suffix miss", func(t *testing.T) {
		t.Parallel()

		tr := &Tree{Provider: func(string) (string, string, bool) {
			return "", "", false
		}}
		_, _, ok := tr.currentProvider(testPkg)
		assert.False(t, ok)
	})
}

// TestResolveSpot_UntrackedFileBackedUp covers the !ok branch: a plain-path
// file that exists but has no recorded provider is renamed to a "~" backup
// rather than clobbered.
func TestResolveSpot_UntrackedFileBackedUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{Paths: Paths{Tree: dir}}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "demo.lua")
	writeInternal(t, plain, "untracked")

	got, err := tr.resolveSpot(deployDir, plain, testPkg, testPkg, testVer)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	backup := plain + "~"
	require.FileExists(t, backup)
	b, err := os.ReadFile(backup) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "untracked", string(b))
}

// TestResolveSpot_ReinstallSameVersion covers the ok==true, same name+version
// branch: neither the demote nor the backup case fires, and the file is left
// for the caller to overwrite in place.
func TestResolveSpot_ReinstallSameVersion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths: Paths{Tree: dir},
		Provider: func(item string) (string, string, bool) {
			if item == testPkg {
				return testPkg, testVer, true
			}

			return "", "", false
		},
	}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "demo.lua")
	writeInternal(t, plain, "old bytes")

	got, err := tr.resolveSpot(deployDir, plain, testPkg, testPkg, testVer)
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	// Untouched: no backup, no demotion — the file is exactly as written.
	assert.NoFileExists(t, plain+"~")
	b, err := os.ReadFile(plain) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "old bytes", string(b))
}

// TestResolveSpot_DemoteMkdirError covers the demote branch's MkdirAll
// failure: the new file's plain target lives in a subdirectory whose munged
// sibling directory must be freshly created, but the parent deploy dir is
// read-only.
func TestResolveSpot_DemoteMkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths: Paths{Tree: dir},
		Provider: func(item string) (string, string, bool) {
			if item == "foo.demo" {
				return "other", testVer, true
			}

			return "", "", false
		},
	}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "foo", "demo.lua")
	writeInternal(t, plain, "old bytes")

	// deployDir must stay read-only until after resolveSpot runs; register the
	// restore before chmod so cleanup can remove the tree afterward.
	chmodCleanup(t, deployDir, 0o500)

	_, err := tr.resolveSpot(deployDir, plain, "foo.demo", testPkg, "2.0-1")
	require.Error(t, err)
}

// TestResolveSpot_DemoteRenameError covers the demote branch's os.Rename
// failure: the munged destination already exists as a non-empty directory,
// which os.Rename refuses to replace with a file.
func TestResolveSpot_DemoteRenameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{
		Paths: Paths{Tree: dir},
		Provider: func(item string) (string, string, bool) {
			if item == testPkg {
				return "other", testVer, true
			}

			return "", "", false
		},
	}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "demo.lua")
	writeInternal(t, plain, "old bytes")

	demoted := MungedPath(deployDir, plain, "other", testVer)
	writeInternal(t, filepath.Join(demoted, "blocker"), "occupied")

	_, err := tr.resolveSpot(deployDir, plain, testPkg, testPkg, "2.0-1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "demote")
}

// TestResolveSpot_BackupRenameError covers the untracked-file branch's
// os.Rename failure: the "~" backup path already exists as a non-empty
// directory.
func TestResolveSpot_BackupRenameError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tr := &Tree{Paths: Paths{Tree: dir}}
	deployDir := tr.DeployLuaDir()
	plain := filepath.Join(deployDir, "demo.lua")
	writeInternal(t, plain, "untracked")
	writeInternal(t, filepath.Join(plain+"~", "blocker"), "occupied")

	_, err := tr.resolveSpot(deployDir, plain, testPkg, testPkg, testVer)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "backup")
}

func TestIsLua_MissingFile(t *testing.T) {
	t.Parallel()

	assert.False(t, isLua(filepath.Join(t.TempDir(), "nope.lua")))
}

func TestDeployDirForKind(t *testing.T) {
	t.Parallel()

	p := Paths{Tree: "/opt/tt"}
	assert.Equal(t, p.DeployLuaDir(), deployDirForKind(p, kindLua))
	assert.Equal(t, p.DeployLibDir(), deployDirForKind(p, kindLib))
	assert.Equal(t, p.Tree, deployDirForKind(p, "bogus"))
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

		src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.lua"}, p)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/src", "foo/bar.lua"), src)
		assert.Equal(t, filepath.Join(p.DeployLuaDir(), "foo", "bar.lua"), dst)
		assert.Equal(t, kindLua, kind)
	})

	t.Run("dylib path", func(t *testing.T) {
		t.Parallel()

		src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.dylib"}, p)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/src", "foo/bar.dylib"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("c source path", func(t *testing.T) {
		t.Parallel()

		src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.c"}, p)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/build", "lib", "foo", "bar.so"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("table form sources", func(t *testing.T) {
		t.Parallel()

		src, dst, kind, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Sources: []string{"a.c"}}, p)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join("/build", "lib", "foo", "bar.so"), src)
		assert.Equal(t, filepath.Join(p.DeployLibDir(), "foo", "bar.so"), dst)
		assert.Equal(t, kindLib, kind)
	})

	t.Run("unsupported extension", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{Path: "foo/bar.xyz"}, p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unsupported source extension")
	})

	t.Run("neither path nor sources", func(t *testing.T) {
		t.Parallel()

		_, _, _, err := resolveModule("/src", "/build", "foo.bar", rocks.Module{}, p)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "neither Path nor Sources set")
	})
}

func TestCopyFile_Errors(t *testing.T) {
	t.Parallel()

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		_, err := copyFile(filepath.Join(dir, "nope"), filepath.Join(dir, "out"), filePerm)
		require.Error(t, err)
	})

	t.Run("mkdir blocked by file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src.txt")
		writeInternal(t, src, "hi")

		blocker := filepath.Join(dir, "blocker")
		writeInternal(t, blocker, "not a dir")

		_, err := copyFile(src, filepath.Join(blocker, "sub", "out"), filePerm)
		require.Error(t, err)
	})

	t.Run("dst is directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "src.txt")
		writeInternal(t, src, "hi")

		dstDir := filepath.Join(dir, "out")
		require.NoError(t, os.MkdirAll(dstDir, dirPerm))

		_, err := copyFile(src, dstDir, filePerm)
		require.Error(t, err)
	})

	t.Run("src is directory", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		srcDir := filepath.Join(dir, "srcdir")
		require.NoError(t, os.MkdirAll(srcDir, dirPerm))

		_, err := copyFile(srcDir, filepath.Join(dir, "out"), filePerm)
		require.Error(t, err)
	})
}

func TestCopyTree_Errors(t *testing.T) {
	t.Parallel()

	t.Run("missing source", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		rm := &rocks.RockManifest{Doc: map[string]string{}}
		err := copyTree(filepath.Join(dir, "nope"), filepath.Join(dir, "out"), rm, "doc")
		require.Error(t, err)
	})

	t.Run("source is a file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		src := filepath.Join(dir, "notadir")
		writeInternal(t, src, "hi")

		rm := &rocks.RockManifest{Doc: map[string]string{}}
		err := copyTree(src, filepath.Join(dir, "out"), rm, "doc")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not a directory")
	})
}

func TestOpen_MkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked")
	writeInternal(t, blocked, "not a dir")

	_, err := Open(rocks.Config{Tree: blocked})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tree.Open")
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
// branch's resolveSpot call to fail: the wrapper's plain target already
// exists as a non-empty directory, so the untracked-file backup rename
// cannot succeed.
func TestDeploy_InstallBinWrappedResolveSpotError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeInternal(t, filepath.Join(src, "scripts", testPkg), "print('hi')\n")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	tr.BinWrap = &BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	// The wrapper's plain target is an untracked existing file, so resolveSpot
	// attempts to back it up to "demo~" — pre-occupy that backup path with a
	// non-empty directory so the rename fails.
	writeInternal(t, filepath.Join(tr.BinDir(), testPkg), "untracked launcher")
	writeInternal(t, filepath.Join(tr.BinDir(), "demo~", "occupied"), "x")

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
// wrapper MkdirAll to fail: BinDir is read-only so the new subdirectory the
// wrapper needs cannot be created.
func TestDeploy_InstallBinWrapperMkdirError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()

	writeInternal(t, filepath.Join(src, "scripts", testPkg), "print('hi')\n")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	tr.BinWrap = &BinWrap{Interpreter: "/usr/bin/lua", Sysconfdir: "/etc/luarocks"}

	chmodCleanup(t, tr.BinDir(), 0o500)

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build: rocks.Build{
			Install: rocks.BuildInstall{Bin: map[string]string{"sub/demo": "scripts/demo"}},
		},
	}
	_, err = tr.Deploy(spec, src, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mkdir bin")
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

// TestDeploy_MakeSubtreeError propagates deployInstalledSubtree's error
// (via a resolveSpot backup-rename failure) through deployMakeSubtrees and
// Deploy.
func TestDeploy_MakeSubtreeError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := t.TempDir()
	buildDir := t.TempDir()

	writeInternal(t, filepath.Join(buildDir, "lua", "demo.lua"), "return 1")

	tr, err := Open(rocks.Config{Tree: dir})
	require.NoError(t, err)

	// The deployed target is an untracked existing file, so resolveSpot
	// attempts to back it up to "demo.lua~" — pre-occupy that backup path
	// with a non-empty directory so the rename fails.
	writeInternal(t, filepath.Join(tr.DeployLuaDir(), "demo.lua"), "untracked")
	writeInternal(t, filepath.Join(tr.DeployLuaDir(), "demo.lua~", "occupied"), "x")

	spec := &rocks.Rockspec{
		Package: testPkg,
		Version: testVer,
		Build:   rocks.Build{Type: "make"},
	}
	_, err = tr.Deploy(spec, src, buildDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scan make lua subtree")
}

// TestDeploy_MakeSubtreeMissingIsNoop covers deployInstalledSubtree's
// no-subtree short-circuit: a make build that only populates buildDir/lua
// leaves buildDir/lib entirely absent, which must not be an error.
func TestDeploy_MakeSubtreeMissingIsNoop(t *testing.T) {
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
