package client_test

// Supplemental coverage tests for the client facade: native-engine write
// paths (Build/Make/Install/Remove) driven hermetically via file:// sources
// and an httptest manifest server, read-path happy paths (Show/List/
// ReadTreeManifest), lua-engine argv option branches through the callImpl
// seam, and the small pure helpers exposed by extra_export_test.go. No real
// network, no fixed sleeps.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// --- fixtures ---

// stageRockSource writes a one-module pure-Lua rock source tree (<name>.lua)
// plus its rockspec into dir and returns the rockspec path. The source URL
// points back at dir itself via file://, so fetch copies the tree without any
// network access.
func stageRockSource(t *testing.T, dir, name, version, extraSpec string) string {
	t.Helper()

	modBody := "return { name = '" + name + "' }\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".lua"), []byte(modBody), 0o600))

	spec := fmt.Sprintf(`
package = %q
version = %q
source = { url = "file://%s" }
%s
build = {
   type = "builtin",
   modules = { [%q] = %q },
}
`, name, version, dir, extraSpec, name, name+".lua")

	specPath := filepath.Join(dir, name+"-"+version+".rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o600))

	return specPath
}

// newNativeClient builds a facade over fresh tree/work dirs with a fake
// Tarantool layout (never exercised by pure-Lua fixtures).
func newNativeClient(t *testing.T, tree, work string) *client.Rocks {
	t.Helper()

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
			Version:    "2.11.0",
		},
	})
	require.NoError(t, err, "New")

	return r
}

// stageInstalledRock lays a fake installed rock (manifest + rock_manifest +
// rockspec) directly on disk, bypassing the build pipeline, so the read-path
// methods can be exercised in isolation.
func stageInstalledRock(t *testing.T, tree string) {
	t.Helper()

	rocksDir := filepath.Join(tree, "share", "tarantool", "rocks")
	installDir := filepath.Join(rocksDir, "hello", "1.0-1")
	require.NoError(t, os.MkdirAll(installDir, 0o750))

	spec := `
package = "hello"
version = "1.0-1"
source = { url = "file://localhost/hello.tar.gz" }
description = {
   summary = "test summary",
   license = "MIT",
   homepage = "https://example.com/hello",
}
dependencies = { "dep >= 1.0" }
`
	require.NoError(t, os.WriteFile(filepath.Join(installDir, "hello-1.0-1.rockspec"), []byte(spec), 0o600))

	rm := &rocks.RockManifest{
		Lua: map[string]string{"hello.lua": "d41d8cd98f00b204e9800998ecf8427e"},
		Lib: map[string]string{"native.so": "d41d8cd98f00b204e9800998ecf8427e"},
	}
	require.NoError(t, (manif.FileStore{}).WriteRock(filepath.Join(installDir, "rock_manifest"), rm), "WriteRock")

	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"hello": {
				"1.0-1": {Arch: "installed"},
				"2.0-1": {Arch: "installed"},
			},
		},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	require.NoError(t, (manif.FileStore{}).WriteTree(rocksDir, m), "WriteTree")
}

// brokenTreeConfig returns a Config whose Tree path is a regular file, so
// tree.Open's MkdirAll fails — the shared error path of every read method.
func brokenTreeConfig(t *testing.T) rocks.Config {
	t.Helper()

	base := t.TempDir()
	file := filepath.Join(base, "not-a-dir")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	return rocks.Config{Tree: filepath.Join(file, "tree")}
}

// --- Backend / New ---

func TestBackendString(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "native", client.BackendNative.String(), "BackendNative")
	assert.Equal(t, "lua", client.BackendLua.String(), "BackendLua")
	assert.Equal(t, "unknown", client.Backend(99).String(), "out-of-range backend")
}

func TestNew_UnknownBackend_FallsBackToNative(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()}, client.WithBackend(client.Backend(99)))
	require.NoError(t, err, "New")

	_, ok := r.Engine().(*client.NativeEngine)
	assert.True(t, ok, "engine = %T, want *NativeEngine for unknown backend", r.Engine())
}

// --- Exec ---

func TestExec_NativeBackend_NotImplemented(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")

	err = r.Exec(context.Background(), "test-prog", []string{"help"})
	require.ErrorIs(t, err, rocks.ErrNotImplemented, "Exec on native backend")
}

func TestExec_LuaBackend_VersionAndFailure(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: dir}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	// --version prints the banner and exits 0 → nil error.
	require.NoError(t, r.Exec(context.Background(), "test-prog", []string{"--version"}), "Exec --version")

	// lint on a nonexistent rockspec exits non-zero → real error.
	err = r.Exec(context.Background(), "test-prog", []string{"lint", filepath.Join(dir, "missing.rockspec")})
	require.Error(t, err, "Exec lint missing.rockspec should fail")
}

// --- read paths: ReadTreeManifest / List / Show ---

func TestReadTreeManifest_HappyAndMissing(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	stageInstalledRock(t, tree)

	r, err := client.New(rocks.Config{Tree: tree})
	require.NoError(t, err, "New")

	m, err := r.ReadTreeManifest()
	require.NoError(t, err, "ReadTreeManifest")
	assert.Contains(t, m.Repository, "hello", "repository")

	// A fresh tree has no manifest file yet: surface the read error verbatim.
	r2, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New empty")

	_, err = r2.ReadTreeManifest()
	require.Error(t, err, "ReadTreeManifest without a manifest file")
}

func TestReadTreeManifest_TreeOpenError(t *testing.T) {
	t.Parallel()

	r, err := client.New(brokenTreeConfig(t))
	require.NoError(t, err, "New")

	_, err = r.ReadTreeManifest()
	require.Error(t, err, "ReadTreeManifest with unusable tree path")
}

func TestList_NonEmptyTree(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	stageInstalledRock(t, tree)

	r, err := client.New(rocks.Config{Tree: tree})
	require.NoError(t, err, "New")

	list, err := r.List(context.Background())
	require.NoError(t, err, "List")
	require.Len(t, list, 2, "installed rocks")

	for _, ir := range list {
		assert.Equal(t, "hello", ir.Name, "rock name")
	}
}

func TestList_TreeOpenError(t *testing.T) {
	t.Parallel()

	r, err := client.New(brokenTreeConfig(t))
	require.NoError(t, err, "New")

	_, err = r.List(context.Background())
	require.Error(t, err, "List with unusable tree path")
}

func TestShow_Installed_FullInfo(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	stageInstalledRock(t, tree)

	r, err := client.New(rocks.Config{Tree: tree})
	require.NoError(t, err, "New")

	info, err := r.Show(context.Background(), "hello")
	require.NoError(t, err, "Show")

	assert.Equal(t, "hello", info.Package, "Package")
	assert.Equal(t, "1.0-1", info.Version, "Version — lexicographically lowest wins")
	assert.ElementsMatch(t, []string{"hello.lua", "native.so"}, info.Modules, "Modules")
	assert.Equal(t, "test summary", info.Summary, "Summary")
	assert.Equal(t, "MIT", info.License, "License")
	assert.Equal(t, "https://example.com/hello", info.Homepage, "Homepage")
	require.Len(t, info.Dependencies, 1, "Dependencies")
	assert.Equal(t, "dep", info.Dependencies[0].Name, "dependency name")
}

func TestShow_TreeOpenError(t *testing.T) {
	t.Parallel()

	r, err := client.New(brokenTreeConfig(t))
	require.NoError(t, err, "New")

	_, err = r.Show(context.Background(), "hello")
	require.Error(t, err, "Show with unusable tree path")
}

func TestWhich_TreeOpenError(t *testing.T) {
	t.Parallel()

	r, err := client.New(brokenTreeConfig(t))
	require.NoError(t, err, "New")

	_, _, err = r.Which(context.Background(), "hello")
	require.Error(t, err, "Which with unusable tree path")
}

// --- native Build ---

func TestNativeBuild_FileSource_Deploys(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()
	work := t.TempDir()
	specPath := stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, tree, work)
	require.NoError(t, r.Build(context.Background(), specPath, client.BuildOpts{}), "Build")

	_, err := os.Stat(filepath.Join(tree, "share", "tarantool", "hello.lua"))
	require.NoError(t, err, "deployed module missing")

	// The staging dir is removed when Keep is false.
	staging, err := filepath.Glob(filepath.Join(work, "rocks-build-*"))
	require.NoError(t, err, "Glob")
	assert.Empty(t, staging, "staging dirs should be cleaned without Keep")
}

func TestNativeBuild_KeepLeavesStagingDir(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	work := t.TempDir()
	specPath := stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, t.TempDir(), work)
	require.NoError(t, r.Build(context.Background(), specPath, client.BuildOpts{Keep: true}), "Build --keep")

	staging, err := filepath.Glob(filepath.Join(work, "rocks-build-*"))
	require.NoError(t, err, "Glob")
	assert.NotEmpty(t, staging, "Keep should leave the staging dir in place")
}

func TestNativeBuild_BadSpecPath(t *testing.T) {
	t.Parallel()

	r := newNativeClient(t, t.TempDir(), t.TempDir())
	err := r.Build(context.Background(), filepath.Join(t.TempDir(), "missing.rockspec"), client.BuildOpts{})
	require.Error(t, err, "Build with missing rockspec")
}

func TestNativeBuild_FetchError(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	spec := `
package = "hello"
version = "1.0-1"
source = { url = "file:///nonexistent/source/path" }
build = { type = "builtin", modules = { hello = "hello.lua" } }
`
	specPath := filepath.Join(src, "hello-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o600))

	r := newNativeClient(t, t.TempDir(), t.TempDir())
	err := r.Build(context.Background(), specPath, client.BuildOpts{})
	require.Error(t, err, "Build with unfetchable source")
}

// --- native Make (rockspec discovery) ---

func TestNativeMake_DiscoversSingleRockspec(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()
	stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, tree, src)
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{}), "Make without explicit rockspec")

	_, err := os.Stat(filepath.Join(tree, "share", "tarantool", "hello.lua"))
	require.NoError(t, err, "deployed module missing")
}

func TestNativeMake_NoRockspec(t *testing.T) {
	t.Parallel()

	r := newNativeClient(t, t.TempDir(), t.TempDir())
	err := r.Make(context.Background(), client.MakeOpts{})
	require.Error(t, err, "Make with no rockspec in WorkingDir")
	assert.Contains(t, err.Error(), "no .rockspec", "error text")
}

func TestNativeMake_MultipleRockspecs(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	stageRockSource(t, src, "hello", "1.0-1", "")
	stageRockSource(t, src, "other", "2.0-1", "")

	r := newNativeClient(t, t.TempDir(), src)
	err := r.Make(context.Background(), client.MakeOpts{})
	require.Error(t, err, "Make with two rockspecs in WorkingDir")
	assert.Contains(t, err.Error(), "multiple .rockspec", "error text")
}

func TestNativeMake_WorkingDirReadError(t *testing.T) {
	t.Parallel()

	r := newNativeClient(t, t.TempDir(), filepath.Join(t.TempDir(), "missing-subdir"))
	err := r.Make(context.Background(), client.MakeOpts{})
	require.Error(t, err, "Make with nonexistent WorkingDir")
}

// --- native Install against a local manifest server ---

// installTestServer serves a Lua manifest listing hello (which depends on
// dep) and both rockspecs; the rock sources are file:// trees on disk.
func installTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	helloSrc := t.TempDir()
	depSrc := t.TempDir()
	stageRockSource(t, helloSrc, "hello", "1.0-1", "dependencies = { 'dep >= 1.0', 'tarantool' }")
	stageRockSource(t, depSrc, "dep", "1.0-1", "")

	helloSpec, err := os.ReadFile(filepath.Join(helloSrc, "hello-1.0-1.rockspec"))
	require.NoError(t, err, "read hello rockspec")

	depSpec, err := os.ReadFile(filepath.Join(depSrc, "dep-1.0-1.rockspec"))
	require.NoError(t, err, "read dep rockspec")

	const manifest = `commands = {}
modules = {}
repository = {
   hello = { ["1.0-1"] = { { arch = "rockspec" } } },
   dep = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest-5.1", "/manifests/myns/manifest-5.1":
			_, _ = w.Write([]byte(manifest))
		case "/hello-1.0-1.rockspec":
			_, _ = w.Write(helloSpec)
		case "/dep-1.0-1.rockspec":
			_, _ = w.Write(depSpec)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv
}

func TestNativeInstall_LocalServer_WithDeps(t *testing.T) {
	t.Parallel()

	srv := installTestServer(t)
	tree := t.TempDir()
	r := newNativeClient(t, tree, t.TempDir())

	ctx := context.Background()
	opts := client.InstallOpts{Servers: []string{srv.URL}, Deps: client.DepsAll}
	require.NoError(t, r.Install(ctx, "hello", opts), "Install hello")

	for _, mod := range []string{"hello.lua", "dep.lua"} {
		_, err := os.Stat(filepath.Join(tree, "share", "tarantool", mod))
		require.NoError(t, err, "deployed module %s missing", mod)
	}

	// Reinstall over a populated tree: the resolver consults installedLookup
	// and skips the already-satisfied dependency.
	require.NoError(t, r.Install(ctx, "hello", opts), "reinstall hello")

	// Namespaced install goes through the per-namespace manifest.
	require.NoError(t, r.Install(ctx, "myns/hello", opts), "install myns/hello")
}

func TestNativeInstall_ErrorPaths(t *testing.T) {
	t.Parallel()

	srv := installTestServer(t)
	r := newNativeClient(t, t.TempDir(), t.TempDir())
	ctx := context.Background()

	// Bad constraint string.
	err := r.Install(ctx, "hello", client.InstallOpts{Version: ">>=nonsense", Servers: []string{srv.URL}})
	require.Error(t, err, "Install with malformed constraint")

	// No version satisfies the constraint.
	err = r.Install(ctx, "hello", client.InstallOpts{Version: ">= 9.0", Servers: []string{srv.URL}})
	require.Error(t, err, "Install with unsatisfiable constraint")
	assert.Contains(t, err.Error(), "no version", "error text")

	// Server without manifests: Query hard-fails.
	dead := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(dead.Close)

	err = r.Install(ctx, "hello", client.InstallOpts{Servers: []string{dead.URL}})
	require.Error(t, err, "Install against a manifest-less server")
}

func TestNativeInstallStep_FetchesSpecWhenNil(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()
	stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, tree, t.TempDir())
	ne, ok := r.Engine().(*client.NativeEngine)
	require.True(t, ok, "engine type")

	step := rocks.InstallStep{Name: "hello", URL: "file://" + src}
	require.NoError(t, client.NativeInstallStep(ne, context.Background(), step), "installStep with nil Rockspec")

	_, err := os.Stat(filepath.Join(tree, "share", "tarantool", "hello.lua"))
	require.NoError(t, err, "deployed module missing")
}

func TestNativeInstalledLookup(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	stageInstalledRock(t, tree)

	r, err := client.New(rocks.Config{Tree: tree})
	require.NoError(t, err, "New")

	ne, ok := r.Engine().(*client.NativeEngine)
	require.True(t, ok, "engine type")

	lookup, err := client.NativeInstalledLookup(ne)
	require.NoError(t, err, "installedLookup")
	assert.Len(t, lookup("hello"), 2, "hello versions")
	assert.Empty(t, lookup("absent"), "unknown rock")
}

// --- native Remove ---

func TestNativeRemove_EndToEnd(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()
	specPath := stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, tree, src)
	ctx := context.Background()
	require.NoError(t, r.Make(ctx, client.MakeOpts{RockspecPath: specPath}), "Make")

	deployed := filepath.Join(tree, "share", "tarantool", "hello.lua")
	_, err := os.Stat(deployed)
	require.NoError(t, err, "deployed module missing before Remove")

	// Not-installed rock and not-installed version are loud errors.
	require.Error(t, r.Remove(ctx, "absent", client.RemoveOpts{}), "Remove absent rock")
	require.Error(t, r.Remove(ctx, "hello", client.RemoveOpts{Version: "9.9-9"}), "Remove absent version")

	// Removing the exact version undeploys and cleans the manifest.
	require.NoError(t, r.Remove(ctx, "hello", client.RemoveOpts{Version: "1.0-1"}), "Remove hello 1.0-1")

	_, err = os.Stat(deployed)
	assert.True(t, os.IsNotExist(err), "deployed module should be gone, stat err = %v", err)

	m, err := r.ReadTreeManifest()
	require.NoError(t, err, "ReadTreeManifest after Remove")
	assert.NotContains(t, m.Repository, "hello", "repository entry should be gone")
}

func TestNativeRemove_AllVersions(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	tree := t.TempDir()
	specPath := stageRockSource(t, src, "hello", "1.0-1", "")

	r := newNativeClient(t, tree, src)
	ctx := context.Background()
	require.NoError(t, r.Make(ctx, client.MakeOpts{RockspecPath: specPath}), "Make")

	// Empty opts.Version removes every installed version.
	require.NoError(t, r.Remove(ctx, "hello", client.RemoveOpts{}), "Remove all versions")

	list, err := r.List(ctx)
	require.NoError(t, err, "List after Remove")
	assert.Empty(t, list, "tree should be empty")
}

func TestNativeRemove_TreeOpenError(t *testing.T) {
	t.Parallel()

	r, err := client.New(brokenTreeConfig(t))
	require.NoError(t, err, "New")

	err = r.Remove(context.Background(), "hello", client.RemoveOpts{})
	require.Error(t, err, "Remove with unusable tree path")
}

// --- native Pack / Unpack error paths ---

func TestNativePack_TargetNotInstalled(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()
	stageInstalledRock(t, tree)

	r, err := client.New(rocks.Config{Tree: tree, WorkingDir: t.TempDir()})
	require.NoError(t, err, "New")

	_, err = r.Pack(context.Background(), "absent", client.PackOpts{})
	require.Error(t, err, "Pack of a rock that is not installed")
	assert.Contains(t, err.Error(), "not installed", "error text")
}

func TestNativePack_NoManifest(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir(), WorkingDir: t.TempDir()})
	require.NoError(t, err, "New")

	_, err = r.Pack(context.Background(), "hello", client.PackOpts{})
	require.Error(t, err, "Pack without a tree manifest")
}

func TestNativeUnpack_Errors(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir(), WorkingDir: t.TempDir()})
	require.NoError(t, err, "New")

	dest := t.TempDir()

	// Missing archive.
	err = r.Unpack(context.Background(), filepath.Join(dest, "missing.rock"), filepath.Join(dest, "out"))
	require.Error(t, err, "Unpack of a missing archive")

	// Zip-slip entry escaping destDir.
	evil := filepath.Join(dest, "evil.rock")
	writeZip(t, evil, map[string]string{"../escape.txt": "x"})

	err = r.Unpack(context.Background(), evil, filepath.Join(dest, "out"))
	require.Error(t, err, "Unpack must reject entries escaping destDir")
	assert.Contains(t, err.Error(), "escapes destDir", "error text")
}

func TestNativeUnpack_DirectoryEntries(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir(), WorkingDir: t.TempDir()})
	require.NoError(t, err, "New")

	dest := t.TempDir()
	archive := filepath.Join(dest, "ok.rock")
	writeZip(t, archive, map[string]string{
		"docs/":         "",
		"docs/read.txt": "hi",
	})

	out := filepath.Join(dest, "out")
	require.NoError(t, r.Unpack(context.Background(), archive, out), "Unpack")

	body, err := os.ReadFile(filepath.Join(out, "docs", "read.txt"))
	require.NoError(t, err, "extracted file")
	assert.Equal(t, "hi", string(body), "extracted contents")
}

// writeZip creates a zip at path; entries with a trailing slash become
// directory entries.
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()

	f, err := os.Create(path)
	require.NoError(t, err, "create zip")

	zw := zip.NewWriter(f)

	for name, body := range entries {
		if name[len(name)-1] == '/' {
			_, err := zw.Create(name)
			require.NoError(t, err, "zip dir entry")

			continue
		}

		w, err := zw.Create(name)
		require.NoError(t, err, "zip entry")

		_, err = w.Write([]byte(body))
		require.NoError(t, err, "zip write")
	}

	require.NoError(t, zw.Close(), "close zip writer")
	require.NoError(t, f.Close(), "close zip file")
}

// --- lua engine: real-VM dispatch (no callImpl seam) ---

func TestLuaBackend_Lint_RealVM(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	spec := `
package = "hello"
version = "1.0-1"
source = { url = "file://localhost/hello.tar.gz" }
description = {
   summary = "test summary",
   license = "MIT",
}
build = { type = "builtin", modules = { hello = "hello.lua" } }
`
	specPath := filepath.Join(dir, "hello-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(specPath, []byte(spec), 0o600))

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: dir}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	require.NoError(t, r.Lint(context.Background(), specPath, client.LintOpts{}), "Lint valid rockspec")

	// A missing rockspec exits non-zero through the same dispatch path.
	err = r.Lint(context.Background(), filepath.Join(dir, "missing.rockspec"), client.LintOpts{})
	require.Error(t, err, "Lint missing rockspec")
}

// --- lua engine: argv option branches via the callImpl seam ---

func TestLuaEngine_Install_DefaultDepsNoVersion_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	require.NoError(t, e.Install(context.Background(), "foo", client.InstallOpts{Deps: client.DepsOnlyNew}), "Install")

	want := []string{"--tree", "/opt/tree", "install", "foo", "--deps-mode", "order"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Make_NoRockspec_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	require.NoError(t, e.Make(context.Background(), client.MakeOpts{}), "Make")

	want := []string{"--tree", "/opt/tree", "make"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Pack_NoPackedLine_Error(t *testing.T) {
	t.Parallel()

	e, _ := newRecorderEngine("nothing here\n")
	_, err := e.Pack(context.Background(), "foo", client.PackOpts{})
	require.Error(t, err, "Pack without a Packed: line")
}

func TestLuaEngine_Purge_ForceFast_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	require.NoError(t, e.Purge(context.Background(), client.PurgeOpts{ForceFast: true}), "Purge")

	want := []string{"--tree", "/opt/tree", "purge", "--force-fast"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Search_BinaryAll_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	_, err := e.Search(context.Background(), "foo", client.SearchOpts{Binary: true, All: true})
	require.NoError(t, err, "Search")

	want := []string{"--tree", "/opt/tree", "search", "foo", "--binary", "--all", "--porcelain"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Search_DispatchError(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree"}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) { return "", errors.New("boom") })

	_, err := e.Search(context.Background(), "foo", client.SearchOpts{})
	require.Error(t, err, "Search must surface dispatch errors")
}

func TestLuaEngine_Download_AllRockspecArch_Argv(t *testing.T) {
	t.Parallel()

	work := t.TempDir()

	var got []string

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree", WorkingDir: work}, nil, nil)
	e.SetCallImpl(func(argv []string) (string, error) {
		got = argv

		return "", nil
	})

	_, err := e.Download(context.Background(), "foo", client.DownloadOpts{
		All:      true,
		Rockspec: true,
		Arch:     "linux-x86_64",
	})
	require.NoError(t, err, "Download")

	want := []string{
		"--tree", "/opt/tree", "download", "foo",
		"--all", "--rockspec", "--arch", "linux-x86_64",
	}
	assert.Equal(t, want, got, "argv")
}

func TestLuaEngine_Download_DispatchError(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree", WorkingDir: t.TempDir()}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) { return "", errors.New("boom") })

	_, err := e.Download(context.Background(), "foo", client.DownloadOpts{})
	require.Error(t, err, "Download must surface dispatch errors")
}

func TestLuaEngine_NewVersion_NewURL_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("Wrote /work/foo-2.0-1.rockspec\n")
	_, err := e.NewVersion(context.Background(), "foo.rockspec", client.NewVersionOpts{
		NewVersion: "2.0",
		NewURL:     "https://example.com/foo-2.0.tar.gz",
	})
	require.NoError(t, err, "NewVersion")

	want := []string{"new_version", "foo.rockspec", "2.0", "https://example.com/foo-2.0.tar.gz"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_NewVersion_DispatchError(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree"}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) { return "", errors.New("boom") })

	_, err := e.NewVersion(context.Background(), "foo.rockspec", client.NewVersionOpts{})
	require.Error(t, err, "NewVersion must surface dispatch errors")
}

func TestLuaEngine_WriteRockspec_AllOpts_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("Wrote template at /work/foo-1.0-1.rockspec -- you should now edit and finish it.\n")
	_, err := e.WriteRockspec(context.Background(), "https://example.com/foo", client.WriteRockspecOpts{
		Output:         "/work/out.rockspec",
		Detailed:       "long text",
		Homepage:       "https://example.com",
		LuaVersions:    "5.1",
		RockspecFormat: "3.0",
		Tag:            "v1.0",
		Lib:            "m",
	})
	require.NoError(t, err, "WriteRockspec")

	want := []string{
		"write_rockspec", "https://example.com/foo",
		"--output", "/work/out.rockspec",
		"--detailed", "long text",
		"--homepage", "https://example.com",
		"--lua-versions", "5.1",
		"--rockspec-format", "3.0",
		"--tag", "v1.0",
		"--lib", "m",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_WriteRockspec_Errors(t *testing.T) {
	t.Parallel()

	// No `Wrote` line in the output → loud error, no fabricated path.
	e, _ := newRecorderEngine("nothing\n")
	_, err := e.WriteRockspec(context.Background(), "u", client.WriteRockspecOpts{})
	require.Error(t, err, "WriteRockspec without a Wrote line")

	// Dispatch failure propagates.
	e2 := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree"}, nil, nil)
	e2.SetCallImpl(func([]string) (string, error) { return "", errors.New("boom") })

	_, err = e2.WriteRockspec(context.Background(), "u", client.WriteRockspecOpts{})
	require.Error(t, err, "WriteRockspec must surface dispatch errors")
}

func TestLuaEngine_Doc_Home_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	require.NoError(t, e.Doc(context.Background(), "foo", client.DocOpts{Home: true}), "Doc")

	want := []string{"--tree", "/opt/tree", "doc", "foo", "--home"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Config_WriteUnsetScopeJSON_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	_, err := e.Config(context.Background(), client.ConfigOpts{
		Key:   "lua_dir",
		Value: "/usr/local",
		Unset: true,
		Scope: "user",
		JSON:  true,
	})
	require.NoError(t, err, "Config")

	want := []string{
		"config", "lua_dir", "/usr/local", "--unset", "--scope", "user", "--json",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Config_DispatchError(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree"}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) { return "", errors.New("boom") })

	_, err := e.Config(context.Background(), client.ConfigOpts{})
	require.Error(t, err, "Config must surface dispatch errors")
}

func TestLuaEngine_Upload_AllOpts_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Upload(context.Background(), "foo.rockspec", client.UploadOpts{
		SrcRock:  "foo-1.0-1.src.rock",
		SkipPack: true,
		TempKey:  "tk",
		Force:    true,
	})
	require.NoError(t, err, "Upload")

	want := []string{
		"upload", "foo.rockspec", "foo-1.0-1.src.rock",
		"--skip-pack", "--temp-key", "tk", "--force",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Admin_NoOpts_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	require.NoError(t, e.Admin(context.Background(), "make_manifest", nil, client.AdminOpts{}), "Admin")

	want := []string{"admin", "make_manifest"}
	assert.Equal(t, want, *got, "argv")
}

// --- exported pure helpers ---

func TestNormalizeOpenMode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"r+b": "rb+",
		"w+b": "wb+",
		"a+b": "ab+",
		"r":   "r",
		"wb+": "wb+",
	}
	for in, want := range cases {
		assert.Equal(t, want, client.NormalizeOpenMode(in), "mode %q", in)
	}
}

func TestDepsModeArg(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "all", client.DepsModeArg(client.DepsAll), "DepsAll")
	assert.Equal(t, "none", client.DepsModeArg(client.DepsNone), "DepsNone")
	assert.Equal(t, "order", client.DepsModeArg(client.DepsOnlyNew), "DepsOnlyNew")
	assert.Equal(t, "all", client.DepsModeArg(client.DepsPolicy(42)), "out-of-range policy")
}

// nilCauseError is an error whose Unwrap chain ends in nil without ever
// containing an *os.PathError.
type nilCauseError struct{}

func (nilCauseError) Error() string { return "no cause" }
func (nilCauseError) Unwrap() error { return nil }

func TestUnwrapPathErr(t *testing.T) {
	t.Parallel()

	// A wrapped *os.PathError unwraps to its inner error.
	pe := &os.PathError{Op: "open", Path: "/x", Err: os.ErrNotExist}
	assert.Equal(t, os.ErrNotExist, client.UnwrapPathErr(fmt.Errorf("read: %w", pe)), "wrapped PathError")

	// A plain error without Unwrap is returned as-is.
	plain := errors.New("plain")
	assert.Equal(t, plain, client.UnwrapPathErr(plain), "plain error")

	// A wrapper chain without any PathError returns the innermost error.
	inner := errors.New("inner")
	assert.Equal(t, inner, client.UnwrapPathErr(fmt.Errorf("outer: %w", inner)), "wrapper chain")

	// A chain that unwraps to nil yields nil.
	assert.NoError(t, client.UnwrapPathErr(nilCauseError{}), "unwraps to nil")
}

func TestSysconfDir(t *testing.T) {
	t.Setenv("LUAROCKS_SYSCONFDIR", "/custom/etc")
	assert.Equal(t, "/custom/etc", client.SysconfDir(), "env override")

	t.Setenv("LUAROCKS_SYSCONFDIR", "")
	assert.Equal(t, "/etc/luarocks", client.SysconfDir(), "default")
}

func TestParseOsExit(t *testing.T) {
	t.Parallel()

	code, ok := client.ParseOsExit("stack: go-luarocks os.exit: 42\ntraceback")
	assert.True(t, ok, "sentinel with digits")
	assert.Equal(t, 42, code, "code")

	_, ok = client.ParseOsExit("some unrelated error")
	assert.False(t, ok, "no sentinel")

	_, ok = client.ParseOsExit("go-luarocks os.exit: nope")
	assert.False(t, ok, "sentinel without digits")
}

func TestEvalAndPrepare_Errors(t *testing.T) {
	t.Parallel()

	cfg := rocks.Config{}

	// Missing file → Eval error.
	_, err := client.EvalAndPrepare(filepath.Join(t.TempDir(), "missing.rockspec"), cfg)
	require.Error(t, err, "missing rockspec")

	// Parses but fails validation (no source.url).
	bad := filepath.Join(t.TempDir(), "bad.rockspec")
	require.NoError(t, os.WriteFile(bad, []byte("package = 'x'\nversion = '1.0-1'\n"), 0o600))

	_, err = client.EvalAndPrepare(bad, cfg)
	require.Error(t, err, "invalid rockspec")
	assert.Contains(t, err.Error(), "Validate", "error text")
}

func TestZipHelpers_Errors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	badOut := filepath.Join(dir, "no-such-dir", "out.zip")

	// zipDir: uncreatable output and unwalkable source.
	require.Error(t, client.ZipDir(badOut, dir), "zipDir with uncreatable outPath")
	require.Error(t, client.ZipDir(filepath.Join(dir, "out.zip"), missing), "zipDir with missing srcDir")

	// zipSingleFile: missing source and uncreatable output.
	require.Error(t, client.ZipSingleFile(filepath.Join(dir, "o.zip"), missing, "e"), "zipSingleFile missing src")

	src := filepath.Join(dir, "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o600))
	require.Error(t, client.ZipSingleFile(badOut, src, "e"), "zipSingleFile uncreatable outPath")
}

func TestDirFiles_EmptyDir(t *testing.T) {
	t.Parallel()

	assert.Empty(t, client.DirFiles(""), "empty dir string")
	assert.Empty(t, client.DirFiles(filepath.Join(t.TempDir(), "missing")), "missing dir")
}

func TestLuaEngineCleanup(t *testing.T) {
	t.Parallel()

	// Cleanup on an engine that never warmed a VM is a safe no-op.
	fresh := client.NewLuaEngine(rocks.Config{Tree: t.TempDir()}, manif.FileStore{}, nil)
	client.LuaEngineCleanup(fresh)

	// After a dispatch the pool holds a warm VM; cleanup drains and closes it.
	dir := t.TempDir()
	e := client.NewLuaEngine(rocks.Config{Tree: dir, WorkingDir: dir}, manif.FileStore{}, nil)
	require.NoError(t, e.Call([]string{"help"}), "dispatch")
	require.True(t, client.WaitForPooledVM(e), "pool never restocked after a dispatch")
	client.LuaEngineCleanup(e)
}
