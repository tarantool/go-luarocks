package client_test

// PackStaged packs a staged binary rock as the native Pack packs the rock
// once installed, and the archive installs as that rock.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// stagedRockSpec is the rockspec of the staged rock.
const stagedRockSpec = `package = "staged"
version = "1.0-1"
source = { url = "file://localhost/staged.tar.gz" }
build = { type = "none" }
`

// fileContents returns every regular file under root by its slash-separated
// path relative to root, root itself replaced by <ROOT> in the contents.
func fileContents(t *testing.T, root string) map[string]string {
	t.Helper()

	out := map[string]string{}

	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}

		b, err := os.ReadFile(p) //nolint:gosec // test temp path
		if err != nil {
			return err
		}

		out[filepath.ToSlash(rel)] = strings.ReplaceAll(string(b), root, "<ROOT>")

		return nil
	}))

	return out
}

// TestPackStaged: the rock PackStaged makes of a staged dir is byte for byte
// the one Pack makes of the rock installed from that dir, and installs as
// that rock does.
func TestPackStaged(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.EvalSource("staged-1.0-1.rockspec", []byte(stagedRockSpec), rocks.RockspecConfig{})
	require.NoError(t, err)

	dir := t.TempDir()
	for rel, body := range map[string]string{
		"lua/staged/init.lua": "return 'staged'\n",
		"lib/staged/core.so":  "\x7fELF not really\n",
		"bin/staged-tool":     "print('staged')\n",
		"doc/README.md":       "# staged\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	}

	out := filepath.Join(t.TempDir(), "staged-1.0-1.linux-x86_64.rock")
	tmp := t.TempDir()

	err = client.PackStaged(spec, dir, out, client.PackStagedOpts{TempDir: filepath.Join(tmp, "absent")})
	require.ErrorContains(t, err, filepath.Join(tmp, "absent"), "PackStaged into a TempDir that does not exist")

	require.NoError(t, client.PackStaged(spec, dir, out, client.PackStagedOpts{TempDir: tmp}), "PackStaged")

	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left, "the temporary tree is removed from TempDir")

	cfg := rocks.Config{Tree: t.TempDir(), WorkingDir: t.TempDir(), Tarantool: rocks.TarantoolConfig{Prefix: "/opt/test"}}

	tr, err := tree.Open(cfg)
	require.NoError(t, err)

	tr.BinWrap = client.BinWrapFor(cfg)
	require.NoError(t, tr.Update(context.Background(), func(tx *tree.Tx) error {
		_, err := tx.InstallStaged(spec, dir, tree.StagedOpts{})

		return err
	}), "InstallStaged")

	packed, err := newClient(t, cfg).Pack(context.Background(), "staged", client.PackOpts{})
	require.NoError(t, err, "Pack")

	want, err := os.ReadFile(packed) //nolint:gosec // test temp path
	require.NoError(t, err)

	got, err := os.ReadFile(out) //nolint:gosec // test temp path
	require.NoError(t, err)

	assert.Equal(t, want, got, "PackStaged vs Pack of the installed rock")

	st, err := os.Stat(out)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), st.Mode().Perm())

	repo := t.TempDir()
	require.NoError(t, os.Rename(packed, filepath.Join(repo, filepath.Base(packed))))
	require.NoError(t, remote.MakeManifest(context.Background(), repo, remote.MakeManifestOpts{}))

	icfg := cfg
	icfg.Tree = t.TempDir()
	icfg.Servers = []string{repo}
	require.NoError(t, newClient(t, icfg).Install(context.Background(), "staged", client.InstallOpts{Deps: client.DepsNone}),
		"Install the packed rock")

	assert.Equal(t, fileContents(t, cfg.Tree), fileContents(t, icfg.Tree), "installed from the rock vs staged")

	entries, err := os.ReadDir(filepath.Dir(out))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temp file left by PackStaged")
}
