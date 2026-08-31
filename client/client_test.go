package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

func TestNew_RequiresTree(t *testing.T) {
	t.Parallel()

	_, err := client.New(rocks.Config{})
	require.Error(t, err, "expected error for empty cfg.Tree")
}

func TestNew_Minimal(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")
	require.NotNil(t, r, "Rocks")
}

func TestWhich_RoutesToTree(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	// Pre-populate a fake module so tree.Which finds it.
	luaDir := filepath.Join(tree, "share", "tarantool")
	require.NoError(t, os.MkdirAll(luaDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(luaDir, "foo.lua"), []byte("return {}\n"), 0o600))

	r, err := client.New(rocks.Config{Tree: tree})
	require.NoError(t, err, "New")
	p, ok, err := r.Which(context.Background(), "foo")
	require.NoError(t, err, "Which")
	require.True(t, ok, "Which(foo) returned not-found; expected %s", filepath.Join(luaDir, "foo.lua"))
	assert.Equal(t, filepath.Join(luaDir, "foo.lua"), p, "Which path")
}

func TestWhich_Miss(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")
	_, ok, err := r.Which(context.Background(), "nope")
	require.NoError(t, err, "Which")
	assert.False(t, ok, "Which(nope) = ok; want miss")
}

func TestList_EmptyTree(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")
	list, err := r.List(context.Background())
	require.NoError(t, err, "List")
	assert.Empty(t, list, "List")
}

func TestShow_NotInstalled(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")
	_, err = r.Show(context.Background(), "metrics")
	require.Error(t, err, "expected error for not-installed rock")
	assert.ErrorIs(t, err, os.ErrNotExist, "err should wrap os.ErrNotExist")
}

func TestInstall_RejectsEmptyName(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")
	err = r.Install(context.Background(), "", client.InstallOpts{})
	require.Error(t, err, "expected error for empty name")
}

func TestPackUnpack_RoundTrip(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	workDir := t.TempDir()

	// Lay out a fake installed rock at the expected tree path.
	rocksDir := filepath.Join(tree, "share", "tarantool", "rocks", "hello", "1.0-1")
	require.NoError(t, os.MkdirAll(rocksDir, 0o750))

	specBytes := []byte("package = 'hello'\nversion = '1.0-1'\n")
	require.NoError(t, os.WriteFile(filepath.Join(rocksDir, "hello-1.0-1.rockspec"), specBytes, 0o600))

	// Write the tree manifest via manif.FileStore so the facade can read it.
	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"hello": {"1.0-1": {Arch: "installed"}},
		},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	require.NoError(t, (manif.FileStore{}).WriteTree(filepath.Join(tree, "share", "tarantool", "rocks"), m), "WriteTree")

	r, err := client.New(rocks.Config{Tree: tree, WorkingDir: workDir})
	require.NoError(t, err, "New")
	archive, err := r.Pack(context.Background(), "hello", client.PackOpts{})
	require.NoError(t, err, "Pack")
	_, err = os.Stat(archive)
	require.NoError(t, err, "Pack archive missing")

	extractDir := filepath.Join(workDir, "extracted")
	require.NoError(t, r.Unpack(context.Background(), archive, extractDir), "Unpack")
	specOut := filepath.Join(extractDir, "hello-1.0-1.rockspec")
	_, err = os.Stat(specOut)
	assert.NoError(t, err, "expected %s after Unpack", specOut)
}

// TestMake_BuiltinPureLua_EndToEnd is a regression test for two review
// Critical findings:
//
//  1. client.deployFromSource passed destDir (post-build) to tree.Deploy,
//     which then looked for install/.lua source files at the wrong path.
//     Any builtin rockspec with .lua modules or build.install.* populated
//     previously failed end-to-end at "no such file".
//  2. The facade silently emitted empty per-arch entries — top-level
//     manifest.modules and manifest.commands were never populated, so
//     upstream `luarocks show` against trees built by this library saw
//     no modules (read-path invariant violation).
//
// This test stages a small pure-Lua rockspec on disk, runs r.Make,
// and asserts the deployed files land at the expected DeployLuaDir paths
// AND the top-level manifest carries the per-arch + top-level index.
func TestMake_BuiltinPureLua_EndToEnd(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()

	// Layout: src/foo.lua, src/foo/bar.lua, extras/extras.lua, bin/foo-cli
	must := func(p, body string) {
		t.Helper()

		full := filepath.Join(src, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	must("src/foo.lua", "return {}\n")
	must("src/foo/bar.lua", "return {}\n")
	must("extras/extras.lua", "return 'extra'\n")
	must("bin/foo-cli", "#!/usr/bin/env tarantool\n")

	rockspec := `
package = "foo"
version = "1.0-1"
source = { url = "file://localhost/foo.tar.gz" }
build = {
   type = "builtin",
   modules = {
      ["foo"] = "src/foo.lua",
      ["foo.bar"] = "src/foo/bar.lua",
   },
   install = {
      lua = { ["foo.extras"] = "extras/extras.lua" },
      bin = { ["foo-cli"] = "bin/foo-cli" },
   },
}
`
	specPath := filepath.Join(src, "foo-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(rockspec), 0o600))

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: src,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
		},
	})
	require.NoError(t, err, "New")

	// Make is the right entry — uses WorkingDir as the source tree
	// without a fetch step. Build would try to fetch spec.Source.URL.
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}), "Make")

	// Modules deployed to DeployLuaDir at slashed paths.
	for _, p := range []string{"share/tarantool/foo.lua", "share/tarantool/foo/bar.lua"} {
		_, err := os.Stat(filepath.Join(tree, p))
		require.NoError(t, err, "expected deployed module at %s", p)
	}
	// install.lua / install.bin deployed. The key "foo.extras" is a DOTTED
	// MODULE NAME (upstream install_files is_module_path=true), so it deploys to
	// foo/extras.lua — not a literal file named "foo.extras".
	_, err = os.Stat(filepath.Join(tree, "share/tarantool/foo/extras.lua"))
	require.NoError(t, err, "expected install.lua entry at module path foo/extras.lua")
	_, err = os.Stat(filepath.Join(tree, "bin/foo-cli"))
	require.NoError(t, err, "expected install.bin entry")

	// Top-level manifest must carry per-arch modules + commands index.
	m, err := manif.ReadTreeManifest(filepath.Join(tree, "share/tarantool/rocks"))
	require.NoError(t, err, "ReadTreeManifest")

	entry := m.Repository["foo"]["1.0-1"]
	assert.Equal(t, "installed", entry.Arch, "arch")
	assert.NotEmpty(t, entry.Modules, "RepoEntry.Modules empty — read-path regression")
	assert.NotEmpty(t, entry.Commands["foo-cli"], "RepoEntry.Commands missing foo-cli")

	if assert.NotEmpty(t, m.Modules["foo"], "top-level m.Modules[\"foo\"] = %v, want [\"foo/1.0-1\"]", m.Modules["foo"]) {
		assert.Equal(t, "foo/1.0-1", m.Modules["foo"][0], "top-level m.Modules[\"foo\"]")
	}

	if assert.NotEmpty(t, m.Commands["foo-cli"], "top-level m.Commands[\"foo-cli\"] = %v", m.Commands["foo-cli"]) {
		assert.Equal(t, "foo/1.0-1", m.Commands["foo-cli"][0], "top-level m.Commands[\"foo-cli\"]")
	}

	// The rockspec's own modules must still be indexed under their dotted
	// names, with the deployed path as the value — deriving the index from the
	// deployed rock_manifest reproduces the rockspec-keyed loop exactly here.
	assert.Equal(t, "foo.lua", entry.Modules["foo"], "RepoEntry.Modules[foo]")
	assert.Equal(t, "foo/bar.lua", entry.Modules["foo.bar"], "RepoEntry.Modules[foo.bar]")
	// ...and covers more than it did: a build.install.lua entry is a deployed
	// module too, which upstream's repos.package_modules also reports and the
	// rockspec-keyed loop missed.
	assert.Equal(t, "foo/extras.lua", entry.Modules["foo.extras"], "RepoEntry.Modules[foo.extras]")
}

// TestMake_NonBuiltinBackendIndexesModules — TNTP-9962: a rock built by any
// backend other than builtin declares no build.modules, so an index keyed on
// that field left the tree manifest with `modules = {}` even though the rock
// deployed a full Lua tree. This is the vshard shape (cmake) reduced to the
// `command` backend so the test needs only /bin/sh.
func TestMake_NonBuiltinBackendIndexesModules(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	treeDir := t.TempDir()

	must := func(p, body string) {
		t.Helper()

		full := filepath.Join(src, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o750))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o600))
	}
	must("shard/init.lua", "return {}\n")
	must("shard/router.lua", "return {}\n")

	// install_command installs into $(LUADIR), which for every non-make backend
	// is <installDir>/lua — exactly what vshard's cmake rockspec does via
	// TARANTOOL_INSTALL_LUADIR. No build.modules anywhere in the rockspec.
	rockspec := `
package = "shard"
version = "2.0-1"
source = { url = "file://localhost/shard.tar.gz" }
build = {
   type = "command",
   install_command = "mkdir -p $(LUADIR)/shard && cp shard/init.lua shard/router.lua $(LUADIR)/shard/",
}
`
	specPath := filepath.Join(src, "shard-2.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(rockspec), 0o600))

	r, err := client.New(rocks.Config{
		Tree:       treeDir,
		WorkingDir: src,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
		},
	})
	require.NoError(t, err, "New")
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath}), "Make")

	m, err := manif.ReadTreeManifest(filepath.Join(treeDir, "share/tarantool/rocks"))
	require.NoError(t, err, "ReadTreeManifest")

	entry := m.Repository["shard"]["2.0-1"]
	assert.Equal(t, map[string]string{
		"shard.init":   "shard/init.lua",
		"shard.router": "shard/router.lua",
	}, entry.Modules, "RepoEntry.Modules must index what the backend deployed")

	for _, mod := range []string{"shard.init", "shard.router"} {
		if assert.NotEmpty(t, m.Modules[mod], "top-level m.Modules[%q] is empty", mod) {
			assert.Equal(t, "shard/2.0-1", m.Modules[mod][0], "top-level m.Modules[%q]", mod)
		}
	}
}

func TestPack_SrcOnly(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	workDir := t.TempDir()
	rocksDir := filepath.Join(tree, "share", "tarantool", "rocks", "hello", "1.0-1")
	require.NoError(t, os.MkdirAll(rocksDir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(rocksDir, "hello-1.0-1.rockspec"), []byte("package = 'hello'\n"), 0o600))

	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{"hello": {"1.0-1": {Arch: "installed"}}},
		Modules:    map[string][]string{}, Commands: map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	require.NoError(t, (manif.FileStore{}).WriteTree(filepath.Join(tree, "share", "tarantool", "rocks"), m))
	r, _ := client.New(rocks.Config{Tree: tree, WorkingDir: workDir})
	archive, err := r.Pack(context.Background(), "hello", client.PackOpts{SrcOnly: true})
	require.NoError(t, err, "Pack")
	assert.True(t, filepath.IsAbs(archive) && filepath.Ext(archive) == ".rock", "archive = %q, want absolute path ending in .rock", archive)
}
