//go:build integration_luaengine

package client_test

// Shape half of the backend-parity harness. The deployed files are compared
// byte for byte (compareDeployed) and the bookkeeping through what its
// manifests record (parity_manifest_test.go); this compares what the bytes
// leave out: the permission bits of every file and directory, every symlink's
// target, never followed, and the set of directories, empty ones included.
// The suite runs under a fixed umask (testenv.SetSuiteUmask, from TestMain),
// so a mode is the same on every machine.

import (
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// treeEntry is what compareShapes knows of one entry of a tree.
type treeEntry struct {
	// typ is the type bits of the entry's mode: 0 for a regular file,
	// fs.ModeDir, fs.ModeSymlink, ….
	typ fs.FileMode
	// perm is the permission bits of a file or directory, the setuid, setgid
	// and sticky bits included.
	perm fs.FileMode
	// target is the text of a symlink.
	target string
}

func (e treeEntry) kind() string {
	switch e.typ { //nolint:exhaustive // the other types name themselves
	case 0:
		return "file"
	case fs.ModeDir:
		return "dir"
	case fs.ModeSymlink:
		return "symlink"
	default:
		return e.typ.String()
	}
}

// permBits are the bits of a mode compareShapes compares.
const permBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// treeEntries walks root without following any symlink and returns every
// entry under it, root itself as ".", by slash path. The build/ staging
// directory the native backend keeps in each install dir — a documented
// difference, which only one backend writes — is left out, contents and all.
func treeEntries(tb testing.TB, root string) map[string]treeEntry {
	tb.Helper()

	out := map[string]treeEntry{}

	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)
		if isStagingDir(rel) {
			return fs.SkipDir
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		e := treeEntry{typ: info.Mode().Type(), perm: info.Mode() & permBits}
		if e.typ == fs.ModeSymlink {
			e.perm = 0 // meaningless on Linux, and not settable

			if e.target, err = os.Readlink(p); err != nil {
				return err
			}
		}

		out[rel] = e

		return nil
	})
	require.NoError(tb, err, "walk %s", root)

	return out
}

// isStagingDir reports whether rel, a slash path in a tree, is the build/
// staging directory the native backend keeps in an install dir,
// share/tarantool/rocks/<name>/<version>/build.
func isStagingDir(rel string) bool {
	rest, ok := strings.CutPrefix(rel, "share/tarantool/rocks/")
	if !ok {
		return false
	}

	parts := strings.Split(rest, "/")

	return len(parts) == 3 && parts[2] == "build"
}

// shapeSide is one of the two trees compareShapes takes: its root, the name
// its messages call it by, and the roots a symlink target may name, which
// are normalized before the targets are compared.
type shapeSide struct {
	name  string
	root  string
	roots []string
}

// compareShapes requires the trees of a and b to hold the same entries of the
// same kinds, directories included, with the same permission bits, and every
// symlink to have the same target once the roots of each side are
// normalized. File contents are compared elsewhere. A difference
// knownModeDiff or emptyDeployDir names is logged with its reason instead.
func compareShapes(tb testing.TB, label string, a, b shapeSide) {
	tb.Helper()

	entA := treeEntries(tb, a.root)
	entB := treeEntries(tb, b.root)
	normA, normB := rootReplacer(a.roots), rootReplacer(b.roots)
	packed := isNativePacked(a.root) || isNativePacked(b.root)

	all := maps.Clone(entA)
	maps.Copy(all, entB)

	for _, rel := range slices.Sorted(maps.Keys(all)) {
		eA, inA := entA[rel]
		eB, inB := entB[rel]

		switch {
		case !inA || !inB:
			side, e, ent := a.name, eA, entA
			if !inA {
				side, e, ent = b.name, eB, entB
			}

			if why, ok := emptyDeployDir(rel, ent); ok {
				tb.Logf("[%s] note: empty %s %q only in the %s tree: %s", label, e.kind(), rel, side, why)

				continue
			}

			if rel == treeLockFile && e.typ == 0 {
				tb.Logf("[%s] note: %q only in the %s tree: %s", label, rel, side, divLuaLockNewTree)

				continue
			}

			tb.Errorf("[%s] %s %q only in the %s tree (%#o)", label, e.kind(), rel, side, e.perm)
		case eA.typ != eB.typ:
			tb.Errorf("[%s] %q is a %s in the %s tree but a %s in the %s tree", label, rel, eA.kind(), a.name, eB.kind(), b.name)
		case eA.typ == fs.ModeSymlink:
			if tA, tB := normA.Replace(eA.target), normB.Replace(eB.target); tA != tB {
				tb.Errorf("[%s] symlink %q: %s -> %q, %s -> %q", label, rel, a.name, eA.target, b.name, eB.target)
			}
		case eA.perm != eB.perm:
			if why, ok := knownModeDiff(rel, eA, eB, packed); ok {
				tb.Logf("[%s] note: %s %q mode %s=%#o %s=%#o: %s", label, eA.kind(), rel, a.name, eA.perm, b.name, eB.perm, why)

				continue
			}

			tb.Errorf("[%s] MODE DIVERGENCE: %s %q: %s=%#o %s=%#o", label, eA.kind(), rel, a.name, eA.perm, b.name, eB.perm)
		}
	}
}

// The shape differences compareShapes accepts, each under a name and with
// its reason. They are findings of the comparison waiting for a fix, not
// differences by design: each names the fix that drops it.
const (
	// divEmptyDeployDirs: a native removal prunes the directories it empties
	// up to, not including, the deploy dir (tree/conflicts.go
	// pruneEmptyParents), where upstream's fs.remove_dir_tree_if_empty climbs
	// past it: after the last command or C module of a tree goes, the native
	// tree keeps an empty bin/, lib/tarantool/ and lib/, the lua tree not.
	//
	// TODO: drop divEmptyDeployDirs when a native removal prunes the deploy
	// dirs it empties too, bounded by the tree's root.
	divEmptyDeployDirs = "pending fix of tree/: a native removal keeps a deploy dir it empties " +
		"(bin/, lib/tarantool/, lib/), where upstream removes it"
	// divPackModes: a rock the native backend packs carries no permission
	// bits (client/native.go zipDir, PackStaged), so upstream, which keeps
	// the bits unzip restores, installs the commands kept in the rock's
	// install dir (share/tarantool/rocks/<name>/<version>/bin/) 0644, where it
	// installs those of a rock upstream packed 0755. The deployed commands are
	// not affected: the deploy makes them executable.
	//
	// TODO: drop divPackModes when the native packer records 0755 for the
	// commands and 0644 for the rest, as upstream's zip does.
	divPackModes = "pending fix of client/native.go: a rock the native backend packs carries no " +
		"permission bits, so upstream installs the commands in its install dir 0644"
	// divLuaLockNewTree: the lua backend takes the tree lock only when the
	// tree's root exists before the write (client/lua.go dispatchLocked), so
	// its first write into a tree that does not exist yet runs unlocked and
	// leaves no lock file, where the native backend creates the tree and
	// takes the lock.
	//
	// TODO: drop divLuaLockNewTree when the lua backend creates a missing
	// tree's rocks dir and locks it as the native backend does.
	divLuaLockNewTree = "pending fix of client/lua.go: the lua backend writes a tree that does not exist yet " +
		"unlocked, so it leaves no lock file"
)

// treeLockFile is the lock file of a tree (internal/treelock).
const treeLockFile = "share/tarantool/rocks/.lock"

// knownModeDiff reports whether a and b, the same entry rel of two trees,
// differ in mode only as one of the named differences says, and which.
// Either tree may be the lua backend's, or hold what it wrote: in the interop
// harness both backends write the same tree. packed says one of the trees was
// installed from a rock the native backend packed (markNativePacked).
func knownModeDiff(rel string, a, b treeEntry, packed bool) (string, bool) {
	pair := func(x, y fs.FileMode) bool {
		return (a.perm == x && b.perm == y) || (a.perm == y && b.perm == x)
	}

	switch {
	case a.typ == 0 && packed && isInstallDirCommand(rel) && pair(0o644, 0o755):
		return divPackModes, true
	default:
		return "", false
	}
}

// isInstallDirCommand reports whether rel is a command a rock keeps in its
// install dir, share/tarantool/rocks/<name>/<version>/bin/….
func isInstallDirCommand(rel string) bool {
	rest, ok := strings.CutPrefix(rel, "share/tarantool/rocks/")
	if !ok {
		return false
	}

	parts := strings.Split(rest, "/")

	return len(parts) > 3 && parts[2] == "bin"
}

// emptyDeployDir reports whether rel, an entry of the tree whose entries are
// ent, is a deploy dir divEmptyDeployDirs names that holds no file, and why.
func emptyDeployDir(rel string, ent map[string]treeEntry) (string, bool) {
	if ent[rel].typ != fs.ModeDir || !slices.Contains([]string{"bin", "lib", "lib/tarantool"}, rel) {
		return "", false
	}

	for p, e := range ent {
		if strings.HasPrefix(p, rel+"/") && e.typ != fs.ModeDir {
			return "", false
		}
	}

	return divEmptyDeployDirs, true
}

// nativePacked holds the roots of the trees installed from a rock the native
// backend packed, which divPackModes applies to.
var nativePacked sync.Map

// markNativePacked records that the tree root was installed from a rock the
// native backend packed.
func markNativePacked(root string) {
	nativePacked.Store(root, true)
}

func isNativePacked(root string) bool {
	_, ok := nativePacked.Load(root)

	return ok
}
