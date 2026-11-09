package tree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestJournal_UndoesEveryChange: each change the journal makes — a file
// created, overwritten and removed, a rename onto an existing entry, a
// directory tree made and an empty directory removed — is undone, newest
// first, to the very bytes and modes there were, and the trash goes.
func TestJournal_UndoesEveryChange(t *testing.T) {
	t.Parallel()

	p := Paths{Tree: t.TempDir()}
	lua := p.DeployLuaDir()

	writeInternal(t, filepath.Join(lua, "kept.lua"), "kept")
	writeInternal(t, filepath.Join(lua, "over.lua"), "old")
	writeInternal(t, filepath.Join(lua, "gone.lua"), "gone")
	writeInternal(t, filepath.Join(lua, "from.lua"), "from")
	writeInternal(t, filepath.Join(lua, "onto.lua"), "onto")
	require.NoError(t, os.Chmod(filepath.Join(lua, "over.lua"), 0o640))
	require.NoError(t, os.Mkdir(filepath.Join(lua, "empty"), 0o711))
	require.NoError(t, os.MkdirAll(p.RocksDir(), 0o755))

	before := snapshotInternal(t, p.Tree)
	j := newJournal(p)

	f, err := j.create(lua, filepath.Join(lua, "over.lua"), filePerm)
	require.NoError(t, err)
	_, err = f.WriteString("new")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, j.writeFile(lua, filepath.Join(lua, "deep", "er", "new.lua"), []byte("new"), filePerm))
	require.NoError(t, j.remove(lua, filepath.Join(lua, "gone.lua")))
	require.NoError(t, j.rename(lua, filepath.Join(lua, "from.lua"), filepath.Join(lua, "onto.lua")))
	require.NoError(t, j.remove(lua, filepath.Join(lua, "empty")))
	require.Error(t, j.remove(lua, lua), "a directory that is not empty")
	require.NoError(t, j.save(filepath.Join(p.RocksDir(), treeManifestName)))
	writeInternal(t, filepath.Join(p.RocksDir(), treeManifestName), "written by a store")

	assert.NotEqual(t, before, snapshotInternal(t, p.Tree))

	require.NoError(t, j.rollback())
	assert.Equal(t, before, snapshotInternal(t, p.Tree), "the tree after the undo")
}

// TestJournal_FailedUndoKeepsTheTrash: an entry the undo cannot put back
// stays in the trash, which the error names and nothing deletes.
func TestJournal_FailedUndoKeepsTheTrash(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("root writes into a read-only directory")
	}

	p := Paths{Tree: t.TempDir()}
	dir := filepath.Join(p.DeployLuaDir(), "locked")

	writeInternal(t, filepath.Join(dir, "x.lua"), "x")
	require.NoError(t, os.MkdirAll(p.RocksDir(), 0o755))

	j := newJournal(p)
	require.NoError(t, j.remove(dir, filepath.Join(dir, "x.lua")))

	trash := j.trash

	require.NoError(t, os.Chmod(dir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	err := j.rollback()
	require.Error(t, err)
	assert.Contains(t, err.Error(), trash)

	j.commit()
	assert.FileExists(t, filepath.Join(trash, "1"), "the entry the undo could not put back")
}

// snapshotInternal returns every entry under root by its path relative to
// it, with its mode and, for a file, its contents.
func snapshotInternal(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		out[rel] = fi.Mode().String()

		if fi.Mode().IsRegular() {
			body, err := os.ReadFile(p) //nolint:gosec // test temp path
			if err != nil {
				return err
			}

			out[rel] += " " + string(body)
		}

		return nil
	}))

	return out
}
