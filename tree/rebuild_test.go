package tree_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/tree"
)

// rebuild runs Tree.Rebuild on the tree at root.
func rebuild(t *testing.T, root string, opts tree.RebuildOpts) (*tree.RebuildReport, error) {
	t.Helper()

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err, "Open")

	return tr.Rebuild(context.Background(), opts)
}

// TestRebuild_AsInstalled: the manifest Rebuild makes of a natively built
// tree — rocks sharing a module and a command, two versions of one, a
// dependent installed after its dependency — is the one the installs wrote,
// byte for byte, whether the manifest was there, gone or broken.
func TestRebuild_AsInstalled(t *testing.T) {
	t.Parallel()

	for _, order := range [][]rockDef{{alpha1, alpha2, beta1, gamma1}, {gamma1, alpha2, alpha1, beta1}} {
		root := buildTree(t, order...)
		manifest := filepath.Join(tree.Paths{Tree: root}.RocksDir(), "manifest")
		want := readFile(t, manifest)

		for name, damage := range map[string]func(){
			"as it is": func() {},
			"gone":     func() { require.NoError(t, os.Remove(manifest)) },
			"broken":   func() { writeFixture(t, manifest, "repository = {") },
			"stale": func() {
				require.NoError(t, update(t, root, func(tx *tree.Tx) error { return tx.Remove(gamma1.ref()) }))
				makeRock(t, root, gamma1)
				writeFixture(t, manifest, "repository = {}\n")
			},
		} {
			damage()

			report, err := rebuild(t, root, tree.RebuildOpts{})
			require.NoError(t, err, "Rebuild, manifest %s", name)
			assert.Equal(t, []tree.RockRef{alpha2.ref(), alpha1.ref(), beta1.ref(), gamma1.ref()}, report.Rocks, name)
			assert.Empty(t, report.Skipped, name)
			assert.Empty(t, report.NoRockspec, name)
			assert.Equal(t, want, readFile(t, manifest), "the rebuilt manifest, manifest %s", name)
		}

		requireConsistent(t, root, order...)
	}
}

// TestRebuild_Skips: a rock dir without a rock_manifest is skipped, or with
// Strict fails Rebuild, the manifest untouched; a rock without its rockspec
// is listed without its declared dependencies; a dir that is no version is
// not a rock.
func TestRebuild_Skips(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, beta1)
	p := tree.Paths{Tree: root}
	manifest := filepath.Join(p.RocksDir(), "manifest")

	writeFixture(t, filepath.Join(p.InstallDir("ghost", "1.0-1"), "leftover"), "x")
	writeFixture(t, filepath.Join(p.InstallDir("ghost", "notes"), "rock_manifest"), "rock_manifest = {}\n")
	writeFixture(t, filepath.Join(p.InstallDir("evil", "1.0-1"), "rock_manifest"),
		"rock_manifest = {\n   lua = {\n      [\"../x.lua\"] = \"0\",\n   },\n}\n")
	writeFixture(t, manifest, "stays")

	_, err := rebuild(t, root, tree.RebuildOpts{Strict: true})
	require.ErrorIs(t, err, tree.ErrCorrupt, "Strict")
	assert.Equal(t, "stays", readFile(t, manifest), "the manifest after a Strict failure")

	require.NoError(t, os.Remove(filepath.Join(p.InstallDir("beta", "1.0-1"), "beta-1.0-1.rockspec")))

	report, err := rebuild(t, root, tree.RebuildOpts{})
	require.NoError(t, err, "Rebuild")
	assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, report.Rocks)
	assert.Equal(t, []tree.RockRef{beta1.ref()}, report.NoRockspec)

	skipped := map[tree.RockRef]error{}
	for _, s := range report.Skipped {
		skipped[s.Rock] = s.Err
		assert.Equal(t, p.InstallDir(s.Rock.Name, s.Rock.Version), s.Dir)
	}

	require.Len(t, skipped, 2)
	require.ErrorIs(t, skipped[tree.RockRef{Name: "ghost", Version: "1.0-1"}], tree.ErrCorrupt)
	require.ErrorIs(t, skipped[tree.RockRef{Name: "evil", Version: "1.0-1"}], rocks.ErrUnsafePath)

	v := inspect(t, root)
	assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, v.Rocks())
	assert.Empty(t, v.Dependencies(beta1.ref()), "beta without its rockspec")
	assert.Equal(t, []tree.RockRef{alpha1.ref(), beta1.ref()}, v.Providers(tree.ModuleItem, "shared"))
}
