//go:build integration_luaengine

package client_test

// Where the two backends differ by design: the native backend keeps
// go-luarocks' own semantics for dependencies and the rock trees, the lua
// backend runs upstream LuaRocks. These tests run the same operations through
// both and check that each does what it is meant to, rather than that they
// agree.

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

// stagePrefixTree returns a fresh directory to serve as the Tarantool prefix
// of a test, and so as the rock tree after Config.Tree on the lua backend: it
// holds bin/tarantool, a link to the real interpreter under prefix, which the
// lua backend runs from the prefix to tell a Lua command script from another
// (fs.is_lua).
func stagePrefixTree(t *testing.T, prefix string) string {
	t.Helper()

	other := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(other, "bin"), 0o750))
	require.NoError(t, os.Symlink(filepath.Join(prefix, "bin", "tarantool"), filepath.Join(other, "bin", "tarantool")))

	return other
}

// TestBackendDivergence_DepsPolicy gets hello, whose dependency dep is
// installed in the rock tree of the Tarantool prefix, into a fresh tree by
// each operation of depsCases through both backends, and checks what each
// does: whether dep is installed into the target tree, which trees hello's
// command wrapper lists, and what the tree manifest records for hello. The
// native backend consults and lists Config.Tree alone, and installs
// dependencies only on Install, as DepsPolicy says; the lua backend runs
// upstream, which takes the policy as --deps-mode (Install) or its default
// "one" (Build, Make) and selects the rock trees by it.
func TestBackendDivergence_DepsPolicy(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	other := stagePrefixTree(t, prefix)
	tarantool := rocks.TarantoolConfig{Prefix: other, IncludeDir: incDir, Version: parityTarantoolVersion}

	require.NoError(t,
		newClient(t, rocks.Config{Tree: other, WorkingDir: t.TempDir(), Servers: []string{repo}, Tarantool: tarantool}).
			Install(context.Background(), "dep", client.InstallOpts{}),
		"Install dep into the other tree")

	for _, tc := range depsCases() {
		t.Run(tc.name, func(t *testing.T) {
			for _, backend := range []client.Backend{client.BackendNative, client.BackendLua} {
				t.Run(backend.String(), func(t *testing.T) {
					target := t.TempDir()
					r := newClient(t, rocks.Config{
						Tree:       target,
						WorkingDir: tc.work(t, repo),
						Servers:    []string{repo},
						Tarantool:  tarantool,
					}, client.WithBackend(backend))
					require.NoError(t, tc.run(r, repo), "%s hello", tc.name)

					want := tc.native
					if backend == client.BackendLua {
						want = tc.lua
					}

					checkDepsOutcome(t, want, target, other)
				})
			}
		})
	}
}

// otherDepRepo returns a directory server holding dep 1.1-1: a version of dep
// that satisfies hello, other than the 1.0-1 a removal takes away.
func otherDepRepo(t *testing.T, repo string) string {
	t.Helper()

	spec, err := os.ReadFile(filepath.Join(repo, "dep-1.0-1.rockspec")) //nolint:gosec // test temp path
	require.NoError(t, err, "read dep's rockspec")

	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "dep-1.1-1.rockspec"),
		[]byte(strings.Replace(string(spec), `version = "1.0-1"`, `version = "1.1-1"`, 1)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(other, "manifest"), []byte(`commands = {}
modules = {}
repository = {
   dep = { ["1.1-1"] = { { arch = "rockspec" } } },
}
`), 0o600))

	return other
}

// TestBackendDivergence_RemoveDependents removes dep, which hello depends
// on, through both backends. The native Remove keeps go-luarocks' own
// semantics and removes it in every case; upstream's, which the lua backend
// runs, refuses unless --force or --force-fast is given or another version in
// the rock trees of its deps mode still satisfies hello (remove.lua
// check_dependents) — here dep 1.1-1 in the rock tree of the Tarantool
// prefix, which --deps-mode all, the zero DepsPolicy, consults and "none"
// does not.
func TestBackendDivergence_RemoveDependents(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	plain := stagePrefixTree(t, prefix)
	holding := stagePrefixTree(t, prefix)
	require.NoError(t,
		newClient(t, rocks.Config{
			Tree:       holding,
			WorkingDir: t.TempDir(),
			Servers:    []string{otherDepRepo(t, repo)},
			Tarantool:  rocks.TarantoolConfig{Prefix: holding, IncludeDir: incDir, Version: parityTarantoolVersion},
		}).Install(context.Background(), "dep", client.InstallOpts{}),
		"Install dep 1.1 into the other tree")

	for _, tc := range []struct {
		name       string
		opts       client.RemoveOpts
		other      string
		luaRemoves bool
	}{
		{"dependent", client.RemoveOpts{}, plain, false},
		{"force", client.RemoveOpts{Force: true}, plain, true},
		{"force-fast", client.RemoveOpts{ForceFast: true}, plain, true},
		{"satisfied by the other tree", client.RemoveOpts{}, holding, true},
		{"satisfied by the other tree, deps none", client.RemoveOpts{Deps: client.DepsNone}, holding, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, backend := range []client.Backend{client.BackendNative, client.BackendLua} {
				t.Run(backend.String(), func(t *testing.T) {
					target := t.TempDir()
					r := newClient(t, rocks.Config{
						Tree:       target,
						WorkingDir: t.TempDir(),
						Servers:    []string{repo},
						Tarantool:  rocks.TarantoolConfig{Prefix: tc.other, IncludeDir: incDir, Version: parityTarantoolVersion},
					}, client.WithBackend(backend))
					// dep first, so that the target tree holds it whatever
					// the other tree does.
					require.NoError(t, r.Install(context.Background(), "dep", client.InstallOpts{}), "Install dep")
					require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{}), "Install hello")

					removes := backend == client.BackendNative || tc.luaRemoves

					err := r.Remove(context.Background(), "dep", tc.opts)
					if removes {
						require.NoError(t, err, "Remove dep")
					} else {
						require.Error(t, err, "Remove dep must refuse: hello depends on it")
					}

					_, err = os.Stat(filepath.Join(target, "share", "tarantool", "dep.lua"))
					assert.Equal(t, removes, os.IsNotExist(err), "dep removed")
				})
			}
		})
	}
}
