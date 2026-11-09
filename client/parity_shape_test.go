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
// normalized. File contents are compared elsewhere.
func compareShapes(tb testing.TB, label string, a, b shapeSide) {
	tb.Helper()

	entA := treeEntries(tb, a.root)
	entB := treeEntries(tb, b.root)
	normA, normB := rootReplacer(a.roots), rootReplacer(b.roots)

	all := maps.Clone(entA)
	maps.Copy(all, entB)

	for _, rel := range slices.Sorted(maps.Keys(all)) {
		eA, inA := entA[rel]
		eB, inB := entB[rel]

		switch {
		case !inA || !inB:
			side, e := a.name, eA
			if !inA {
				side, e = b.name, eB
			}

			tb.Errorf("[%s] %s %q only in the %s tree (%#o)", label, e.kind(), rel, side, e.perm)
		case eA.typ != eB.typ:
			tb.Errorf("[%s] %q is a %s in the %s tree but a %s in the %s tree", label, rel, eA.kind(), a.name, eB.kind(), b.name)
		case eA.typ == fs.ModeSymlink:
			if tA, tB := normA.Replace(eA.target), normB.Replace(eB.target); tA != tB {
				tb.Errorf("[%s] symlink %q: %s -> %q, %s -> %q", label, rel, a.name, eA.target, b.name, eB.target)
			}
		case eA.perm != eB.perm:
			tb.Errorf("[%s] MODE DIVERGENCE: %s %q: %s=%#o %s=%#o", label, eA.kind(), rel, a.name, eA.perm, b.name, eB.perm)
		}
	}
}
