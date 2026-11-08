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

// gamma1 is a rock whose command is a shell script, which Deploy copies
// verbatim rather than wrapping.
var gamma1 = rockDef{
	name: "gamma", version: "1.0-1",
	modules: map[string]string{"gamma": "return 'gamma 1'\n"},
	bins:    map[string]string{"gsh": "#!/bin/sh\necho gamma\n"},
}

// rewrap runs Tx.Rewrap on the tree at root opened with wrap and returns
// what it rewrote.
func rewrap(t *testing.T, root string, wrap *tree.BinWrap) []string {
	t.Helper()

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err, "Open")

	tr.BinWrap = wrap

	var out []string

	require.NoError(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		var err error

		out, err = tx.Rewrap()

		return err
	}), "Rewrap")

	return out
}

// TestRewrap_MovedTree: a tree moved to another root, once rewrapped, holds
// the wrappers a tree built there holds, byte for byte; commands copied
// verbatim are left alone, and a second Rewrap rewrites nothing. Another
// interpreter rewrites every wrapper again.
func TestRewrap_MovedTree(t *testing.T) {
	t.Parallel()

	defs := []rockDef{alpha1, beta1, gamma1}

	parent := t.TempDir()
	built := filepath.Join(parent, "built")

	for _, d := range defs {
		makeRock(t, built, d)
	}

	moved := filepath.Join(parent, "moved")
	require.NoError(t, os.Rename(built, moved))

	fresh := buildTree(t, defs...)
	assert.NotEqual(t, treeState(t, fresh), treeState(t, moved), "the moved tree's wrappers name where it was built")

	bin := tree.Paths{Tree: moved}.BinDir()
	want := []string{
		filepath.Join(bin, "tool"),
		filepath.Join(bin, "beta_1_0_1-tool"),
	}

	assert.Equal(t, want, rewrap(t, moved, openWrapped(t, moved).BinWrap), "rewritten")
	assert.Equal(t, treeState(t, fresh), treeState(t, moved), "the rewrapped tree vs one built there")
	requireConsistent(t, moved, defs...)
	assert.Equal(t, gamma1.bins["gsh"], readFile(t, filepath.Join(bin, "gsh")), "the shell script")

	assert.Empty(t, rewrap(t, moved, openWrapped(t, moved).BinWrap), "a second Rewrap")

	other := *openWrapped(t, moved).BinWrap
	other.Interpreter = "/opt/other/bin/tarantool"
	assert.Equal(t, want, rewrap(t, moved, &other), "another interpreter")

	for _, p := range want {
		assert.Contains(t, readFile(t, p), "'/opt/other/bin/tarantool'", "%s runs the new interpreter", p)
	}

	tr, err := tree.Open(rocks.Config{Tree: moved})
	require.NoError(t, err)
	require.Error(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		_, err := tx.Rewrap()

		return err
	}), "Rewrap without a BinWrap")
}
