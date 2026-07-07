package manif_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/manif"
)

func TestFileStoreTreeRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"metrics": {
				"1.0.0-1": {Arch: "installed"},
			},
		},
		Modules: map[string][]string{
			"metrics": {"metrics/1.0.0-1"},
		},
		Commands: map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{
			"metrics": {
				"1.0.0-1": {
					{
						Name: "checks",
						Constraints: []rocks.VersionConstraint{
							{Op: ">=", Version: rocks.Version{Raw: "3.1.0"}},
						},
					},
				},
			},
		},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteTree(dir, in), "WriteTree")
	out, err := store.ReadTree(dir)
	require.NoError(t, err, "ReadTree")
	assert.Equal(t, in.Modules, out.Modules, "modules differ")
	assert.Equal(t, in.Commands, out.Commands, "commands differ")
	assert.Equal(t, in.Repository, out.Repository, "repository differ")
	assert.Equal(t, "checks", out.Dependencies["metrics"]["1.0.0-1"][0].Name, "dep name")
	assert.Equal(t, ">=", out.Dependencies["metrics"]["1.0.0-1"][0].Constraints[0].Op, "dep op")
	assert.Equal(t, "3.1.0", out.Dependencies["metrics"]["1.0.0-1"][0].Constraints[0].Version.Raw, "dep ver")
}

func TestFileStoreTree_ConstraintVersionSerializedAsTable(t *testing.T) {
	t.Parallel()

	// glr-4x1: a constraint version with parsed Components must serialize as the
	// upstream parsed-version table `{ 3, 1, 0, string = "3.1.0" }`, not a bare
	// string, and round-trip back with Components intact.
	dir := t.TempDir()
	ver := rocks.Version{Raw: "3.1.0", Components: []float64{3, 1, 0}}
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{"m": {"1.0-1": {Arch: "installed"}}},
		Modules:    map[string][]string{},
		Commands:   map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{
			"m": {"1.0-1": {{Name: "checks", Constraints: []rocks.VersionConstraint{{Op: ">=", Version: ver}}}}},
		},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteTree(dir, in))

	raw, err := os.ReadFile(filepath.Join(dir, "manifest")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Contains(t, string(raw), `version = {`, "constraint version must serialize as a table")
	assert.Contains(t, string(raw), `string = "3.1.0"`, "table must carry the string field")

	out, err := store.ReadTree(dir)
	require.NoError(t, err)

	got := out.Dependencies["m"]["1.0-1"][0].Constraints[0].Version
	assert.Equal(t, "3.1.0", got.Raw)
	assert.Equal(t, []float64{3, 1, 0}, got.Components, "Components must round-trip from the table form")
}

// loadDepList must accept the empty dependency table upstream LuaRocks emits.
// A rock with no dependencies is written by upstream as `{}`, which the Lua
// parser decodes to an empty map (an empty table is ambiguous between array
// and map). The native reader must treat it as "no dependencies" so it can
// load manifests written by the gopher-lua backend. Regression for the
// finding where `r.List` failed with "expected dep array, got map" on a
// lua-installed tree.
func TestLoadDepList_EmptyUpstreamTable(t *testing.T) {
	t.Parallel()

	// Empty map (upstream's empty `{}`) → no deps, no error.
	got, err := manif.LoadDepList(map[string]any{})
	require.NoError(t, err, "empty deps table should parse cleanly")
	assert.Empty(t, got, "want 0 deps")

	// A populated array still parses.
	got, err = manif.LoadDepList([]any{map[string]any{"name": "checks"}})
	require.NoError(t, err, "array deps")
	require.Len(t, got, 1, "array deps")
	assert.Equal(t, "checks", got[0].Name, "array deps")

	// A non-empty map is still genuinely malformed and must error (fail loud).
	_, err = manif.LoadDepList(map[string]any{"checks": "x"})
	assert.Error(t, err, "non-empty map should still error")
}

// Round-trips a manifest whose RepoEntry carries Modules and Commands —
// asserts the per-arch index that `luarocks show` reads survives a
// write → read cycle. Regression for the finding where the facade
// silently emitted empty per-arch entries.
func TestFileStoreTreeRoundTrip_RepoEntryModulesAndCommands(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"metrics": {
				"1.0.0-1": {
					Arch: "installed",
					Modules: map[string]string{
						"metrics":                    "lua/metrics/init.lua",
						"metrics.collectors.counter": "lua/metrics/collectors/counter.lua",
					},
					Commands: map[string]string{
						"metrics-cli": "bin/metrics-cli",
					},
				},
			},
		},
		Modules: map[string][]string{
			"metrics":                    {"metrics/1.0.0-1"},
			"metrics.collectors.counter": {"metrics/1.0.0-1"},
		},
		Commands: map[string][]string{
			"metrics-cli": {"metrics/1.0.0-1"},
		},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteTree(dir, in), "WriteTree")
	out, err := store.ReadTree(dir)
	require.NoError(t, err, "ReadTree")

	got := out.Repository["metrics"]["1.0.0-1"]
	assert.Equal(t, "installed", got.Arch, "arch")
	assert.Equal(t, in.Repository["metrics"]["1.0.0-1"].Modules, got.Modules,
		"RepoEntry.Modules round-trip lost data")
	assert.Equal(t, in.Repository["metrics"]["1.0.0-1"].Commands, got.Commands,
		"RepoEntry.Commands round-trip lost data")
}

func TestWriteTree_AlwaysEmitsModulesAndCommands(t *testing.T) {
	t.Parallel()

	// glr-txq: an installed entry with no modules/commands must still emit
	// `modules = {}` and `commands = {}` to match upstream byte-for-byte.
	dir := t.TempDir()
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"doconly": {"1.0-1": {Arch: "installed"}}, // nil Modules/Commands
		},
		Modules:      map[string][]string{},
		Commands:     map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteTree(dir, in), "WriteTree")

	var manifestBody string

	require.NoError(t, filepath.Walk(dir, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}

		if !info.IsDir() && filepath.Base(p) == "manifest" {
			b, rerr := os.ReadFile(p) //nolint:gosec // reading a t.TempDir() path
			if rerr != nil {
				return rerr
			}

			manifestBody = string(b)
		}

		return nil
	}), "walk tree")

	require.NotEmpty(t, manifestBody, "manifest file not written")
	assert.Contains(t, manifestBody, "modules", "installed entry must emit a modules subtable")
	assert.Contains(t, manifestBody, "commands", "installed entry must emit a commands subtable")
	// glr-6y6: the per-entry dependencies map and the top-level dependencies
	// section are always emitted (as {} for a dep-less rock).
	assert.Contains(t, manifestBody, "dependencies", "installed entry must emit a dependencies subtable")
}

func TestWriteTree_DependenciesSection(t *testing.T) {
	t.Parallel()

	// glr-6y6: an installed rock's own dependency list is recorded under the
	// top-level `dependencies` section and its resolved deps under the per-entry
	// `dependencies` map.
	dir := t.TempDir()
	in := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"app": {"2.0-1": {Arch: "installed", Dependencies: map[string]string{"lib": "1.5-1"}}},
			"lib": {"1.5-1": {Arch: "installed"}},
		},
		Modules:  map[string][]string{},
		Commands: map[string][]string{},
		Dependencies: map[string]map[string][]rocks.Dep{
			"app": {"2.0-1": {{Name: "lib", Constraints: []rocks.VersionConstraint{{Op: ">=", Version: rocks.Version{Raw: "1.0"}}}}}},
			"lib": {"1.5-1": {}},
		},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteTree(dir, in))
	out, err := store.ReadTree(dir)
	require.NoError(t, err)

	// Top-level dependency list round-trips.
	require.Len(t, out.Dependencies["app"]["2.0-1"], 1)
	assert.Equal(t, "lib", out.Dependencies["app"]["2.0-1"][0].Name)
	// Per-entry resolved map round-trips.
	assert.Equal(t, "1.5-1", out.Repository["app"]["2.0-1"].Dependencies["lib"])
}

func TestFileStoreRockRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file := filepath.Join(dir, "share", "tarantool", "rocks", "metrics", "1.0.0-1", "rock_manifest")
	in := &rocks.RockManifest{
		// glr-b5y: the rockspec is keyed by its versioned filename.
		RockspecFile: "metrics-1.0.0-1.rockspec",
		Rockspec:     "abc123",
		Lua: map[string]string{
			"metrics/init.lua":               "1111",
			"metrics/collectors/counter.lua": "2222",
		},
		Lib: map[string]string{
			"native.so": "3333",
		},
	}
	store := manif.FileStore{}
	require.NoError(t, store.WriteRock(file, in), "WriteRock")

	raw, err := os.ReadFile(file) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Contains(t, string(raw), `["metrics-1.0.0-1.rockspec"]`, "rockspec keyed by versioned filename")
	assert.NotContains(t, string(raw), "\nrockspec = ", "must not use the fixed 'rockspec' key")

	out, err := store.ReadRock(file)
	require.NoError(t, err, "ReadRock")
	assert.Equal(t, in.RockspecFile, out.RockspecFile, "rockspec filename")
	assert.Equal(t, in.Rockspec, out.Rockspec, "rockspec md5")
	assert.Equal(t, in.Lua, out.Lua, "lua differ")
	assert.Equal(t, in.Lib, out.Lib, "lib differ")
}

func TestReadTreeManifestHelper(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := []byte(`commands = {}
modules = {
   metrics = {
      "metrics/1.0.0-1"
   }
}
repository = {
   metrics = {
      ["1.0.0-1"] = {
         {
            arch = "installed"
         }
      }
   }
}
`)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "manifest"), src, 0o600))
	m, err := manif.ReadTreeManifest(dir)
	require.NoError(t, err, "ReadTreeManifest")
	assert.Equal(t, "installed", m.Repository["metrics"]["1.0.0-1"].Arch, "arch")
	require.Len(t, m.Modules["metrics"], 1, "modules")
	assert.Equal(t, "metrics/1.0.0-1", m.Modules["metrics"][0], "modules")
}
