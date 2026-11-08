package remote_test

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/luaref"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/remote"
)

// writeRepo writes files into a new directory and returns it.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()

	for n, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(content), 0o600))
	}

	return dir
}

// readFile returns the content of dir/name.
func readFile(t *testing.T, dir, name string) string {
	t.Helper()

	b, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)

	return string(b)
}

// requireFailsCleanly runs MakeManifest, requires it to fail with an error
// that wraps target (when set) and to leave dir as it was.
func requireFailsCleanly(t *testing.T, dir string, opts remote.MakeManifestOpts, target error) error {
	t.Helper()

	before := snapshot(t, dir)
	err := remote.MakeManifest(context.Background(), dir, opts)
	require.Error(t, err)

	if target != nil {
		require.ErrorIs(t, err, target)
	}

	require.Equal(t, before, snapshot(t, dir), "a failing MakeManifest changed %s", dir)

	return err
}

func TestMakeManifest_WritesManifestsAtomically(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{
		"foo-1.0-1.rockspec": mmSpec("foo", "1.0-1", "dependencies = { 'lua ~> 5.1' }"),
		"foo-1.0-1.all.rock": "x",
	})

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{}))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, e.Name())

		if strings.HasPrefix(e.Name(), "manifest") {
			info, err := e.Info()
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(), "mode of %s", e.Name())
		}
	}

	// No index.html without Index, no zip without Zip, no temporary file.
	assert.Equal(t, []string{
		"foo-1.0-1.all.rock", "foo-1.0-1.rockspec",
		"manifest", "manifest-5.1", "manifest-5.2", "manifest-5.3", "manifest-5.4",
	}, names)

	assert.Contains(t, readFile(t, dir, "manifest-5.1"), "foo = {")
	assert.Equal(t, "commands = {}\nmodules = {}\nrepository = {}\n", readFile(t, dir, "manifest-5.2"))
}

func TestMakeManifest_LuaVersions(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{"foo-1.0-1.all.rock": "x"})

	require.NoError(t, remote.MakeManifest(context.Background(), dir,
		remote.MakeManifestOpts{LuaVersions: []string{"5.1"}, Index: true}))

	_, err := os.Stat(filepath.Join(dir, "manifest-5.2"))
	require.ErrorIs(t, err, os.ErrNotExist)

	page := readFile(t, dir, "index.html")
	assert.Contains(t, page, `<a href="manifest-5.1">Lua 5.1 manifest file</a>`)
	assert.NotContains(t, page, "manifest-5.2")

	for _, bad := range [][]string{{""}, {"../5.1"}, {"5.1/x"}, {"5.1", "5.1"}} {
		_ = requireFailsCleanly(t, writeRepo(t, nil), remote.MakeManifestOpts{LuaVersions: bad}, nil)
	}
}

func TestMakeManifest_NotADirectory(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	err := remote.MakeManifest(context.Background(), filepath.Join(dir, "missing"), remote.MakeManifestOpts{})
	require.ErrorIs(t, err, os.ErrNotExist)

	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Error(t, remote.MakeManifest(context.Background(), file, remote.MakeManifestOpts{}))
}

func TestMakeManifest_Cancelled(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{"foo-1.0-1.all.rock": "x"})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := snapshot(t, dir)
	require.ErrorIs(t, remote.MakeManifest(ctx, dir, remote.MakeManifestOpts{}), context.Canceled)
	require.Equal(t, before, snapshot(t, dir))
}

// TestMakeManifest_RockspecLoopIsCancelled pins that a rockspec that never
// returns stops with the context, where upstream would hang.
func TestMakeManifest_RockspecLoopIsCancelled(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{"foo-1.0-1.rockspec": "while true do end"})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	require.ErrorIs(t, remote.MakeManifest(ctx, dir, remote.MakeManifestOpts{}), context.DeadlineExceeded)
}

func TestMakeManifest_Warn(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{
		"foo-1.0-1.rockspec": mmSpec("bar", "1.0-1", "dependencies = { 'lua >= 5.4' }"),
	})

	var warnings []string

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{
		LuaVersions: []string{"5.1", "5.2"},
		Warn:        func(msg string) { warnings = append(warnings, msg) },
	}))

	// Once per Lua version, as upstream prints it; the version stays listed.
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "Error loading rockspec for foo 1.0-1: ")
	assert.Contains(t, warnings[0], "Inconsistency between rockspec filename (foo-1.0-1.rockspec) and its contents (bar-1.0-1.rockspec)")
	assert.Contains(t, readFile(t, dir, "manifest-5.1"), "foo = {")
}

// TestMakeManifest_RefreshesStaleManifests pins the shadowing case: a
// client reads manifest-5.1.zip, then manifest-5.1, then manifest, so a
// stale manifest-5.1 or zip next to a fresh manifest would hide the new
// rocks. MakeManifest rewrites all of them.
func TestMakeManifest_RefreshesStaleManifests(t *testing.T) {
	t.Parallel()

	const stale = "commands = {}\nmodules = {}\nrepository = {\n   old = {\n      [\"1.0-1\"] = {\n         {\n            arch = \"all\"\n         }\n      }\n   }\n}\n"

	staleZip, err := zipOf("manifest-5.1", stale)
	require.NoError(t, err)

	dir := writeRepo(t, map[string]string{
		"manifest-5.1":       stale,
		"manifest-5.1.zip":   string(staleZip),
		"new-2.0-1.all.rock": "x",
	})

	// The stale files are what a client would read before the fix.
	idx := &remote.FileRemoteIndex{Server: dir, Arch: "linux-x86_64"}
	got, err := idx.Query(context.Background(), "new", "")
	require.NoError(t, err)
	require.Empty(t, got)

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{}))

	plain := readFile(t, dir, "manifest-5.1")
	assert.Contains(t, plain, "new = {")
	assert.NotContains(t, plain, "old = {")
	assert.Equal(t, plain, unzipOnly(t, filepath.Join(dir, "manifest-5.1.zip"), "manifest-5.1"))

	for _, lv := range []string{"5.2", "5.3", "5.4"} {
		_, err := os.Stat(filepath.Join(dir, "manifest-"+lv+".zip"))
		require.ErrorIs(t, err, os.ErrNotExist, "no zip asked for %s", lv)
	}

	idx = &remote.FileRemoteIndex{Server: dir, Arch: "linux-x86_64"}
	got, err = idx.Query(context.Background(), "new", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "2.0-1", got[0].Version.Raw)
}

func TestMakeManifest_Zip(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{"foo-1.0-1.all.rock": "x"})

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{Zip: true}))

	for _, lv := range remote.DefaultLuaVersions {
		name := "manifest-" + lv
		assert.Equal(t, readFile(t, dir, name), unzipOnly(t, filepath.Join(dir, name+".zip"), name))
	}
}

func zipOf(name, content string) ([]byte, error) {
	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	w, err := zw.Create(name)
	if err != nil {
		return nil, err
	}

	if _, err := io.WriteString(w, content); err != nil {
		return nil, err
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// unzipOnly requires the zip archive at path to hold the single entry name
// and returns its content.
func unzipOnly(t *testing.T, path, name string) string {
	t.Helper()

	zr, err := zip.OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = zr.Close() }()

	require.Len(t, zr.File, 1)
	require.Equal(t, name, zr.File[0].Name)

	r, err := zr.File[0].Open()
	require.NoError(t, err)

	defer func() { _ = r.Close() }()

	b, err := io.ReadAll(r)
	require.NoError(t, err)

	return string(b)
}

// TestMakeManifest_RockContentIsNotRead pins that a rock is listed under its
// file name whatever its embedded rockspec says, as upstream lists it.
func TestMakeManifest_RockContentIsNotRead(t *testing.T) {
	t.Parallel()

	rock, err := zipOf("other-9.9-1.rockspec", mmSpec("other", "9.9-1", ""))
	require.NoError(t, err)

	files := map[string]string{"foo-1.0-1.all.rock": string(rock)}
	dir := writeRepo(t, files)

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{}))

	parsed, err := manif.Parse([]byte(readFile(t, dir, "manifest")))
	require.NoError(t, err)

	m, ok := parsed.(map[string]any)
	require.True(t, ok)

	repo, ok := m["repository"].(map[string]any)
	require.True(t, ok)
	assert.Contains(t, repo, "foo")
	assert.NotContains(t, repo, "other")

	vm := newMakeManifestVM(t)

	written, _, ok, _, err := vm.MakeManifest(luaref.Repo{Files: files})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, written[luaref.RepoDir+"/manifest"], readFile(t, dir, "manifest"))
}

func TestMakeManifest_SymlinkOutOfDir(t *testing.T) {
	t.Parallel()

	outside := t.TempDir()
	target := filepath.Join(outside, "foo-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(target, []byte(mmSpec("foo", "1.0-1", "")), 0o600))

	for _, name := range []string{"foo-1.0-1.rockspec", "foo-1.0-1.all.rock"} {
		dir := writeRepo(t, nil)
		require.NoError(t, os.Symlink(target, filepath.Join(dir, name)))

		err := requireFailsCleanly(t, dir, remote.MakeManifestOpts{Index: true}, rocks.ErrUnsafePath)
		assert.Contains(t, err.Error(), name)

		// A relative one that leaves too.
		dir = writeRepo(t, nil)
		rel, err := filepath.Rel(dir, target)
		require.NoError(t, err)
		require.NoError(t, os.Symlink(rel, filepath.Join(dir, name)))
		_ = requireFailsCleanly(t, dir, remote.MakeManifestOpts{}, rocks.ErrUnsafePath)
	}
}

// TestMakeManifest_SymlinkInsideDir pins that a link that stays inside the
// directory is followed, as the plain file would be read.
func TestMakeManifest_SymlinkInsideDir(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{"store.rock": "x"})
	require.NoError(t, os.Symlink("store.rock", filepath.Join(dir, "foo-1.0-1.all.rock")))
	require.NoError(t, os.Symlink(filepath.Join(dir, "store.rock"), filepath.Join(dir, "foo-2.0-1.all.rock")))

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{}))
	assert.Contains(t, readFile(t, dir, "manifest"), `["2.0-1"]`)
}

// TestMakeManifest_UnreadableEntries pins the strictness over upstream,
// which lists any name that parses: a listed name must be a readable regular
// file.
func TestMakeManifest_UnreadableEntries(t *testing.T) {
	t.Parallel()

	t.Run("dangling symlink", func(t *testing.T) {
		t.Parallel()

		dir := writeRepo(t, nil)
		require.NoError(t, os.Symlink("missing", filepath.Join(dir, "foo-1.0-1.all.rock")))
		_ = requireFailsCleanly(t, dir, remote.MakeManifestOpts{}, os.ErrNotExist)
	})

	t.Run("directory", func(t *testing.T) {
		t.Parallel()

		dir := writeRepo(t, nil)
		require.NoError(t, os.Mkdir(filepath.Join(dir, "foo-1.0-1.all.rock"), 0o750))
		err := requireFailsCleanly(t, dir, remote.MakeManifestOpts{}, nil)
		assert.Contains(t, err.Error(), "not a regular file")
	})

	t.Run("no read permission", func(t *testing.T) {
		t.Parallel()

		if os.Geteuid() == 0 {
			t.Skip("root reads a file without read permission")
		}

		dir := writeRepo(t, map[string]string{"foo-1.0-1.src.rock": "x"})
		require.NoError(t, os.Chmod(filepath.Join(dir, "foo-1.0-1.src.rock"), 0))
		_ = requireFailsCleanly(t, dir, remote.MakeManifestOpts{}, os.ErrPermission)
	})

	t.Run("installed rock layout", func(t *testing.T) {
		t.Parallel()

		dir := writeRepo(t, nil)
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "foo", "1.0-1"), 0o750))
		err := requireFailsCleanly(t, dir, remote.MakeManifestOpts{}, nil)
		assert.Contains(t, err.Error(), "installed rock foo 1.0-1")
	})
}

// TestMakeManifest_DotNames pins that a name starting with a dot, ".."
// among them, is not listed, as upstream's `ls` does not list it, while a
// name holding ".." inside is one path element like any other.
func TestMakeManifest_DotNames(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{
		"..-1.0-1.all.rock":   "x",
		".foo-1.0-1.all.rock": "x",
		"a..b-1.0-1.all.rock": "x",
	})

	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{Index: true}))

	m := readFile(t, dir, "manifest")
	assert.NotContains(t, m, `[".."]`)
	assert.NotContains(t, m, `[".foo"]`)
	assert.Contains(t, m, `["a..b"] = {`)

	got, err := remote.NewIndex(dir, remote.IndexOptions{}).Query(context.Background(), "a..b", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, filepath.Join(dir, "a..b-1.0-1.all.rock"), got[0].URL)
}

// TestMakeManifest_IndexFailureWritesNothing pins the difference from
// upstream's order: upstream writes the manifests and then fails in
// index.html; MakeManifest writes nothing.
func TestMakeManifest_IndexFailureWritesNothing(t *testing.T) {
	t.Parallel()

	dir := writeRepo(t, map[string]string{
		"foo-1.0-1.rockspec": "this is not Lua",
		"manifest":           "old",
	})

	_ = requireFailsCleanly(t, dir, remote.MakeManifestOpts{Index: true}, nil)
	require.NoError(t, remote.MakeManifest(context.Background(), dir, remote.MakeManifestOpts{}))
	assert.NotEqual(t, "old", readFile(t, dir, "manifest"))
}

// TestMakeManifest_IndexFindsEveryRock is the round trip through the
// indexes: every rock file of the directory resolves to itself, and Search
// reports every one, as over the manifest upstream writes for the same
// files.
func TestMakeManifest_IndexFindsEveryRock(t *testing.T) {
	t.Parallel()

	files := map[string]string{}
	maps.Copy(files, mmCases[1].files) // The wasm arches.
	maps.Copy(files, mmCases[2].files) // The mirror.

	native := writeRepo(t, files)
	require.NoError(t, remote.MakeManifest(context.Background(), native, remote.MakeManifestOpts{Index: true}))

	// The same directory indexed by upstream.
	vm := newMakeManifestVM(t)

	written, _, ok, msg, err := vm.MakeManifest(luaref.Repo{Files: files})
	require.NoError(t, err)
	require.True(t, ok, msg)

	upstream := writeRepo(t, files)
	for full, content := range written {
		require.NoError(t, os.WriteFile(
			filepath.Join(upstream, strings.TrimPrefix(full, luaref.RepoDir+"/")), []byte(content), 0o600))
	}

	// What a client reads is manifest-5.1, which leaves out metrics scm-1
	// (lua == 5.2) and checks 3.2.0-1 (lua >= 5.3).
	parsed, err := manif.Parse([]byte(written[luaref.RepoDir+"/manifest-5.1"]))
	require.NoError(t, err)

	top, ok := parsed.(map[string]any)
	require.True(t, ok)

	listed, ok := top["repository"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, listed["metrics"], "scm-1")
	require.NotContains(t, listed["checks"], "3.2.0-1")

	for _, arch := range []string{"linux-x86_64", "linux-aarch64", "macosx-aarch64"} {
		idx := remote.NewIndex(native, remote.IndexOptions{Arch: arch})

		for n := range files {
			name, version, fileArch, ok := remote.ParseRockFileName(n)
			require.True(t, ok)

			versions, _ := listed[name].(map[string]any)
			if _, ok := versions[version]; !ok {
				continue
			}

			if strings.Contains(fileArch, "-") && fileArch != arch {
				continue // A foreign binary rock is not installable here.
			}

			got, err := idx.Query(context.Background(), name, "")
			require.NoError(t, err, "query %s", name)

			i := slices.IndexFunc(got, func(r rocks.VersionedRock) bool { return r.Version.Raw == version })
			require.GreaterOrEqual(t, i, 0, "%s %s not found for %s", name, version, arch)
			_, err = os.Stat(got[i].URL)
			require.NoError(t, err, "the artifact of %s %s", name, version)
		}

		opts := remote.IndexOptions{Arch: arch}
		q := remote.Query{Substring: true}

		want, err := remote.Search(context.Background(), []string{upstream}, opts, q)
		require.NoError(t, err)

		have, err := remote.Search(context.Background(), []string{native}, opts, q)
		require.NoError(t, err)

		for i := range want {
			want[i].Server = native
			want[i].URL = native + strings.TrimPrefix(want[i].URL, upstream)
		}

		assert.Equal(t, want, have, "search for %s", arch)
		assert.NotEmpty(t, have)
	}
}
