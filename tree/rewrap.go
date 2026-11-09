package tree

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// errNoBinWrap is the error of Rewrap on a tree without a BinWrap.
var errNoBinWrap = errors.New("the tree has no BinWrap to generate wrappers with")

// Rewrap regenerates the command wrapper of every command of every rock the
// transaction has, for this tree's root and t.BinWrap, exactly where Deploy
// would write one: for a command whose real script, in the rock's install
// dir, is Lua, of a rock whose rockspec does not set deploy.wrap_bin_scripts
// = false, at its plain or versioned path in BinDir as the providers have it.
// A command copied verbatim is left as it is. It returns the paths in BinDir
// it rewrote, those whose wrapper differed, in View.Rocks order.
//
// It is for a tree moved or extracted to another root, or run by another
// interpreter: the wrappers name the tree's module dirs, the rock's real
// script and the interpreter by absolute path. The tree manifest does not
// change. A tree without a BinWrap fails it, and so does a rock whose
// rock_manifest or rockspec cannot be read, with an error wrapping
// ErrCorrupt; the wrappers rewritten before are put back then.
func (tx *Tx) Rewrap() ([]string, error) {
	if tx.done {
		return nil, errTxDone
	}

	var out []string

	err := tx.atomic(func() error {
		var err error

		out, err = tx.rewrap()

		return err
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// rewrap is Rewrap, without putting back what it rewrote when it fails.
func (tx *Tx) rewrap() ([]string, error) {
	t := tx.treeFor()
	if t.BinWrap == nil {
		return nil, fmt.Errorf("tree.Rewrap: %w", errNoBinWrap)
	}

	v := tx.View()

	var out []string

	for _, ref := range v.Rocks() {
		files, err := v.Files(ref)
		if err != nil {
			return out, fmt.Errorf("tree.Rewrap: %w", err)
		}

		wraps, err := t.wrapsInstalled(ref, files)
		if err != nil {
			return out, fmt.Errorf("tree.Rewrap: %w", err)
		}

		if !wraps {
			continue
		}

		for _, f := range files {
			if f.Section != SectionBin || !t.isLuaScript(f.Script) {
				continue
			}

			body := []byte(t.binWrapperScript(f.Script, ref.Name, ref.Version))
			if have, err := readInside(t.BinDir(), f.Path); err == nil && bytes.Equal(have, body) {
				continue
			}

			if err := t.j.remove(t.BinDir(), f.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return out, fmt.Errorf("tree.Rewrap: %w", err)
			}

			if err := t.j.writeFile(t.BinDir(), f.Path, body, execPerm); err != nil {
				return out, fmt.Errorf("tree.Rewrap: write %s: %w", f.Path, err)
			}

			out = append(out, f.Path)
		}
	}

	return out, nil
}

// wrapsInstalled reports whether the installed rock ref, whose files are
// files, gets command wrappers: whether its installed rockspec leaves
// deploy.wrap_bin_scripts on. A rock without commands needs no rockspec read.
func (t *Tree) wrapsInstalled(ref RockRef, files []File) (bool, error) {
	for _, f := range files {
		if f.Section != SectionBin {
			continue
		}

		spec, err := t.installedRockspec(ref, files)
		if err != nil {
			return false, err
		}

		return wrapsCommands(spec), nil
	}

	return false, nil
}

// installedRockspec evaluates the rockspec of the installed rock ref, whose
// files are files: the one its rock_manifest lists, or the one named after
// it in its install dir. One that cannot be read fails it with an error
// wrapping ErrCorrupt.
func (t *Tree) installedRockspec(ref RockRef, files []File) (*rocks.Rockspec, error) {
	path := filepath.Join(t.InstallDir(ref.Name, ref.Version), rockspecName(ref.Name, ref.Version))

	for _, f := range files {
		if f.Section == SectionRockspec {
			path = f.Path
		}
	}

	spec, err := readRockspec(t.Tree, path)
	if err != nil {
		return nil, fmt.Errorf("%w: rockspec of %s: %w", ErrCorrupt, ref, err)
	}

	return spec, nil
}

// isLuaScript is isLua of the file at path, which must be a regular file
// inside the tree.
func (t *Tree) isLuaScript(path string) bool {
	st, err := safepath.Lstat(t.Tree, path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}

	return isLua(path)
}

// readInside returns the contents of the regular file path inside root.
func readInside(root, path string) ([]byte, error) {
	st, err := safepath.Lstat(root, path)
	if err != nil {
		return nil, err
	}

	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	f, err := safepath.Open(root, path)
	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	return io.ReadAll(f)
}
