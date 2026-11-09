package tree

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/internal/treelock"
)

// treeManifestName is the file name of the tree manifest in the rocks dir.
const treeManifestName = "manifest"

// The tree manifest, <rocks-dir>/manifest, lists every installed rock and,
// for each module and command, the rocks providing it. Deploy and
// DeleteVersion change the files of a tree; the code here keeps the manifest
// in step with them, so a tree's files and its manifest change together.

// RockRef names one installed version of a rock.
type RockRef struct {
	// Name is the rock's name.
	Name string
	// Version is its version, as the tree manifest spells it.
	Version string
}

// String returns the "name/version" form the tree manifest's provider lists
// use.
func (r RockRef) String() string {
	return r.Name + "/" + r.Version
}

// ErrNotInstalled is wrapped by the error of an operation on a rock version
// the tree manifest does not list.
var ErrNotInstalled = errors.New("rock is not installed")

// ErrLocked is wrapped by the error of an operation that takes a tree's lock
// — Update and Rebuild, exclusive; Inspect, Tree.View, Check and Tx.Graft,
// of its source, shared; and the client's writes to a tree — when another
// holds it until the context ends: a change, or for a change, a reader.
var ErrLocked = errors.New("rock tree is locked by another change")

// ErrConflict is wrapped by the error of Tx.Graft when a rock version it is to
// install is installed already with other contents: a rock_manifest that
// differs from the one it brings.
var ErrConflict = errors.New("rock version is installed with other contents")

// lockError is the error of taking a tree's lock: one wrapping ErrLocked when
// the lock stayed held.
func lockError(err error) error {
	if errors.Is(err, treelock.ErrBusy) {
		return fmt.Errorf("%w: %w", ErrLocked, err)
	}

	return err
}

// Tx is a change to a tree in progress: Update passes one to its function,
// and its methods change the files of the tree and the tree manifest
// together. A Tx is valid only while that function runs.
//
// Each method is a change of its own: one that fails undoes what it changed,
// the files and the manifest, and leaves the tree as it was before the call.
type Tx struct {
	tree *Tree
	m    *rocks.Manifest

	// j records the changes to the files of the tree since the Update began,
	// or since the last Commit, to undo them.
	j *journal

	// ctx is the context of the Update, which bounds the waits for another
	// tree's lock a Tx method takes.
	ctx context.Context //nolint:containedctx // a Tx lives only while its Update runs

	// view is the View of m, until m changes.
	view *View

	// dirty records that m differs from the manifest on disk.
	dirty bool
	done  bool
}

// Update changes the tree through fn: it takes the tree's lock, exclusive,
// loads the tree manifest — an empty one when the tree has none — and runs fn
// with a Tx over it, then writes the manifest when a Tx method changed it,
// and releases the lock.
//
// The change is all or nothing, as upstream LuaRocks rolls back a deploy, a
// build or an install that fails: when fn fails, or the manifest cannot be
// written, every change the Tx made since it began, or since its last Commit,
// is undone — files created are removed, files removed, overwritten or
// renamed are put back, the rock_manifests and the tree manifest are as they
// were — and the error is fn's, joined with that of the undo if it fails too.
// Removed and overwritten files are kept until then in a directory of the
// rocks dir named .tx-*, which the undo or the end of the change deletes; an
// undo that fails keeps it, and its error names it. A directory a Tx method
// creates for a writer it does not see — the install dir Deploy hands its
// deploy function, where a build writes — is removed whole. What fn changes
// in the tree by itself, other than through the Tx, is not undone.
//
// The lock is an advisory lock on the file .lock in the rocks dir (flock(2)
// on Linux, macOS and the BSDs; elsewhere it guards nothing), which Update
// creates, with the rocks dir, when the tree has none, even when fn changes
// nothing: to look into a tree that may not exist — whether it has anything
// to prune, say — take its View, which creates nothing. It orders every
// Update of the tree, every write of the client to it, and the loads of
// Inspect and View, which take it shared, across processes and goroutines
// alike: Update waits for the changes and loads before it to end, and fails
// with an error wrapping ErrLocked and the cause of ctx's end when ctx ends
// first. Other writers — an external `luarocks`, or Exec of the client's lua
// backend — do not take it. fn must not call Update, View or Inspect on the
// same tree, nor the client's writes to it: they would wait for the lock fn
// holds, until ctx ends, or for ever; tx.View is the view fn has.
func (t *Tree) Update(ctx context.Context, fn func(tx *Tx) error) error {
	return t.update(ctx, "tree.Update", t.readManifest, fn)
}

// update runs fn as Update does, over the manifest load returns, under the
// exclusive lock; op names the operation in its errors.
func (t *Tree) update(ctx context.Context, op string, load func() (*rocks.Manifest, error), fn func(tx *Tx) error) error {
	if err := os.MkdirAll(t.RocksDir(), dirPerm); err != nil {
		return fmt.Errorf("%s: mkdir %q: %w", op, t.RocksDir(), err)
	}

	lock, err := treelock.Exclusive(ctx, t.RocksDir())
	if err != nil {
		return fmt.Errorf("%s: %w", op, lockError(err))
	}

	defer lock.Unlock()

	m, err := load()
	if err != nil {
		return err
	}

	tx := &Tx{tree: t, m: m, ctx: ctx, j: newJournal(t.Paths)}

	err = fn(tx)
	if err == nil {
		err = tx.flush()
	}

	tx.done = true

	if err != nil {
		return errors.Join(err, tx.j.rollback())
	}

	tx.j.commit()

	return nil
}

// Commit makes the changes of the transaction so far permanent: it writes the
// tree manifest when they changed it, and a later failure — of a Tx method,
// of fn or of the final write of the manifest — no longer undoes them, only
// what changes after the Commit. It is for an fn that makes several changes
// and wants those that succeeded kept whatever happens to the next; the
// native client commits the removal of an installed copy of the version it
// builds, as upstream deletes that copy for good before it builds. A Commit
// that fails to write the manifest is undone, and so is everything since the
// last Commit when fn returns its error.
func (tx *Tx) Commit() error {
	if tx.done {
		return errTxDone
	}

	if err := tx.flush(); err != nil {
		return err
	}

	tx.j.commit()

	return nil
}

// atomic runs fn, a change of the transaction, so that when it fails every
// change it made to the files of the tree and to the manifest is undone, and
// the tree is as it was before.
func (tx *Tx) atomic(fn func() error) error {
	mark := tx.j.mark()
	m, dirty := cloneManifest(tx.m), tx.dirty

	err := fn()
	if err == nil {
		return nil
	}

	uerr := tx.j.undoTo(mark)
	tx.m, tx.dirty, tx.view = m, dirty, nil

	return errors.Join(err, uerr)
}

// createdDir records, when dir does not exist, that it is about to be created
// by a writer the journal does not see, so that undoing it removes it whole.
func (tx *Tx) createdDir(dir string) {
	if _, err := safepath.Lstat(tx.tree.Tree, dir); errors.Is(err, fs.ErrNotExist) {
		tx.j.created(dir)
	}
}

// errTxDone is the error of a Tx method called after its Update returned.
var errTxDone = errors.New("tree: the transaction is over")

// flush writes the manifest when it changed since the last write.
func (tx *Tx) flush() error {
	if !tx.dirty {
		return nil
	}

	if err := tx.j.save(filepath.Join(tx.tree.RocksDir(), treeManifestName)); err != nil {
		return fmt.Errorf("write tree manifest: %w", err)
	}

	if err := tx.tree.Store.WriteTree(tx.tree.RocksDir(), tx.m); err != nil {
		return fmt.Errorf("write tree manifest: %w", err)
	}

	tx.dirty = false

	return nil
}

// treeFor returns a copy of the Tx's tree whose Providers follow the Tx's
// manifest, so Deploy and DeleteVersion see the providers as they stand.
func (tx *Tx) treeFor() *Tree {
	t := *tx.tree
	t.Providers = manifestProviders(tx.m)
	t.j = tx.j

	return &t
}

// Deploy installs one rock in the transaction: it removes an installed copy
// of spec's version first, as upstream build_rockspec and install_binary_rock
// do (build.lua:393-395, install.lua:86-88), so a file the new copy no longer
// ships does not linger and the old copy is not taken for another provider of
// the new one's files; then it runs deploy, which puts the rock's files into
// the tree — tree.Deploy, after a build — given a copy of the tree whose
// Providers follow the transaction; then it writes the rock_manifest deploy
// returns and records the rock in the tree manifest. The copy has no BinWrap
// when the rockspec sets deploy.wrap_bin_scripts = false, so its commands are
// copied verbatim, as upstream repos.should_wrap_bin_scripts decides.
//
// The entry recorded indexes the modules and commands of the rock_manifest
// (ModuleIndex, CommandIndex), adds the rock to the provider list of each in
// upstream's order — which is the order of the spots Deploy gave them — and
// records the rock's declared dependencies and, resolved, its direct ones:
// each at the newest installed version its constraints accept. The entries of
// the other rocks are left as they are.
//
// A Deploy that fails — deploy, or the write of the rock_manifest — undoes
// what it changed, the removal of the installed copy included: the files
// tree.Deploy wrote, renamed or removed go back, and the rock's install dir,
// where deploy may build before it deploys, is removed whole when it did not
// exist before. A caller that wants the removal kept, as upstream keeps it,
// removes the copy first and commits (Tx.Commit).
func (tx *Tx) Deploy(spec *rocks.Rockspec, deploy func(t *Tree) (*rocks.RockManifest, error)) error {
	if tx.done {
		return errTxDone
	}

	return tx.atomic(func() error {
		t := tx.treeFor()
		if !wrapsCommands(spec) {
			t.BinWrap = nil
		}

		installDir := t.InstallDir(spec.Package, spec.Version)

		if _, err := os.Stat(installDir); err == nil {
			if err := tx.deleteVersion(spec.Package, spec.Version); err != nil {
				return err
			}

			if err := tx.flush(); err != nil {
				return err
			}
		}

		tx.createdDir(filepath.Dir(installDir))
		tx.createdDir(installDir)

		rm, err := deploy(t)
		if err != nil {
			return err
		}

		rockManifestPath := filepath.Join(installDir, rockManifestName)
		if err := t.j.save(rockManifestPath); err != nil {
			return fmt.Errorf("write rock_manifest: %w", err)
		}

		if err := t.Store.WriteRock(rockManifestPath, rm); err != nil {
			return fmt.Errorf("write rock_manifest: %w", err)
		}

		addToManifest(tx.m, spec, rm)
		tx.changed()

		return nil
	})
}

// Remove undeploys an installed rock version and drops it from the tree
// manifest, as upstream repos.delete_version and remove_from_manifest do: each
// file the version holds is deleted from its spot, the plain path or a
// versioned one, and an item whose plain path it held passes to the item's
// next provider. It does not ask whether another installed rock depends on
// the version: the caller owns the dependency graph.
//
// A version whose rock_manifest is missing or cannot be read, which upstream
// drops from the manifest alone, is undeployed by the files its entry in the
// tree manifest names: its commands, and its modules, each with the other
// form of the module beside it, so that the plain paths it held pass on; a
// file the entry does not name, such as a data file beside its modules, is
// left, and the install dir is removed whole.
//
// r.Version may be any spelling of an installed version that deps.Compare
// finds equal. A version the tree manifest does not list fails with an error
// wrapping ErrNotInstalled.
func (tx *Tx) Remove(r RockRef) error {
	if tx.done {
		return errTxDone
	}

	ref, ok := installedRef(tx.m, r.Name, r.Version)
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotInstalled, r)
	}

	return tx.atomic(func() error { return tx.deleteVersion(ref.Name, ref.Version) })
}

// deleteVersion undeploys name/version from the tree and drops it from the
// manifest: DeleteVersion consults the manifest, which still lists the
// version, to tell which of its files hold plain paths and which provider
// takes each over, and the version leaves it afterwards. When the version's
// rock_manifest cannot be read, the files its manifest entry names are
// undeployed instead (entryFiles).
func (tx *Tx) deleteVersion(name, version string) error {
	if err := tx.treeFor().deleteVersion(name, version, entryFiles(tx.m.Repository[name][version])); err != nil {
		return err
	}

	removeFromManifest(tx.m, name, version)
	tx.changed()

	return nil
}

// entryFiles lists, in the deployed sections of a rock_manifest, the files
// the tree manifest's entry of a rock names, to stand in for the rock's
// rock_manifest when that cannot be read: each command, and each module at
// the path the entry gives and at the other form of the module beside it —
// the .so of a .lua and the .lua of a .so — since an entry names one file
// per module, the .so of a module shipping both (ModuleIndex). A module file
// of another extension may be in either deploy dir and is listed in both. A
// file of the rock the entry does not name, such as a data file beside its
// modules, is not listed.
func entryFiles(e rocks.RepoEntry) *rocks.RockManifest {
	rm := &rocks.RockManifest{Lua: map[string]string{}, Lib: map[string]string{}, Bin: map[string]string{}}

	for _, rel := range e.Modules {
		switch {
		case strings.HasSuffix(rel, ".lua"):
			rm.Lua[rel], rm.Lib[withSoExt(rel)] = "", ""
		case strings.HasSuffix(rel, ".so"):
			rm.Lib[rel], rm.Lua[withLuaExt(rel)] = "", ""
		default:
			rm.Lua[rel], rm.Lib[rel] = "", ""
		}
	}

	for _, rel := range e.Commands {
		rm.Bin[rel] = ""
	}

	return rm
}

// changed records that the transaction changed its manifest.
func (tx *Tx) changed() {
	tx.dirty = true
	tx.view = nil
}

// readManifest loads the tree manifest, returning a fresh (empty) one with
// initialized maps when none exists yet. A manifest that cannot be read for
// another reason than I/O — one that does not parse — fails it with an error
// wrapping ErrCorrupt.
func (t *Tree) readManifest() (*rocks.Manifest, error) {
	m, err := t.Store.ReadTree(t.RocksDir())
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			if _, ok := errors.AsType[*fs.PathError](err); !ok {
				err = &corruptError{err: err}
			}

			return nil, fmt.Errorf("read tree manifest: %w", err)
		}

		m = &rocks.Manifest{}
	}

	return normalizeManifest(m), nil
}

// corruptError marks err as one of a corrupt tree, keeping its text.
type corruptError struct {
	err error
}

func (e *corruptError) Error() string { return e.err.Error() }

func (e *corruptError) Unwrap() []error { return []error{ErrCorrupt, e.err} }

// normalizeManifest allocates every index of m that is nil and returns m.
func normalizeManifest(m *rocks.Manifest) *rocks.Manifest {
	if m.Repository == nil {
		m.Repository = map[string]map[string]rocks.RepoEntry{}
	}

	if m.Modules == nil {
		m.Modules = map[string][]string{}
	}

	if m.Commands == nil {
		m.Commands = map[string][]string{}
	}

	if m.Dependencies == nil {
		m.Dependencies = map[string]map[string][]rocks.Dep{}
	}

	return m
}

// installedRef finds the installed version of name that version names: the
// very key of the manifest's repository, or else the one deps.Compare finds
// equal to it.
func installedRef(m *rocks.Manifest, name, version string) (RockRef, bool) {
	versions := m.Repository[name]
	if _, ok := versions[version]; ok {
		return RockRef{Name: name, Version: version}, true
	}

	for _, v := range slices.Sorted(maps.Keys(versions)) {
		if sameVersion(v, version) {
			return RockRef{Name: name, Version: v}, true
		}
	}

	return RockRef{}, false
}

// sameVersion reports whether a and b name the same version: equal strings,
// or two that parse and deps.Compare finds equal.
func sameVersion(a, b string) bool {
	if a == b {
		return true
	}

	av, err := deps.ParseVersion(a)
	if err != nil {
		return false
	}

	bv, err := deps.ParseVersion(b)
	if err != nil {
		return false
	}

	return deps.Compare(av, bv) == 0
}

// addToManifest records name/version, deployed with the rock_manifest rm, in
// m. The entry's modules/commands index comes from what Deploy actually wrote
// (rm.Lua, rm.Lib, rm.Bin) — NOT from spec.Build.Modules, which only the
// builtin backend populates: a cmake, make or command rock declares no
// modules. ModuleIndex is the inversion upstream's repos.package_modules
// performs over the rock_manifest, and CommandIndex that of
// repos.package_commands: keyed and valued by the path under the bin dir, not
// by the rockspec's build.install.bin source paths.
//
// Every provider is listed, whether it holds an item's plain path or a
// versioned one, in upstream's order (addProvider), which is the order of the
// spots Deploy gave them.
//
// Upstream update_dependencies records both the rock's own declared
// dependency list (top-level manifest.dependencies) and the resolved
// name→version map of its installed deps (per-entry repo.dependencies). Both
// are always present — {} for a dep-less rock.
func addToManifest(m *rocks.Manifest, spec *rocks.Rockspec, rm *rocks.RockManifest) {
	if m.Repository[spec.Package] == nil {
		m.Repository[spec.Package] = map[string]rocks.RepoEntry{}
	}

	entry := rocks.RepoEntry{Arch: "installed"}
	pkgVer := RockRef{Name: spec.Package, Version: spec.Version}.String()

	if mods := ModuleIndex(rm); len(mods) > 0 {
		entry.Modules = mods
		for modName := range mods {
			m.Modules[modName] = addProvider(m.Modules[modName], pkgVer)
		}
	}

	if cmds := CommandIndex(rm); len(cmds) > 0 {
		entry.Commands = cmds
		for cmd := range cmds {
			m.Commands[cmd] = addProvider(m.Commands[cmd], pkgVer)
		}
	}

	if m.Dependencies[spec.Package] == nil {
		m.Dependencies[spec.Package] = map[string][]rocks.Dep{}
	}

	m.Dependencies[spec.Package][spec.Version] = spec.Dependencies

	entry.Dependencies = resolveInstalledDeps(spec, m)

	m.Repository[spec.Package][spec.Version] = entry
}

// addProvider records provider (a "name/version" token) in a module or command
// provider list and keeps the list in upstream's order (manif/writer.lua
// sort_package_matching_table): by rock name, then newest version first, with
// no duplicates. Index 0 is then the provider holding the item's plain path,
// the one Deploy gives it (repos.lua check_spot_if_available), and index 1
// the one that takes it over when index 0 is removed.
func addProvider(list []string, provider string) []string {
	out := make([]string, 0, len(list)+1)
	for _, x := range list {
		if x != provider {
			out = append(out, x)
		}
	}

	out = append(out, provider)
	slices.SortStableFunc(out, compareProviders)

	return out
}

// compareProviders orders provider tokens as upstream sort_pkgs does
// (manif/writer.lua:116-127): rocks by name, and versions of one rock newest
// first. Versions that do not parse compare as strings.
func compareProviders(a, b string) int {
	na, va, _ := strings.Cut(a, "/")
	nb, vb, _ := strings.Cut(b, "/")

	if na != nb {
		return strings.Compare(na, nb)
	}

	pa, errA := deps.ParseVersion(va)
	pb, errB := deps.ParseVersion(vb)

	if errA != nil || errB != nil {
		return strings.Compare(vb, va)
	}

	return deps.Compare(pb, pa)
}

// resolveInstalledDeps builds the per-entry repo.dependencies map (name →
// installed version) for a rock, mirroring upstream scan_deps' observable
// output: each of the rock's declared dependencies is resolved to the newest
// version present in the tree manifest's repository that satisfies its
// constraints, in deps version order, as upstream match_dep picks it.
// Dependencies with no such version (not yet installed, or installed at
// versions the constraints exclude) are omitted; the self-entry never
// appears. Always returns a non-nil map so the manifest carries
// `dependencies = {}` for a dep-less rock.
//
// It is computed once, when the rock is deployed, and the entries of the
// other rocks are left as they are, where upstream recomputes every entry,
// transitively, on each write of the manifest.
func resolveInstalledDeps(spec *rocks.Rockspec, m *rocks.Manifest) map[string]string {
	out := map[string]string{}

	for _, d := range spec.Dependencies {
		if d.Name == spec.Package {
			continue
		}

		var (
			best rocks.Version
			have bool
		)

		for ver := range m.Repository[d.Name] {
			v, err := deps.ParseVersion(ver)
			if err != nil || !deps.Match(v, d.Constraints) {
				continue
			}

			if !have || deps.Compare(v, best) > 0 {
				best, have = v, true
			}
		}

		if have {
			out[d.Name] = best.Raw
		}
	}

	return out
}

// removeProvider drops the "name/version" provider from every list in a
// module/command index, deleting a list that becomes empty. The first surviving
// provider (if any) remains the active one at index 0.
func removeProvider(index map[string][]string, provider string) {
	for item, list := range index {
		out := list[:0]

		for _, x := range list {
			if x != provider {
				out = append(out, x)
			}
		}

		if len(out) == 0 {
			delete(index, item)
		} else {
			index[item] = out
		}
	}
}

// manifestProviders builds the Tree.Providers lookup over the manifest's
// module and command indexes, as upstream manif.get_providers reads them: a
// module is looked up in m.Modules and a command in m.Commands, never one in
// the other's index, since a rock may well ship a module and a command of the
// same name. The lookup reads m as it is when called, so it follows the
// manifest's updates.
func manifestProviders(m *rocks.Manifest) func(kind ItemKind, item string) []string {
	return func(kind ItemKind, item string) []string {
		if kind == CommandItem {
			return m.Commands[item]
		}

		return m.Modules[item]
	}
}

// removeFromManifest drops name/version from the tree manifest: its
// repository entry, its place in the module and command provider lists, and
// its dependency list (upstream writer.remove_from_manifest).
func removeFromManifest(m *rocks.Manifest, name, version string) {
	pkgVer := RockRef{Name: name, Version: version}.String()

	delete(m.Repository[name], version)
	removeProvider(m.Modules, pkgVer)
	removeProvider(m.Commands, pkgVer)

	if vers, ok := m.Dependencies[name]; ok {
		delete(vers, version)
	}

	if len(m.Repository[name]) == 0 {
		delete(m.Repository, name)
		delete(m.Dependencies, name)
	}
}
