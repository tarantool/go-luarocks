package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/manif"
)

// The helpers here read a tree from its files rather than from its manifest:
// the rock dirs under its rocks dir, and the files of its deploy dirs. Graft,
// Rebuild and Check use them; every path is opened inside the tree's root, so
// a symlink leading out of it is never followed.

// diskRock is a rock dir found under a rocks dir: <rocks-dir>/<name>/<version>.
type diskRock struct {
	ref RockRef
	dir string
}

// subdirs returns the names of the directories in dir, inside root, sorted: a
// subdirectory, or a symlink to one that stays inside root. A symlink that
// leads out fails it with an error wrapping rocks.ErrUnsafePath; any other
// entry is left out, and so is a dir that does not exist.
func subdirs(root, dir string) ([]string, error) {
	f, err := safepath.Open(root, dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	entries, err := f.ReadDir(-1)
	_ = f.Close()

	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	var out []string

	for _, e := range entries {
		switch {
		case e.IsDir():
		case e.Type()&fs.ModeSymlink != 0:
			isDir, err := linksToDir(root, filepath.Join(dir, e.Name()))
			if err != nil {
				return nil, err
			}

			if !isDir {
				continue
			}
		default:
			continue
		}

		out = append(out, e.Name())
	}

	slices.Sort(out)

	return out, nil
}

// linksToDir reports whether the symlink at p, inside root, leads to a
// directory inside root; one that leads out fails it with an error wrapping
// rocks.ErrUnsafePath, a dangling one is no directory.
func linksToDir(root, p string) (bool, error) {
	f, err := safepath.Open(root, p)
	if errors.Is(err, rocks.ErrUnsafePath) {
		return false, err
	}

	if err != nil {
		return false, nil // a dangling link is no directory
	}

	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("stat %s: %w", p, err)
	}

	return st.IsDir(), nil
}

// isRockVersion reports whether name is the name of a version dir as
// upstream's search.disk_search takes one: it ends in a dash and a revision
// number.
func isRockVersion(name string) bool {
	i := strings.LastIndexByte(name, '-')
	if i < 0 || i == len(name)-1 {
		return false
	}

	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}

	return true
}

// diskRocks returns every rock dir under the rocks dir of p, in View.Rocks
// order: each <name>/<version> dir whose name and version are single path
// elements, the version one isRockVersion takes. With all, a version dir of
// any name is returned.
func diskRocks(p Paths, all bool) ([]diskRock, error) {
	names, err := subdirs(p.Tree, p.RocksDir())
	if err != nil {
		return nil, err
	}

	var out []diskRock

	for _, name := range names {
		versions, err := subdirs(p.Tree, filepath.Join(p.RocksDir(), name))
		if err != nil {
			return nil, err
		}

		for _, version := range versions {
			if !all && !isRockVersion(version) {
				continue
			}

			ref := RockRef{Name: name, Version: version}
			out = append(out, diskRock{ref: ref, dir: p.InstallDir(name, version)})
		}
	}

	slices.SortFunc(out, func(a, b diskRock) int { return compareRefs(a.ref, b.ref) })

	return out, nil
}

// readRockManifestIn reads the rock_manifest of the rock dir dir, inside
// root: it must be a regular file reached without leaving root. A
// rock_manifest that is missing or does not parse fails it with an error
// wrapping ErrCorrupt, and so does one naming a path outside its directory.
func readRockManifestIn(root, dir string) (*rocks.RockManifest, error) {
	p := filepath.Join(dir, rockManifestName)

	st, err := safepath.Lstat(root, p)

	switch {
	case errors.Is(err, rocks.ErrUnsafePath):
		return nil, err
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	case !st.Mode().IsRegular():
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrCorrupt, p)
	}

	rm, err := manif.FileStore{}.ReadRock(p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}

	if err := checkRockManifest(rm); err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrCorrupt, p, err)
	}

	return rm, nil
}

// checkRockManifest checks that every entry of rm names a file inside its
// section's directory, failing with an error wrapping rocks.ErrUnsafePath.
func checkRockManifest(rm *rocks.RockManifest) error {
	if rm.RockspecFile != "" {
		if err := safepath.Name(rm.RockspecFile); err != nil {
			return fmt.Errorf("rockspec entry: %w", err)
		}
	}

	for _, sec := range []struct {
		name  string
		files map[string]string
	}{
		{deployLua, rm.Lua}, {deployLib, rm.Lib}, {deployBin, rm.Bin},
		{string(SectionConf), rm.Conf}, {string(SectionDoc), rm.Doc}, {string(SectionOther), rm.Other},
	} {
		for rel := range sec.files {
			if _, err := safepath.JoinEntry(sec.name, filepath.FromSlash(rel)); err != nil {
				return fmt.Errorf("%s entry: %w", sec.name, err)
			}
		}
	}

	return nil
}

// sameRockManifest reports whether two rock_manifests record the same files
// with the same digests.
func sameRockManifest(a, b *rocks.RockManifest) bool {
	return a.RockspecFile == b.RockspecFile && a.Rockspec == b.Rockspec &&
		maps.Equal(a.Lua, b.Lua) && maps.Equal(a.Lib, b.Lib) && maps.Equal(a.Bin, b.Bin) &&
		maps.Equal(a.Conf, b.Conf) && maps.Equal(a.Doc, b.Doc) && maps.Equal(a.Other, b.Other)
}

// deployRoots are the directories at a tree's root whose files are a rock's
// deployed ones — under share/tarantool and lib/tarantool — or none's: Check
// and Graft look for files no rock owns there. The other directories at the
// root are a tool's own, as tt keeps its metadata in one.
var deployRoots = []string{"share", "lib", "bin"}

// walkDeployed calls fn with every entry that is not a directory under the
// deploy roots of p, the rocks dir left out, by its path and its path
// relative to the root, slash-separated. A symlink is an entry, followed
// nowhere; a deploy root that is one leading out of the tree is reported as
// an entry too.
func walkDeployed(p Paths, fn func(path, rel string) error) error {
	rocksDir := filepath.Clean(p.RocksDir())

	if _, err := os.Stat(p.Tree); errors.Is(err, fs.ErrNotExist) {
		return nil
	}

	for _, top := range deployRoots {
		dir := filepath.Join(p.Tree, top)

		err := safepath.WalkDir(p.Tree, dir, func(path string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && path == dir && errors.Is(err, fs.ErrNotExist):
				return nil
			case err != nil && path == dir && errors.Is(err, rocks.ErrUnsafePath):
				return fn(path, top)
			case err != nil:
				return err
			case d.IsDir() && filepath.Clean(path) == rocksDir:
				return filepath.SkipDir
			case d.IsDir():
				return nil
			}

			rel, err := filepath.Rel(p.Tree, path)
			if err != nil {
				return err
			}

			return fn(path, filepath.ToSlash(rel))
		})
		if err != nil {
			return fmt.Errorf("list %s: %w", dir, err)
		}
	}

	return nil
}

// treeRel returns the slash-separated path of path, under the root of p,
// relative to the root.
func treeRel(p Paths, path string) string {
	rel, err := filepath.Rel(p.Tree, path)
	if err != nil {
		return filepath.ToSlash(path)
	}

	return filepath.ToSlash(rel)
}
