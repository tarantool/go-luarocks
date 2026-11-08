package tree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// check runs Tree.Check on the tree at root with the command wrapper inputs
// the fixtures are built with.
func check(t *testing.T, root string) *tree.Report {
	t.Helper()

	tr := openWrapped(t, root)

	report, err := tr.Check(context.Background(), tree.CheckOpts{BinWrap: tr.BinWrap})
	require.NoError(t, err, "Check %s", root)

	return report
}

// issueKey is what TestCheck compares of an issue: its kind, rock and path
// relative to the tree's root.
type issueKey struct {
	kind tree.IssueKind
	rock tree.RockRef
	path string
}

// issueKeys returns the keys of the issues of report.
func issueKeys(t *testing.T, root string, report *tree.Report) []issueKey {
	t.Helper()

	out := make([]issueKey, 0, len(report.Issues))

	for _, i := range report.Issues {
		rel, err := filepath.Rel(root, i.Path)
		require.NoError(t, err)

		out = append(out, issueKey{i.Kind, i.Rock, filepath.ToSlash(rel)})
	}

	return out
}

// TestCheck_Healthy: Check finds nothing on trees the native client built,
// grafted, pruned or rebuilt, nor on one that does not exist.
func TestCheck_Healthy(t *testing.T) {
	t.Parallel()

	defs := []rockDef{alpha1, alpha2, beta1, gamma1}
	built := buildTree(t, defs...)
	assert.Empty(t, check(t, built).Issues, "a built tree")

	grafted := t.TempDir()
	_, err := graft(t, grafted, built, tree.GraftOpts{})
	require.NoError(t, err)
	assert.Empty(t, check(t, grafted).Issues, "a grafted tree")

	require.NoError(t, update(t, grafted, func(tx *tree.Tx) error {
		_, err := tx.Prune(tree.KeepVersions(map[string]string{"alpha": "1.0-1", "gamma": "1.0-1"}), tree.PruneOpts{})

		return err
	}))
	assert.Empty(t, check(t, grafted).Issues, "a pruned tree")

	require.NoError(t, os.Remove(filepath.Join(tree.Paths{Tree: built}.RocksDir(), "manifest")))
	_, err = rebuild(t, built, tree.RebuildOpts{})
	require.NoError(t, err)
	assert.Empty(t, check(t, built).Issues, "a rebuilt tree")

	assert.True(t, check(t, filepath.Join(t.TempDir(), "missing")).OK(), "a missing tree")
}

// TestCheck_Damaged: Check finds each kind of problem on a tree damaged by
// hand, and that problem alone.
func TestCheck_Damaged(t *testing.T) {
	t.Parallel()

	paths := func(root string) tree.Paths { return tree.Paths{Tree: root} }
	lua := func(root string) string { return paths(root).DeployLuaDir() }
	shared := func(root string) string { return filepath.Join(lua(root), "shared.lua") }
	v1Shared := func(root string) string { return tree.MungedPath(lua(root), shared(root), "alpha", "1.0-1") }

	cases := []struct {
		name   string
		damage func(t *testing.T, root string)
		want   []issueKey
		wrap   *tree.BinWrap
	}{
		{
			name: "missing file",
			damage: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(lua(root), "beta.lua")))
			},
			want: []issueKey{{tree.IssueMissingFile, beta1.ref(), "share/tarantool/beta.lua"}},
		},
		{
			name: "missing script",
			damage: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(paths(root).InstallDir("gamma", "1.0-1"), "bin", "gsh")))
			},
			want: []issueKey{{tree.IssueMissingFile, gamma1.ref(), "share/tarantool/rocks/gamma/1.0-1/bin/gsh"}},
		},
		{
			name: "digest mismatch",
			damage: func(t *testing.T, root string) {
				t.Helper()
				writeFixture(t, v1Shared(root), "return 'edited'\n")
			},
			want: []issueKey{{tree.IssueDigestMismatch, alpha1.ref(), "share/tarantool/alpha_1_0_1-shared.lua"}},
		},
		{
			name: "modified verbatim command",
			damage: func(t *testing.T, root string) {
				t.Helper()
				writeFixture(t, filepath.Join(paths(root).BinDir(), "gsh"), "#!/bin/sh\necho edited\n")
			},
			want: []issueKey{{tree.IssueDigestMismatch, gamma1.ref(), "bin/gsh"}},
		},
		{
			name: "wrong spot",
			damage: func(t *testing.T, root string) {
				t.Helper()

				v2 := tree.MungedPath(lua(root), shared(root), "alpha", "2.0-1")
				require.NoError(t, os.Rename(shared(root), v2))
				require.NoError(t, os.Rename(v1Shared(root), shared(root)))
			},
			want: []issueKey{
				{tree.IssueWrongSpot, alpha2.ref(), "share/tarantool/alpha_2_0_1-shared.lua"},
				{tree.IssueWrongSpot, alpha1.ref(), "share/tarantool/shared.lua"},
			},
		},
		{
			name: "orphan rock",
			damage: func(t *testing.T, root string) {
				t.Helper()
				writeFixture(t, filepath.Join(paths(root).InstallDir("ghost", "1.0-1"), "rock_manifest"), "rock_manifest = {}\n")
			},
			want: []issueKey{{tree.IssueOrphanRock, tree.RockRef{Name: "ghost", Version: "1.0-1"}, "share/tarantool/rocks/ghost/1.0-1"}},
		},
		{
			name: "stale entry",
			damage: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Remove(filepath.Join(paths(root).InstallDir("gamma", "1.0-1"), "rock_manifest")))
			},
			want: []issueKey{
				{tree.IssueStaleEntry, gamma1.ref(), "share/tarantool/rocks/gamma/1.0-1/rock_manifest"},
				{tree.IssueUntracked, tree.RockRef{}, "bin/gsh"},
				{tree.IssueUntracked, tree.RockRef{}, "share/tarantool/gamma.lua"},
			},
		},
		{
			name: "untracked and backup",
			damage: func(t *testing.T, root string) {
				t.Helper()
				writeFixture(t, filepath.Join(lua(root), "app", "init.lua"), "stray\n")
				writeFixture(t, filepath.Join(paths(root).DeployLibDir(), "native.so"), "stray\n")
				writeFixture(t, filepath.Join(lua(root), "alpha.lua~"), "backup\n")
				writeFixture(t, filepath.Join(root, ".meta", "lock"), "a tool's own\n")
				writeFixture(t, filepath.Join(root, "share", "doc", "x"), "stray\n")
			},
			want: []issueKey{
				{tree.IssueUntracked, tree.RockRef{}, "lib/tarantool/native.so"},
				{tree.IssueUntracked, tree.RockRef{}, "share/doc/x"},
				{tree.IssueBackup, tree.RockRef{}, "share/tarantool/alpha.lua~"},
				{tree.IssueUntracked, tree.RockRef{}, "share/tarantool/app/init.lua"},
			},
		},
		{
			name:   "stale wrapper",
			damage: func(*testing.T, string) {},
			wrap:   &tree.BinWrap{Interpreter: "/opt/other/bin/tarantool", Sysconfdir: "/etc/luarocks"},
			want: []issueKey{
				{tree.IssueStaleWrapper, alpha2.ref(), "bin/tool"},
				{tree.IssueStaleWrapper, alpha1.ref(), "bin/alpha_1_0_1-tool"},
				{tree.IssueStaleWrapper, beta1.ref(), "bin/beta_1_0_1-tool"},
			},
		},
		{
			name: "manifest unreadable",
			damage: func(t *testing.T, root string) {
				t.Helper()
				writeFixture(t, filepath.Join(paths(root).RocksDir(), "manifest"), "repository = {")
			},
			want: []issueKey{{tree.IssueManifestUnreadable, tree.RockRef{}, "share/tarantool/rocks/manifest"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := buildTree(t, alpha1, alpha2, beta1, gamma1)
			tc.damage(t, root)

			tr, err := tree.Open(rocks.Config{Tree: root})
			require.NoError(t, err)

			wrap := tc.wrap
			if wrap == nil {
				wrap = openWrapped(t, root).BinWrap
			}

			report, err := tr.Check(context.Background(), tree.CheckOpts{BinWrap: wrap})
			require.NoError(t, err, "Check")
			assert.Equal(t, tc.want, issueKeys(t, root, report), "the issues: %v", report.Issues)
			assert.False(t, report.OK())

			for _, i := range report.Issues {
				assert.NotEmpty(t, i.String())
			}
		})
	}
}
