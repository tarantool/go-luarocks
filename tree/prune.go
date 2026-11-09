package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/tarantool/go-luarocks/internal/safepath"
)

// PruneOpts tunes Tx.Prune.
type PruneOpts struct {
	// Orphans also removes the install dirs, <rocks-dir>/<name>/<version>,
	// that the tree manifest does not list — what an interrupted install, or
	// a removal by hand, leaves — whatever keep says of them. Their deployed
	// files, if any, are left alone: the manifest no longer tells which spots
	// they hold.
	Orphans bool
}

// Prune removes, in one pass, every installed rock version keep does not
// keep, as Remove removes one, and returns what it removed, in View.Rocks
// order. A nil keep keeps every rock.
//
// The versions are removed one by one, as Remove removes each, in a fixed
// order whatever order the rocks were installed in or keep is asked in: the
// reverse of the provider lists' — rock names descending, and of each rock
// the oldest version first — so the plain path of an item only ever passes to
// a version Prune keeps, never to one it removes next.
//
// On an error Prune stops, undoes what it removed and returns nothing.
func (tx *Tx) Prune(keep func(RockRef) bool, opts PruneOpts) ([]RockRef, error) {
	if tx.done {
		return nil, errTxDone
	}

	var removed []RockRef

	err := tx.atomic(func() error {
		var err error

		removed, err = tx.prune(keep, opts)

		return err
	})
	if err != nil {
		return nil, err
	}

	return removed, nil
}

// prune is Prune, without undoing what it removed when it fails.
func (tx *Tx) prune(keep func(RockRef) bool, opts PruneOpts) ([]RockRef, error) {
	var doomed []RockRef

	if keep != nil {
		for _, ref := range tx.View().Rocks() {
			if !keep(ref) {
				doomed = append(doomed, ref)
			}
		}
	}

	var removed []RockRef

	for _, ref := range slices.Backward(doomed) {
		if err := tx.deleteVersion(ref.Name, ref.Version); err != nil {
			return sortedRefs(removed), fmt.Errorf("tree.Prune: %s: %w", ref, err)
		}

		removed = append(removed, ref)
	}

	if opts.Orphans {
		orphans, err := tx.removeOrphans()
		removed = append(removed, orphans...)

		if err != nil {
			return sortedRefs(removed), fmt.Errorf("tree.Prune: %w", err)
		}
	}

	return sortedRefs(removed), nil
}

// sortedRefs sorts refs in View.Rocks order and returns them.
func sortedRefs(refs []RockRef) []RockRef {
	slices.SortFunc(refs, compareRefs)

	return refs
}

// removeOrphans removes every install dir under the rocks dir that the
// manifest does not list, and every rock name dir left empty, and returns the
// install dirs it removed as rock versions. An entry that is not a directory,
// or not a single path element, is left alone.
func (tx *Tx) removeOrphans() ([]RockRef, error) {
	rocksDir := tx.tree.RocksDir()

	names, err := os.ReadDir(rocksDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("list orphans: %w", err)
	}

	var removed []RockRef

	for _, name := range names {
		if !name.IsDir() || safepath.Name(name.Name()) != nil {
			continue
		}

		nameDir := filepath.Join(rocksDir, name.Name())
		if tx.j.isTrash(nameDir) {
			continue
		}

		versions, err := os.ReadDir(nameDir)
		if err != nil {
			return removed, fmt.Errorf("list orphans: %w", err)
		}

		for _, ver := range versions {
			if !ver.IsDir() || safepath.Name(ver.Name()) != nil {
				continue
			}

			if _, listed := tx.m.Repository[name.Name()][ver.Name()]; listed {
				continue
			}

			if err := tx.j.removeAll(rocksDir, filepath.Join(nameDir, ver.Name())); err != nil {
				return removed, fmt.Errorf("remove orphan: %w", err)
			}

			removed = append(removed, RockRef{Name: name.Name(), Version: ver.Name()})
		}

		if rest, err := os.ReadDir(nameDir); err == nil && len(rest) == 0 {
			_ = tx.j.remove(rocksDir, nameDir)
		}
	}

	return removed, nil
}

// KeepVersions returns the keep function of Prune for a set of pins, rock
// name → version: it keeps a rock version when the pins name the rock at a
// version deps.Compare finds equal (or, for a version that does not parse, the
// very string), and no other — a rock the pins do not name is pruned.
func KeepVersions(pins map[string]string) func(RockRef) bool {
	return func(r RockRef) bool {
		pinned, ok := pins[r.Name]

		return ok && sameVersion(pinned, r.Version)
	}
}
