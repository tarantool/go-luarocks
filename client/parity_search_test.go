//go:build integration_luaengine

package client_test

// Backend parity gate for Search. The native backend reimplements
// `luarocks search`; the lua backend runs the real thing and parses its
// --porcelain listing. This test runs the SAME searches through both against
// one local rock server and requires the []SearchResult slices to be equal —
// element for element, in order — which is the acceptance criterion for the
// native port.
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

// parityTarantoolVersion is handed to both backends as
// Config.Tarantool.Version. It carries a revision on purpose: upstream keeps
// everything before the first dash of TT_CLI_TARANTOOL_VERSION and provides
// no `tarantool` rock at all for a version without one, so a bare "3.9.0"
// would make the --all case silently weaker.
const parityTarantoolVersion = "3.9.0-1"

// paritySearchManifest is the fixture server: two rocks, three versions, and
// every arch class a search has to tell apart — a bare rockspec, a packaged
// source rock, and a portable binary. Without the binary the --binary case
// would compare two empty slices and prove nothing.
const paritySearchManifest = `commands = {}
modules = {}
repository = {
   glrparityalpha = {
      ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" } },
      ["2.0-1"] = { { arch = "rockspec" }, { arch = "all" } },
   },
   glrparitybeta = {
      ["1.5-2"] = { { arch = "rockspec" } },
   },
}
`

// newParitySearchServer materializes the fixture as a directory, which is a
// rock server in the form `luarocks --only-server` takes and the only form
// that keeps this test offline.
func newParitySearchServer(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t,
		os.WriteFile(filepath.Join(dir, "manifest"), []byte(paritySearchManifest), 0o600),
		"write manifest")

	return dir
}

// TestBackendParity_Search compares the two backends' Search over one local
// rock server.
//
// Two things about the setup are load-bearing and neither is incidental:
//
//   - The proxy variables point at a closed port. The lua backend's
//     cfg.rocks_servers is not empty to begin with — hardcoded.lua puts
//     http://rocks.tarantool.org/ in it and --server PREPENDS rather than
//     replaces — so without this the lua side would additionally list
//     whatever that live server holds, and the test would measure the
//     internet. The downloader is curl or wget, both of which honor these.
//   - Each search gets a FRESH lua client. `--server` is applied by mutating
//     cfg.rocks_servers inside the embedded VM, and that VM outlives a single
//     dispatch, so the N-th search through one lua client searches the
//     override N times over and reports every match N times. Measured while
//     writing this test; it is a property of the lua backend, not of the
//     fixture.
func TestBackendParity_Search(t *testing.T) {
	// The lua backend runs real LuaRocks, which shells out to the configured
	// interpreter — including to learn the LuaJIT version behind the
	// `luajit`/`luabitop` rocks it reports as VM-provided. Locate a real
	// tarantool and derive its prefix; skip when there is none, since a faked
	// path would make the two backends agree for the wrong reason.
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH; search parity needs a real interpreter: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin)) // <prefix>/bin/tarantool -> <prefix>

	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		t.Setenv(v, "http://127.0.0.1:1")
	}

	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	repo := newParitySearchServer(t)

	// Both clients are handed the server the same way — through
	// SearchOpts.Servers — so the effective server list is identical: the
	// native backend prepends it to an empty Config.Servers, the lua backend
	// prepends it to the hardcoded default that the proxy above renders inert.
	newClient := func(backend client.Backend) *client.Rocks {
		t.Helper()

		r, err := client.New(rocks.Config{
			Tree:       t.TempDir(),
			WorkingDir: t.TempDir(),
			Tarantool: rocks.TarantoolConfig{
				Prefix:     prefix,
				IncludeDir: filepath.Join(prefix, "include", "tarantool"),
				Version:    parityTarantoolVersion,
			},
		}, client.WithBackend(backend))
		require.NoError(t, err, "New(backend=%v)", backend)

		return r
	}

	cases := []struct {
		name    string
		pattern string
		opts    client.SearchOpts
		// empty marks the cases that legitimately match nothing, so the
		// others are asserted to be non-empty and cannot pass vacuously.
		empty bool
	}{
		{name: "plain pattern", pattern: "glrparity"},
		{name: "exact version", pattern: "glrparityalpha", opts: client.SearchOpts{Version: "1.0-1"}},
		{name: "source only", pattern: "glrparity", opts: client.SearchOpts{Source: true}},
		{name: "binary only", pattern: "glrparity", opts: client.SearchOpts{Binary: true}},
		{name: "all", pattern: "", opts: client.SearchOpts{All: true}},
		{name: "no match", pattern: "glrnosuchrock", empty: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := c.opts
			opts.Servers = []string{repo}

			native, err := newClient(client.BackendNative).Search(context.Background(), c.pattern, opts)
			require.NoError(t, err, "native Search")

			lua, err := newClient(client.BackendLua).Search(context.Background(), c.pattern, opts)
			require.NoError(t, err, "lua Search")

			if c.empty {
				require.Empty(t, lua, "the lua backend must find nothing for this case")
			} else {
				require.NotEmpty(t, lua, "the lua backend found nothing; the case proves nothing")
			}

			require.Equal(t, lua, native, "native Search must return exactly what the lua backend does")
		})
	}
}

// TestBackendParity_Search_ProvidedRocks pins the half of the result that
// comes from no server at all: upstream appends the rocks the VM supplies,
// and the native backend must report the same names AND the same versions —
// which for luajit and luabitop means asking the interpreter rather than
// guessing, exactly as util.get_luajit_version does.
func TestBackendParity_Search_ProvidedRocks(t *testing.T) {
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		t.Skipf("tarantool not found on PATH; search parity needs a real interpreter: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin))

	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		t.Setenv(v, "http://127.0.0.1:1")
	}

	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	repo := newParitySearchServer(t)

	search := func(backend client.Backend, pattern string) []client.SearchResult {
		t.Helper()

		r, err := client.New(rocks.Config{
			Tree:       t.TempDir(),
			WorkingDir: t.TempDir(),
			Tarantool: rocks.TarantoolConfig{
				Prefix:     prefix,
				IncludeDir: filepath.Join(prefix, "include", "tarantool"),
				Version:    parityTarantoolVersion,
			},
		}, client.WithBackend(backend))
		require.NoError(t, err, "New(backend=%v)", backend)

		found, err := r.Search(context.Background(), pattern,
			client.SearchOpts{Servers: []string{repo}})
		require.NoError(t, err, "Search(backend=%v)", backend)

		return found
	}

	for _, pattern := range []string{"lua", "tarantool"} {
		t.Run(pattern, func(t *testing.T) {
			lua := search(client.BackendLua, pattern)
			require.NotEmpty(t, lua, "the lua backend must report VM-provided rocks for %q", pattern)
			require.Equal(t, lua, search(client.BackendNative, pattern))
		})
	}
}
