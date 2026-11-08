package safepath_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/safepath"
)

func TestJoin(t *testing.T) {
	t.Parallel()

	root := filepath.Join("/srv", "tree")

	for _, tc := range []struct {
		rel     string
		want    string
		entryOK bool
	}{
		{rel: "a", want: "/srv/tree/a", entryOK: true},
		{rel: "a/b/c.lua", want: "/srv/tree/a/b/c.lua", entryOK: true},
		{rel: "a/", want: "/srv/tree/a", entryOK: true},
		{rel: "./a", want: "/srv/tree/a", entryOK: true},
		{rel: "a/../b", want: "/srv/tree/b", entryOK: true},
		{rel: "модуль/файл.lua", want: "/srv/tree/модуль/файл.lua", entryOK: true},
		{rel: "..a", want: "/srv/tree/..a", entryOK: true},
		{rel: "a..", want: "/srv/tree/a..", entryOK: true},
		{rel: ".", want: "/srv/tree"},
		{rel: "a/..", want: "/srv/tree"},
		{rel: ""},
		{rel: ".."},
		{rel: "../x"},
		{rel: "a/../../x"},
		{rel: "../tree/x"},
		{rel: "/etc/passwd"},
		{rel: "/"},
	} {
		t.Run(tc.rel, func(t *testing.T) {
			t.Parallel()

			got, err := safepath.Join(root, tc.rel)
			if tc.want == "" {
				require.ErrorIs(t, err, safepath.ErrEscapes)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}

			got, err = safepath.JoinEntry(root, tc.rel)
			if !tc.entryOK {
				require.ErrorIs(t, err, safepath.ErrEscapes)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestRel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		root, path string
		want       string
	}{
		{"/srv/tree", "/srv/tree/a/b", "a/b"},
		{"/srv/tree", "/srv/tree", "."},
		{"/srv/tree/", "/srv/tree/a/", "a"},
		{"tree", "tree/a", "a"},
		{"/srv/tree", "/srv/tree/a/../b", "b"},
		{"/srv/tree", "/srv/tree/../x", ""},
		{"/srv/tree", "/srv/treex/a", ""},
		{"/srv/tree", "/srv", ""},
		{"/srv/tree", "a", ""},
		{"tree", "/srv/tree/a", ""},
	} {
		got, err := safepath.Rel(tc.root, tc.path)
		if tc.want == "" {
			require.ErrorIs(t, err, safepath.ErrEscapes, "%s in %s", tc.path, tc.root)

			continue
		}

		require.NoError(t, err, "%s in %s", tc.path, tc.root)
		assert.Equal(t, tc.want, got)
	}
}

func TestName(t *testing.T) {
	t.Parallel()

	for _, ok := range []string{"lua-cjson", "1.0-1", "scm-1", "..a", "a..", "ünïcode", "a b"} {
		require.NoError(t, safepath.Name(ok), ok)
	}

	for _, bad := range []string{"", ".", "..", "a/b", "/", "../x", `a\b`, "a/"} {
		require.ErrorIs(t, safepath.Name(bad), safepath.ErrEscapes, bad)
	}
}

// layout makes a root and a sibling outside dir, both temp dirs, and in root
// a link "out" to outside, a dir "in" and a link "inlink" to it.
func layout(t *testing.T) (root, outside string) {
	t.Helper()

	parent := t.TempDir()
	root = filepath.Join(parent, "root")
	outside = filepath.Join(parent, "outside")

	require.NoError(t, os.MkdirAll(filepath.Join(root, "in"), 0o750))
	require.NoError(t, os.MkdirAll(outside, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "out")))
	require.NoError(t, os.Symlink("in", filepath.Join(root, "inlink")))

	return root, outside
}

// assertOutsideUntouched checks that outside still holds only its victim file,
// unchanged.
func assertOutsideUntouched(t *testing.T, outside string) {
	t.Helper()

	entries, err := os.ReadDir(outside)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	body, err := os.ReadFile(filepath.Join(outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, "keep", string(body))

	st, err := os.Stat(filepath.Join(outside, "victim"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), st.Mode().Perm())
}

// TestOps_RefuseToLeaveRoot runs every operation on a path that leaves the
// root lexically and on one that leaves it through a symlink; all must fail
// and the outside dir must stay as it was.
func TestOps_RefuseToLeaveRoot(t *testing.T) {
	t.Parallel()

	root, outside := layout(t)

	for _, p := range []string{
		filepath.Join(root, "..", "outside", "victim"),
		filepath.Join(root, "out", "victim"),
		filepath.Join(outside, "victim"),
	} {
		_, err := safepath.Create(root, p, 0o644)
		require.ErrorIs(t, err, safepath.ErrEscapes, "create %s", p)

		_, err = safepath.Open(root, p)
		require.ErrorIs(t, err, safepath.ErrEscapes, "open %s", p)

		_, err = safepath.OpenFile(root, p, os.O_WRONLY|os.O_TRUNC, 0)
		require.ErrorIs(t, err, safepath.ErrEscapes, "openfile %s", p)

		require.ErrorIs(t, safepath.Chmod(root, p, 0o4777), safepath.ErrEscapes, "chmod %s", p)
		require.ErrorIs(t, safepath.Remove(root, p), safepath.ErrEscapes, "remove %s", p)
		require.ErrorIs(t, safepath.RemoveAll(root, p), safepath.ErrEscapes, "removeall %s", p)
		require.ErrorIs(t, safepath.MkdirAll(root, filepath.Join(p, "sub"), 0o750), safepath.ErrEscapes, "mkdirall %s", p)
		require.ErrorIs(t, safepath.Rename(root, filepath.Join(root, "in"), p), safepath.ErrEscapes, "rename to %s", p)
		require.ErrorIs(t, safepath.Rename(root, p, filepath.Join(root, "stolen")), safepath.ErrEscapes, "rename from %s", p)
	}

	// Through the link to outside the link itself is inside root, the names
	// beneath it are not.
	require.ErrorIs(t, safepath.MkdirAll(root, filepath.Join(root, "out", "new"), 0o750), safepath.ErrEscapes)
	_, err := safepath.Create(root, filepath.Join(root, "out", "new"), 0o644)
	require.ErrorIs(t, err, safepath.ErrEscapes)

	assertOutsideUntouched(t, outside)
}

// TestOps_StayInsideRoot checks the operations do what the os functions do
// inside the root, links that stay inside included.
func TestOps_StayInsideRoot(t *testing.T) {
	t.Parallel()

	root, outside := layout(t)

	nested := filepath.Join(root, "a", "b", "файл")
	require.NoError(t, safepath.MkdirAll(root, filepath.Dir(nested), 0o750))

	f, err := safepath.Create(root, nested, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("x")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, safepath.Chmod(root, nested, 0o600))
	fi, err := safepath.Lstat(root, nested)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o600), fi.Mode().Perm())

	// A link that stays inside the root is followed.
	viaLink := filepath.Join(root, "inlink", "f")
	f, err = safepath.Create(root, viaLink, 0o644)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.FileExists(t, filepath.Join(root, "in", "f"))

	// Open follows a link with an absolute target that stays inside the
	// root, which os.Root alone refuses, and still refuses one leading out.
	absLink := filepath.Join(root, "abslink")
	require.NoError(t, os.Symlink(filepath.Join(root, "in", "f"), absLink))
	f, err = safepath.Open(root, absLink)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	_, err = safepath.Open(root, filepath.Join(root, "out", "victim"))
	require.ErrorIs(t, err, safepath.ErrEscapes)

	require.NoError(t, safepath.Rename(root, nested, filepath.Join(root, "in", "moved")))
	assert.FileExists(t, filepath.Join(root, "in", "moved"))

	// The link to outside is an entry of root: removing it removes the link,
	// not what it points at.
	require.NoError(t, safepath.Remove(root, filepath.Join(root, "out")))

	require.NoError(t, safepath.RemoveAll(root, filepath.Join(root, "a")))
	assert.NoDirExists(t, filepath.Join(root, "a"))

	require.NoError(t, safepath.RemoveAll(root, filepath.Join(root, "missing")))
	require.NoError(t, safepath.RemoveAll(filepath.Join(root, "no-root"), filepath.Join(root, "no-root", "x")))

	assertOutsideUntouched(t, outside)
}

// TestMkdirAll_CreatesRoot checks MkdirAll creates a missing root, which is
// the caller's own directory.
func TestMkdirAll_CreatesRoot(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "new", "root")
	require.NoError(t, safepath.MkdirAll(root, filepath.Join(root, "a"), 0o750))
	assert.DirExists(t, filepath.Join(root, "a"))
}

// FuzzJoin checks that whatever Join accepts lies inside the root.
func FuzzJoin(f *testing.F) {
	for _, seed := range []string{"a", "../a", "a/../../b", "/abs", ".", "", "a/./b/", "..", "...", "a\x00b"} {
		f.Add(seed)
	}

	root := filepath.Join(string(filepath.Separator)+"srv", "tree")

	f.Fuzz(func(t *testing.T, rel string) {
		p, err := safepath.Join(root, rel)
		if err != nil {
			return
		}

		back, err := safepath.Rel(root, p)
		require.NoError(t, err)
		assert.False(t, back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)), back)

		if _, err := safepath.JoinEntry(root, rel); err == nil {
			assert.NotEqual(t, ".", back)
		}
	})
}
