//go:build unix

package safepath_test

import (
	"archive/zip"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/safepath"
)

// zipEntry is one entry of the archive writeZip writes: a mode of nil leaves
// the entry without one, as an archive made elsewhere than on Unix is.
type zipEntry struct {
	name string
	mode *fs.FileMode
}

// writeZip writes an archive of the entries, each file holding its name, to
// a fresh directory and returns its path.
func writeZip(t *testing.T, entries []zipEntry) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "x.zip")
	f, err := os.Create(path) //nolint:gosec // test temp path
	require.NoError(t, err)

	zw := zip.NewWriter(f)

	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		if e.mode != nil {
			h.SetMode(*e.mode)
		}

		w, err := zw.CreateHeader(h)
		require.NoError(t, err)

		if e.name[len(e.name)-1] != '/' {
			_, err = w.Write([]byte(e.name))
			require.NoError(t, err)
		}
	}

	require.NoError(t, zw.Close())
	require.NoError(t, f.Close())

	return path
}

// TestUnzip_Modes: an entry made on Unix gets the permission bits it records,
// the umask not applied, as Info-ZIP's unzip restores them, but for setuid,
// setgid and sticky; an entry made elsewhere gets 0666 less the umask; a
// directory is created with dirPerm less the umask, and one the archive has
// an entry of made on Unix gets its bits once its files are in, its owner
// keeping the right to list, enter and change it.
func TestUnzip_Modes(t *testing.T) {
	t.Parallel()

	archive := writeZip(t, []zipEntry{
		{"ro/", new(fs.ModeDir | 0o555)},
		{"ro/file", new(fs.FileMode(0o644))},
		{"private/", new(fs.ModeDir | 0o711)},
		{"implicit/rw", new(fs.FileMode(0o666))},
		{"implicit/x", new(fs.FileMode(0o775))},
		{"implicit/readonly", new(fs.FileMode(0o444))},
		{"implicit/suid", new(fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky | 0o755)},
		{"implicit/fat", nil},
		{"fatdir/", nil},
	})

	// What the process umask makes of 0666 and of the directory mode.
	ref := t.TempDir()
	refFile, err := os.OpenFile(filepath.Join(ref, "f"), os.O_CREATE|os.O_WRONLY, 0o666) //nolint:gosec // test temp path
	require.NoError(t, err)
	require.NoError(t, refFile.Close())
	require.NoError(t, os.Mkdir(filepath.Join(ref, "d"), 0o777))

	refMode := func(name string) fs.FileMode {
		st, err := os.Stat(filepath.Join(ref, name))
		require.NoError(t, err)

		return st.Mode().Perm()
	}

	dst := t.TempDir()
	require.NoError(t, safepath.Unzip(archive, dst, 0o777))

	for name, want := range map[string]fs.FileMode{
		"ro":                0o755,
		"ro/file":           0o644,
		"private":           0o711,
		"implicit":          refMode("d"),
		"implicit/rw":       0o666,
		"implicit/x":        0o775,
		"implicit/readonly": 0o444,
		"implicit/suid":     0o755,
		"implicit/fat":      refMode("f"),
		"fatdir":            refMode("d"),
	} {
		st, err := os.Stat(filepath.Join(dst, name))
		require.NoError(t, err, name)
		assert.Equal(t, want.String(), (st.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)).String(), name)
	}
}
