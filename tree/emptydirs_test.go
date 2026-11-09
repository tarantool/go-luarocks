package tree_test

import (
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/tree"
)

// TestRemove_PrunesEmptiedDeployDirs: the removal of the last rock with a
// command and a C module removes bin/, lib/tarantool/ and lib/ with them, as
// upstream's fs.remove_dir_tree_if_empty, `rmdir -p`, does after deleting
// the files (repos.lua:379), and the directories the rock alone had below
// share/tarantool/; the root, the lua deploy dir and the rocks dir stay. A
// deploy dir another rock still has a file in stays too.
func TestRemove_PrunesEmptiedDeployDirs(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	paths := tree.Paths{Tree: root}

	for _, name := range []string{"alpha", "beta"} {
		spec := stagedSpec(t, name, "1.0-1")
		_, err := installStaged(t, root, spec, stageRockDir(t, spec), tree.StagedOpts{})
		require.NoError(t, err, "InstallStaged %s", name)
	}

	require.FileExists(t, filepath.Join(paths.BinDir(), "alpha-tool"))
	require.FileExists(t, filepath.Join(paths.DeployLibDir(), "alpha", "core.so"))

	require.NoError(t, update(t, root, func(tx *tree.Tx) error {
		return tx.Remove(tree.RockRef{Name: "alpha", Version: "1.0-1"})
	}))

	assert.NoDirExists(t, filepath.Join(paths.DeployLibDir(), "alpha"), "the C module dir alpha alone had")
	assert.NoDirExists(t, filepath.Join(paths.DeployLuaDir(), "alpha"), "the Lua module dir alpha alone had")
	assert.DirExists(t, paths.BinDir(), "bin/ beta's commands are in")
	assert.DirExists(t, paths.DeployLibDir(), "lib/tarantool/ beta's C module is in")
	assert.Empty(t, check(t, root).Issues)

	require.NoError(t, update(t, root, func(tx *tree.Tx) error {
		return tx.Remove(tree.RockRef{Name: "beta", Version: "1.0-1"})
	}))

	assert.NoDirExists(t, paths.BinDir(), "bin/ the last command leaves empty")
	assert.NoDirExists(t, paths.DeployLibDir(), "lib/tarantool/ the last C module leaves empty")
	assert.NoDirExists(t, filepath.Join(root, "lib"), "lib/ the last C module leaves empty")
	assert.NoDirExists(t, filepath.Join(paths.RocksDir(), "beta"), "the rock dir of the last version")
	assert.DirExists(t, paths.RocksDir(), "the rocks dir, which holds the manifest")
	assert.DirExists(t, root, "the tree's root")
	assert.Equal(t, []string{
		".", "share", "share/tarantool", "share/tarantool/rocks",
		"share/tarantool/rocks/.lock", "share/tarantool/rocks/manifest",
	}, slices.Sorted(maps.Keys(snapshot(t, root))), "the tree once its last rock is removed")
	assert.Empty(t, check(t, root).Issues)
}

// TestRemove_PrunedDeployDirsUndone: a change that fails after a removal
// pruned the deploy dirs puts them back with the files in them: the pruning
// goes through the journal.
func TestRemove_PrunedDeployDirsUndone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	spec := stagedSpec(t, "alpha", "1.0-1")
	_, err := installStaged(t, root, spec, stageRockDir(t, spec), tree.StagedOpts{})
	require.NoError(t, err)

	before := snapshot(t, root)
	errFn := errors.New("fn failed")

	err = update(t, root, func(tx *tree.Tx) error {
		require.NoError(t, tx.Remove(tree.RockRef{Name: "alpha", Version: "1.0-1"}))
		require.NoDirExists(t, tree.Paths{Tree: root}.BinDir(), "bin/ pruned by the removal")

		return errFn
	})
	require.ErrorIs(t, err, errFn)
	requireAsItWas(t, root, before)
}
