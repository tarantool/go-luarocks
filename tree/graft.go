package tree

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/internal/treelock"
	"github.com/tarantool/go-luarocks/rockspec"
)

// GraftOpts tunes Tx.Graft.
type GraftOpts struct {
	// Select picks the rocks of the source tree to graft; nil grafts every
	// one. It is asked before anything of the rock is read.
	Select func(RockRef) bool
	// Replace grafts a version installed here already with other contents,
	// removing the installed one first, where Graft fails with ErrConflict
	// otherwise.
	Replace bool
	// RemoveOtherVersions removes, before grafting, every installed version of
	// a grafted rock that Graft does not graft, so the tree ends with the
	// versions of the source tree; Graft keeps them otherwise.
	RemoveOtherVersions bool
	// NoDisplace refuses, with an error wrapping ErrNotPlain and before
	// anything changes, a graft that would take the plain path of a file
	// from a rock installed here of another name than every grafted rock's,
	// as StagedOpts.NoDisplace refuses an install that would. The grafted
	// rocks share their items among themselves, and with the other versions
	// of their own names, by the tree's conflict rules.
	NoDisplace bool
	// RequirePlain refuses, with an error wrapping ErrNotPlain and before
	// anything changes, a graft a file of a grafted rock of which would not
	// be at its plain path once grafted, another rock, grafted or installed
	// here, holding it, as StagedOpts.RequirePlain refuses an install. To ask
	// it of one rock, the package of an archive say, graft that rock alone
	// with it (Select), after the others.
	RequirePlain bool
}

// GraftReport is what Tx.Graft did.
type GraftReport struct {
	// Grafted are the rock versions installed, in View.Rocks order.
	Grafted []RockRef
	// Skipped are the rock versions installed here already with the same
	// rock_manifest, which Graft left as they were, in View.Rocks order.
	Skipped []RockRef
	// Untracked are the files in the source tree's deploy dirs — under
	// share/, lib/ and bin/, its rocks dir aside — that no rock of the source
	// tree lists, at their plain or versioned path: files laid there by hand
	// or by another tool, and backups. Graft does not copy them. Sorted, each
	// joined onto the source tree's root.
	Untracked []string
}

// graftRock is a rock of the source tree staged for Graft.
type graftRock struct {
	ref  RockRef
	rm   *rocks.RockManifest
	spec *rocks.Rockspec
	dir  string
}

// Graft installs rocks of the tree rooted at src — an extracted package's
// .rocks, say — into this one, as the native client would install their
// binary rocks. Each rock dir src has on disk, <rocks-dir>/<name>/<version>
// with a version upstream takes for one, that opts.Select picks is staged as
// the native Pack stages an installed rock: its install dir copied whole but
// for the build/ staging dir of a native build, and each lua and lib file its rock_manifest lists copied back from the deploy
// dirs, from its versioned or its plain path, whichever has the digest the
// rock_manifest records. Every file staged must have its digest — a missing
// or a different one fails Graft with an error wrapping ErrIntegrity — and
// the rockspec must evaluate to the rock's name and version. src's tree
// manifest is neither trusted nor copied: which rock held a plain path there
// does not matter. Then each rock is installed as InstallStaged installs one,
// with this tree's conflict rules and BinWrap, and recorded in the manifest;
// a rock goes after the grafted rocks it depends on, so their resolved
// dependencies are what a native install in dependency order records.
//
// A version installed here already is skipped when its rock_manifest is the
// same as the one src has; one with another rock_manifest fails Graft with an
// error wrapping ErrConflict, unless opts.Replace.
//
// Graft reads src only inside it, and changes nothing here before every rock
// is staged and checked: a symlink in src leading out of it, or a
// rock_manifest entry leaving its directory, fails it with an error wrapping
// rocks.ErrUnsafePath, a rock dir without a rock_manifest that parses with
// one wrapping ErrCorrupt, and a conflict with ErrConflict, with this tree as
// it was. The rocks are staged in a temporary directory, removed afterwards.
// While it reads src it holds src's lock, shared, when src has a lock file,
// waiting for it until the Update's context ends (ErrLocked); src must not be
// this tree.
//
// A rock that fails to install undoes the whole Graft: what it removed and
// installed before is put back, and it returns no report.
//
// The report lists the files of src's deploy dirs no rock of src lists,
// which Graft does not copy.
func (tx *Tx) Graft(src string, opts GraftOpts) (*GraftReport, error) {
	if tx.done {
		return nil, errTxDone
	}

	if st, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("tree.Graft: %w", err)
	} else if !st.IsDir() {
		return nil, fmt.Errorf("tree.Graft: %s is not a directory", src)
	}

	from := Paths{Tree: src}
	if sameDir(from.RocksDir(), tx.tree.RocksDir()) {
		return nil, fmt.Errorf("tree.Graft: %s is this tree", src)
	}

	lock, err := treelock.Shared(tx.ctx, from.RocksDir(), false)
	if err != nil {
		return nil, fmt.Errorf("tree.Graft: %s: %w", src, lockError(err))
	}

	stageRoot, err := os.MkdirTemp("", "rocks-graft-*")
	if err != nil {
		lock.Unlock()

		return nil, fmt.Errorf("tree.Graft: %w", err)
	}

	defer func() { _ = os.RemoveAll(stageRoot) }()

	report, staged, err := stageGraft(from, stageRoot, opts.Select)

	lock.Unlock()

	if err != nil {
		return nil, fmt.Errorf("tree.Graft: %w", err)
	}

	var todo []graftRock

	var conflicts []string

	for _, r := range staged {
		installed, ok := installedRef(tx.m, r.ref.Name, r.ref.Version)
		if !ok {
			todo = append(todo, r)

			continue
		}

		have, err := tx.tree.Store.ReadRock(filepath.Join(tx.tree.InstallDir(installed.Name, installed.Version), rockManifestName))
		if err == nil && sameRockManifest(have, r.rm) {
			report.Skipped = append(report.Skipped, r.ref)

			continue
		}

		if !opts.Replace {
			conflicts = append(conflicts, installed.String())

			continue
		}

		todo = append(todo, r)
	}

	if len(conflicts) > 0 {
		return nil, fmt.Errorf("tree.Graft: %w: %s", ErrConflict, strings.Join(conflicts, ", "))
	}

	if opts.NoDisplace || opts.RequirePlain {
		if err := tx.checkGraftPlain(staged, todo, opts); err != nil {
			return nil, fmt.Errorf("tree.Graft: %w", err)
		}
	}

	err = tx.atomic(func() error {
		if opts.RemoveOtherVersions {
			if err := tx.removeUngrafted(staged); err != nil {
				return err
			}
		}

		for _, r := range graftOrder(todo) {
			// A spelling of the version other than src's is removed by its own
			// name: Deploy removes the very one it installs only.
			if installed, ok := installedRef(tx.m, r.ref.Name, r.ref.Version); ok && installed != r.ref {
				if err := tx.deleteVersion(installed.Name, installed.Version); err != nil {
					return err
				}
			}

			if _, err := tx.InstallStaged(r.spec, r.dir, StagedOpts{}); err != nil {
				return err
			}

			report.Grafted = append(report.Grafted, r.ref)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("tree.Graft: %w", err)
	}

	sortedRefs(report.Grafted)

	return report, nil
}

// sameDir reports whether a and b name the same directory: the same path, or
// two paths of one existing directory.
func sameDir(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)

	if errA == nil && errB == nil && absA == absB {
		return true
	}

	stA, errA := os.Stat(a)
	stB, errB := os.Stat(b)

	return errA == nil && errB == nil && os.SameFile(stA, stB)
}

// stageGraft stages every rock of from that sel picks under stageRoot and
// lists the untracked files of from.
func stageGraft(from Paths, stageRoot string, sel func(RockRef) bool) (*GraftReport, []graftRock, error) {
	found, err := diskRocks(from, false)
	if err != nil {
		return nil, nil, err
	}

	owned := map[string]bool{}

	var staged []graftRock

	for i, d := range found {
		picked := sel == nil || sel(d.ref)

		rm, err := readRockManifestIn(from.Tree, d.dir)
		if err != nil {
			if picked {
				return nil, nil, fmt.Errorf("%s: %w", d.ref, err)
			}

			continue
		}

		ownDeployed(from, d.ref, rm, owned)

		if !picked {
			continue
		}

		dir := filepath.Join(stageRoot, strconv.Itoa(i))
		if err := stageRock(from, d.ref, rm, dir); err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.ref, err)
		}

		spec, err := stagedRockspec(d.ref, rm, dir)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", d.ref, err)
		}

		staged = append(staged, graftRock{ref: d.ref, rm: rm, spec: spec, dir: dir})
	}

	report := &GraftReport{}

	err = walkDeployed(from, func(path, rel string) error {
		if !owned[rel] {
			report.Untracked = append(report.Untracked, path)
		}

		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	slices.Sort(report.Untracked)

	return report, staged, nil
}

// ownDeployed records in owned the paths, relative to the root of p, that
// the deployed files rm lists may be at: each plain path, and the versioned
// one.
func ownDeployed(p Paths, ref RockRef, rm *rocks.RockManifest, owned map[string]bool) {
	t := &Tree{Paths: p}

	for _, sec := range []struct {
		dt    string
		files map[string]string
	}{{deployLua, rm.Lua}, {deployLib, rm.Lib}, {deployBin, rm.Bin}} {
		dir := t.deployDirOf(sec.dt)

		for rel := range sec.files {
			plain, err := safepath.JoinEntry(dir, filepath.FromSlash(rel))
			if err != nil {
				continue
			}

			owned[treeRel(p, plain)] = true
			owned[treeRel(p, MungedPath(dir, plain, ref.Name, ref.Version))] = true
		}
	}
}

// buildStagingDir is the directory of a rock's install dir a native build
// stages the rock in and keeps; no rock_manifest lists it.
const buildStagingDir = "build"

// stageRock lays the installed rock ref of the tree p out in dir as an
// unpacked binary rock, as the native Pack does: the rock's install dir
// copied whole but for its build/ staging dir, which is no file of the rock,
// and the lua and lib files of its rock_manifest rm copied back
// from the deploy dirs into lua/ and lib/. A file is copied back from its
// versioned path or from its plain one, whichever holds the digest rm
// records, so it does not matter which rock held the plain path. Every file
// rm lists must then be in dir with its digest, or stageRock fails with an
// error wrapping ErrIntegrity. Everything is read inside the root of p.
func stageRock(p Paths, ref RockRef, rm *rocks.RockManifest, dir string) error {
	installDir := p.InstallDir(ref.Name, ref.Version)

	err := safepath.WalkDir(p.Tree, installDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(installDir, path)
		if err != nil {
			return err
		}

		if rel == buildStagingDir && d.IsDir() {
			return filepath.SkipDir
		}

		if d.IsDir() {
			return safepath.MkdirAll(dir, filepath.Join(dir, rel), dirPerm)
		}

		return copyKeepMode(p.Tree, path, dir, filepath.Join(dir, rel))
	})
	if err != nil {
		return fmt.Errorf("copy %s: %w", installDir, err)
	}

	t := &Tree{Paths: p}

	var bad []string

	for _, sec := range []struct {
		dt    string
		files map[string]string
		mode  os.FileMode
	}{{deployLua, rm.Lua, filePerm}, {deployLib, rm.Lib, execPerm}} {
		deployDir := t.deployDirOf(sec.dt)

		for _, rel := range sortedKeys(sec.files) {
			found, err := copyBack(p.Tree, deployDir, dir, sec.dt, ref, rel, sec.files[rel], sec.mode)
			if err != nil {
				return err
			}

			if !found {
				bad = append(bad, sec.dt+"/"+rel+": missing or modified at both its plain and its versioned path")
			}
		}
	}

	bad = append(bad, checkStaged(dir, rm)...)

	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrIntegrity, strings.Join(bad, "; "))
	}

	return nil
}

// copyBack copies the deployed file rel of ref, under deployDir inside root,
// to rel under the subdirectory sub of the stage dir, from its versioned path
// or its plain one, the first with the digest want, and reports whether
// either had it. An entry that leaves its directory, or a path that leads out
// of root, fails it with an error wrapping rocks.ErrUnsafePath.
func copyBack(root, deployDir, stage, sub string, ref RockRef, rel, want string, mode os.FileMode) (bool, error) {
	plain, err := safepath.JoinEntry(deployDir, filepath.FromSlash(rel))
	if err != nil {
		return false, err
	}

	target, err := safepath.JoinEntry(filepath.Join(stage, sub), filepath.FromSlash(rel))
	if err != nil {
		return false, err
	}

	for _, from := range []string{MungedPath(deployDir, plain, ref.Name, ref.Version), plain} {
		sum, err := digestOf(root, from)
		if errors.Is(err, rocks.ErrUnsafePath) {
			return false, err
		}

		if err != nil || sum != want {
			continue
		}

		if _, err := copyFile(root, from, stage, target, mode); err != nil {
			return false, err
		}

		return true, nil
	}

	return false, nil
}

// checkStaged checks the files of rm a staged rock in dir holds besides its
// lua and lib ones — its rockspec, its commands' scripts in bin/, and its
// conf, doc and other files — against their digests, and describes each that
// is missing or differs.
func checkStaged(dir string, rm *rocks.RockManifest) []string {
	var bad []string

	check := func(desc, path, want string) {
		got, err := digestOf(dir, path)

		switch {
		case err != nil:
			bad = append(bad, desc+": missing")
		case got != want:
			bad = append(bad, desc+": modified")
		}
	}

	if rm.Rockspec != "" {
		check(rm.RockspecFile, filepath.Join(dir, rm.RockspecFile), rm.Rockspec)
	}

	for _, sec := range []struct {
		name, sub string
		files     map[string]string
	}{
		{deployBin, deployBin, rm.Bin},
		{string(SectionConf), string(SectionConf), rm.Conf},
		{string(SectionDoc), string(SectionDoc), rm.Doc},
		{string(SectionOther), "", rm.Other},
	} {
		for _, rel := range sortedKeys(sec.files) {
			check(sec.name+"/"+rel, filepath.Join(dir, sec.sub, filepath.FromSlash(rel)), sec.files[rel])
		}
	}

	return bad
}

// stagedRockspec evaluates the rockspec of the rock ref staged in dir, as the
// native client evaluates the rockspec of a rock it installs, and checks
// that it is ref's.
func stagedRockspec(ref RockRef, rm *rocks.RockManifest, dir string) (*rocks.Rockspec, error) {
	name := rm.RockspecFile
	if name == "" {
		name = rockspecName(ref.Name, ref.Version)
	}

	spec, err := readRockspec(dir, filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("%w: rockspec: %w", ErrCorrupt, err)
	}

	if err := rockspec.Validate(spec); err != nil {
		return nil, fmt.Errorf("rockspec: %w", err)
	}

	if spec.Package != ref.Name || spec.Version != ref.Version {
		return nil, fmt.Errorf("%w: the rockspec is the one of %s %s", ErrCorrupt, spec.Package, spec.Version)
	}

	return spec, nil
}

// checkGraftPlain checks grafting todo, of the rocks staged, against the
// plain-path options of opts: it fails as checkDisplaced does with
// opts.NoDisplace, and as checkPlainAfter does with opts.RequirePlain. It
// plays the graft on a copy of the manifest: the versions of the staged
// rocks' names staged does not have removed when opts.RemoveOtherVersions is
// set, as removeUngrafted removes them, then each rock of todo, in the order
// Graft installs them, put in the place of its installed version.
func (tx *Tx) checkGraftPlain(staged, todo []graftRock, opts GraftOpts) error {
	m := cloneManifest(tx.m)

	if opts.RemoveOtherVersions {
		keep := map[string][]string{}
		for _, r := range staged {
			keep[r.ref.Name] = append(keep[r.ref.Name], r.ref.Version)
		}

		for _, name := range slices.Sorted(maps.Keys(keep)) {
			for _, version := range slices.Sorted(maps.Keys(m.Repository[name])) {
				if !slices.ContainsFunc(keep[name], func(k string) bool { return sameVersion(k, version) }) {
					removeFromManifest(m, name, version)
				}
			}
		}
	}

	in := make([]incoming, 0, len(todo))

	for _, r := range graftOrder(todo) {
		if installed, ok := installedRef(m, r.ref.Name, r.ref.Version); ok {
			removeFromManifest(m, installed.Name, installed.Version)
		}

		addToManifest(m, r.spec, r.rm)

		in = append(in, incoming{ref: r.ref, rm: r.rm})
	}

	if opts.RequirePlain {
		if err := checkPlainAfter(tx.tree.Paths, m, in); err != nil {
			return err
		}
	}

	if opts.NoDisplace {
		return checkDisplaced(tx.tree.Paths, tx.m, m, in)
	}

	return nil
}

// removeUngrafted removes every installed version of a rock of staged that
// staged does not have.
func (tx *Tx) removeUngrafted(staged []graftRock) error {
	keep := map[string][]string{}
	for _, r := range staged {
		keep[r.ref.Name] = append(keep[r.ref.Name], r.ref.Version)
	}

	for _, name := range slices.Sorted(maps.Keys(keep)) {
		if err := tx.removeOtherVersions(name, keep[name]); err != nil {
			return err
		}
	}

	return nil
}

// graftOrder orders rocks so that each comes after the rocks of the list it
// depends on, by name, and otherwise in the order given.
func graftOrder(rs []graftRock) []graftRock {
	byName := map[string][]int{}
	for i, r := range rs {
		byName[r.ref.Name] = append(byName[r.ref.Name], i)
	}

	visited := make([]bool, len(rs))
	out := make([]graftRock, 0, len(rs))

	var visit func(i int)

	visit = func(i int) {
		if visited[i] {
			return
		}

		visited[i] = true

		for _, d := range rs[i].spec.Dependencies {
			if d.Name == rs[i].ref.Name {
				continue
			}

			for _, j := range byName[d.Name] {
				visit(j)
			}
		}

		out = append(out, rs[i])
	}

	for i := range rs {
		visit(i)
	}

	return out
}
