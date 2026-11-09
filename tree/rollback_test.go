package tree_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// snapshot returns every entry under root by its slash path relative to it:
// a directory and its mode, a file, its mode and digest, a symlink and its
// target.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		st, err := d.Info()
		if err != nil {
			return err
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}

			out[filepath.ToSlash(rel)] = "link " + target
		case d.IsDir():
			out[filepath.ToSlash(rel)] = "dir " + st.Mode().Perm().String()
		default:
			body, err := os.ReadFile(p) //nolint:gosec // test temp path
			if err != nil {
				return err
			}

			sum := md5.Sum(body)
			out[filepath.ToSlash(rel)] = "file " + st.Mode().Perm().String() + " " + hex.EncodeToString(sum[:])
		}

		return nil
	}))

	return out
}

// requireAsItWas checks that the tree at root is the snapshot before and that
// Check finds nothing wrong with it.
func requireAsItWas(t *testing.T, root string, before map[string]string) {
	t.Helper()

	assert.Equal(t, before, snapshot(t, root), "the tree after the failed change")
	assert.Empty(t, check(t, root).Issues, "Check after the failed change")
}

// builtinSpec returns the rockspec of a builtin rock name/version shipping
// the modules shared and alpha and the command tool, from the source files
// shared.lua, alpha.lua and bin.
func builtinSpec(t *testing.T, name, version, bin string) *rocks.Rockspec {
	t.Helper()

	src := `package = "` + name + `"
version = "` + version + `"
source = { url = "file://localhost/` + name + `.tar.gz" }
build = {
   type = "builtin",
   modules = { shared = "shared.lua", alpha = "alpha.lua" },
   install = { bin = { tool = "` + bin + `" } },
}
`

	spec, err := rockspec.EvalSource(name+".rockspec", []byte(src), rocks.RockspecConfig{})
	require.NoError(t, err)

	return spec
}

// TestUpdate_FailedDeployUndone: a deploy that fails late — a command its
// rockspec names is missing, after its modules took the plain paths of
// alpha 1.0's and demoted them — leaves the tree as it was: alpha 1.0's
// files back at their plain paths, beta's where they were, no install dir
// and no staging dir of the new version, and the manifests unchanged. The
// same holds for a reinstall of the very version installed, whose old copy
// Deploy removes first, and for a deploy function that fails after building
// into the install dir.
func TestUpdate_FailedDeployUndone(t *testing.T) {
	t.Parallel()

	src := stageFiles(t, map[string]string{
		"shared.lua": "return 'new shared'\n",
		"alpha.lua":  "return 'new alpha'\n",
	})

	for _, tc := range []struct {
		name, version string
		built         bool
	}{
		{"upgrade", "2.0-1", false},
		{"reinstall", "1.0-1", false},
		{"build", "2.0-1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root := buildTree(t, alpha1, beta1)
			before := snapshot(t, root)

			spec := builtinSpec(t, "alpha", tc.version, "bin/missing")
			errBuild := errors.New("the build failed")

			err := openWrapped(t, root).Update(context.Background(), func(tx *tree.Tx) error {
				return tx.Deploy(spec, func(tr *tree.Tree) (*rocks.RockManifest, error) {
					buildDir := filepath.Join(tr.InstallDir(spec.Package, spec.Version), "build")
					writeFixture(t, filepath.Join(buildDir, "lua", "shared.lua"), "return 'built'\n")

					if tc.built {
						return nil, errBuild
					}

					return tr.Deploy(spec, src, buildDir)
				})
			})
			require.Error(t, err)

			if tc.built {
				require.ErrorIs(t, err, errBuild)
			}

			requireAsItWas(t, root, before)
			requireConsistent(t, root, alpha1, beta1)
		})
	}
}

// TestDeploy_FailedDeployUndone: Tree.Deploy on its own undoes what it
// changed when it fails.
func TestDeploy_FailedDeployUndone(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, beta1)
	before := snapshot(t, root)

	src := stageFiles(t, map[string]string{
		"shared.lua": "return 'new shared'\n",
		"alpha.lua":  "return 'new alpha'\n",
	})

	tr := openWrapped(t, root)
	v := inspect(t, root)
	tr.Providers = func(kind tree.ItemKind, item string) []string {
		refs := v.Providers(kind, item)

		out := make([]string, 0, len(refs))
		for _, r := range refs {
			out = append(out, r.String())
		}

		return out
	}

	_, err := tr.Deploy(builtinSpec(t, "alpha", "2.0-1", "bin/missing"), src, src)
	require.Error(t, err)

	// The install dir Deploy wrote the rockspec into is the caller's, as the
	// rock_manifest it writes is; Deploy put back the rest.
	require.NoError(t, os.RemoveAll(tr.InstallDir("alpha", "2.0-1")))
	requireAsItWas(t, root, before)
}

// TestInstallStaged_FailedInstallUndone: a staged install that fails on a
// regular file where one of its module directories must go — beta's
// beta.lua — after it removed the other versions of the rock and deployed its
// first module, leaves the tree as it was, alpha 1.0 and its plain paths
// back.
func TestInstallStaged_FailedInstallUndone(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, beta1)
	before := snapshot(t, root)

	dir := stageFiles(t, map[string]string{
		"lua/alpha.lua":      "return 'staged alpha'\n",
		"lua/beta.lua/x.lua": "return 'blocked'\n",
	})

	_, err := installStaged(t, root, stagedSpec(t, "alpha", "2.0-1"), dir, tree.StagedOpts{RemoveOtherVersions: true})
	require.Error(t, err)
	requireAsItWas(t, root, before)
}

// TestTx_Commit: the changes a Tx committed stay when fn fails afterwards,
// and only those after the Commit are undone; a change that succeeds leaves
// no trash in the rocks dir.
func TestTx_Commit(t *testing.T) {
	t.Parallel()

	root := buildTree(t, alpha1, beta1)
	errFn := errors.New("fn failed")

	err := update(t, root, func(tx *tree.Tx) error {
		require.NoError(t, tx.Remove(beta1.ref()))
		require.NoError(t, tx.Commit())
		require.NoError(t, tx.Remove(alpha1.ref()))

		return errFn
	})
	require.ErrorIs(t, err, errFn)

	assert.Equal(t, []tree.RockRef{alpha1.ref()}, inspect(t, root).Rocks(), "beta's removal was committed, alpha's undone")
	requireConsistent(t, root, alpha1)
	assert.Empty(t, check(t, root).Issues)

	require.NoError(t, update(t, root, func(tx *tree.Tx) error { return tx.Remove(alpha1.ref()) }))

	trash, err := filepath.Glob(filepath.Join(tree.Paths{Tree: root}.RocksDir(), ".tx-*"))
	require.NoError(t, err)
	assert.Empty(t, trash, "the trash of a change that succeeded")
	assert.Empty(t, inspect(t, root).Rocks())
}
