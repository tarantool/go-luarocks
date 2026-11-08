package tree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/internal/treelock"
)

// CheckOpts tunes Tree.Check.
type CheckOpts struct {
	// BinWrap is what the tree's command wrappers should have been generated
	// with: a wrapper other than the one Deploy writes with it is an
	// IssueStaleWrapper. Nil checks no wrapper's contents, only that it is
	// there.
	BinWrap *BinWrap
}

// IssueKind says what Check found wrong.
type IssueKind int

const (
	// IssueMissingFile is a file a rock's rock_manifest lists that is at
	// neither of its spots, or is not a regular file.
	IssueMissingFile IssueKind = iota + 1
	// IssueDigestMismatch is a file at its spot whose digest differs from the
	// one its rock_manifest records, or a command copied verbatim that is not
	// its script.
	IssueDigestMismatch
	// IssueWrongSpot is a deployed file at its plain path where the provider
	// lists give it a versioned one, or at its versioned path where they give
	// it the plain one: the plain path is not held by the item's first
	// provider.
	IssueWrongSpot
	// IssueOrphanRock is a rock dir, <rocks-dir>/<name>/<version>, the tree
	// manifest does not list: what an interrupted install, or a removal by
	// hand, leaves.
	IssueOrphanRock
	// IssueStaleEntry is a rock the tree manifest lists whose install dir or
	// rock_manifest is missing or cannot be read, whose modules or commands
	// are not the ones of its rock_manifest, or which is missing from the
	// provider list of one of them.
	IssueStaleEntry
	// IssueUntracked is a file in a deploy dir no rock owns: laid there by
	// hand or by another tool, or left by a rock the manifest does not list.
	IssueUntracked
	// IssueBackup is a file in a deploy dir no rock owns whose name ends in
	// "~": the backup Deploy makes of a file in a new rock's way.
	IssueBackup
	// IssueStaleWrapper is a command wrapper other than the one Deploy would
	// write with CheckOpts.BinWrap, such as one naming another root or
	// interpreter, which Tx.Rewrap rewrites.
	IssueStaleWrapper
	// IssueManifestUnreadable is a tree manifest that does not parse. Check
	// looks no further; Rebuild writes a new one.
	IssueManifestUnreadable
)

// String names the kind.
func (k IssueKind) String() string {
	switch k {
	case IssueMissingFile:
		return "missing file"
	case IssueDigestMismatch:
		return "digest mismatch"
	case IssueWrongSpot:
		return "wrong spot"
	case IssueOrphanRock:
		return "orphan rock"
	case IssueStaleEntry:
		return "stale entry"
	case IssueUntracked:
		return "untracked"
	case IssueBackup:
		return "backup"
	case IssueStaleWrapper:
		return "stale wrapper"
	case IssueManifestUnreadable:
		return "manifest unreadable"
	default:
		return fmt.Sprintf("IssueKind(%d)", int(k))
	}
}

// Issue is one thing Check found wrong.
type Issue struct {
	// Kind is what is wrong.
	Kind IssueKind
	// Rock is the rock version the issue is of; zero for an untracked file,
	// a backup and an unreadable manifest.
	Rock RockRef
	// Path is the file or directory found wrong, joined onto the tree's root.
	Path string
	// Detail says more, for a person to read.
	Detail string
}

// String describes the issue.
func (i Issue) String() string {
	var b strings.Builder

	b.WriteString(i.Kind.String())

	if i.Rock != (RockRef{}) {
		b.WriteString(" " + i.Rock.String())
	}

	b.WriteString(": " + i.Path)

	if i.Detail != "" {
		b.WriteString(" (" + i.Detail + ")")
	}

	return b.String()
}

// Report is what Check found.
type Report struct {
	// Issues are the problems found: the rocks' in View.Rocks order, then the
	// orphan rocks, then the untracked files and backups by path.
	Issues []Issue
}

// OK reports whether Check found nothing wrong.
func (r *Report) OK() bool {
	return len(r.Issues) == 0
}

// Check checks the tree without changing anything — an fsck. For every rock
// the tree manifest lists it checks that its install dir and rock_manifest
// are there and agree with the manifest entry (IssueStaleEntry), and that
// each file of its rock_manifest is at the spot the provider lists give it
// (IssueWrongSpot when it is at its other one, IssueMissingFile when at
// neither) with the digest recorded (IssueDigestMismatch); a command's real
// script and its entry in BinDir, which is a wrapper where Deploy writes one,
// compared with the one Deploy would write with opts.BinWrap
// (IssueStaleWrapper), and a verbatim copy of the script elsewhere. Then it
// lists the rock dirs the manifest does not list (IssueOrphanRock) and the
// files under share/, lib/ and bin/ no rock owns (IssueUntracked, or
// IssueBackup for a name ending in "~"), the rocks dir aside; the other
// directories at the tree's root are a tool's own, as tt keeps its metadata
// in one, and are not looked at. A tree manifest that does not parse is
// IssueManifestUnreadable alone.
//
// The problems are the report; the error is for a failure to read the tree,
// or to take its lock, shared, which Check waits for until ctx ends
// (ErrLocked). Nothing is followed out of the tree: a symlink at a spot is a
// file missing there. A tree that does not exist is an empty one.
func (t *Tree) Check(ctx context.Context, opts CheckOpts) (*Report, error) {
	lock, err := treelock.Shared(ctx, t.RocksDir(), false)
	if err != nil {
		return nil, fmt.Errorf("tree.Check: %w", lockError(err))
	}

	defer lock.Unlock()

	m, err := t.readManifest()
	if errors.Is(err, ErrCorrupt) {
		issue := Issue{Kind: IssueManifestUnreadable, Path: filepath.Join(t.RocksDir(), "manifest"), Detail: err.Error()}

		return &Report{Issues: []Issue{issue}}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("tree.Check: %w", err)
	}

	c := &checker{t: t, m: m, v: newView(t, m), wrap: opts.BinWrap, owned: map[string]bool{}}

	for _, ref := range c.v.Rocks() {
		c.checkRock(ref)
	}

	if err := c.checkOrphans(); err != nil {
		return nil, fmt.Errorf("tree.Check: %w", err)
	}

	if err := c.checkUntracked(); err != nil {
		return nil, fmt.Errorf("tree.Check: %w", err)
	}

	return &Report{Issues: c.issues}, nil
}

// checker is the state of one Check.
type checker struct {
	t    *Tree
	m    *rocks.Manifest
	v    *View
	wrap *BinWrap

	// owned holds the tree-relative paths of the deployed files the rocks
	// own, where they were found.
	owned  map[string]bool
	issues []Issue
}

// add records an issue.
func (c *checker) add(kind IssueKind, ref RockRef, path, detail string) {
	c.issues = append(c.issues, Issue{Kind: kind, Rock: ref, Path: path, Detail: detail})
}

// checkRock checks the installed rock ref.
func (c *checker) checkRock(ref RockRef) {
	installDir := c.t.InstallDir(ref.Name, ref.Version)

	if err := checkNameVersion(ref.Name, ref.Version); err != nil {
		c.add(IssueStaleEntry, ref, c.t.RocksDir(), err.Error())

		return
	}

	if st, err := safepath.Lstat(c.t.RocksDir(), installDir); err != nil || !st.IsDir() {
		c.add(IssueStaleEntry, ref, installDir, "no install dir")

		return
	}

	rm, err := c.v.rockManifest(ref)
	if err != nil {
		c.add(IssueStaleEntry, ref, filepath.Join(installDir, rockManifestName), err.Error())

		return
	}

	files, err := c.v.files(ref, rm)
	if err != nil {
		c.add(IssueStaleEntry, ref, filepath.Join(installDir, rockManifestName), err.Error())

		return
	}

	c.checkEntry(ref, rm, installDir)

	wraps := true
	if spec, err := c.t.installedRockspec(ref, files); err == nil {
		wraps = wrapsCommands(spec)
	}

	for _, f := range files {
		switch f.Section {
		case SectionLua, SectionLib:
			c.checkDeployed(ref, f)
		case SectionBin:
			c.checkCommand(ref, f, wraps)
		case SectionRockspec, SectionConf, SectionDoc, SectionOther:
			c.checkInstalled(ref, f.Path, f.MD5)
		}
	}
}

// checkEntry checks that the manifest entry of ref lists the modules and
// commands of its rock_manifest rm, and that the provider list of each
// lists ref.
func (c *checker) checkEntry(ref RockRef, rm *rocks.RockManifest, installDir string) {
	entry := c.m.Repository[ref.Name][ref.Version]
	where := filepath.Join(c.t.RocksDir(), "manifest")

	// A module of both forms may be indexed by either file: the native
	// backend takes the .so, upstream the .lua.
	files := map[string]bool{}
	for rel := range rm.Lua {
		files[rel] = true
	}

	for rel := range rm.Lib {
		files[rel] = true
	}

	mods := ModuleIndex(rm)
	if !slices.Equal(slices.Sorted(maps.Keys(entry.Modules)), slices.Sorted(maps.Keys(mods))) {
		c.add(IssueStaleEntry, ref, where, "its modules are not the ones of "+filepath.Join(installDir, rockManifestName))
	} else {
		for mod, rel := range entry.Modules {
			if !files[rel] || pathToModule(rel) != mod {
				c.add(IssueStaleEntry, ref, where, fmt.Sprintf("module %s is indexed by %s, which its rock_manifest does not list for it", mod, rel))
			}
		}
	}

	if !slices.Equal(slices.Sorted(maps.Keys(entry.Commands)), slices.Sorted(maps.Keys(CommandIndex(rm)))) {
		c.add(IssueStaleEntry, ref, where, "its commands are not the ones of "+filepath.Join(installDir, rockManifestName))
	}

	for _, idx := range []struct {
		kind  string
		items map[string]string
		lists map[string][]string
	}{{"module", entry.Modules, c.m.Modules}, {"command", entry.Commands, c.m.Commands}} {
		for _, item := range slices.Sorted(maps.Keys(idx.items)) {
			if !slices.Contains(idx.lists[item], ref.String()) {
				c.add(IssueStaleEntry, ref, where, fmt.Sprintf("not among the providers of %s %s", idx.kind, item))
			}
		}
	}
}

// fileState is what a spot holds, against a digest.
type fileState int

const (
	stateMissing fileState = iota
	stateNotRegular
	stateModified
	stateOK
)

// stateOf returns what path, inside the tree, holds against the digest want;
// an empty want takes any regular file.
func (c *checker) stateOf(path, want string) fileState {
	st, err := safepath.Lstat(c.t.Tree, path)

	switch {
	case err != nil:
		return stateMissing
	case !st.Mode().IsRegular():
		return stateNotRegular
	case want == "":
		return stateOK
	}

	got, err := digestOf(c.t.Tree, path)
	if err != nil {
		return stateMissing
	}

	if got != want {
		return stateModified
	}

	return stateOK
}

// own records that a rock owns the deployed file at path.
func (c *checker) own(path string) {
	c.owned[treeRel(c.t.Paths, path)] = true
}

// checkInstalled checks a file of ref's install dir.
func (c *checker) checkInstalled(ref RockRef, path, want string) {
	switch c.stateOf(path, want) {
	case stateMissing:
		c.add(IssueMissingFile, ref, path, "")
	case stateNotRegular:
		c.add(IssueMissingFile, ref, path, "not a regular file")
	case stateModified:
		c.add(IssueDigestMismatch, ref, path, "")
	case stateOK:
	}
}

// otherSpot returns the spot of the deployed file f other than f.Path: the
// versioned one of a file at its plain path, and the reverse.
func (c *checker) otherSpot(ref RockRef, f File) string {
	dir := c.t.deployDirOf(string(f.Section))

	plain, err := safepath.JoinEntry(dir, filepath.FromSlash(f.Entry))
	if err != nil {
		return f.Path
	}

	if f.Plain {
		return MungedPath(dir, plain, ref.Name, ref.Version)
	}

	return plain
}

// checkDeployed checks a lua or lib file of ref.
func (c *checker) checkDeployed(ref RockRef, f File) {
	state := c.stateOf(f.Path, f.MD5)
	if state == stateOK {
		c.own(f.Path)

		return
	}

	if other := c.otherSpot(ref, f); other != f.Path && c.stateOf(other, f.MD5) == stateOK {
		c.own(other)
		c.add(IssueWrongSpot, ref, other, "the provider lists put it at "+f.Path)

		return
	}

	switch state {
	case stateMissing:
		c.add(IssueMissingFile, ref, f.Path, "")
	case stateNotRegular:
		c.add(IssueMissingFile, ref, f.Path, "not a regular file")
	case stateModified:
		c.own(f.Path)
		c.add(IssueDigestMismatch, ref, f.Path, "")
	case stateOK:
	}
}

// checkCommand checks a command of ref: its real script, and its entry in
// BinDir, a wrapper when wraps and the script is Lua, a copy of the script
// otherwise.
func (c *checker) checkCommand(ref RockRef, f File, wraps bool) {
	scriptOK := c.stateOf(f.Script, f.MD5) == stateOK
	c.checkInstalled(ref, f.Script, f.MD5)

	switch c.stateOf(f.Path, "") {
	case stateOK:
		c.own(f.Path)
	case stateNotRegular:
		c.add(IssueMissingFile, ref, f.Path, "not a regular file")

		return
	case stateMissing, stateModified:
		if other := c.otherSpot(ref, f); other != f.Path && c.stateOf(other, "") == stateOK {
			c.own(other)
			c.add(IssueWrongSpot, ref, other, "the provider lists put it at "+f.Path)
		} else {
			c.add(IssueMissingFile, ref, f.Path, "")
		}

		return
	}

	if !scriptOK {
		return
	}

	if !wraps || !c.t.isLuaScript(f.Script) {
		if c.stateOf(f.Path, f.MD5) != stateOK {
			c.add(IssueDigestMismatch, ref, f.Path, "not a copy of "+f.Script)
		}

		return
	}

	if c.wrap == nil {
		return
	}

	want := (&Tree{Paths: c.t.Paths, BinWrap: c.wrap}).binWrapperScript(f.Script, ref.Name, ref.Version)
	if have, err := readInside(c.t.BinDir(), f.Path); err != nil || !bytes.Equal(have, []byte(want)) {
		c.add(IssueStaleWrapper, ref, f.Path, "")
	}
}

// checkOrphans lists the rock dirs the manifest does not list.
func (c *checker) checkOrphans() error {
	found, err := diskRocks(c.t.Paths, true)
	if errors.Is(err, rocks.ErrUnsafePath) {
		c.add(IssueOrphanRock, RockRef{}, c.t.RocksDir(), err.Error())

		return nil
	}

	if err != nil {
		return err
	}

	for _, d := range found {
		if _, listed := c.m.Repository[d.ref.Name][d.ref.Version]; !listed {
			c.add(IssueOrphanRock, d.ref, d.dir, "")
		}
	}

	return nil
}

// checkUntracked lists the files of the deploy dirs no rock owns.
func (c *checker) checkUntracked() error {
	start := len(c.issues)

	err := walkDeployed(c.t.Paths, func(path, rel string) error {
		switch {
		case c.owned[rel]:
		case strings.HasSuffix(rel, "~"):
			c.add(IssueBackup, RockRef{}, path, "")
		default:
			c.add(IssueUntracked, RockRef{}, path, "")
		}

		return nil
	})

	slices.SortFunc(c.issues[start:], func(a, b Issue) int { return strings.Compare(a.Path, b.Path) })

	return err
}
