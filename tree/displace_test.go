package tree_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/tree"
)

// stageFiles lays the files out, each path relative to the stage, in a
// fresh stage dir.
func stageFiles(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for rel, body := range files {
		writeFixture(t, filepath.Join(dir, filepath.FromSlash(rel)), body)
	}

	return dir
}

// TestInstallStaged_NoDisplace: a rock that sorts first takes the plain path
// of a module from the rock holding it, which RequirePlain, asking about the
// rock's own files only, lets through; NoDisplace refuses it, with the tree
// as it was, naming the file and its holder, and lets through what keeps
// every other rock's files where they were: a file held by an older version
// of the rock itself, and a file the rock gets the versioned path of.
func TestInstallStaged_NoDisplace(t *testing.T) {
	t.Parallel()

	zz := stagedSpec(t, "zz", "1.0-1")
	aa := stagedSpec(t, "aa", "1.0-1")
	files := map[string]string{"lua/x.lua": "return 'x'\n", "bin/x-tool": "print(1)\n"}

	// What RequirePlain alone lets through.
	root := t.TempDir()
	_, err := installStaged(t, root, zz, stageFiles(t, files), tree.StagedOpts{})
	require.NoError(t, err, "zz")

	_, err = installStaged(t, root, aa, stageFiles(t, files), tree.StagedOpts{RequirePlain: true})
	require.NoError(t, err, "aa holds its plain paths")
	require.ErrorIs(t, inspect(t, root).CheckPlain(tree.RockRef{Name: "zz", Version: "1.0-1"}), tree.ErrNotPlain,
		"zz lost lua/x.lua to aa")

	// What NoDisplace refuses.
	root = t.TempDir()
	_, err = installStaged(t, root, zz, stageFiles(t, files), tree.StagedOpts{})
	require.NoError(t, err, "zz")

	before := treeState(t, root)

	for _, opts := range []tree.StagedOpts{
		{NoDisplace: true},
		{NoDisplace: true, RequirePlain: true},
		{NoDisplace: true, RemoveOtherVersions: true},
	} {
		_, err = installStaged(t, root, aa, stageFiles(t, files), opts)
		require.ErrorIs(t, err, tree.ErrNotPlain, "%+v", opts)
		require.ErrorContains(t, err, "aa 1.0-1: lua/x.lua (held by zz/1.0-1), bin/x-tool (held by zz/1.0-1)")
		assert.Equal(t, before, treeState(t, root), "the tree, %+v", opts)
	}

	// A newer version of the holder takes its paths from the older.
	zz2 := stagedSpec(t, "zz", "2.0-1")
	_, err = installStaged(t, root, zz2, stageFiles(t, files), tree.StagedOpts{NoDisplace: true, RequirePlain: true})
	require.NoError(t, err, "zz 2 over zz 1")

	// A rock sorting after the holder takes the versioned paths.
	zzz := stagedSpec(t, "zzz", "1.0-1")
	_, err = installStaged(t, root, zzz, stageFiles(t, files), tree.StagedOpts{NoDisplace: true})
	require.NoError(t, err, "zzz below zz")

	// A file no rock has.
	_, err = installStaged(t, root, aa, stageFiles(t, map[string]string{"lua/y.lua": "return 'y'\n"}), tree.StagedOpts{NoDisplace: true})
	require.NoError(t, err, "aa with a module of its own")

	v := inspect(t, root)
	require.NoError(t, v.CheckPlain(tree.RockRef{Name: "zz", Version: "2.0-1"}), "zz 2 holds its plain paths")
	require.ErrorIs(t, v.CheckPlain(tree.RockRef{Name: "zz", Version: "1.0-1"}), tree.ErrNotPlain, "zz 1 below zz 2")
	assert.Empty(t, check(t, root).Issues)
}

// TestGraft_NoDisplace: a graft that would take a plain path from a rock
// installed here fails with NoDisplace, the tree as it was, and the rocks
// it grafts share their items among themselves and with the versions of
// their own names by the conflict rules.
func TestGraft_NoDisplace(t *testing.T) {
	t.Parallel()

	dst := buildTree(t, beta1)
	before := treeState(t, dst)

	src := buildTree(t, alpha1)

	_, err := graft(t, dst, src, tree.GraftOpts{NoDisplace: true})
	require.ErrorIs(t, err, tree.ErrNotPlain, "alpha would take shared and tool from beta")
	require.ErrorContains(t, err, "alpha 1.0-1: lua/shared.lua (held by beta/1.0-1), bin/tool (held by beta/1.0-1)")
	assert.Equal(t, before, treeState(t, dst), "the tree")

	_, err = graft(t, dst, src, tree.GraftOpts{})
	require.NoError(t, err, "Graft without NoDisplace")
	requireConsistent(t, dst, alpha1, beta1)

	// Rocks grafted together, into a tree of none of theirs.
	both := t.TempDir()
	_, err = graft(t, both, buildTree(t, alpha1, beta1), tree.GraftOpts{NoDisplace: true})
	require.NoError(t, err, "alpha and beta grafted together")
	requireConsistent(t, both, alpha1, beta1)

	// alpha 2 replacing alpha 1, which held shared and tool: beta, below
	// alpha 1, is below alpha 2 again.
	_, err = graft(t, both, buildTree(t, alpha2), tree.GraftOpts{NoDisplace: true, RemoveOtherVersions: true})
	require.NoError(t, err, "alpha 2 replacing alpha 1")
	requireConsistent(t, both, alpha2, beta1)
}

// TestGraft_RequirePlain: a graft a rock of which would not hold the plain
// paths of its files once grafted fails with RequirePlain, the tree as it
// was, whether a rock installed here or another grafted rock holds them; a
// rock grafted alone, after the others, is asked it alone.
func TestGraft_RequirePlain(t *testing.T) {
	t.Parallel()

	zeta1 := rockDef{
		name: "zeta", version: "1.0-1",
		modules: map[string]string{"shared": "return 'zeta 1'\n", "zeta": "return 'zeta 1 own'\n"},
	}

	dst := buildTree(t, beta1)
	before := treeState(t, dst)

	_, err := graft(t, dst, buildTree(t, zeta1), tree.GraftOpts{RequirePlain: true})
	require.ErrorIs(t, err, tree.ErrNotPlain, "beta holds shared")
	require.ErrorContains(t, err, "zeta 1.0-1: lua/shared.lua (held by beta/1.0-1)")
	assert.Equal(t, before, treeState(t, dst), "the tree")

	_, err = graft(t, dst, buildTree(t, alpha1), tree.GraftOpts{RequirePlain: true})
	require.NoError(t, err, "alpha takes shared and tool from beta")
	requireConsistent(t, dst, alpha1, beta1)

	src := buildTree(t, alpha1, beta1)
	empty := t.TempDir()

	_, err = graft(t, empty, src, tree.GraftOpts{RequirePlain: true})
	require.ErrorIs(t, err, tree.ErrNotPlain, "beta below alpha, grafted with it")
	require.ErrorContains(t, err, "beta 1.0-1: lua/shared.lua (held by alpha/1.0-1), bin/tool (held by alpha/1.0-1)")
	assert.Empty(t, inspect(t, empty).Rocks())

	isBeta := func(r tree.RockRef) bool { return r.Name == "beta" }

	require.NoError(t, update(t, empty, func(tx *tree.Tx) error {
		if _, err := tx.Graft(src, tree.GraftOpts{Select: isBeta}); err != nil {
			return err
		}

		_, err := tx.Graft(src, tree.GraftOpts{Select: func(r tree.RockRef) bool { return !isBeta(r) }, RequirePlain: true})

		return err
	}), "beta, then alpha alone with RequirePlain")
	requireConsistent(t, empty, alpha1, beta1)
}
