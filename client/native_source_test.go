package client_test

// White-box unit tests (no network) for the native backend's source-root
// resolution: findRockspecIn (top-level-only scan) and findSourceBaseDir
// (find_base_dir descent). These guard the two regressions behind the
// "cannot fetch+build standard rocks" bug without needing luarocks.org.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// TestFindRockspecIn_TopLevelOnly: a .src.rock unpacks to a top-level
// rockspec PLUS a bundled source tree that may ship its own rockspecs/ dir
// (the `say` failure mode). findRockspecIn must return the top-level one and
// ignore the nested ones — not error on "multiple .rockspec".
func TestFindRockspecIn_TopLevelOnly(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "say-1.4.1-1.rockspec"), "package='say'\n")
	// Bundled source tree, with extra rockspecs nested inside — must be ignored.
	writeFile(t, filepath.Join(dir, "say", "rockspecs", "say-1.3-1.rockspec"), "package='say'\n")
	writeFile(t, filepath.Join(dir, "say", "rockspecs", "say-1.4.0-1.rockspec"), "package='say'\n")
	writeFile(t, filepath.Join(dir, "say", "src", "say.lua"), "return {}\n")

	got, err := client.FindRockspecIn(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "say-1.4.1-1.rockspec"), got)
}

// TestFindRockspecIn_None: a directory with no top-level rockspec errors.
func TestFindRockspecIn_None(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "sub", "x-1.0-1.rockspec"), "package='x'\n")

	_, err := client.FindRockspecIn(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .rockspec")
}

// TestFindRockspecIn_MultipleTopLevel still flags genuine ambiguity at the
// top level.
func TestFindRockspecIn_MultipleTopLevel(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a-1.0-1.rockspec"), "package='a'\n")
	writeFile(t, filepath.Join(dir, "b-1.0-1.rockspec"), "package='b'\n")

	_, err := client.FindRockspecIn(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple .rockspec")
}

// TestFindSourceBaseDir_SingleSubdir: the common tarball layout where the
// source unpacks into one versioned directory — descend into it.
func TestFindSourceBaseDir_SingleSubdir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	sub := filepath.Join(dir, "inspect.lua-3.1.3")
	writeFile(t, filepath.Join(sub, "inspect.lua"), "return {}\n")

	got, err := client.FindSourceBaseDir(dir, &rocks.Rockspec{})
	require.NoError(t, err)
	assert.Equal(t, sub, got)
}

// TestFindSourceBaseDir_FlatLayout: when source files already sit at the top
// level (git/file checkout, or a bare module file), do not descend.
func TestFindSourceBaseDir_FlatLayout(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "foo.lua"), "return {}\n")
	writeFile(t, filepath.Join(dir, "README.md"), "x\n")

	got, err := client.FindSourceBaseDir(dir, &rocks.Rockspec{})
	require.NoError(t, err)
	assert.Equal(t, dir, got)
}

// TestFindSourceBaseDir_InferredFromURL: glr-qht — when the archive unpacks to
// its versioned dir plus stray top-level entries, the dir inferred from
// source.url (deduce_base_dir) is preferred over the first sorted subdir.
func TestFindSourceBaseDir_InferredFromURL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A stray dir that sorts BEFORE the real one, plus a stray top-level file.
	writeFile(t, filepath.Join(dir, "aaa-extra", "junk.txt"), "x\n")
	writeFile(t, filepath.Join(dir, "foo-1.0", "foo.lua"), "return {}\n")
	writeFile(t, filepath.Join(dir, "README"), "readme\n")

	spec := &rocks.Rockspec{Source: rocks.Source{URL: "https://example.com/foo-1.0.tar.gz"}}
	got, err := client.FindSourceBaseDir(dir, spec)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "foo-1.0"), got, "should infer foo-1.0 from the URL, not pick aaa-extra")
}

// TestFindSourceBaseDir_SourceDirOverride: an explicit source.dir wins even
// when other heuristics would pick differently.
func TestFindSourceBaseDir_SourceDirOverride(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Two top-level dirs: the single-subdir heuristic would NOT fire, so the
	// override is what makes this resolve.
	writeFile(t, filepath.Join(dir, "real-src", "mod.lua"), "return {}\n")
	writeFile(t, filepath.Join(dir, "extras", "note.txt"), "x\n")

	spec := &rocks.Rockspec{Source: rocks.Source{Dir: "real-src"}}
	got, err := client.FindSourceBaseDir(dir, spec)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "real-src"), got)
}

// --- fetchSource: who decides the source root ---
//
// findSourceBaseDir above is the ARCHIVE heuristic. The tests below cover the
// decision one level up: whether that heuristic runs at all. It must not run
// over a path the backend already returned as the source root, or a checkout
// carrying a subdirectory named like the repository gets descended into.

// gitRepo initializes a git repository at dir with the given files committed,
// using go-git so no `git` binary is needed.
func gitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(dir, 0o750), "mkdir repo")

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err, "PlainInit")

	wt, err := repo.Worktree()
	require.NoError(t, err, "Worktree")

	for name, body := range files {
		writeFile(t, filepath.Join(dir, name), body)

		_, err := wt.Add(name)
		require.NoError(t, err, "Add %s", name)
	}

	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}
	_, err = wt.Commit("init", &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err, "Commit")
}

// nativeEngineFor builds a native engine over throwaway tree/work dirs.
func nativeEngineFor(t *testing.T) *client.NativeEngine {
	t.Helper()

	ne, ok := newNativeClient(t, t.TempDir(), t.TempDir()).Engine().(*client.NativeEngine)
	require.True(t, ok, "engine type")

	return ne
}

// TestFetchSource_GitCloneIsNotDescendedInto is the tarantool/checks
// regression. That repo's rockspec has no source.dir, so the archive
// heuristic infers "checks" from the URL (checks.git → checks) and finds a
// checks/ directory inside the clone — the module directory that sits next to
// CMakeLists.txt. Descending into it handed CMake a directory with no
// CMakeLists.txt and the build died there. The git backend reports the clone
// as the source root, so no descent may happen.
func TestFetchSource_GitCloneIsNotDescendedInto(t *testing.T) {
	t.Parallel()

	repo := filepath.Join(t.TempDir(), "checks")
	gitRepo(t, repo, map[string]string{
		"CMakeLists.txt": "project(checks C)\n",
		"checks.lua":     "return {}\n",
		// The trap: a subdirectory named after the repository.
		"checks/init.lua": "return {}\n",
	})

	tmp := t.TempDir()
	spec := &rocks.Rockspec{
		Package: "checks",
		Version: "3.3.0-1",
		Source:  rocks.Source{URL: "git+file://" + repo},
	}

	got, err := client.NativeFetchSource(nativeEngineFor(t), context.Background(), spec, tmp)
	require.NoError(t, err, "fetchSource")
	assert.Equal(t, filepath.Join(tmp, "checks"), got, "build root must be the clone, not clone/checks")
	assert.FileExists(t, filepath.Join(got, "CMakeLists.txt"), "the build root must hold CMakeLists.txt")
}

// TestFetchSource_LocalTreeIsNotDescendedInto: the same trap through the file
// backend's tree path, which copies a whole source tree into destDir. A
// URL-sniffing fix for the git case would leave this one broken.
func TestFetchSource_LocalTreeIsNotDescendedInto(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "checks")
	writeFile(t, filepath.Join(src, "CMakeLists.txt"), "project(checks C)\n")
	writeFile(t, filepath.Join(src, "checks", "init.lua"), "return {}\n")

	tmp := t.TempDir()
	spec := &rocks.Rockspec{
		Package: "checks",
		Version: "3.3.0-1",
		Source:  rocks.Source{URL: "file://" + src},
	}

	got, err := client.NativeFetchSource(nativeEngineFor(t), context.Background(), spec, tmp)
	require.NoError(t, err, "fetchSource")
	assert.Equal(t, tmp, got, "build root must be the copied tree, not its checks/ subdirectory")
	assert.FileExists(t, filepath.Join(got, "CMakeLists.txt"), "the build root must hold CMakeLists.txt")
}

// TestFetchSource_ArchiveStillDescends is the other half of the contract: an
// unpacked archive is NOT a source root, so the single-versioned-subdirectory
// descent must still happen.
func TestFetchSource_ArchiveStillDescends(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := "return {}\n"
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "foo-1.0/foo.lua",
		Size: int64(len(body)),
		Mode: 0o644,
	}), "tar header")
	_, err := tw.Write([]byte(body))
	require.NoError(t, err, "tar write")
	require.NoError(t, tw.Close(), "tar close")
	require.NoError(t, gz.Close(), "gzip close")

	archive := filepath.Join(t.TempDir(), "foo-1.0.tar.gz")
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o600), "write archive")

	tmp := t.TempDir()
	spec := &rocks.Rockspec{
		Package: "foo",
		Version: "1.0-1",
		Source:  rocks.Source{URL: "file://" + archive},
	}

	got, err := client.NativeFetchSource(nativeEngineFor(t), context.Background(), spec, tmp)
	require.NoError(t, err, "fetchSource")
	assert.Equal(t, filepath.Join(tmp, "foo-1.0"), got, "an unpacked archive still descends")
	assert.FileExists(t, filepath.Join(got, "foo.lua"), "the build root must hold the module")
}
