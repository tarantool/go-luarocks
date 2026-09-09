//go:build integration_luaengine

package client_test

// Backend parity gate for Download. The native backend reimplements
// `luarocks download`; the lua backend runs the real thing and reports the
// file that appeared in its working directory. This test runs the SAME
// downloads through both against one local rock server and requires the same
// file — same name relative to each backend's own working directory, and the
// same bytes — which is the acceptance criterion for the native port.
//
// Build-tagged off by default; run with `-tags integration_luaengine`.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// parityDownloadManifest is the fixture server. alpha carries every arch
// class at its newest version, so the default pick has something to prefer
// over; its older version has no binary, so an exact-version case can pin the
// src-over-rockspec half of the same rule.
const parityDownloadManifest = `commands = {}
modules = {}
repository = {
   glrdlalpha = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" } },
      ["2.0-1"] = { { arch = "rockspec" }, { arch = "src" }, { arch = "all" } },
   },
   glrdlbeta = {
      ["1.5-2"] = { { arch = "rockspec" } },
   },
}
`

// parityDownloadFiles are the artifacts parityDownloadManifest advertises.
// They must really exist: a download parity test over a manifest with no
// files behind it would compare two failures.
var parityDownloadFiles = []string{
	"glrdlalpha-1.0-1.rockspec",
	"glrdlalpha-1.0-1.src.rock",
	"glrdlalpha-2.0-1.rockspec",
	"glrdlalpha-2.0-1.src.rock",
	"glrdlalpha-2.0-1.all.rock",
	"glrdlbeta-1.5-2.rockspec",
}

// newParityDownloadServer materializes the fixture as a directory, which is a
// rock server in the form `luarocks --only-server` takes and the only form
// that keeps this test offline.
func newParityDownloadServer(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "manifest"), []byte(parityDownloadManifest), 0o600),
		"write manifest")

	for _, f := range parityDownloadFiles {
		require.NoError(t,
			os.WriteFile(filepath.Join(dir, f), []byte("body of "+f+"\n"), 0o600),
			"write artifact %s", f)
	}

	return dir
}

// TestBackendParity_Download compares the two backends' Download over one
// local rock server.
//
// The setup discipline is TestBackendParity_Search's and is load-bearing for
// the same two reasons: the proxy variables point at a closed port so the lua
// backend cannot reach the hardcoded rocks.tarantool.org that `--server`
// merely prepends to, and each download gets a FRESH lua client because
// `--server` accumulates inside the embedded VM across dispatches.
//
// A third thing is specific to this test: the two clients get DIFFERENT
// working directories, because the download lands in one and the comparison
// is of the path relative to it. Sharing one would let the second backend
// find the first's file already there — and the lua backend, which can only
// diff the directory, would then report no new file at all.
func TestBackendParity_Download(t *testing.T) {
	// The lua backend runs real LuaRocks, which shells out to the configured
	// interpreter. Locate a real tarantool and derive its prefix; skip when
	// there is none, since a faked path would make the two backends agree for
	// the wrong reason.
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH; download parity needs a real interpreter: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin)) // <prefix>/bin/tarantool -> <prefix>

	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		t.Setenv(v, "http://127.0.0.1:1")
	}

	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	repo := newParityDownloadServer(t)

	// Each call gets its own working directory, returned alongside the client
	// so the caller can relativize the answer.
	newClient := func(backend client.Backend) (*client.Rocks, string) {
		t.Helper()

		work := t.TempDir()

		r, err := client.New(rocks.Config{
			Tree:       t.TempDir(),
			WorkingDir: work,
			Tarantool: rocks.TarantoolConfig{
				Prefix:     prefix,
				IncludeDir: filepath.Join(prefix, "include", "tarantool"),
				Version:    parityTarantoolVersion,
			},
		}, client.WithBackend(backend))
		require.NoError(t, err, "New(backend=%v)", backend)

		return r, work
	}

	// download runs one case through one backend and returns the answer as a
	// path relative to that backend's working directory, plus the contents of
	// every file that landed there.
	download := func(
		backend client.Backend, name string, opts client.DownloadOpts,
	) (string, map[string]string) {
		t.Helper()

		r, work := newClient(backend)
		opts.Servers = []string{repo}

		got, err := r.Download(context.Background(), name, opts)
		require.NoError(t, err, "Download(backend=%v, %q)", backend, name)

		rel, err := filepath.Rel(work, got)
		require.NoError(t, err, "the returned path must be inside the working directory")

		entries, err := os.ReadDir(work)
		require.NoError(t, err, "read the working directory")

		landed := map[string]string{}

		for _, ent := range entries {
			body, err := os.ReadFile(filepath.Join(work, ent.Name()))
			require.NoError(t, err, "read %s", ent.Name())

			landed[ent.Name()] = string(body)
		}

		require.NotEmpty(t, landed, "nothing was downloaded; the case proves nothing")

		return rel, landed
	}

	cases := []struct {
		name string
		rock string
		opts client.DownloadOpts
		// wantFiles is how many files the case must leave in the working
		// directory. Asserted against the LUA side, so a case that quietly
		// stopped downloading what it was written for cannot pass by having
		// both backends agree on nothing.
		wantFiles int
	}{
		{name: "default pick", rock: "glrdlalpha", wantFiles: 1},
		{name: "source", rock: "glrdlalpha", opts: client.DownloadOpts{Source: true}, wantFiles: 1},
		{name: "rockspec", rock: "glrdlalpha", opts: client.DownloadOpts{Rockspec: true}, wantFiles: 1},
		{name: "arch all", rock: "glrdlalpha", opts: client.DownloadOpts{Arch: "all"}, wantFiles: 1},
		{
			name: "exact version", rock: "glrdlalpha",
			opts: client.DownloadOpts{Version: "1.0-1"}, wantFiles: 1,
		},
		{name: "all", rock: "glrdlalpha", opts: client.DownloadOpts{All: true}, wantFiles: 5},
		{name: "all, single match", rock: "glrdlbeta", opts: client.DownloadOpts{All: true}, wantFiles: 1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			luaPath, luaFiles := download(client.BackendLua, c.rock, c.opts)
			require.Len(t, luaFiles, c.wantFiles, "files the lua backend downloaded")

			nativePath, nativeFiles := download(client.BackendNative, c.rock, c.opts)

			require.Equal(t, luaFiles, nativeFiles,
				"native Download must leave exactly the files the lua backend does, byte for byte")
			require.Equal(t, luaPath, nativePath,
				"native Download must return the same path the lua backend does, relative to the working dir")
		})
	}
}
