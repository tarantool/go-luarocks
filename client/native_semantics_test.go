package client_test

// The native backend keeps go-luarocks' own semantics where they differ from
// upstream LuaRocks, by decision: tt, its main consumer, drives it as a
// deterministic executor — exact pins, client.DepsNone, a topological order
// and its own pruning — and relies on them. The lua backend runs upstream. The
// tests here pin the native side of each such difference; the parity and
// interop harnesses list the same differences as known divergences.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// semanticsTarantool is the Tarantool configuration of these tests: nothing
// is compiled, so the prefix need not exist; the version makes `tarantool`,
// which hello depends on, a provided rock.
var semanticsTarantool = rocks.TarantoolConfig{Prefix: "/opt/test", IncludeDir: "/opt/test/include", Version: "2.11.0"}

// semanticsClient returns a native client over tree, installing from repo.
func semanticsClient(t *testing.T, tree, repo string) *client.Rocks {
	t.Helper()

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: t.TempDir(),
		Servers:    []string{repo},
		Tarantool:  semanticsTarantool,
	})
	require.NoError(t, err, "New")

	return r
}

// installedList returns what List reports, as "name version" strings.
func installedList(t *testing.T, r *client.Rocks) []string {
	t.Helper()

	installed, err := r.List(context.Background())
	require.NoError(t, err, "List")

	out := make([]string, 0, len(installed))
	for _, rock := range installed {
		out = append(out, rock.Name+" "+rock.Version)
	}

	return out
}

// TestDepsPolicy_Values pins the DepsPolicy constants go-luarocks has always
// had, which callers such as tt spell out: DepsAll is the zero value, so an
// InstallOpts that sets no policy resolves and installs dependencies.
func TestDepsPolicy_Values(t *testing.T) {
	t.Parallel()

	assert.Equal(t, client.DepsAll, client.DepsPolicy(0), "DepsAll")
	assert.Equal(t, client.DepsNone, client.DepsPolicy(1), "DepsNone")
	assert.Equal(t, client.DepsOnlyNew, client.DepsPolicy(2), "DepsOnlyNew")
	assert.Equal(t, client.DepsAll, client.InstallOpts{}.Deps, "the zero InstallOpts")
	assert.Equal(t, client.DepsAll, client.RemoveOpts{}.Deps, "the zero RemoveOpts")
}

// TestNativeRemove_NoDependentsCheck pins the native Remove: it removes the
// rock it is asked to even though another installed rock depends on it, with
// or without Force and ForceFast, where upstream `luarocks remove` refuses
// unless forced.
func TestNativeRemove_NoDependentsCheck(t *testing.T) {
	t.Parallel()

	repo := installTestRepo(t)

	for _, opts := range []client.RemoveOpts{{}, {Force: true}, {ForceFast: true}} {
		tree := t.TempDir()
		r := semanticsClient(t, tree, repo)
		require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{}), "Install hello")

		require.NoError(t, r.Remove(context.Background(), "dep", opts), "Remove dep (%+v), which hello depends on", opts)
		assert.ElementsMatch(t, []string{"hello 1.0-1"}, installedList(t, r), "dep is gone, hello stays (%+v)", opts)

		_, err := os.Stat(filepath.Join(tree, "share", "tarantool", "dep.lua"))
		assert.True(t, os.IsNotExist(err), "dep's module is undeployed (%+v)", opts)
	}
}

// TestNativeBuildMake_InstallNoDependencies pins the native Build and Make:
// they build and deploy the rockspec alone, installing neither its
// dependencies nor its build_dependencies from the configured servers, where
// upstream `luarocks build` and `make` install the missing ones first.
func TestNativeBuildMake_InstallNoDependencies(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	writeTarballRock(t, repo, "tool", "", "", nil)
	writeTarballRock(t, repo, "dep", "", "", nil)
	writeTarballRock(t, repo, "needy", "rockspec_format = '3.0'\nbuild_dependencies = { 'tool' }\ndependencies = { 'dep' }",
		"", nil)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(`repository = {
   tool = { ["1.0-1"] = { { arch = "rockspec" } } },
   dep = { ["1.0-1"] = { { arch = "rockspec" } } },
   needy = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`), 0o600))

	specPath := filepath.Join(repo, "needy-1.0-1.rockspec")

	r := semanticsClient(t, t.TempDir(), repo)
	require.NoError(t, r.Build(context.Background(), specPath, client.BuildOpts{}), "Build needy")
	assert.ElementsMatch(t, []string{"needy 1.0-1"}, installedList(t, r), "Build installs no dependency")

	src := t.TempDir()
	spec, err := os.ReadFile(specPath) //nolint:gosec // test temp path
	require.NoError(t, err, "read needy's rockspec")
	require.NoError(t, os.WriteFile(filepath.Join(src, "needy-1.0-1.rockspec"), spec, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "needy.lua"), []byte("return {}\n"), 0o600))

	r, err = client.New(rocks.Config{Tree: t.TempDir(), WorkingDir: src, Servers: []string{repo}, Tarantool: semanticsTarantool})
	require.NoError(t, err, "New")
	require.NoError(t, r.Make(context.Background(), client.MakeOpts{}), "Make needy")
	assert.ElementsMatch(t, []string{"needy 1.0-1"}, installedList(t, r), "Make installs no dependency")
}

// TestNativeBuildMake_KeepOtherVersions pins the native Build and Make: a
// build of another version of an installed rock leaves the installed one in
// place, its files demoted to their versioned names, with or without
// BuildOpts.Keep, where upstream `luarocks build` and `make` remove it unless
// --keep. BuildOpts.Keep only keeps the staging directory of the build.
func TestNativeBuildMake_KeepOtherVersions(t *testing.T) {
	t.Parallel()

	ops := map[string]func(r *client.Rocks, specPath string) error{
		"build": func(r *client.Rocks, specPath string) error {
			return r.Build(context.Background(), specPath, client.BuildOpts{})
		},
		"make": func(r *client.Rocks, specPath string) error {
			return r.Make(context.Background(), client.MakeOpts{RockspecPath: specPath})
		},
	}

	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			tree := t.TempDir()
			work := t.TempDir()

			for _, v := range []string{"1.0-1", "2.0-1"} {
				src := t.TempDir()
				specPath := stageRockSource(t, src, "hello", v, "")

				wd := work
				if name == "make" {
					wd = src
				}

				require.NoError(t, op(newNativeClient(t, tree, wd), specPath), "%s %s", name, v)
			}

			assert.ElementsMatch(t, []string{"hello 1.0-1", "hello 2.0-1"}, installedList(t, newNativeClient(t, tree, work)),
				"%s keeps the other version", name)

			lua := filepath.Join(tree, "share", "tarantool")
			assert.FileExists(t, filepath.Join(lua, "hello.lua"), "the new version holds the plain path")
			assert.FileExists(t, filepath.Join(lua, "hello_1_0_1-hello.lua"), "the kept version is demoted")

			staging, err := filepath.Glob(filepath.Join(work, "rocks-build-*"))
			require.NoError(t, err, "Glob")
			assert.Empty(t, staging, "the staging dirs are removed without BuildOpts.Keep")
		})
	}
}

// TestNativeManifest_ResolvedDepsAtDeploy pins how the native backend records
// each rock's resolved dependencies in the tree manifest: once, when the rock
// is deployed, from its direct dependencies and the versions installed then.
// A later write leaves the entries of the other rocks as they are, where
// upstream recomputes every entry, transitively, on each write.
func TestNativeManifest_ResolvedDepsAtDeploy(t *testing.T) {
	t.Parallel()

	tree := t.TempDir()

	for _, rock := range []struct{ name, version, deps string }{
		{"base", "1.0-1", ""},
		{"mid", "1.0-1", "dependencies = { 'base' }"},
		{"top", "1.0-1", "dependencies = { 'mid' }"},
		{"base", "2.0-1", ""},
	} {
		src := t.TempDir()
		specPath := stageRockSource(t, src, rock.name, rock.version, rock.deps)
		require.NoError(t, newNativeClient(t, tree, src).Make(context.Background(), client.MakeOpts{RockspecPath: specPath}),
			"Make %s %s", rock.name, rock.version)
	}

	r := newNativeClient(t, tree, t.TempDir())
	require.NoError(t, r.Remove(context.Background(), "base", client.RemoveOpts{Version: "1.0-1"}), "Remove base 1.0")

	m, err := r.ReadTreeManifest()
	require.NoError(t, err, "ReadTreeManifest")

	assert.Equal(t, map[string]string{"mid": "1.0-1"}, m.Repository["top"]["1.0-1"].Dependencies,
		"top records its direct dependency alone")
	assert.Equal(t, map[string]string{"base": "1.0-1"}, m.Repository["mid"]["1.0-1"].Dependencies,
		"mid keeps the version it was deployed against")
}

// twoVersionRepo returns a directory server holding dep 1.0-1 and 2.0-1, and
// user 1.0-1, which depends on dep >= 2.0.
func twoVersionRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()
	writeTarballRock(t, repo, "dep", "", "", nil)
	writeTarballRock(t, repo, "user", "dependencies = { 'dep >= 2.0' }", "", nil)

	spec, err := os.ReadFile(filepath.Join(repo, "dep-1.0-1.rockspec")) //nolint:gosec // test temp path
	require.NoError(t, err, "read dep's rockspec")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "dep-2.0-1.rockspec"),
		[]byte(strings.Replace(string(spec), `version = "1.0-1"`, `version = "2.0-1"`, 1)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(`repository = {
   dep = { ["1.0-1"] = { { arch = "rockspec" } }, ["2.0-1"] = { { arch = "rockspec" } } },
   user = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`), 0o600))

	return repo
}

// TestNativeInstall_KeepsOtherVersions pins the native Install: installing
// another version of an installed rock, directly or as a dependency, leaves
// the installed one in place, where upstream `luarocks install` removes it
// unless --keep.
func TestNativeInstall_KeepsOtherVersions(t *testing.T) {
	t.Parallel()

	repo := twoVersionRepo(t)

	r := semanticsClient(t, t.TempDir(), repo)
	require.NoError(t, r.Install(context.Background(), "dep", client.InstallOpts{Version: "1.0-1"}), "Install dep 1.0")
	require.NoError(t, r.Install(context.Background(), "dep", client.InstallOpts{Version: "2.0-1"}), "Install dep 2.0")
	assert.ElementsMatch(t, []string{"dep 1.0-1", "dep 2.0-1"}, installedList(t, r), "dep 1.0 stays")

	r = semanticsClient(t, t.TempDir(), repo)
	require.NoError(t, r.Install(context.Background(), "dep", client.InstallOpts{Version: "1.0-1"}), "Install dep 1.0")
	require.NoError(t, r.Install(context.Background(), "user", client.InstallOpts{}), "Install user")
	assert.ElementsMatch(t, []string{"dep 1.0-1", "dep 2.0-1", "user 1.0-1"}, installedList(t, r),
		"the dependency the install pulls in joins the other version")
}
