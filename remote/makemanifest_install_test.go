package remote_test

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

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/remote"
)

// newNativeRocks returns a native client over tree whose only server is
// server.
func newNativeRocks(t *testing.T, tree, work, server string) *client.Rocks {
	t.Helper()

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Servers:    []string{server},
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
			Version:    "2.11.0",
		},
	})
	require.NoError(t, err)

	return r
}

// writeHelloSources packs hello.lua into a tarball and returns the rockspec
// of hello 1.0-1 that fetches it.
func writeHelloSources(t *testing.T) string {
	t.Helper()

	var buf bytes.Buffer

	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	body := "return { name = 'hello' }\n"

	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name: "hello/hello.lua", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}))

	_, err := tw.Write([]byte(body))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, zw.Close())

	tarball := filepath.Join(t.TempDir(), "hello.tar.gz")
	require.NoError(t, os.WriteFile(tarball, buf.Bytes(), 0o600))

	return mmSpec("hello", "1.0-1", "source.url = 'file://"+tarball+"'\n"+
		"dependencies = { 'lua >= 5.1' }\n"+
		"build = { type = 'builtin', modules = { hello = 'hello.lua' } }")
}

// TestMakeManifest_NativeInstallRoundTrip makes a rock server the way tt's
// download does — the rockspec first, then the binary rock packed from its
// install, beside binary rocks of other arches — and installs, searches and
// downloads from it with the native backend.
//
//nolint:paralleltest // testenv.Hermetic sets the environment of the process.
func TestMakeManifest_NativeInstallRoundTrip(t *testing.T) {
	testenv.Hermetic(t)

	ctx := context.Background()
	spec := writeHelloSources(t)

	// A server holding only the rockspec: the native install builds it.
	staging := writeRepo(t, map[string]string{"hello-1.0-1.rockspec": spec})
	require.NoError(t, remote.MakeManifest(ctx, staging, remote.MakeManifestOpts{}))

	work := t.TempDir()
	builder := newNativeRocks(t, t.TempDir(), work, staging)
	require.NoError(t, builder.Install(ctx, "hello", client.InstallOpts{}))

	rock, err := builder.Pack(ctx, "hello", client.PackOpts{})
	require.NoError(t, err)
	require.Equal(t, "hello-1.0-1.all.rock", filepath.Base(rock))

	packed, err := os.ReadFile(rock)
	require.NoError(t, err)

	// The server: rockspec, the pure-Lua rock and binary rocks of the wasm
	// arches other than the host's, which an install here must pass over.
	files := map[string]string{"hello-1.0-1.rockspec": spec, "hello-1.0-1.all.rock": string(packed)}

	for _, arch := range []string{"linux-x86_64", "linux-aarch64", "macosx-aarch64"} {
		if arch != remote.HostArch() {
			files["hello-1.0-1."+arch+".rock"] = "not a rock"
		}
	}

	server := writeRepo(t, files)
	require.NoError(t, remote.MakeManifest(ctx, server, remote.MakeManifestOpts{Index: true}))

	tree := t.TempDir()
	r := newNativeRocks(t, tree, t.TempDir(), server)
	require.NoError(t, r.Install(ctx, "hello", client.InstallOpts{}))

	got, err := os.ReadFile(filepath.Join(tree, "share", "tarantool", "hello.lua"))
	require.NoError(t, err)
	assert.Equal(t, "return { name = 'hello' }\n", string(got))

	results, err := r.Search(ctx, "hello", client.SearchOpts{})
	require.NoError(t, err)

	arches := map[string]bool{}
	for _, res := range results {
		arches[res.Arch] = true
	}

	assert.True(t, arches["all"] && arches["rockspec"], "search sees %v", arches)

	work = t.TempDir()
	r = newNativeRocks(t, t.TempDir(), work, server)

	path, err := r.Download(ctx, "hello", client.DownloadOpts{})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(work, "hello-1.0-1.all.rock"), path)
}
