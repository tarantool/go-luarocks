// Package safepath keeps the file operations driven by untrusted names inside
// the directory they are meant for.
//
// A rockspec, a rock_manifest, a manifest or an archive names the files an
// operation writes, renames, chmods or removes. Upstream LuaRocks joins those
// names onto its directories as they come, so a "../../x" in a
// build.install.bin key, a source.file or a rock_manifest entry reaches
// outside the tree. The native backend refuses such a name at two layers:
// rockspec.Validate rejects the path-valued fields of a rockspec, and every
// write, rename, chmod, removal and extraction goes through this package,
// which also catches what never passes Validate — a planted rock_manifest or
// an archive entry.
//
// Join and Rel are lexical: they check that a name stays inside its root
// before it is used. The file operations go further: each opens its root with
// os.Root and acts through it, so a symbolic link on the way — one pointing
// out of the tree, or one an archive extracted first and then wrote a file
// through — fails the operation instead of being followed outside. A
// relative link that stays inside the root is followed, as the plain os
// function would; os.Root refuses an absolute one wherever it points, and of
// the operations here only Open follows it when it stays inside.
//
// Every function takes the root and the full path, as filepath.Join(root,
// rel) forms it, so a call site keeps the path it already has. Both must be
// absolute, or both relative to the current directory.
package safepath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// ErrEscapes is wrapped by the error of every function here for a path that
// is not inside its root, lexically or once a symlink on the way is followed.
// It is rocks.ErrUnsafePath, so a caller of the public packages can branch on
// it.
var ErrEscapes = rocks.ErrUnsafePath

// rootEscapeText is the text of the error an os.Root operation fails with
// when a symlink takes its path out of the root; package os does not export
// that error, and the tests of this package pin the text.
const rootEscapeText = "path escapes from parent"

// escapes is the error for path, which is not inside root.
func escapes(root, path string) error {
	return fmt.Errorf("%w: %q is not inside %q", ErrEscapes, path, root)
}

// Join joins rel onto root after checking that rel is local in the sense of
// filepath.IsLocal: relative, not empty, and not reaching above root through
// "..". A rel of "." is root itself.
func Join(root, rel string) (string, error) {
	if !filepath.IsLocal(rel) {
		return "", escapes(root, rel)
	}

	return filepath.Join(root, rel), nil
}

// JoinEntry is Join for a rel that must name an entry of root, a file or a
// directory beneath it, so a rel that cleans to "." fails too.
func JoinEntry(root, rel string) (string, error) {
	p, err := Join(root, rel)
	if err != nil {
		return "", err
	}

	if p == filepath.Clean(root) {
		return "", fmt.Errorf("%w: %q names %q itself, not an entry of it", ErrEscapes, rel, root)
	}

	return p, nil
}

// Rel returns path relative to root, failing unless path is root or
// lexically beneath it.
func Rel(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return "", escapes(root, path)
	}

	return rel, nil
}

// Name checks that s is a single path element that names a directory entry:
// not empty, without a separator, and neither "." nor "..". A rock name, a
// version or a manifest key that becomes one directory of a path must be one.
func Name(s string) error {
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, `/\`) {
		return fmt.Errorf("%w: %q is not a single path element", ErrEscapes, s)
	}

	return nil
}

// in runs op on the os.Root of root with path made relative to it.
func in(root, path string, op func(r *os.Root, rel string) error) error {
	rel, err := Rel(root, path)
	if err != nil {
		return err
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	if err := op(r, rel); err != nil {
		if strings.Contains(err.Error(), rootEscapeText) {
			return fmt.Errorf("%w: %w", ErrEscapes, err)
		}

		return err
	}

	return nil
}

// Open opens path, inside root, for reading. os.Root takes a symlink with an
// absolute target for one leaving the root wherever it points, so Open
// resolves a path that failed that way and opens it when it still lies inside
// root: a build that installed an absolute link to its own file, or a source
// tree holding one, reads as the plain os functions would.
func Open(root, path string) (*os.File, error) {
	var f *os.File

	err := in(root, path, func(r *os.Root, rel string) error {
		var err error

		f, err = r.Open(rel)

		return err
	})
	if err == nil || !errors.Is(err, ErrEscapes) {
		return f, err
	}

	realRoot, rerr := filepath.EvalSymlinks(root)
	if rerr != nil {
		return nil, err
	}

	resolved, rerr := filepath.EvalSymlinks(path)
	if rerr != nil {
		return nil, err
	}

	if _, rerr := Rel(realRoot, resolved); rerr != nil {
		return nil, err
	}

	err = in(realRoot, resolved, func(r *os.Root, rel string) error {
		var err error

		f, err = r.Open(rel)

		return err
	})

	return f, err
}

// OpenFile is os.OpenFile for path inside root.
func OpenFile(root, path string, flag int, perm fs.FileMode) (*os.File, error) {
	var f *os.File

	err := in(root, path, func(r *os.Root, rel string) error {
		var err error

		f, err = r.OpenFile(rel, flag, perm)

		return err
	})

	return f, err
}

// Create opens path, inside root, for writing, creating it with perm or
// truncating it. Its directory must exist; see MkdirAll.
func Create(root, path string, perm fs.FileMode) (*os.File, error) {
	return OpenFile(root, path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
}

// MkdirAll creates the directory path inside root and every missing parent,
// with perm. It creates root first when it does not exist: root is the
// caller's own directory, never a name from untrusted input.
func MkdirAll(root, path string, perm fs.FileMode) error {
	if _, err := Rel(root, path); err != nil {
		return err
	}

	if err := os.MkdirAll(root, perm); err != nil {
		return err
	}

	return in(root, path, func(r *os.Root, rel string) error {
		return r.MkdirAll(rel, perm)
	})
}

// Lstat is os.Lstat for path inside root.
func Lstat(root, path string) (fs.FileInfo, error) {
	var fi fs.FileInfo

	err := in(root, path, func(r *os.Root, rel string) error {
		var err error

		fi, err = r.Lstat(rel)

		return err
	})

	return fi, err
}

// Remove removes the file or empty directory path inside root.
func Remove(root, path string) error {
	return in(root, path, func(r *os.Root, rel string) error {
		return r.Remove(rel)
	})
}

// RemoveAll removes path inside root and everything it holds; like
// os.RemoveAll, a path that does not exist, under a root that may not either,
// is no error. Unlike os.RemoveAll it never removes root itself.
func RemoveAll(root, path string) error {
	if _, err := Rel(root, path); err != nil {
		return err
	}

	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	return in(root, path, func(r *os.Root, rel string) error {
		return r.RemoveAll(rel)
	})
}

// Rename renames oldpath to newpath, both inside root.
func Rename(root, oldpath, newpath string) error {
	newRel, err := Rel(root, newpath)
	if err != nil {
		return err
	}

	return in(root, oldpath, func(r *os.Root, rel string) error {
		return r.Rename(rel, newRel)
	})
}

// Chmod is os.Chmod for path inside root.
func Chmod(root, path string, mode fs.FileMode) error {
	return in(root, path, func(r *os.Root, rel string) error {
		return r.Chmod(rel, mode)
	})
}

// WalkDir walks the directory dir, inside root, as filepath.WalkDir does,
// through os.Root: fn gets each path joined onto root as filepath.Join(root,
// rel) forms it, and a symlink on the way that leads out of root fails the
// walk with an error wrapping ErrEscapes. Like filepath.WalkDir it does not
// descend into a symlink to a directory, which fn gets as a non-directory
// entry; dir itself may be a symlink that stays inside root. A dir that does
// not exist is an error passed to fn, as with filepath.WalkDir.
func WalkDir(root, dir string, fn fs.WalkDirFunc) error {
	rel, err := Rel(root, dir)
	if err != nil {
		return err
	}

	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}

	defer func() { _ = r.Close() }()

	return fs.WalkDir(r.FS(), filepath.ToSlash(rel), func(p string, d fs.DirEntry, err error) error {
		if err != nil && strings.Contains(err.Error(), rootEscapeText) {
			err = fmt.Errorf("%w: %w", ErrEscapes, err)
		}

		return fn(filepath.Join(root, filepath.FromSlash(p)), d, err)
	})
}
