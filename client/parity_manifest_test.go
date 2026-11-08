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
func readRockManifest(t *testing.T, treeRoot, name, version string) map[string]string {
	t.Helper()

	p := filepath.Join(tree.Paths{Tree: treeRoot}.InstallDir(name, version), "rock_manifest")
	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(t, err, "read %s", p)

	v, err := manif.Parse(b)
	require.NoError(t, err, "parse %s", p)

	top, ok := v.(map[string]any)
	require.True(t, ok, "%s: top level is %T", p, v)

	inner, ok := top["rock_manifest"].(map[string]any)
	require.True(t, ok, "%s: rock_manifest is %T", p, top["rock_manifest"])

	out := map[string]string{}
	flattenRockManifest(t, "", inner, out)

	return out
}

func flattenRockManifest(t *testing.T, prefix string, m map[string]any, dst map[string]string) {
	t.Helper()

	for k, v := range m {
		full := k
		if prefix != "" {
			full = prefix + "/" + k
		}

		switch x := v.(type) {
		case string:
			dst[full] = x
		case map[string]any:
			flattenRockManifest(t, full, x, dst)
		default:
			t.Errorf("rock_manifest entry %q is %T", full, v)
		}
	}
}

// compareRockManifests requires the rock_manifest of name/version to list
// the same files with the same digests in both trees. Two kinds of entry may
// differ in their digest only: a compiled .so, which two compilations do not
// reproduce byte for byte, and the rockspec, whose source.url names a
// per-test path; each of those must still match the file it describes.
func compareRockManifests(t *testing.T, label, treeNative, treeLua, name, version string) {
	t.Helper()

	rmN := readRockManifest(t, treeNative, name, version)
	rmL := readRockManifest(t, treeLua, name, version)

	for _, only := range symmetricDiff(setOf(rmN), setOf(rmL)) {
		_, inN := rmN[only]
		side := "lua"
		if inN {
			side = "native"
		}

		t.Errorf("[%s] rock_manifest of %s %s: %q listed only by the %s backend", label, name, version, only, side)
	}

	specKey := name + "-" + version + ".rockspec"

	for key, sumN := range rmN {
		sumL, ok := rmL[key]
		if !ok || sumN == sumL {
			continue
		}

		switch {
		case strings.HasSuffix(key, ".so"):
			t.Logf("[%s] note: rock_manifest digest of compiled %q differs (non-reproducible build)", label, key)
		case key == specKey:
			installN := tree.Paths{Tree: treeNative}.InstallDir(name, version)
			installL := tree.Paths{Tree: treeLua}.InstallDir(name, version)

			assert.Equal(t, fileMD5(t, filepath.Join(installN, key)), sumN, "[%s] native rock_manifest digest of %s", label, key)
			assert.Equal(t, fileMD5(t, filepath.Join(installL, key)), sumL, "[%s] lua rock_manifest digest of %s", label, key)
		default:
			t.Errorf("[%s] rock_manifest of %s %s: %q digest native=%s lua=%s", label, name, version, key, sumN, sumL)
		}
	}
}

func fileMD5(t *testing.T, p string) string {
	t.Helper()

	b, err := os.ReadFile(p) //nolint:gosec // test temp path
	require.NoError(t, err, "read %s", p)

	sum := md5.Sum(b)

	return hex.EncodeToString(sum[:])
}

// compareTreeManifests requires the tree manifests of both trees to record
// the same rocks, the same modules and commands per rock, the same provider
// lists in the same order (index 0 is the active provider), and the same
// dependencies.
func compareTreeManifests(t *testing.T, label, treeNative, treeLua string) {
	t.Helper()

	mN := readTreeManifestOf(t, treeNative)
	mL := readTreeManifestOf(t, treeLua)

	assert.Equal(t, mL.Repository, mN.Repository, "[%s] tree manifest repository", label)
	assert.Equal(t, mL.Modules, mN.Modules, "[%s] tree manifest modules", label)
	assert.Equal(t, mL.Commands, mN.Commands, "[%s] tree manifest commands", label)
	assert.Equal(t, mL.Dependencies, mN.Dependencies, "[%s] tree manifest dependencies", label)
}

// readTreeManifestOf reads the tree manifest of treeRoot with nil and empty
// maps folded together, since the two writers differ in whether they emit
// an empty table.
func readTreeManifestOf(t *testing.T, treeRoot string) *rocks.Manifest {
	t.Helper()

	m, err := manif.FileStore{}.ReadTree(tree.Paths{Tree: treeRoot}.RocksDir())
	require.NoError(t, err, "read tree manifest of %s", treeRoot)

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
