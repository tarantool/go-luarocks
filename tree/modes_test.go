//go:build unix

package tree_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/tree"
)

// TestTreeModes_UnderUmask: the directories and files a tree change creates
// have the modes upstream LuaRocks gives them, the process umask applied —
// directories 0777, commands and C modules 0755, the rest 0644 or 0666 —
// so a tree made under the usual umask, 022, is readable by everyone, and
// one made under 077 by its owner alone.
func TestTreeModes_UnderUmask(t *testing.T) {
	t.Parallel()

	for _, mask := range []int{0o022, 0o077} {
		root := filepath.Join(t.TempDir(), "tree")
		testenv.RunWithUmask(t, "TestTreeModes_Child", mask, root)

		requireModes(t, root, os.FileMode(mask), func(rel string) bool {
			return strings.HasPrefix(rel, "lib/") || strings.HasPrefix(rel, "bin/") ||
				strings.HasPrefix(rel, "share/tarantool/rocks/modes/1.0-1/bin/")
		})
	}
}

// TestTreeModes_Child is the body of the child processes of
// TestTreeModes_UnderUmask: it installs a staged rock, with Lua and C
// modules, a Lua command behind a wrapper, docs and a file at its top, into
// a tree that does not exist yet.
func TestTreeModes_Child(t *testing.T) {
	t.Parallel()

	root := testenv.UmaskChild(t)
	stage := filepath.Join(filepath.Dir(root), "stage")

	for rel, mode := range map[string]os.FileMode{
		"lua/modes/init.lua": 0o644,
		"lib/modes/core.so":  0o755,
		"bin/modes-tool":     0o755,
		"doc/README.md":      0o644,
		"NOTES":              0o644,
	} {
		p := filepath.Join(stage, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o777))
		require.NoError(t, os.WriteFile(p, []byte("return 1\n"), mode))
	}

	spec := stagedSpec(t, "modes", "1.0-1")

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	tr.BinWrap = &tree.BinWrap{Interpreter: "/opt/test/bin/tarantool", Sysconfdir: "/etc/luarocks"}

	require.NoError(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		_, err := tx.InstallStaged(spec, stage, tree.StagedOpts{})

		return err
	}))
}

// requireModes checks that every directory under root has the mode 0777,
// every file exec says is executable 0755 and every other file 0666 or
// 0644, each less mask; for the masks tested, 022 and 077, 0666 and 0644 come
// out the same.
func requireModes(t *testing.T, root string, mask os.FileMode, exec func(rel string) bool) {
	t.Helper()

	var dirs int

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		st, err := d.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		want := 0o666 &^ mask

		switch {
		case d.IsDir():
			want = 0o777 &^ mask
			dirs++
		case exec(rel):
			want = 0o755 &^ mask
		}

		assert.Equal(t, want.String(), st.Mode().Perm().String(), "%s under umask %#o", rel, mask)

		return nil
	}))

	assert.Greater(t, dirs, 1, "the tree's directories")
}
