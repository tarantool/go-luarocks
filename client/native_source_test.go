package client_test

// White-box unit tests (no network) for the native backend's source-root
// resolution: findRockspecIn (top-level-only scan) and findSourceBaseDir
// (find_base_dir descent). These guard the two regressions behind the
// "cannot fetch+build standard rocks" bug without needing luarocks.org.

import (
	"os"
	"path/filepath"
	"testing"

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
