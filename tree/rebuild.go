package tree

import (
	"context"
	"fmt"
	"path/filepath"

	rocks "github.com/tarantool/go-luarocks"
)

// RebuildOpts tunes Tree.Rebuild.
type RebuildOpts struct {
	// Strict fails Rebuild on a rock dir it would skip, writing nothing, as
	// upstream make_manifest fails on a rock_manifest it cannot load.
	Strict bool
}

// RebuildReport is what Tree.Rebuild found.
type RebuildReport struct {
	// Rocks are the rock versions the rebuilt manifest lists, in View.Rocks
	// order.
	Rocks []RockRef
	// Skipped are the rock dirs left out of the manifest, in View.Rocks
	// order.
	Skipped []SkippedRock
	// NoRockspec are the rocks listed without their declared dependencies,
	// since their installed rockspec is missing or does not evaluate, as
	// upstream lists such a rock.
	NoRockspec []RockRef
}

// SkippedRock is a rock dir Rebuild left out of the manifest.
type SkippedRock struct {
	// Rock is the rock version the dir is named after.
	Rock RockRef
	// Dir is the rock dir, <rocks-dir>/<name>/<version>.
	Dir string
	// Err is why: its rock_manifest is missing, does not parse (ErrCorrupt),
	// or names a path outside its directory (rocks.ErrUnsafePath).
	Err error
}

// Rebuild rewrites the tree manifest from the rocks on disk, as `luarocks-admin
// make_manifest --local-tree` does: every rock dir under the rocks dir,
// <name>/<version> with a version upstream takes for one (it ends in a
// revision number), is listed with the modules and commands of its
// rock_manifest (ModuleIndex, CommandIndex) and the dependencies its installed
// rockspec declares, and is added to the provider list of each of its items
// in the native order, which is upstream's: by rock name, newest version
// first. The manifest Rebuild reads nothing of the manifest there was, which
// may be missing or not parse.
//
// The resolved dependencies of each rock are its direct ones, each at the
// newest version of the rebuilt tree its constraints accept, as a native
// install of the rocks in dependency order records them; upstream recomputes
// them transitively. Deployed files are not moved: a file whose spot does not
// match the rebuilt provider lists shows in Check.
//
// A rock dir whose rock_manifest is missing, does not parse or names a path
// outside its directory is skipped and reported, or with opts.Strict fails
// Rebuild, the manifest unchanged; one whose rockspec is missing or does not
// evaluate is listed without its declared dependencies, as upstream lists it,
// and reported. Rebuild holds the tree's lock, exclusive, as Update does,
// waiting for it until ctx ends (ErrLocked), and writes the manifest once.
func (t *Tree) Rebuild(ctx context.Context, opts RebuildOpts) (*RebuildReport, error) {
	report := &RebuildReport{}

	empty := func() (*rocks.Manifest, error) { return normalizeManifest(&rocks.Manifest{}), nil }

	err := t.update(ctx, "tree.Rebuild", empty, func(tx *Tx) error {
		found, err := diskRocks(t.Paths, false)
		if err != nil {
			return fmt.Errorf("tree.Rebuild: %w", err)
		}

		specs := make([]*rocks.Rockspec, 0, len(found))

		for _, d := range found {
			rm, err := readRockManifestIn(t.Tree, d.dir)
			if err != nil {
				if opts.Strict {
					return fmt.Errorf("tree.Rebuild: %s: %w", d.ref, err)
				}

				report.Skipped = append(report.Skipped, SkippedRock{Rock: d.ref, Dir: d.dir, Err: err})

				continue
			}

			name := rm.RockspecFile
			if name == "" {
				name = rockspecName(d.ref.Name, d.ref.Version)
			}

			spec, err := readRockspec(t.Tree, filepath.Join(d.dir, name))
			if err != nil || spec.Package != d.ref.Name || spec.Version != d.ref.Version {
				report.NoRockspec = append(report.NoRockspec, d.ref)
				spec = &rocks.Rockspec{}
			}

			spec.Package, spec.Version = d.ref.Name, d.ref.Version

			addToManifest(tx.m, spec, rm)

			specs = append(specs, spec)
			report.Rocks = append(report.Rocks, d.ref)
		}

		// Every rock resolves its dependencies against the whole tree.
		for _, spec := range specs {
			entry := tx.m.Repository[spec.Package][spec.Version]
			entry.Dependencies = resolveInstalledDeps(spec, tx.m)
			tx.m.Repository[spec.Package][spec.Version] = entry
		}

		tx.changed()

		return nil
	})
	if err != nil {
		return nil, err
	}

	return report, nil
}
