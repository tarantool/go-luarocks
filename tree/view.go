package tree

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/internal/treelock"
	"github.com/tarantool/go-luarocks/manif"
)

// ErrCorrupt is wrapped by the error of reading a tree whose manifest, or a
// rock's rock_manifest, does not parse, is missing where the tree manifest
// says a rock is, or names a path outside its directory.
var ErrCorrupt = errors.New("rock tree is corrupt")

// ErrIntegrity is wrapped by the error of View.Verify when a file of the rock
// is missing from its spot or differs from what its rock_manifest records.
var ErrIntegrity = errors.New("rock files do not match their rock_manifest")

// ErrNotPlain is wrapped by the error of View.CheckPlain when a deployed file
// of the rock is not at its plain path, another rock holding it, and by the
// error of a change refused because a rock's file would not be at its plain
// path after it: the rock's own, with StagedOpts.RequirePlain and
// GraftOpts.RequirePlain, or another rock's, with StagedOpts.NoDisplace and
// GraftOpts.NoDisplace.
var ErrNotPlain = errors.New("rock files are not at their plain paths")

// View is a read-only snapshot of a tree: its manifest, and the rock_manifest
// of each rock it lists, as they stood when the View was taken. Its methods
// read those, and the files on disk where they say so; none of them changes
// the tree. A View is safe for concurrent use.
type View struct {
	paths Paths
	store rocks.ManifestStore
	m     *rocks.Manifest

	mu  sync.Mutex
	rms map[RockRef]rockManifest

	ownersOnce sync.Once
	owners     map[string]owned
}

// rockManifest is the outcome of reading one rock's rock_manifest.
type rockManifest struct {
	rm  *rocks.RockManifest
	err error
}

// owned is the owner of a deployed file.
type owned struct {
	ref  RockRef
	kind ItemKind
}

// Inspect returns a View of the tree rooted at root, as rocks.Config.Tree
// names it, without changing anything there: a tree that does not exist, or
// has no tree manifest, is an empty one, and nothing is created, not even the
// tree's lock file. While it reads the manifests it holds the tree's lock,
// shared, when the tree has a lock file, so it reads a tree no Update is
// changing; it waits for one in progress to end, and fails with an error
// wrapping ErrLocked and the cause of ctx's end when ctx ends first.
//
// A tree manifest that does not parse fails it with an error wrapping
// ErrCorrupt. A rock_manifest that cannot be read does not: Files, Verify
// and CheckPlain of that rock fail instead.
func Inspect(ctx context.Context, root string) (*View, error) {
	return inspect(ctx, &Tree{Paths: Paths{Tree: root}, Store: manif.FileStore{}}, false)
}

// View returns a View of t, as Inspect does, but takes the tree's lock even
// when the tree has no lock file yet, creating one when it can.
func (t *Tree) View(ctx context.Context) (*View, error) {
	return inspect(ctx, t, true)
}

// inspect loads the View of t under the shared lock.
func inspect(ctx context.Context, t *Tree, create bool) (*View, error) {
	if _, err := os.Stat(t.RocksDir()); errors.Is(err, fs.ErrNotExist) {
		return newView(t, normalizeManifest(&rocks.Manifest{})), nil
	}

	lock, err := treelock.Shared(ctx, t.RocksDir(), create)
	if err != nil {
		return nil, fmt.Errorf("tree.View: %w", lockError(err))
	}

	defer lock.Unlock()

	m, err := t.readManifest()
	if err != nil {
		return nil, err
	}

	v := newView(t, m)
	for _, ref := range v.Rocks() {
		_, _ = v.rockManifest(ref)
	}

	return v, nil
}

// newView returns a View of m, the manifest of t, that reads the
// rock_manifests through t's store on demand.
func newView(t *Tree, m *rocks.Manifest) *View {
	return &View{paths: t.Paths, store: t.Store, m: m, rms: map[RockRef]rockManifest{}}
}

// View returns a snapshot of the tree as the transaction has it now, its own
// changes included. Later changes do not reach the returned View.
func (tx *Tx) View() *View {
	if tx.view == nil {
		tx.view = newView(tx.tree, cloneManifest(tx.m))
	}

	return tx.view
}

// Rocks returns every installed rock version, by name, and of each rock the
// newest version first.
func (v *View) Rocks() []RockRef {
	var out []RockRef

	for name, versions := range v.m.Repository {
		for ver := range versions {
			out = append(out, RockRef{Name: name, Version: ver})
		}
	}

	slices.SortFunc(out, compareRefs)

	return out
}

// compareRefs orders rock versions as the provider lists do: by name, then
// newest version first.
func compareRefs(a, b RockRef) int {
	return compareProviders(a.String(), b.String())
}

// Versions returns the installed versions of the rock name, newest first; nil
// when none is installed.
func (v *View) Versions(name string) []string {
	versions := v.m.Repository[name]
	if len(versions) == 0 {
		return nil
	}

	out := slices.Collect(maps.Keys(versions))
	slices.SortFunc(out, func(a, b string) int {
		return compareRefs(RockRef{Name: name, Version: a}, RockRef{Name: name, Version: b})
	})

	return out
}

// Installed reports whether version of the rock name is installed — the tree
// manifest lists a version deps.Compare finds equal to it, and its install
// dir exists — and returns it as the manifest spells it.
func (v *View) Installed(name, version string) (RockRef, bool) {
	ref, ok := installedRef(v.m, name, version)
	if !ok || checkNameVersion(ref.Name, ref.Version) != nil {
		return RockRef{}, false
	}

	st, err := safepath.Lstat(v.paths.RocksDir(), v.paths.InstallDir(ref.Name, ref.Version))
	if err != nil || !st.IsDir() {
		return RockRef{}, false
	}

	return ref, true
}

// Providers returns the installed rocks that provide item — a module or a
// command, by kind — in the tree manifest's order: the first holds the plain
// path of the item's files, the others versioned ones.
func (v *View) Providers(kind ItemKind, item string) []RockRef {
	list := manifestProviders(v.m)(kind, item)

	out := make([]RockRef, 0, len(list))
	for _, token := range list {
		name, version, _ := splitProvider(token)
		out = append(out, RockRef{Name: name, Version: version})
	}

	return out
}

// Section names the part of a rock's rock_manifest a file is listed in, and
// so where the file sits.
type Section string

// The sections of a rock_manifest.
const (
	// SectionRockspec is the rock's rockspec, in its install dir.
	SectionRockspec Section = "rockspec"
	// SectionLua is a Lua module, deployed under DeployLuaDir.
	SectionLua Section = deployLua
	// SectionLib is a compiled module, deployed under DeployLibDir.
	SectionLib Section = deployLib
	// SectionBin is a command, deployed under BinDir, whose real script
	// stays in the install dir's bin/.
	SectionBin Section = deployBin
	// SectionConf is a configuration file, in the install dir's conf/.
	SectionConf Section = "conf"
	// SectionDoc is a documentation file, in the install dir's doc/.
	SectionDoc Section = "doc"
	// SectionOther is any other file of the install dir.
	SectionOther Section = "other"
)

// File is one file of an installed rock: an entry of its rock_manifest and
// where the file is now.
type File struct {
	// Section is the part of the rock_manifest that lists the file.
	Section Section
	// Entry is the file's rock_manifest key, slash-separated: its path under
	// the section's directory, or for SectionOther under the install dir,
	// and for SectionRockspec the rockspec's file name.
	Entry string
	// MD5 is the digest the rock_manifest records for the file.
	MD5 string
	// Path is where the file is: for a deployed one (lua, lib, bin) its plain
	// path when the rock holds it, the versioned one (MungedPath) when
	// another rock does, as the tree manifest's provider lists tell; for the
	// rest, its path under the install dir.
	Path string
	// Plain says, for a deployed file, that Path is its plain path.
	Plain bool
	// Script is, for a command, the real script in the install dir's bin/,
	// which Path, a launcher or a copy, runs.
	Script string
}

// Files returns every file the rock_manifest of the installed rock r lists,
// and where each is now: the rockspec, then the lua, lib, bin, conf, doc and
// other sections, each in entry order. A rock the tree manifest does not list
// fails it with an error wrapping ErrNotInstalled; one whose rock_manifest
// cannot be read, or names a path outside its directory, with one wrapping
// ErrCorrupt.
func (v *View) Files(r RockRef) ([]File, error) {
	ref, ok := installedRef(v.m, r.Name, r.Version)
	if !ok {
		return nil, fmt.Errorf("tree.View.Files: %w: %s", ErrNotInstalled, r)
	}

	rm, err := v.rockManifest(ref)
	if err != nil {
		return nil, fmt.Errorf("tree.View.Files: %w", err)
	}

	files, err := v.files(ref, rm)
	if err != nil {
		return nil, fmt.Errorf("tree.View.Files: %w: rock_manifest of %s: %w", ErrCorrupt, ref, err)
	}

	return files, nil
}

// files lists the files of rm, the rock_manifest of ref.
func (v *View) files(ref RockRef, rm *rocks.RockManifest) ([]File, error) {
	t := &Tree{Paths: v.paths, Providers: manifestProviders(v.m)}
	installDir := v.paths.InstallDir(ref.Name, ref.Version)

	var out []File

	if rm.Rockspec != "" {
		entry := rm.RockspecFile
		if entry == "" {
			entry = ref.Name + "-" + ref.Version + ".rockspec"
		}

		p, err := safepath.JoinEntry(installDir, entry)
		if err != nil {
			return nil, err
		}

		out = append(out, File{Section: SectionRockspec, Entry: entry, MD5: rm.Rockspec, Path: p})
	}

	for _, sec := range []struct {
		section Section
		files   map[string]string
	}{{SectionLua, rm.Lua}, {SectionLib, rm.Lib}, {SectionBin, rm.Bin}} {
		dt := string(sec.section)
		dir := t.deployDirOf(dt)

		for _, rel := range sortedKeys(sec.files) {
			plainPath, err := safepath.JoinEntry(dir, filepath.FromSlash(rel))
			if err != nil {
				return nil, err
			}

			f := File{Section: sec.section, Entry: rel, MD5: sec.files[rel], Path: plainPath}

			f.Plain, _, _ = t.takesPlainPath(dt, rel, ref.Name, ref.Version)
			if !f.Plain {
				f.Path = MungedPath(dir, plainPath, ref.Name, ref.Version)
			}

			if sec.section == SectionBin {
				if f.Script, err = safepath.JoinEntry(filepath.Join(installDir, "bin"), filepath.FromSlash(rel)); err != nil {
					return nil, err
				}
			}

			out = append(out, f)
		}
	}

	for _, sec := range []struct {
		section Section
		dir     string
		files   map[string]string
	}{
		{SectionConf, filepath.Join(installDir, "conf"), rm.Conf},
		{SectionDoc, filepath.Join(installDir, "doc"), rm.Doc},
		{SectionOther, installDir, rm.Other},
	} {
		for _, rel := range sortedKeys(sec.files) {
			p, err := safepath.JoinEntry(sec.dir, filepath.FromSlash(rel))
			if err != nil {
				return nil, err
			}

			out = append(out, File{Section: sec.section, Entry: rel, MD5: sec.files[rel], Path: p})
		}
	}

	return out, nil
}

// rockManifest returns the rock_manifest of the installed rock ref, reading it
// on first use.
func (v *View) rockManifest(ref RockRef) (*rocks.RockManifest, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if got, ok := v.rms[ref]; ok {
		return got.rm, got.err
	}

	var got rockManifest

	if err := checkNameVersion(ref.Name, ref.Version); err != nil {
		got.err = fmt.Errorf("%w: %s: %w", ErrCorrupt, ref, err)
	} else if got.rm, err = v.store.ReadRock(filepath.Join(v.paths.InstallDir(ref.Name, ref.Version), "rock_manifest")); err != nil {
		got.err = fmt.Errorf("%w: rock_manifest of %s: %w", ErrCorrupt, ref, err)
	}

	v.rms[ref] = got

	return got.rm, got.err
}

// Owner returns the installed rock a deployed file belongs to — a module file
// under DeployLuaDir or DeployLibDir, or a command under BinDir, at its plain
// path or a versioned one — and the kind of item the file provides. rel is
// the file's path, absolute or relative to the tree's root. A file no rock's
// rock_manifest lists there, or a path outside the tree, has no owner.
func (v *View) Owner(rel string) (RockRef, ItemKind, bool) {
	key := filepath.Clean(filepath.FromSlash(rel))

	if filepath.IsAbs(key) {
		root, err := filepath.Abs(v.paths.Tree)
		if err != nil {
			return RockRef{}, 0, false
		}

		if key, err = filepath.Rel(root, key); err != nil {
			return RockRef{}, 0, false
		}
	}

	if !filepath.IsLocal(key) {
		return RockRef{}, 0, false
	}

	o, ok := v.ownerIndex()[filepath.ToSlash(key)]

	return o.ref, o.kind, ok
}

// ownerIndex maps the tree-relative, slash-separated path of every deployed
// file of every rock to its owner. A rock whose files cannot be listed owns
// none.
func (v *View) ownerIndex() map[string]owned {
	v.ownersOnce.Do(func() {
		v.owners = map[string]owned{}

		for _, ref := range v.Rocks() {
			files, err := v.Files(ref)
			if err != nil {
				continue
			}

			for _, f := range files {
				kind := ModuleItem

				switch f.Section {
				case SectionLua, SectionLib:
				case SectionBin:
					kind = CommandItem
				case SectionRockspec, SectionConf, SectionDoc, SectionOther:
					continue
				}

				if rel, err := filepath.Rel(v.paths.Tree, f.Path); err == nil {
					v.owners[filepath.ToSlash(rel)] = owned{ref: ref, kind: kind}
				}
			}
		}
	})

	return v.owners
}

// Command returns the command name as its first provider has it — the rock
// holding the plain path <tree>/bin/<name> — as the real script in that rock's
// install dir and the entry in BinDir, which runs it (a launcher, or a copy).
// It reports false when no rock provides the command, or the provider's
// script is not a regular file.
func (v *View) Command(name string) (script, entry string, ok bool) {
	providers := v.Providers(CommandItem, name)
	if len(providers) == 0 || checkNameVersion(providers[0].Name, providers[0].Version) != nil {
		return "", "", false
	}

	ref := providers[0]

	entry, err := safepath.JoinEntry(v.paths.BinDir(), filepath.FromSlash(name))
	if err != nil {
		return "", "", false
	}

	script, err = safepath.JoinEntry(filepath.Join(v.paths.InstallDir(ref.Name, ref.Version), "bin"), filepath.FromSlash(name))
	if err != nil {
		return "", "", false
	}

	if st, err := safepath.Lstat(v.paths.RocksDir(), script); err != nil || !st.Mode().IsRegular() {
		return "", "", false
	}

	return script, entry, true
}

// Dependencies returns the dependencies the installed rock r declares, as the
// tree manifest records them; nil for a rock not installed.
func (v *View) Dependencies(r RockRef) []rocks.Dep {
	ref, ok := installedRef(v.m, r.Name, r.Version)
	if !ok {
		return nil
	}

	return slices.Clone(v.m.Dependencies[ref.Name][ref.Version])
}

// Requires returns the installed rock r's dependencies resolved to installed
// versions, as its tree manifest entry records them (name → version). The
// native backend records a rock's direct dependencies when it deploys the
// rock, each at the newest installed version its constraints accept, and
// leaves the entry so; upstream recomputes every entry, transitively, on each
// write of the manifest. Nil for a rock not installed.
func (v *View) Requires(r RockRef) map[string]string {
	ref, ok := installedRef(v.m, r.Name, r.Version)
	if !ok {
		return nil
	}

	return maps.Clone(v.m.Repository[ref.Name][ref.Version].Dependencies)
}

// Dependents returns the installed rocks, other versions of r's own rock
// aside, that declare a dependency r satisfies — by name and constraints —
// in Rocks order. It is informational: another installed version may satisfy
// the same dependency, and nothing here, Remove included, refuses to break
// one.
func (v *View) Dependents(r RockRef) []RockRef {
	ref, ok := installedRef(v.m, r.Name, r.Version)
	if !ok {
		return nil
	}

	have, err := deps.ParseVersion(ref.Version)
	if err != nil {
		return nil
	}

	var out []RockRef

	for _, other := range v.Rocks() {
		if other.Name == ref.Name {
			continue
		}

		for _, d := range v.m.Dependencies[other.Name][other.Version] {
			if d.Name == ref.Name && deps.Match(have, d.Constraints) {
				out = append(out, other)

				break
			}
		}
	}

	return out
}

// ProblemKind says what Verify found wrong with a file.
type ProblemKind int

const (
	// ProblemMissing is a file that is not at its spot.
	ProblemMissing ProblemKind = iota + 1
	// ProblemNotRegular is a spot that holds something other than a regular
	// file: a directory, or a symbolic link.
	ProblemNotRegular
	// ProblemModified is a file whose digest differs from the one its
	// rock_manifest records.
	ProblemModified
)

// String names the kind.
func (k ProblemKind) String() string {
	switch k {
	case ProblemMissing:
		return "missing"
	case ProblemNotRegular:
		return "not a regular file"
	case ProblemModified:
		return "modified"
	default:
		return fmt.Sprintf("ProblemKind(%d)", int(k))
	}
}

// Problem is a file of a rock that is not as its rock_manifest says.
type Problem struct {
	// File is the file, as Files lists it.
	File File
	// Path is the path found wrong: File.Path, or File.Script for the real
	// script of a command.
	Path string
	// Kind is what is wrong.
	Kind ProblemKind
	// MD5 is, for ProblemModified, the digest of the file found.
	MD5 string
}

// String describes the problem.
func (p Problem) String() string {
	if p.Kind == ProblemModified {
		return fmt.Sprintf("%s: %s (md5 %s, want %s)", p.Path, p.Kind, p.MD5, p.File.MD5)
	}

	return fmt.Sprintf("%s: %s", p.Path, p.Kind)
}

// Verify checks every file of the installed rock r against its
// rock_manifest: that each is a regular file at its spot (Files) and has the
// digest recorded, and for a command, that the entry in BinDir exists and the
// real script has the digest. It returns the problems found, in Files order,
// with an error wrapping ErrIntegrity when there is any; a rock it cannot
// list the files of fails it as Files does. Nothing is followed out of the
// tree: a symbolic link at a spot is a problem.
func (v *View) Verify(r RockRef) ([]Problem, error) {
	files, err := v.Files(r)
	if err != nil {
		return nil, err
	}

	var problems []Problem

	for _, f := range files {
		check := f.Path
		if f.Section == SectionBin {
			if p, ok := v.checkFile(f, f.Path, ""); !ok {
				problems = append(problems, p)
			}

			check = f.Script
		}

		if p, ok := v.checkFile(f, check, f.MD5); !ok {
			problems = append(problems, p)
		}
	}

	if len(problems) == 0 {
		return nil, nil
	}

	descs := make([]string, 0, len(problems))
	for _, p := range problems {
		descs = append(descs, p.String())
	}

	return problems, fmt.Errorf("tree.View.Verify: %w: %s: %s", ErrIntegrity, r, strings.Join(descs, "; "))
}

// checkFile checks that the file f has at path, inside the tree, is a regular
// file with the digest want, when want is not empty.
func (v *View) checkFile(f File, path, want string) (Problem, bool) {
	problem := Problem{File: f, Path: path}

	st, err := safepath.Lstat(v.paths.Tree, path)

	switch {
	case err != nil:
		problem.Kind = ProblemMissing

		return problem, false
	case !st.Mode().IsRegular():
		problem.Kind = ProblemNotRegular

		return problem, false
	case want == "":
		return Problem{}, true
	}

	got, err := digestOf(v.paths.Tree, path)
	if err != nil {
		problem.Kind = ProblemMissing

		return problem, false
	}

	if got != want {
		problem.Kind, problem.MD5 = ProblemModified, got

		return problem, false
	}

	return Problem{}, true
}

// digestOf returns the md5 hex of the file path inside root.
func digestOf(root, path string) (string, error) {
	f, err := safepath.Open(root, path)
	if err != nil {
		return "", err
	}

	defer func() { _ = f.Close() }()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// CheckPlain checks that every deployed file of the installed rock r — lua,
// lib and bin — is at its plain path, so that `require` and the tree's bin
// reach r's own files, as upstream check_everything_is_installed does without
// accepting versioned names. A file another rock holds the plain path of
// fails it with an error wrapping ErrNotPlain that names them and their
// holders; a rock it cannot list the files of fails it as Files does. It reads
// the tree manifest only: Verify checks the files on disk.
func (v *View) CheckPlain(r RockRef) error {
	files, err := v.Files(r)
	if err != nil {
		return err
	}

	var notPlain []string

	for _, f := range files {
		if f.Plain || (f.Section != SectionLua && f.Section != SectionLib && f.Section != SectionBin) {
			continue
		}

		desc := string(f.Section) + "/" + f.Entry
		if plain, err := safepath.JoinEntry(v.deployDir(f.Section), filepath.FromSlash(f.Entry)); err == nil {
			if owner, _, ok := v.Owner(plain); ok {
				desc += " (held by " + owner.String() + ")"
			}
		}

		notPlain = append(notPlain, desc)
	}

	if len(notPlain) > 0 {
		return fmt.Errorf("tree.View.CheckPlain: %w: %s: %s", ErrNotPlain, r, strings.Join(notPlain, ", "))
	}

	return nil
}

// deployDir returns the deploy dir of a deployed section.
func (v *View) deployDir(s Section) string {
	return (&Tree{Paths: v.paths}).deployDirOf(string(s))
}

// cloneManifest returns a deep copy of m.
func cloneManifest(m *rocks.Manifest) *rocks.Manifest {
	out := &rocks.Manifest{
		Repository:   make(map[string]map[string]rocks.RepoEntry, len(m.Repository)),
		Modules:      make(map[string][]string, len(m.Modules)),
		Commands:     make(map[string][]string, len(m.Commands)),
		Dependencies: make(map[string]map[string][]rocks.Dep, len(m.Dependencies)),
	}

	for name, versions := range m.Repository {
		vs := make(map[string]rocks.RepoEntry, len(versions))
		for ver, e := range versions {
			e.Modules = maps.Clone(e.Modules)
			e.Commands = maps.Clone(e.Commands)
			e.Dependencies = maps.Clone(e.Dependencies)
			vs[ver] = e
		}

		out.Repository[name] = vs
	}

	for item, list := range m.Modules {
		out.Modules[item] = slices.Clone(list)
	}

	for item, list := range m.Commands {
		out.Commands[item] = slices.Clone(list)
	}

	for name, versions := range m.Dependencies {
		vs := make(map[string][]rocks.Dep, len(versions))
		for ver, ds := range versions {
			vs[ver] = slices.Clone(ds)
		}

		out.Dependencies[name] = vs
	}

	return out
}
