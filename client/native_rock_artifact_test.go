package client_test

// Tests that the native engine installs a rock from the artifact the index
// chose for it. A binary rock (.all.rock, .<arch>.rock) is deployed as built
// and a source rock (.src.rock) is built from the sources it bundles; only a
// bare rockspec sends the build to source.url. Every rockspec here points
// source.url at a host that does not resolve, so an install that went there
// instead of using the artifact fails.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
)

// unreachableSource is a source.url no install may fetch: .invalid never
// resolves (RFC 2606).
const unreachableSource = "git+https://rocks.invalid/"

// tarGz returns a gzipped tar holding files (slash-separated name → body).
func tarGz(t *testing.T, files map[string]string) string {
	t.Helper()

	var buf bytes.Buffer

	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}), "tar header %s", name)

		_, err := tw.Write([]byte(body))
		require.NoError(t, err, "tar body %s", name)
	}

	require.NoError(t, tw.Close(), "close tar")
	require.NoError(t, gz.Close(), "close gzip")

	return buf.String()
}

// rockRepo writes a directory rock server holding the given artifacts
// (file name → zip entries) and a manifest listing each under its arch.
func rockRepo(t *testing.T, manifest string, rocksByFile map[string]map[string]string) string {
	t.Helper()

	repo := t.TempDir()
	for file, entries := range rocksByFile {
		writeZip(t, filepath.Join(repo, file), entries)
	}

	require.NoError(t,
		os.WriteFile(filepath.Join(repo, "manifest"), []byte(manifest), 0o600), "write manifest")

	return repo
}

func installFrom(t *testing.T, repo, name string) string {
	t.Helper()

	tree := t.TempDir()
	r := newNativeClient(t, tree, t.TempDir())

	require.NoError(t,
		r.Install(context.Background(), name, client.InstallOpts{Servers: []string{repo}, Deps: client.DepsAll}),
		"Install %s from %s", name, repo)

	return tree
}

func requireFile(t *testing.T, path, body string) {
	t.Helper()

	got, err := os.ReadFile(path)
	require.NoError(t, err, "read %s", path)
	assert.Equal(t, body, string(got), "content of %s", path)
}

// TestNativeInstall_BinaryRockDeploysWithoutBuild pins install_binary_rock: an
// .all.rock — here of a cmake rock, which a build would need cmake and the
// sources for — is deployed from its lua/, bin/ and doc/ trees, and its
// dependency, a source rock, is built from the checkout it bundles.
func TestNativeInstall_BinaryRockDeploysWithoutBuild(t *testing.T) {
	t.Parallel()

	repo := rockRepo(t, `repository = {
   hello = { ["1.0-1"] = { { arch = "all" } } },
   dep = { ["1.0-1"] = { { arch = "src" } } },
}
`, map[string]map[string]string{
		"hello-1.0-1.all.rock": {
			"hello-1.0-1.rockspec": `package = "hello"
version = "1.0-1"
source = { url = "` + unreachableSource + `hello.git", tag = "1.0" }
dependencies = { "dep >= 1.0" }
build = { type = "cmake" }
`,
			"lua/hello.lua":      "return 'hello'\n",
			"lua/hello/util.lua": "return 'util'\n",
			"bin/hello":          "print('hello')\n",
			"doc/README.md":      "hello\n",
		},
		// LuaRocks packs a git source as the directory it was cloned into.
		"dep-1.0-1.src.rock": {
			"dep-1.0-1.rockspec": `package = "dep"
version = "1.0-1"
source = { url = "` + unreachableSource + `dep.git", tag = "1.0" }
build = { type = "builtin", modules = { dep = "dep.lua" } }
`,
			"dep/dep.lua": "return 'dep'\n",
		},
	})

	tree := installFrom(t, repo, "hello")

	lua := filepath.Join(tree, "share", "tarantool")
	requireFile(t, filepath.Join(lua, "hello.lua"), "return 'hello'\n")
	requireFile(t, filepath.Join(lua, "hello", "util.lua"), "return 'util'\n")
	requireFile(t, filepath.Join(lua, "dep.lua"), "return 'dep'\n")

	installDir := filepath.Join(lua, "rocks", "hello", "1.0-1")
	requireFile(t, filepath.Join(installDir, "doc", "README.md"), "hello\n")
	requireFile(t, filepath.Join(installDir, "bin", "hello"), "print('hello')\n")
	assert.FileExists(t, filepath.Join(installDir, "hello-1.0-1.rockspec"), "deployed rockspec")
	assert.FileExists(t, filepath.Join(installDir, "rock_manifest"), "rock_manifest")

	wrapper, err := os.ReadFile(filepath.Join(tree, "bin", "hello"))
	require.NoError(t, err, "read bin wrapper")
	assert.Contains(t, string(wrapper), filepath.Join(installDir, "bin", "hello"),
		"the launcher runs the script from the install dir")

	r := newNativeClient(t, tree, t.TempDir())
	path, ok, err := r.Which(context.Background(), "hello.util")
	require.NoError(t, err, "Which")
	assert.True(t, ok, "hello.util is indexed in the tree manifest")
	assert.Equal(t, filepath.Join(lua, "hello", "util.lua"), path)
}

// TestNativeInstall_SourceRockUnpacksBundledArchive pins build.build_rock for
// an archive source: LuaRocks packs the archive itself, which is unpacked
// before the build enters the directory it holds.
func TestNativeInstall_SourceRockUnpacksBundledArchive(t *testing.T) {
	t.Parallel()

	repo := rockRepo(t, `repository = {
   arc = { ["1.0-1"] = { { arch = "src" } } },
}
`, map[string]map[string]string{
		"arc-1.0-1.src.rock": {
			"arc-1.0-1.rockspec": `package = "arc"
version = "1.0-1"
source = { url = "https://rocks.invalid/arc-1.0.tar.gz" }
build = { type = "builtin", modules = { arc = "src/arc.lua" } }
`,
			"arc-1.0.tar.gz": tarGz(t, map[string]string{"arc-1.0/src/arc.lua": "return 'arc'\n"}),
		},
	})

	tree := installFrom(t, repo, "arc")

	requireFile(t, filepath.Join(tree, "share", "tarantool", "arc.lua"), "return 'arc'\n")
}

// TestNativeInstall_HostArchRockDeploysLib pins the binary rock built for the
// host: its lib/ tree reaches the shared lib dir as built.
func TestNativeInstall_HostArchRockDeploysLib(t *testing.T) {
	t.Parallel()

	arch := remote.HostArch()
	repo := rockRepo(t, `repository = {
   nat = { ["1.0-1"] = { { arch = "`+arch+`" } } },
}
`, map[string]map[string]string{
		"nat-1.0-1." + arch + ".rock": {
			"nat-1.0-1.rockspec": `package = "nat"
version = "1.0-1"
source = { url = "` + unreachableSource + `nat.git" }
build = { type = "builtin", modules = { nat = "nat.c" } }
`,
			"lib/nat.so": "not really ELF\n",
		},
	})

	tree := installFrom(t, repo, "nat")

	requireFile(t, filepath.Join(tree, "lib", "tarantool", "nat.so"), "not really ELF\n")
}
