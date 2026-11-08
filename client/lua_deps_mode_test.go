//go:build integration_luaengine

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// TestLuaEngine_Install_TreeOutsideWorkingDir installs a rock with a
// dependency through the lua backend into a tree that is not
// <WorkingDir>/.rocks, the tree upstream's default rocks_trees list would
// hold. The dependency must be installed into that tree and found there
// afterwards, as upstream's default deps mode ("one") does.
func TestLuaEngine_Install_TreeOutsideWorkingDir(t *testing.T) {
	t.Parallel()

	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)
	tree := t.TempDir()

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: t.TempDir(),
		Servers:    []string{repo},
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
			Version:    parityTarantoolVersion,
		},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{}),
		"Install hello with the default deps mode")
	requireDeployed(t, tree)

	_, err = os.Stat(filepath.Join(tree, "bin", "hello-cli"))
	require.NoError(t, err, "hello's command wrapper")
}

// TestLuaEngine_TypedInstallIgnoresConfigTrees installs and builds hello
// through the lua backend under a system config file that sets deps_mode
// "all" and a rocks_trees list without the target tree: a typed method runs
// over the Config's rock trees all the same — dep installed into the target
// tree, the wrapper listing it and then the prefix, the trees deps mode
// "all" selects (Install passes it as --deps-mode for DepsAll, Build takes
// it from the file), and dep recorded as hello's resolved dependency.
//
//nolint:paralleltest // sets the environment
func TestLuaEngine_TypedInstallIgnoresConfigTrees(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	dirs := testenv.Hermetic(t)
	writeHostileConfig(t, dirs.SysconfDir)

	repo := installTestRepo(t)
	want := depsOutcome{depInstalled: true, listsOther: true, resolved: map[string]string{"dep": "1.0-1"}}

	for _, op := range []struct {
		name string
		run  func(r *client.Rocks) error
	}{
		{"install", func(r *client.Rocks) error {
			return r.Install(context.Background(), "hello", client.InstallOpts{})
		}},
		{"build", func(r *client.Rocks) error {
			return r.Build(context.Background(), filepath.Join(repo, "hello-1.0-1.rockspec"), client.BuildOpts{})
		}},
	} {
		t.Run(op.name, func(t *testing.T) {
			target := t.TempDir()
			r := newClient(t, rocks.Config{
				Tree:       target,
				WorkingDir: t.TempDir(),
				Servers:    []string{repo},
				Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: parityTarantoolVersion},
			}, client.WithBackend(client.BackendLua))

			require.NoError(t, op.run(r), "%s hello", op.name)
			checkDepsOutcome(t, want, target, prefix)
		})
	}
}
