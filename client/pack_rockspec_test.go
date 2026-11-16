package client_test

// Tests of the native Pack of a rockspec, `luarocks pack <rockspec>`: the
// source rock it makes holds the rockspec and its sources, fetched at what the
// rockspec pins, and installs with the sources gone. And of how a lock that
// pins only the rockspec of a version takes such a source rock.

import (
	"archive/zip"
	"context"
	"crypto/md5" //nolint:gosec // source.md5 is an md5
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
)

// taggedRepo makes a git repository at dir whose tag "1.0" has hello.lua
// returning '1.0', and whose later commit, on the default branch, has it
// return 'head': a pack that fetched the branch rather than the tag shows.
func taggedRepo(t *testing.T, dir string) {
	t.Helper()

	repo, err := git.PlainInit(dir, false)
	require.NoError(t, err)

	wt, err := repo.Worktree()
	require.NoError(t, err)

	sig := &object.Signature{Name: "t", Email: "t@t", When: time.Unix(0, 0)}

	commit := func(body string) {
		writeFile(t, filepath.Join(dir, "hello.lua"), body)

		_, err := wt.Add("hello.lua")
		require.NoError(t, err)

		_, err = wt.Commit(body, &git.CommitOptions{Author: sig, Committer: sig})
		require.NoError(t, err)
	}

	commit("return '1.0'\n")

	head, err := repo.Head()
	require.NoError(t, err)

	_, err = repo.CreateTag("1.0", head.Hash(), nil)
	require.NoError(t, err)

	commit("return 'head'\n")
}

// helloSpec is the rockspec of hello 1.0-1 whose sources are at source,
// the source table's contents.
func helloSpec(source string) []byte {
	return fmt.Appendf(nil, `package = "hello"
version = "1.0-1"
source = { %s }
build = { type = "builtin", modules = { hello = "hello.lua" } }
`, source)
}

// zipNames lists the file entries of the zip archive at path.
func zipNames(t *testing.T, path string) []string {
	t.Helper()

	zr, err := zip.OpenReader(path)
	require.NoError(t, err)

	defer func() { _ = zr.Close() }()

	names := make([]string, 0, len(zr.File))
	for _, f := range zr.File {
		names = append(names, f.Name)
	}

	slices.Sort(names)

	return names
}

// packSpec writes spec into a fresh working directory as
// hello-1.0-1.rockspec and packs it there, returning the source rock.
func packSpec(t *testing.T, spec []byte) (string, error) {
	t.Helper()

	work := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(work, "hello-1.0-1.rockspec"), spec, 0o600))

	r := newNativeClient(t, t.TempDir(), work)

	return r.Pack(context.Background(), "hello-1.0-1.rockspec", client.PackOpts{})
}

// srcRockServer is a local rock server whose tt-rocks-index.json lists hello
// 1.0-1 as the source rock at rock and the rockspec spec beside it.
func srcRockServer(t *testing.T, rock string, spec []byte) string {
	t.Helper()

	dir := t.TempDir()
	body, err := os.ReadFile(rock)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello-1.0-1.src.rock"), body, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello-1.0-1.rockspec"), spec, 0o600))

	writeRocksIndex(t, dir, remote.RocksIndex{
		Format: remote.RocksIndexFormat,
		Repository: map[string]map[string]remote.IndexVersion{"hello": {"1.0-1": {
			Rockspec: &remote.IndexFile{URL: "hello-1.0-1.rockspec", SHA256: sha256Of(spec)},
			Artifacts: []remote.IndexArtifact{{
				Arch: "src", URL: "hello-1.0-1.src.rock", SHA256: sha256Of(body), Size: int64(len(body)),
			}},
		}}},
	})

	return dir
}

// TestNativePack_RockspecGitSource: a rockspec whose source is a git
// repository packs into a source rock of the rockspec and the clone at the
// tag, without .git, which installs once the repository is gone.
func TestNativePack_RockspecGitSource(t *testing.T) {
	t.Parallel()

	repo := filepath.Join(t.TempDir(), "hello.git")
	taggedRepo(t, repo)

	spec := helloSpec(fmt.Sprintf(`url = "git+file://%s", tag = "1.0"`, repo))

	rock, err := packSpec(t, spec)
	require.NoError(t, err)
	assert.Equal(t, "hello-1.0-1.src.rock", filepath.Base(rock))
	assert.Equal(t, []string{"hello-1.0-1.rockspec", "hello/hello.lua"}, zipNames(t, rock))

	again, err := packSpec(t, spec)
	require.NoError(t, err)

	first, err := os.ReadFile(rock)
	require.NoError(t, err)

	second, err := os.ReadFile(again)
	require.NoError(t, err)
	assert.Equal(t, first, second, "packing the same sources twice gives the same bytes")

	require.NoError(t, os.RemoveAll(repo))

	server := srcRockServer(t, rock, spec)

	r, tree := newIndexClient(t, nil)
	require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{
		Version: "1.0-1", Servers: []string{server},
	}))
	requireFile(t, filepath.Join(tree, "share", "tarantool", "hello.lua"), "return '1.0'\n")
}

// TestNativePack_RockspecArchiveSource: an archive source is packed as it
// is, under source.file, checked against source.md5, and the install
// unpacks it.
func TestNativePack_RockspecArchiveSource(t *testing.T) {
	t.Parallel()

	body := []byte(tarGz(t, map[string]string{"hello-1.0/hello.lua": "return 'archive'\n"}))
	// The URL names the file otherwise than source.file does.
	archive := filepath.Join(t.TempDir(), "download")
	require.NoError(t, os.WriteFile(archive, body, 0o600))

	sum := md5.Sum(body) //nolint:gosec // source.md5 is an md5
	spec := helloSpec(fmt.Sprintf(`url = "file://%s", file = "hello-1.0.tar.gz", md5 = %q`,
		archive, hex.EncodeToString(sum[:])))

	rock, err := packSpec(t, spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello-1.0-1.rockspec", "hello-1.0.tar.gz"}, zipNames(t, rock))

	require.NoError(t, os.Remove(archive))

	r, tree := newIndexClient(t, nil)
	require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{
		Version: "1.0-1", Servers: []string{srcRockServer(t, rock, spec)},
	}))
	requireFile(t, filepath.Join(tree, "share", "tarantool", "hello.lua"), "return 'archive'\n")

	bad := filepath.Join(t.TempDir(), "hello-1.0.tar.gz")
	require.NoError(t, os.WriteFile(bad, body, 0o600))

	_, err = packSpec(t, helloSpec(fmt.Sprintf(`url = "file://%s", md5 = "00"`, bad)))
	require.ErrorIs(t, err, rocks.ErrChecksumMismatch)
}

// TestNativePack_RockspecDirSource: a local directory is copied into the
// directory its URL names.
func TestNativePack_RockspecDirSource(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "hello-src")
	writeFile(t, filepath.Join(src, "hello.lua"), "return 'dir'\n")

	spec := helloSpec(fmt.Sprintf(`url = "file://%s"`, src))

	rock, err := packSpec(t, spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello-1.0-1.rockspec", "hello-src/hello.lua"}, zipNames(t, rock))

	require.NoError(t, os.RemoveAll(src))

	r, tree := newIndexClient(t, nil)
	require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{
		Version: "1.0-1", Servers: []string{srcRockServer(t, rock, spec)},
	}))
	requireFile(t, filepath.Join(tree, "share", "tarantool", "hello.lua"), "return 'dir'\n")
}

// TestNativePack_RockspecUnreachableSource: a source that cannot be fetched
// fails the pack and leaves no rock behind.
func TestNativePack_RockspecUnreachableSource(t *testing.T) {
	t.Parallel()

	work := t.TempDir()
	spec := helloSpec(fmt.Sprintf(`url = "git+file://%s", tag = "1.0"`, filepath.Join(work, "missing.git")))
	require.NoError(t, os.WriteFile(filepath.Join(work, "hello-1.0-1.rockspec"), spec, 0o600))

	_, err := newNativeClient(t, t.TempDir(), work).Pack(context.Background(),
		filepath.Join(work, "hello-1.0-1.rockspec"), client.PackOpts{})
	require.Error(t, err)

	entries, err := os.ReadDir(work)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	assert.Equal(t, []string{"hello-1.0-1.rockspec"}, names, "nothing is left of a failed pack")
}

// TestNativeInstall_SrcRockPinnedByRockspec: a lock that pins the rockspec
// of a version and no source rock takes a source rock that bundles that very
// rockspec, and no other.
func TestNativeInstall_SrcRockPinnedByRockspec(t *testing.T) {
	t.Parallel()

	src := filepath.Join(t.TempDir(), "hello")
	writeFile(t, filepath.Join(src, "hello.lua"), "return 'dir'\n")

	spec := helloSpec(fmt.Sprintf(`url = "file://%s"`, src))
	rock, err := packSpec(t, spec)
	require.NoError(t, err)

	body, err := os.ReadFile(rock)
	require.NoError(t, err)

	other := helloSpec(`url = "` + unreachableSource + `hello.git"`)

	// classic is a server of manifests alone, which publishes no rockspec
	// apart: the rockspec is read from the source rock.
	classic := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(classic, "hello-1.0-1.src.rock"), body, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(classic, "manifest"),
		[]byte(`repository = { hello = { ["1.0-1"] = { { arch = "src" } } } }`), 0o600))

	for name, tc := range map[string]struct {
		classic bool
		pins    map[string]string
		want    error
	}{
		"index, rockspec pinned":           {pins: map[string]string{"rockspec": sha256Of(spec)}},
		"index, rockspec pinned uppercase": {pins: map[string]string{"rockspec": strings.ToUpper(sha256Of(spec))}},
		"index, src pinned":                {pins: map[string]string{"src": sha256Of(body), "rockspec": sha256Of(spec)}},
		"index, another rockspec pinned":   {pins: map[string]string{"rockspec": sha256Of(other)}, want: rocks.ErrChecksumMismatch},
		"index, another src pinned":        {pins: map[string]string{"src": sha256Of(other), "rockspec": sha256Of(spec)}, want: rocks.ErrChecksumMismatch},
		"index, a platform pinned":         {pins: map[string]string{"linux-x86_64": sha256Of(body)}, want: rocks.ErrArtifactNotPinned},
		"classic, rockspec pinned":         {classic: true, pins: map[string]string{"rockspec": sha256Of(spec)}},
		"classic, another rockspec pinned": {classic: true, pins: map[string]string{"rockspec": sha256Of(other)}, want: rocks.ErrChecksumMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			server := classic
			if !tc.classic {
				server = srcRockServer(t, rock, spec)
			}

			r, tree := newIndexClient(t, nil)
			err := r.Install(context.Background(), "hello", client.InstallOpts{
				Version: "1.0-1", Servers: []string{server}, Artifacts: tc.pins,
			})

			if tc.want == nil {
				require.NoError(t, err)
				requireFile(t, filepath.Join(tree, "share", "tarantool", "hello.lua"), "return 'dir'\n")

				return
			}

			require.ErrorIs(t, err, tc.want)
			assert.NoFileExists(t, filepath.Join(tree, "share", "tarantool", "hello.lua"))
		})
	}
}
