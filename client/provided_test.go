package client_test

// Tests for the `tarantool` rock the VM provides on the native backend,
// derived from Config.Tarantool.Version, which tt fills with the tail of
// `tarantool --version`.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// tarantoolVersions are the forms tt passes as Config.Tarantool.Version, each
// with the version of the tarantool rock it provides.
var tarantoolVersions = []struct{ version, provided string }{
	{"3.9.0-0-g1a2b3c4", "3.9.0-1"},
	{"2.11.1-0-g57f5f6e-r579", "2.11.1-1"},
	{"3.3.0-entrypoint-31-g9eb3974", "3.3.0-1"},
	{"3.9.0", "3.9.0-1"},
	{"3.9.0-1", "3.9.0-1"},
}

// needsTarantoolRepo is a local rock server holding needstt 1.0-1, which
// depends on dep.
func needsTarantoolRepo(t *testing.T, dep string) string {
	t.Helper()

	repo := t.TempDir()
	writeTarballRock(t, repo, "needstt", "dependencies = { '"+dep+"' }", "", nil)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(`commands = {}
modules = {}
repository = { needstt = { ["1.0-1"] = { { arch = "rockspec" } } } }
`), 0o600))

	return repo
}

// TestNativeInstall_ProvidedTarantoolVersion — native Install provided
// tarantool at Config.Tarantool.Version + "-1" and dropped the parse error,
// so every `tarantool --version` tail tt passes ("3.9.0-0-g1a2b3c4", …) and
// a "3.9.0-1" provided nothing and a dependency on tarantool never resolved.
// Each form must provide tarantool at its x.y.z part.
func TestNativeInstall_ProvidedTarantoolVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range tarantoolVersions {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()

			tree := t.TempDir()
			r := newClient(t, rocks.Config{
				Tree:       tree,
				WorkingDir: t.TempDir(),
				Servers:    []string{needsTarantoolRepo(t, "tarantool == "+tc.provided)},
				Tarantool: rocks.TarantoolConfig{
					Prefix:  filepath.Join(t.TempDir(), "no-such-prefix"),
					Version: tc.version,
				},
			})

			require.NoError(t, r.Install(context.Background(), "needstt", client.InstallOpts{}),
				"the provided tarantool must satisfy tarantool == %s", tc.provided)
			assert.FileExists(t, filepath.Join(tree, "share", "tarantool", "needstt.lua"))
		})
	}
}

// TestNativeSearch_ProvidedTarantoolMatchesInstall — native Search cut the
// version at its first dash and provided nothing for a bare "3.9.0", which
// Install provides: the two disagreed on what the VM provides. Search must
// report the version Install resolves against.
func TestNativeSearch_ProvidedTarantoolMatchesInstall(t *testing.T) {
	t.Parallel()

	for _, tc := range tarantoolVersions {
		t.Run(tc.version, func(t *testing.T) {
			t.Parallel()

			r := newClient(t, rocks.Config{
				Tree:       t.TempDir(),
				WorkingDir: t.TempDir(),
				Servers:    []string{newSearchServer(t, searchRepoManifest)},
				Tarantool: rocks.TarantoolConfig{
					Prefix:  filepath.Join(t.TempDir(), "no-such-prefix"),
					Version: tc.version,
				},
			})

			found, err := r.Search(context.Background(), "tarantool", client.SearchOpts{})
			require.NoError(t, err, "Search")
			assert.Equal(t, []string{"tarantool " + tc.provided + " installed"}, searchTriples(found))
		})
	}
}

// TestNative_UnreadableTarantoolVersionIsLogged — a Tarantool.Version with no
// x.y.z part that parses provides no tarantool rock, and says so in the log
// instead of dropping the error: a dependency on tarantool fails, Search
// lists no tarantool, and an operation that never needs it is unaffected.
func TestNative_UnreadableTarantoolVersionIsLogged(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"-3.9.0", "3.9.0+local-0-g1a2b3c4"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			var logs bytes.Buffer

			r := newClient(t, rocks.Config{
				Tree:       t.TempDir(),
				WorkingDir: t.TempDir(),
				Servers:    []string{needsTarantoolRepo(t, "tarantool")},
				Logger:     slog.New(slog.NewTextHandler(&logs, nil)),
				Tarantool: rocks.TarantoolConfig{
					Prefix:  filepath.Join(t.TempDir(), "no-such-prefix"),
					Version: version,
				},
			})

			require.Error(t, r.Install(context.Background(), "needstt", client.InstallOpts{}),
				"no tarantool rock is provided")
			assert.Contains(t, logs.String(), "level=WARN msg=\"rocks: the VM provides no tarantool rock\"")
			assert.Contains(t, logs.String(), version)

			found, err := r.Search(context.Background(), "tarantool", client.SearchOpts{})
			require.NoError(t, err, "Search")
			assert.Empty(t, found, "no tarantool rock is listed")
		})
	}
}
