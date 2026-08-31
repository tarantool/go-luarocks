package client_test

// Tests that the facade's server wiring dispatches on each configured
// server's form, so a rock server may be a local directory. The index itself
// is covered in package remote; what these pin is the connection — that
// client.New and the native engine's per-install Servers override both build
// their index through that dispatch rather than assuming HTTP.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// installTestRepo is the on-disk twin of installTestServer: a directory in
// the shape LuaRocks serves offline (`luarocks --only-server=<dir>`), holding
// a manifest that lists hello (depending on dep) plus both rockspecs. The
// rock sources stay file:// trees elsewhere on disk, exactly as in the HTTP
// fixture, so the only difference between the two is where the *index* reads
// from.
func installTestRepo(t *testing.T) string {
	t.Helper()

	helloSrc := t.TempDir()
	depSrc := t.TempDir()
	stageRockSource(t, helloSrc, "hello", "1.0-1", "dependencies = { 'dep >= 1.0', 'tarantool' }")
	stageRockSource(t, depSrc, "dep", "1.0-1", "")

	repo := t.TempDir()
	copyRockspec(t, filepath.Join(helloSrc, "hello-1.0-1.rockspec"), repo)
	copyRockspec(t, filepath.Join(depSrc, "dep-1.0-1.rockspec"), repo)

	const manifest = `commands = {}
modules = {}
repository = {
   hello = { ["1.0-1"] = { { arch = "rockspec" } } },
   dep = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`

	require.NoError(t,
		os.WriteFile(filepath.Join(repo, "manifest"), []byte(manifest), 0o600), "write manifest")

	return repo
}

func copyRockspec(t *testing.T, src, destDir string) {
	t.Helper()

	body, err := os.ReadFile(src)
	require.NoError(t, err, "read %q", src)

	require.NoError(t,
		os.WriteFile(filepath.Join(destDir, filepath.Base(src)), body, 0o600), "copy %q", src)
}

// deadServer is an HTTP rock server that has no manifest, so every index
// probe against it fails.
func deadServer(t *testing.T) string {
	t.Helper()

	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)

	return srv.URL
}

// requireDeployed asserts the rock modules reached the tree, which is what
// proves the install ran end to end rather than merely resolving.
func requireDeployed(t *testing.T, tree string) {
	t.Helper()

	for _, mod := range []string{"hello.lua", "dep.lua"} {
		_, err := os.Stat(filepath.Join(tree, "share", "tarantool", mod))
		require.NoError(t, err, "deployed module %s missing", mod)
	}
}

// TestNativeInstall_LocalDirectoryServerOverride pins the per-install
// override site: InstallOpts.Servers holding a directory must install from
// it. Both accepted spellings are exercised, since the bare path is the form
// `luarocks --only-server` takes and the one most easily mistaken for a host.
func TestNativeInstall_LocalDirectoryServerOverride(t *testing.T) {
	t.Parallel()

	repo := installTestRepo(t)

	for _, form := range []struct{ name, server string }{
		{name: "bare path", server: repo},
		{name: "file URL", server: "file://" + repo},
	} {
		t.Run(form.name, func(t *testing.T) {
			t.Parallel()

			tree := t.TempDir()
			r := newNativeClient(t, tree, t.TempDir())

			err := r.Install(context.Background(), "hello",
				client.InstallOpts{Servers: []string{form.server}, Deps: client.DepsAll})
			require.NoError(t, err, "Install hello from %s", form.server)
			requireDeployed(t, tree)
		})
	}
}

// TestNew_LocalDirectoryServerFromConfig pins the other construction site:
// the index client.New builds from Config.Servers, reached by an Install that
// passes no override at all.
func TestNew_LocalDirectoryServerFromConfig(t *testing.T) {
	t.Parallel()

	repo := installTestRepo(t)
	tree := t.TempDir()

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: t.TempDir(),
		Servers:    []string{repo},
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
			Version:    "2.11.0",
		},
	})
	require.NoError(t, err, "New")

	require.NoError(t,
		r.Install(context.Background(), "hello", client.InstallOpts{Deps: client.DepsAll}),
		"Install hello from the configured directory server")
	requireDeployed(t, tree)
}

// TestNew_MixedServerListPrefersFirst — a directory and an HTTP server in one
// Config.Servers list are consulted in configuration order. The HTTP member
// here serves nothing, so reaching the rock at all proves the local member
// was queried and that a broken sibling did not abort the list.
func TestNew_MixedServerListPrefersFirst(t *testing.T) {
	t.Parallel()

	repo := installTestRepo(t)
	tree := t.TempDir()
	dead := deadServer(t)

	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: t.TempDir(),
		Servers:    []string{dead, repo},
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/test",
			IncludeDir: "/opt/test/include",
			Version:    "2.11.0",
		},
	})
	require.NoError(t, err, "New")

	require.NoError(t,
		r.Install(context.Background(), "hello", client.InstallOpts{Deps: client.DepsAll}),
		"a dead HTTP server ahead of the local mirror must not abort the query")
	requireDeployed(t, tree)
}

// TestNew_NoServersStillErrors — the wiring must not turn "nothing is
// configured" into "no such rock". An empty Config.Servers previously errored
// out of HTTPRemoteIndex; it must keep erroring through the aggregate.
func TestNew_NoServersStillErrors(t *testing.T) {
	t.Parallel()

	r := newNativeClient(t, t.TempDir(), t.TempDir())

	err := r.Install(context.Background(), "hello", client.InstallOpts{Deps: client.DepsNone})
	require.Error(t, err, "an install with no servers configured must fail loudly")
	assert.Contains(t, err.Error(), "no servers configured")
}

// TestNew_MissingLocalDirectoryErrors — a configured directory that is not
// there is a server failure, not a missing rock, so the message must name the
// directory rather than blaming the rock.
func TestNew_MissingLocalDirectoryErrors(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "absent")
	r := newNativeClient(t, t.TempDir(), t.TempDir())

	err := r.Install(context.Background(), "hello",
		client.InstallOpts{Servers: []string{missing}, Deps: client.DepsNone})
	require.Error(t, err, "a missing server directory must fail")
	assert.Contains(t, err.Error(), missing, "the error must name the directory")
}
