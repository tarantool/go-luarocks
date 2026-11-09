package tree

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/tarantool/go-luarocks/internal/safepath"
)

// A journal records the changes a transaction makes to the files of a tree,
// so that a change that fails can be undone, as upstream LuaRocks rolls back
// a failed deploy (repos.lua rollback_ops) and a failed build or install
// (util.schedule_function in build.lua and cmd/install.lua). Every file the
// tree package creates, overwrites, renames or removes in a tree goes through
// the journal of the Tree it is a method of: a created entry is removed on
// undo, a removed one is not deleted but moved into the journal's trash, a
// directory of the rocks dir, and moved back on undo, an overwritten file is
// copied there first, and a rename is renamed back. The trash is deleted when
// the change commits.
//
// A nil journal is no journal: its operations change the tree directly.
type journal struct {
	// root is the tree root: every path the journal handles is inside it.
	root string
	// rocksDir is the rocks dir of the tree, where the trash is made.
	rocksDir string
	// trash is the directory removed and overwritten entries are kept in
	// until the change commits; empty until the first is.
	trash string
	// seq numbers the entries of the trash.
	seq int
	// undo lists what undoes each change, in the order the changes were made.
	undo []undoOp
}

// trashPrefix starts the name of a journal's trash in the rocks dir.
const trashPrefix = ".tx-"

// ownerRWX is the mode bits that let a directory's owner list, enter and
// change it.
const ownerRWX fs.FileMode = 0o700

// undoKind is what undoes one change.
type undoKind int

const (
	// undoCreate removes path, which did not exist before the change.
	undoCreate undoKind = iota
	// undoMove moves path back to from, replacing what is there.
	undoMove
)

// undoOp undoes one change of the tree.
type undoOp struct {
	kind undoKind
	path string
	from string
}

// newJournal returns an empty journal of the tree of p.
func newJournal(p Paths) *journal {
	return &journal{root: p.Tree, rocksDir: p.RocksDir()}
}

// mark returns the point of the journal undoTo goes back to.
func (j *journal) mark() int {
	if j == nil {
		return 0
	}

	return len(j.undo)
}

// isTrash reports whether path is the journal's trash.
func (j *journal) isTrash(path string) bool {
	return j != nil && j.trash != "" && filepath.Clean(path) == filepath.Clean(j.trash)
}

// commit makes every change recorded permanent: it forgets them and deletes
// the trash, which holds the entries they removed or overwrote.
func (j *journal) commit() {
	if j == nil {
		return
	}

	j.undo = nil

	if j.trash != "" {
		_ = removeAllForce(j.trash)
		j.trash = ""
	}
}

// rollback undoes every change recorded and deletes the trash. When an undo
// fails, the trash is kept, and the error names it: it holds the entries the
// undo could not put back (see undoTo).
func (j *journal) rollback() error {
	if j == nil {
		return nil
	}

	if err := j.undoTo(0); err != nil {
		return err
	}

	j.commit()

	return nil
}

// undoTo undoes the changes recorded since mark, the newest first, and
// forgets them. It goes on past an undo that fails, so that as much as can be
// is put back, and returns the errors joined.
func (j *journal) undoTo(mark int) error {
	if j == nil {
		return nil
	}

	var errs []error

	for i := len(j.undo) - 1; i >= mark; i-- {
		if err := j.undoOne(j.undo[i]); err != nil {
			errs = append(errs, err)
		}
	}

	j.undo = j.undo[:mark]

	if len(errs) > 0 {
		err := fmt.Errorf("tree: undo a failed change: %w (what was not put back is in %s)", errors.Join(errs...), j.trash)

		// The trash holds what was not put back: no commit or rollback
		// deletes it, and what is set aside next goes to a new one.
		j.trash = ""

		return err
	}

	return nil
}

// undoOne undoes one change.
func (j *journal) undoOne(op undoOp) error {
	switch op.kind {
	case undoCreate:
		if err := safepath.RemoveAll(j.root, op.path); err != nil {
			return fmt.Errorf("remove %s: %w", op.path, err)
		}
	case undoMove:
		if err := safepath.RemoveAll(j.root, op.from); err != nil {
			return fmt.Errorf("clear %s: %w", op.from, err)
		}

		if err := safepath.MkdirAll(j.root, filepath.Dir(op.from), dirPerm); err != nil {
			return fmt.Errorf("restore %s: %w", op.from, err)
		}

		if err := j.move(op.path, op.from); err != nil {
			return fmt.Errorf("restore %s: %w", op.from, err)
		}
	}

	return nil
}

// record adds the undo of a change.
func (j *journal) record(op undoOp) {
	j.undo = append(j.undo, op)
}

// created records that path, which did not exist, is being created by a
// writer the journal does not see — a build backend writing into a rock's
// install dir — so that undoing it removes path whole.
func (j *journal) created(path string) {
	if j == nil {
		return
	}

	j.record(undoOp{kind: undoCreate, path: path})
}

// nextTrash returns a free name in the trash, creating the trash first.
func (j *journal) nextTrash() (string, error) {
	if j.trash == "" {
		if err := j.mkdirAll(j.root, j.rocksDir); err != nil {
			return "", err
		}

		dir, err := os.MkdirTemp(j.rocksDir, trashPrefix)
		if err != nil {
			return "", fmt.Errorf("make the trash of a change: %w", err)
		}

		j.trash = dir
	}

	j.seq++

	return filepath.Join(j.trash, strconv.Itoa(j.seq)), nil
}

// move renames src to dst, both inside the tree root, and copies src to dst
// and removes it when the two are on different file systems.
func (j *journal) move(src, dst string) error {
	err := safepath.Rename(j.root, src, dst)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	if err := copyEntry(src, dst); err != nil {
		return err
	}

	return removeAllForce(src)
}

// stash moves the entry at path into the trash, recording its move back.
func (j *journal) stash(path string) error {
	dst, err := j.nextTrash()
	if err != nil {
		return err
	}

	if err := j.move(path, dst); err != nil {
		return err
	}

	j.record(undoOp{kind: undoMove, path: dst, from: path})

	return nil
}

// save records path as it is before a writer the journal does not see
// overwrites or creates it — the manifest store writing a manifest: a
// regular file is copied into the trash, to be put back on undo, and a path
// that does not exist is removed on undo.
func (j *journal) save(path string) error {
	if j == nil {
		return nil
	}

	fi, err := safepath.Lstat(j.root, path)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		j.record(undoOp{kind: undoCreate, path: path})

		return nil
	case err != nil:
		return err
	case !fi.Mode().IsRegular():
		return j.stash(path)
	}

	dst, err := j.nextTrash()
	if err != nil {
		return err
	}

	if err := copyEntry(path, dst); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}

	j.record(undoOp{kind: undoMove, path: dst, from: path})

	return nil
}

// mkdirAll creates dir, inside root, and its missing parents with dirPerm,
// as safepath.MkdirAll does, recording the first directory it creates, whose
// removal undoes it.
func (j *journal) mkdirAll(root, dir string) error {
	if j == nil {
		return safepath.MkdirAll(root, dir, dirPerm)
	}

	if _, err := safepath.Rel(root, dir); err != nil {
		return err
	}

	top := ""

	for d := filepath.Clean(dir); isBelow(d, j.root); d = filepath.Dir(d) {
		if _, err := safepath.Lstat(j.root, d); !errors.Is(err, fs.ErrNotExist) {
			break
		}

		top = d
	}

	err := safepath.MkdirAll(root, dir, dirPerm)

	if top != "" {
		if _, serr := safepath.Lstat(j.root, top); serr == nil {
			j.record(undoOp{kind: undoCreate, path: top})
		}
	}

	return err
}

// isBelow reports whether path is lexically beneath root, root excluded.
func isBelow(path, root string) bool {
	rel, err := filepath.Rel(root, path)

	return err == nil && rel != "." && filepath.IsLocal(rel)
}

// create opens path, inside root, for writing, creating it with perm or
// truncating it, as safepath.Create does: a file that was there is saved
// first, and one that was not is removed on undo.
func (j *journal) create(root, path string, perm fs.FileMode) (*os.File, error) {
	if j == nil {
		return safepath.Create(root, path, perm)
	}

	fi, err := safepath.Lstat(root, path)

	switch {
	case err == nil && fi.IsDir():
		// Create fails on it, as it would without the journal.
	case err == nil:
		if err := j.save(path); err != nil {
			return nil, err
		}
	case errors.Is(err, fs.ErrNotExist):
		defer func() {
			if _, serr := safepath.Lstat(root, path); serr == nil {
				j.record(undoOp{kind: undoCreate, path: path})
			}
		}()
	default:
		return nil, err
	}

	return safepath.Create(root, path, perm)
}

// remove removes the file or empty directory path inside root, as
// safepath.Remove does, into the trash.
func (j *journal) remove(root, path string) error {
	if j == nil {
		return safepath.Remove(root, path)
	}

	fi, err := safepath.Lstat(root, path)
	if err != nil {
		return err
	}

	if fi.IsDir() {
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}

		if len(entries) > 0 {
			return &fs.PathError{Op: "remove", Path: path, Err: syscall.ENOTEMPTY}
		}
	}

	return j.stash(path)
}

// removeAll removes path inside root and everything it holds, as
// safepath.RemoveAll does, into the trash; a path that does not exist is no
// error.
func (j *journal) removeAll(root, path string) error {
	if j == nil {
		return safepath.RemoveAll(root, path)
	}

	if _, err := safepath.Rel(root, path); err != nil {
		return err
	}

	if _, err := safepath.Lstat(root, path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}

		return err
	}

	return j.stash(path)
}

// rename renames src to dst, both inside root, as safepath.Rename does; an
// entry at dst is moved into the trash first.
func (j *journal) rename(root, src, dst string) error {
	if j == nil {
		return safepath.Rename(root, src, dst)
	}

	if _, err := safepath.Rel(root, dst); err != nil {
		return err
	}

	if _, err := safepath.Lstat(root, src); err != nil {
		return err
	}

	if err := j.removeAll(root, dst); err != nil {
		return err
	}

	if err := safepath.Rename(root, src, dst); err != nil {
		return err
	}

	j.record(undoOp{kind: undoMove, path: dst, from: src})

	return nil
}

// copyEntry copies the file, symlink or directory src to dst, which must not
// exist, keeping permission bits.
func copyEntry(src, dst string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}

	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}

		return os.Symlink(target, dst)
	case fi.IsDir():
		if err := os.Mkdir(dst, fi.Mode().Perm()|ownerRWX); err != nil {
			return err
		}

		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}

		for _, e := range entries {
			if err := copyEntry(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}

		return os.Chmod(dst, fi.Mode().Perm())
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fi.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return os.Chmod(dst, fi.Mode().Perm())
}

// removeAllForce removes path and everything it holds, giving its owner the
// permission to empty a directory that does not allow it, as a read-only
// directory moved into the trash may.
func removeAllForce(path string) error {
	if err := os.RemoveAll(path); err == nil {
		return nil
	}

	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, ownerRWX)
		}

		return nil
	})

	return os.RemoveAll(path)
}
