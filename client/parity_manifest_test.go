//go:build integration_luaengine

package client_test

// Manifest half of the backend-parity harness. The deployed files are
// compared byte for byte (parity_extra_test.go); the two files LuaRocks keeps
// about them — each rock's rock_manifest and the tree's manifest — may be
// formatted differently by the two backends, but they must say the same
// thing, since the native manifest reader and writer are meant to be
// interchangeable with upstream's. These helpers parse both and compare
// their contents.

import (
	"crypto/md5"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/tree"
)

// readRockManifest parses the rock_manifest of name/version in treeRoot and
// flattens it into slash path → md5, the shape upstream make_rock_manifest
// builds by walking the install dir.
func readRockManifest(tb testing.TB, treeRoot, name, version string) map[string]string {
	tb.Helper()

	p := filepath.Join(tree.Paths{Tree: treeRoot}.InstallDir(name, version), "rock_manifest")
	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(tb, err, "read %s", p)

	v, err := manif.Parse(b)
	require.NoError(tb, err, "parse %s", p)

	top, ok := v.(map[string]any)
	require.True(tb, ok, "%s: top level is %T", p, v)

	inner, ok := top["rock_manifest"].(map[string]any)
	require.True(tb, ok, "%s: rock_manifest is %T", p, top["rock_manifest"])

	out := map[string]string{}
	flattenRockManifest(tb, "", inner, out)

	return out
}

func flattenRockManifest(tb testing.TB, prefix string, m map[string]any, dst map[string]string) {
	tb.Helper()

	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "/" + k
		}

		switch x := v.(type) {
		case string:
			dst[full] = x
		case map[string]any:
			flattenRockManifest(tb, full, x, dst)
		default:
			tb.Errorf("rock_manifest entry %q is %T", full, v)
		}
	}
}

// compareRockManifests requires the rock_manifest of name/version to list
// the same files with the same digests in both trees. Two kinds of entry may
// differ in their digest only: a compiled .so, which two compilations do not
// reproduce byte for byte, and the rockspec, whose source.url names a
// per-test path; each of those must still match the file it describes.
func compareRockManifests(tb testing.TB, label, treeNative, treeLua, name, version string) {
	tb.Helper()

	compareRockManifestsOf(tb, label, manifestSide{"native", treeNative}, manifestSide{"lua", treeLua}, name, version)
}

// manifestSide is one of the two trees a comparison takes, under the name its
// messages call it by.
type manifestSide struct {
	name string
	tree string
}

// compareRockManifestsOf is compareRockManifests over two trees named a and b.
func compareRockManifestsOf(tb testing.TB, label string, a, b manifestSide, name, version string) {
	tb.Helper()

	rmA := readRockManifest(tb, a.tree, name, version)
	rmB := readRockManifest(tb, b.tree, name, version)

	for _, only := range symmetricDiff(setOf(rmA), setOf(rmB)) {
		side := b.name
		if _, inA := rmA[only]; inA {
			side = a.name
		}

		tb.Errorf("[%s] rock_manifest of %s %s: %q listed only by the %s tree", label, name, version, only, side)
	}

	specKey := name + "-" + version + ".rockspec"

	for key, sumA := range rmA {
		sumB, ok := rmB[key]
		if !ok || sumA == sumB {
			continue
		}

		switch {
		case strings.HasSuffix(key, ".so"):
			tb.Logf("[%s] note: rock_manifest digest of compiled %q differs (non-reproducible build)", label, key)
		case key == specKey:
			installA := tree.Paths{Tree: a.tree}.InstallDir(name, version)
			installB := tree.Paths{Tree: b.tree}.InstallDir(name, version)

			assert.Equal(tb, fileMD5(tb, filepath.Join(installA, key)), sumA, "[%s] %s rock_manifest digest of %s", label, a.name, key)
			assert.Equal(tb, fileMD5(tb, filepath.Join(installB, key)), sumB, "[%s] %s rock_manifest digest of %s", label, b.name, key)
		default:
			tb.Errorf("[%s] rock_manifest of %s %s: %q digest %s=%s %s=%s", label, name, version, key, a.name, sumA, b.name, sumB)
		}
	}
}

func fileMD5(tb testing.TB, p string) string {
	tb.Helper()

	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(tb, err, "read %s", p)

	sum := md5.Sum(b)

	return hex.EncodeToString(sum[:])
}

// compareTreeManifests requires the tree manifests of both trees to record
// the same rocks, the same modules and commands per rock, the same provider
// lists in the same order (index 0 is the active provider), and the same
// dependencies.
func compareTreeManifests(tb testing.TB, label, treeNative, treeLua string) {
	tb.Helper()

	mN := readTreeManifestOf(tb, treeNative)
	mL := readTreeManifestOf(tb, treeLua)

	assert.Equal(tb, mL.Repository, mN.Repository, "[%s] tree manifest repository", label)
	assert.Equal(tb, mL.Modules, mN.Modules, "[%s] tree manifest modules", label)
	assert.Equal(tb, mL.Commands, mN.Commands, "[%s] tree manifest commands", label)
	assert.Equal(tb, mL.Dependencies, mN.Dependencies, "[%s] tree manifest dependencies", label)
}

// readTreeManifestOf reads the tree manifest of treeRoot with nil and empty
// maps folded together, since the two writers differ in whether they emit
// an empty table.
func readTreeManifestOf(tb testing.TB, treeRoot string) *rocks.Manifest {
	tb.Helper()

	m, err := manif.FileStore{}.ReadTree(tree.Paths{Tree: treeRoot}.RocksDir())
	require.NoError(tb, err, "read tree manifest of %s", treeRoot)

	out := &rocks.Manifest{
		Repository:   map[string]map[string]rocks.RepoEntry{},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}

	for name, versions := range m.Repository {
		out.Repository[name] = map[string]rocks.RepoEntry{}

		for v, e := range versions {
			out.Repository[name][v] = rocks.RepoEntry{
				Arch:         e.Arch,
				Modules:      nonNilMap(e.Modules),
				Commands:     nonNilMap(e.Commands),
				Dependencies: nonNilMap(e.Dependencies),
			}
		}
	}

	maps.Copy(out.Modules, m.Modules)
	maps.Copy(out.Commands, m.Commands)

	for name, versions := range m.Dependencies {
		out.Dependencies[name] = map[string][]rocks.Dep{}

		for v, d := range versions {
			if len(d) == 0 {
				d = nil
			}

			out.Dependencies[name][v] = d
		}
	}

	return out
}

func nonNilMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}

	return m
}

// installedVersions lists the "name version" pairs of the tree manifest of
// treeRoot, sorted.
func installedVersions(t *testing.T, treeRoot string) [][2]string {
	t.Helper()

	m := readTreeManifestOf(t, treeRoot)

	var out [][2]string

	for name, versions := range m.Repository {
		for v := range versions {
			out = append(out, [2]string{name, v})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}

		return out[i][1] < out[j][1]
	})

	return out
}
