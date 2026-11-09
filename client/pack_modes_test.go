//go:build unix

package client_test

// The permission bits a packed rock records, and restores when installed.
// The suite runs under umask 022 (TestMain).

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// TestNativePack_RecordsModes: Pack and PackStaged record the permission
// bits of every file of the rock, made on Unix, as upstream's zip stores
// them — the commands kept in the install dir and the C modules 0755, the
// rest 0644 — and no modification time; a native install of the rock gets
// the commands of its install dir executable, as an install of a rock
// upstream packed does.
func TestNativePack_RecordsModes(t *testing.T) {
	t.Parallel()

	spec, err := rockspec.EvalSource("staged-1.0-1.rockspec", []byte(stagedRockSpec), rocks.RockspecConfig{})
	require.NoError(t, err)

	dir := t.TempDir()
	for rel, f := range map[string]struct {
		body string
		mode os.FileMode
	}{
		"lua/staged/init.lua": {"return 'staged'\n", 0o644},
		"lib/staged/core.so":  {"\x7fELF not really\n", 0o755},
		"bin/staged-tool":     {"print('staged')\n", 0o755},
		"doc/README.md":       {"# staged\n", 0o644},
	} {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(f.body), f.mode))
	}

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

	staged := filepath.Join(t.TempDir(), filepath.Base(packed))
	require.NoError(t, client.PackStaged(spec, dir, staged, client.PackStagedOpts{TempDir: t.TempDir()}), "PackStaged")

	want := map[string]os.FileMode{
		"bin/staged-tool":       0o755,
		"doc/README.md":         0o644,
		"lib/staged/core.so":    0o755,
		"lua/staged/init.lua":   0o644,
		"rock_manifest":         0o644,
		"staged-1.0-1.rockspec": 0o644,
	}

	for _, archive := range []string{packed, staged} {
		zr, err := zip.OpenReader(archive)
		require.NoError(t, err)

		got := map[string]os.FileMode{}

		for _, f := range zr.File {
			got[f.Name] = f.Mode()

			assert.Equal(t, uint16(3), f.CreatorVersion>>8, "%s: made on Unix", f.Name)
			assert.True(t, f.Mode().IsRegular(), "%s: a regular file", f.Name)
			assert.Zero(t, f.ModifiedTime, "%s: no modification time", f.Name)
			assert.Zero(t, f.ModifiedDate, "%s: no modification date", f.Name)
			assert.Empty(t, f.Extra, "%s: no extra field", f.Name)
		}

		require.NoError(t, zr.Close())
		assert.Equal(t, want, got, "the modes %s records", filepath.Base(archive))
	}

	repo := t.TempDir()
	require.NoError(t, os.Rename(packed, filepath.Join(repo, filepath.Base(packed))))
	require.NoError(t, remote.MakeManifest(context.Background(), repo, remote.MakeManifestOpts{}))

	icfg := cfg
	icfg.Tree = t.TempDir()
	icfg.Servers = []string{repo}
	require.NoError(t, newClient(t, icfg).Install(context.Background(), "staged", client.InstallOpts{Deps: client.DepsNone}),
		"Install the packed rock")

	itr, err := tree.Open(icfg)
	require.NoError(t, err)

	for rel, mode := range map[string]os.FileMode{
		"bin/staged-tool":       0o755,
		"doc/README.md":         0o644,
		"staged-1.0-1.rockspec": 0o644,
	} {
		st, err := os.Stat(filepath.Join(itr.InstallDir("staged", "1.0-1"), filepath.FromSlash(rel)))
		require.NoError(t, err)
		assert.Equal(t, mode.String(), st.Mode().Perm().String(), "%s in the install dir", rel)
	}
}

// TestNativeUnpack_RestoresRecordedModes: Unpack gives a file of an archive
// made on Unix the permission bits it records, the umask, 022 here, not
// applied, as unzip does, and one of an archive made elsewhere 0666 less the
// umask.
func TestNativeUnpack_RestoresRecordedModes(t *testing.T) {
	t.Parallel()

	archive := filepath.Join(t.TempDir(), "x.rock")
	f, err := os.Create(archive) //nolint:gosec // test temp path
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	for name, mode := range map[string]os.FileMode{"bin/group-tool": 0o775, "doc/shared": 0o666, "doc/fat": 0} {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if mode != 0 {
			h.SetMode(mode)
		}

		w, err := zw.CreateHeader(h)
		require.NoError(t, err)

		_, err = w.Write([]byte(name))
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	dest := filepath.Join(t.TempDir(), "x")
	require.NoError(t, newNativeClient(t, t.TempDir(), t.TempDir()).Unpack(context.Background(), archive, dest))

	for name, want := range map[string]os.FileMode{"bin/group-tool": 0o775, "doc/shared": 0o666, "doc/fat": 0o644} {
		st, err := os.Stat(filepath.Join(dest, filepath.FromSlash(name)))
		require.NoError(t, err)
		assert.Equal(t, want.String(), st.Mode().String(), name)
	}
}
