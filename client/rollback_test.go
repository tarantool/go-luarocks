package client_test

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

// snapshotTree returns every entry under root by its slash path relative to
// it — a directory and its mode, a file, its mode and digest, a symlink and
// its target — so that two snapshots are equal when nothing in the tree
// changed.
func snapshotTree(tb testing.TB, root string) map[string]string {
	tb.Helper()

	out := map[string]string{}

	require.NoError(tb, filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
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

		rel = filepath.ToSlash(rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}

			out[rel] = "link " + target
		case d.IsDir():
			out[rel] = "dir " + st.Mode().Perm().String()
		default:
			body, err := os.ReadFile(p) //nolint:gosec // test temp path
			if err != nil {
				return err
			}

			sum := md5.Sum(body)
			out[rel] = "file " + st.Mode().Perm().String() + " " + hex.EncodeToString(sum[:])
		}

		return nil
	}), "snapshot %s", root)

	return out
}

// requireTreeClean requires Tree.Check to find nothing wrong with the tree
// of cfg, its command wrappers checked against the ones the native backend
// writes for cfg.
func requireTreeClean(tb testing.TB, label string, cfg rocks.Config) {
	tb.Helper()

	tr, err := tree.Open(cfg)
	require.NoError(tb, err, "[%s] Open", label)

	report, err := tr.Check(context.Background(), tree.CheckOpts{BinWrap: client.BinWrapFor(cfg)})
	require.NoError(tb, err, "[%s] Check", label)
	assert.Empty(tb, report.Issues, "[%s] Check", label)
}

// rollbackRockspec is the rockspec of a pure-Lua builtin rock roll at
// version, shipping the modules roll and shared and the command roll-tool
// from bin, which may be missing.
func rollbackRockspec(version, bin string) string {
	return `package = "roll"
version = "` + version + `"
source = { url = "file://localhost/roll.tar.gz" }
build = {
   type = "builtin",
   modules = { roll = "roll.lua", shared = "shared.lua" },
   install = { bin = { ["roll-tool"] = "` + bin + `" } },
}
`
}

// makeRollback makes roll at version from fresh sources into tree with the
// native backend and returns the Make's error and the config it ran with.
func makeRollback(t *testing.T, tree, version, bin string) (rocks.Config, error) {
	t.Helper()

	src := t.TempDir()
	for rel, body := range map[string]string{
		"roll.lua":                      "return 'roll " + version + "'\n",
		"shared.lua":                    "return 'shared " + version + "'\n",
		"bin/roll-tool":                 "print('roll " + version + "')\n",
		"roll-" + version + ".rockspec": rollbackRockspec(version, bin),
	} {
		p := filepath.Join(src, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}

	cfg := rocks.Config{
		Tree:       tree,
		WorkingDir: src,
		Tarantool:  rocks.TarantoolConfig{Prefix: "/opt/test", IncludeDir: "/opt/test/include"},
	}

	r, err := client.New(cfg)
	require.NoError(t, err, "New")

	return cfg, r.Make(context.Background(), client.MakeOpts{RockspecPath: filepath.Join(src, "roll-"+version+".rockspec")})
}

// TestNative_FailedMakeUndone: a native Make that fails after it built —
// its command is missing, which the builtin backend finds as it stages the
// rock in its install dir's build/ — leaves the tree as it was: the version
// installed before, its files at their plain paths, no install dir or build/
// staging dir of the new version, and the manifests unchanged.
func TestNative_FailedMakeUndone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := makeRollback(t, root, "1.0-1", "bin/roll-tool")
	require.NoError(t, err, "Make 1.0-1")

	before := snapshotTree(t, root)

	_, err = makeRollback(t, root, "2.0-1", "bin/missing")
	require.Error(t, err, "Make 2.0-1 with its command missing")

	assert.Equal(t, before, snapshotTree(t, root), "the tree after the failed Make")
	assert.NoDirExists(t, filepath.Join(root, "share", "tarantool", "rocks", "roll", "2.0-1"))
	requireTreeClean(t, "failed upgrade", cfg)
}

// TestNative_FailedRemakeRemovesTheCopy: a native Make of the version
// installed that fails leaves the version removed, as upstream's build does:
// it deletes the installed copy for good before it builds. The rest is
// undone, and the tree is consistent.
func TestNative_FailedRemakeRemovesTheCopy(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cfg, err := makeRollback(t, root, "1.0-1", "bin/roll-tool")
	require.NoError(t, err, "Make 1.0-1")

	_, err = makeRollback(t, root, "1.0-1", "bin/missing")
	require.Error(t, err, "Make 1.0-1 again with its command missing")

	v, err := tree.Inspect(context.Background(), root)
	require.NoError(t, err)
	assert.Empty(t, v.Rocks(), "the copy the Make removed before it built")
	assert.NoDirExists(t, filepath.Join(root, "share", "tarantool", "rocks", "roll"))
	assert.NoFileExists(t, filepath.Join(root, "share", "tarantool", "roll.lua"))
	requireTreeClean(t, "failed reinstall", cfg)
}
