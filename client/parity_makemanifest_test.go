//go:build integration_luaengine

package client_test

// Backend parity gate for remote.MakeManifest, the native port of
// `luarocks-admin make_manifest`, which the native backend's Admin does not
// serve: the lua backend's Admin("make_manifest", dir) indexes one copy of a
// rock server, MakeManifest with Index another, and every file of the two
// directories must be byte-equal — manifest, manifest-5.1 to 5.4,
// index.html and the rock files left alone. The host-runnable twin, against
// upstream run by internal/luaref, is remote's TestMakeManifest_MatchesUpstream.
//
// Build-tagged off by default; run with `-tags integration_luaengine`.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/remote"
)

// makeManifestSpec returns a rockspec of name and version with the extra
// top-level fields.
func makeManifestSpec(name, version, extra string) string {
	return "package = '" + name + "'\nversion = '" + version + "'\n" +
		"source = { url = 'https://example.org/" + name + "-" + version + ".tar.gz' }\n" + extra + "\n"
}

// makeManifestServer is one rock server both backends index. Its rock files
// are placeholders: make_manifest reads rockspecs only.
type makeManifestServer struct {
	name  string
	files map[string]string
	dirs  []string
	// indexFails is set when upstream writes the manifests and then fails
	// in index.html; fails when it fails before writing anything.
	indexFails, fails bool
}

var makeManifestServers = []makeManifestServer{
	{name: "wasm arches", files: map[string]string{
		"tt-wasm-0.1.0-1.linux-x86_64.rock":   "x",
		"tt-wasm-0.1.0-1.linux-aarch64.rock":  "x",
		"tt-wasm-0.1.0-1.macosx-aarch64.rock": "x",
		"tt-wasm-0.2.0-1.linux-x86_64.rock":   "x",
	}},
	{name: "mirror", files: map[string]string{
		"crud-1.4.0-1.rockspec": makeManifestSpec("crud", "1.4.0-1",
			"dependencies = { 'lua >= 5.1', 'tarantool' }\n"+
				"description = { summary = 'CRUD', detailed = [[\nRows.\n\nSee https://github.com/tarantool/crud.\n]], "+
				"homepage = 'https://github.com/tarantool/crud', license = 'BSD' }"),
		"crud-1.4.0-1.src.rock":   "x",
		"crud-1.5.0-1.rockspec":   makeManifestSpec("crud", "1.5.0-1", "dependencies = { 'lua ~> 5.1' }"),
		"crud-1.5.0-1.all.rock":   "x",
		"checks-3.1.0-1.all.rock": "x",
		"checks-3.2.0-1.rockspec": makeManifestSpec("checks", "3.2.0-1", "dependencies = { 'lua >= 5.3' }"),
		"luatest-1.0.0-1.rockspec": makeManifestSpec("luatest", "1.0.0-1",
			"dependencies = { 'lua < 5.3', platforms = { linux = { 'lua >= 5.2' } } }\n"+
				"external_dependencies = { OPENSSL = {}, platforms = { linux = { LIBZ = {} } } }"),
		"metrics-scm-1.rockspec":            makeManifestSpec("metrics", "scm-1", "dependencies = { 'lua == 5.2' }"),
		"metrics-2.0.0-1.linux-x86_64.rock": "x",
		"metrics-2.0.0-1.src.rock":          "x",
		"other-1.0-1.rockspec":              makeManifestSpec("another", "1.0-1", "dependencies = { 'lua >= 5.4' }"),
		"baddep-1.0-1.rockspec":             makeManifestSpec("baddep", "1.0-1", "dependencies = { 'lua >= 5.4', '>= 1' }"),
		"future-1.0-1.rockspec": makeManifestSpec("future", "1.0-1",
			"rockspec_format = '4.0'\ndependencies = { 'lua >= 5.4' }"),
		"ns-1.0-1.rockspec": makeManifestSpec("ns", "1.0-1",
			"rockspec_format = '3.0'\ndependencies = { 'user/lua >= 5.4' }"),
		"Upper-1.0-1.all.rock":           "x",
		"weird$v-1.0-1.src.rock":         "x",
		"multi-dash-name-1.0-1.all.rock": "x",
		`a\bar-1.0-1.all.rock`:           "x",
		".hidden-1.0-1.all.rock":         "x",
		"foo-1.0-1.rockspec.bak":         "x",
		"README":                         "x",
		"manifest-5.1":                   "stale",
	}, dirs: []string{"docs"}},
	{name: "a rockspec index.html cannot load", indexFails: true, files: map[string]string{
		"crud-1.5.0-1.rockspec": makeManifestSpec("crud", "1.5.0-1", "dependencies = { 'lua ~> 5.1' }"),
		"s-1.0-1.rockspec":      "s = ",
		"s-1.0-1.all.rock":      "x",
	}},
	{name: "an installed rock", fails: true, files: map[string]string{
		"crud-1.5.0-1.all.rock": "x",
	}, dirs: []string{"foo/1.0-1"}},
}

// TestBackendParity_MakeManifest compares the lua backend's make_manifest
// with remote.MakeManifest over the same rock servers.
func TestBackendParity_MakeManifest(t *testing.T) {
	t.Parallel()

	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		testenv.Missingf(t, "tarantool not found on PATH; make_manifest parity needs a real interpreter: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin)) // <prefix>/bin/tarantool -> <prefix>

	for _, srv := range makeManifestServers {
		t.Run(srv.name, func(t *testing.T) {
			t.Parallel()

			upstream, native, plain := t.TempDir(), t.TempDir(), t.TempDir()

			for _, dir := range []string{upstream, native, plain} {
				for n, content := range srv.files {
					require.NoError(t, os.WriteFile(filepath.Join(dir, n), []byte(content), 0o600))
				}

				for _, d := range srv.dirs {
					require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o750))
				}
			}

			r, err := client.New(rocks.Config{
				Tree:       t.TempDir(),
				WorkingDir: t.TempDir(),
				Servers:    []string{upstream},
				Tarantool: rocks.TarantoolConfig{
					Prefix:     prefix,
					IncludeDir: filepath.Join(prefix, "include", "tarantool"),
					Version:    parityTarantoolVersion,
				},
			}, client.WithBackend(client.BackendLua))
			require.NoError(t, err)

			ctx := context.Background()
			before := dirFiles(t, native)

			adminErr := r.Admin(ctx, "make_manifest", []string{upstream}, client.AdminOpts{})
			nativeErr := remote.MakeManifest(ctx, native, remote.MakeManifestOpts{Index: true})
			plainErr := remote.MakeManifest(ctx, plain, remote.MakeManifestOpts{})

			want := dirFiles(t, upstream)

			switch {
			case srv.fails:
				require.Error(t, adminErr, "lua backend make_manifest")
				require.Error(t, nativeErr, "remote.MakeManifest")
				require.Error(t, plainErr, "remote.MakeManifest without Index")
				assert.Equal(t, before, dirFiles(t, upstream), "upstream wrote files")
				assert.Equal(t, before, dirFiles(t, native), "MakeManifest wrote files")

				return
			case srv.indexFails:
				// Upstream wrote the manifests and failed in index.html;
				// MakeManifest with Index writes nothing, without it the
				// manifests upstream wrote.
				require.Error(t, adminErr, "lua backend make_manifest")
				require.Error(t, nativeErr, "remote.MakeManifest")
				require.NoError(t, plainErr, "remote.MakeManifest without Index")
				assert.Equal(t, before, dirFiles(t, native), "a failing MakeManifest wrote files")

				delete(want, "index.html")
				requireSameFiles(t, want, dirFiles(t, plain))

				return
			}

			require.NoError(t, adminErr, "lua backend make_manifest")
			require.NoError(t, nativeErr, "remote.MakeManifest")
			require.Contains(t, want, "index.html")
			require.Contains(t, want, "manifest-5.4")
			requireSameFiles(t, want, dirFiles(t, native))
		})
	}
}

// requireSameFiles requires the two directory listings to hold the same
// files with the same bytes.
func requireSameFiles(t *testing.T, want, got map[string]string) {
	t.Helper()

	for n := range want {
		assert.Equal(t, want[n], got[n], "%s differs", n)
	}

	for n := range got {
		assert.Contains(t, want, n, "MakeManifest wrote %s, upstream did not", n)
	}
}

// dirFiles returns the content of every file directly in dir, by name; a
// directory is listed with an empty content and a trailing slash.
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	out := map[string]string{}

	for _, e := range entries {
		if e.IsDir() {
			out[e.Name()+"/"] = ""

			continue
		}

		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		require.NoError(t, err)

		out[e.Name()] = string(b)
	}

	return out
}
