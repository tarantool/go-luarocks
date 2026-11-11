package client_test

// Tests of the native backend against rock servers that publish a
// tt-rocks-index.json: which version it chooses, what it downloads and
// what it checks. The rocks are pure-Lua binary rocks built here, each
// rockspec pointing source.url at a host that does not resolve, so an
// install that went to the sources instead of the rock fails.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/remote"
)

// indexRock is one version of a pure-Lua rock for an index.
type indexRock struct {
	name, version string
	// deps are the rockspec's dependency strings.
	deps   []string
	yanked bool
}

// specName and rockName are the file names the index lists the rock's files
// under.
func (r indexRock) specName() string { return r.name + "-" + r.version + ".rockspec" }
func (r indexRock) rockName() string { return r.name + "-" + r.version + ".all.rock" }

// spec is the rock's rockspec: a builtin build of one module, which returns
// the rock's version.
func (r indexRock) spec() []byte {
	quoted := make([]string, 0, len(r.deps))
	for _, d := range r.deps {
		quoted = append(quoted, fmt.Sprintf("%q", d))
	}

	return fmt.Appendf(nil, `package = %q
version = %q
source = { url = %q }
dependencies = { %s }
build = { type = "builtin", modules = { [%q] = %q } }
`, r.name, r.version, unreachableSource+r.name+".git", strings.Join(quoted, ", "), r.name, r.name+".lua")
}

// rock is the rock's .all.rock: the rockspec and the built module.
func (r indexRock) rock(t *testing.T) []byte {
	t.Helper()

	return zipEntries(t, map[string][]byte{
		r.specName():             r.spec(),
		"lua/" + r.name + ".lua": r.module(),
	})
}

// module is the file the rock deploys as share/tarantool/<name>.lua.
func (r indexRock) module() []byte { return []byte("return '" + r.version + "'\n") }

func zipEntries(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	for _, name := range slices.Sorted(maps.Keys(entries)) {
		w, err := zw.Create(name)
		require.NoError(t, err)

		_, err = w.Write(entries[name])
		require.NoError(t, err)
	}

	require.NoError(t, zw.Close())

	return buf.Bytes()
}

func sha256Of(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// rocksIndex lists rs, each file placed by place, which stores it and
// returns the url to list it under.
func rocksIndex(t *testing.T, place func(name string, body []byte) string, rs ...indexRock) remote.RocksIndex {
	t.Helper()

	idx := remote.RocksIndex{Format: remote.RocksIndexFormat, Repository: map[string]map[string]remote.IndexVersion{}}

	for _, r := range rs {
		spec, rock := r.spec(), r.rock(t)

		if idx.Repository[r.name] == nil {
			idx.Repository[r.name] = map[string]remote.IndexVersion{}
		}

		idx.Repository[r.name][r.version] = remote.IndexVersion{
			Rockspec: &remote.IndexFile{URL: place(r.specName(), spec), SHA256: sha256Of(spec)},
			Artifacts: []remote.IndexArtifact{
				{Arch: "all", URL: place(r.rockName(), rock), SHA256: sha256Of(rock), Size: int64(len(rock))},
			},
			Yanked: r.yanked,
		}
	}

	return idx
}

// indexDir writes a local rock server holding nothing but a
// tt-rocks-index.json listing rs, their files beside it.
func indexDir(t *testing.T, rs ...indexRock) string {
	t.Helper()

	dir := t.TempDir()
	idx := rocksIndex(t, func(name string, body []byte) string {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), body, 0o600))

		return name
	}, rs...)

	writeRocksIndex(t, dir, idx)

	return dir
}

func writeRocksIndex(t *testing.T, dir string, idx remote.RocksIndex) {
	t.Helper()

	body, err := json.Marshal(idx)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, remote.RocksIndexFile), body, 0o600))
}

// newIndexClient is a native client whose tree and working dir are fresh,
// trusting the insecure hosts.
func newIndexClient(t *testing.T, insecure []string) (*client.Rocks, string) {
	t.Helper()

	tree := t.TempDir()

	r, err := client.New(rocks.Config{
		Tree:            tree,
		WorkingDir:      t.TempDir(),
		InsecureServers: insecure,
		Tarantool:       rocks.TarantoolConfig{Prefix: "/opt/test", IncludeDir: "/opt/test/include", Version: "3.5.0"},
	})
	require.NoError(t, err)

	return r, tree
}

// requireDeployedVersion checks that the tree holds r's module, as r's
// version deploys it.
func requireDeployedVersion(t *testing.T, tree string, r indexRock) {
	t.Helper()

	requireFile(t, filepath.Join(tree, "share", "tarantool", r.name+".lua"), string(r.module()))
}

// TestNativeInstall_IndexYanked pins how Install treats a version the index
// yanked: a fresh install passes it over, even when it is the newest, a pin
// to it with ==, as a lock records, installs it, and a range only yanked
// versions match fails, saying why.
func TestNativeInstall_IndexYanked(t *testing.T) {
	t.Parallel()

	old := indexRock{name: "hello", version: "1.0-1"}
	yanked := indexRock{name: "hello", version: "2.0-1", yanked: true}
	server := indexDir(t, old, yanked)

	for version, want := range map[string]indexRock{"": old, ">= 1.0": old, "== 2.0-1": yanked, "2.0-1": yanked} {
		r, tree := newIndexClient(t, nil)
		require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{
			Version: version, Servers: []string{server},
		}), "Install hello %q", version)
		requireDeployedVersion(t, tree, want)
	}

	r, _ := newIndexClient(t, nil)
	err := r.Install(context.Background(), "hello", client.InstallOpts{Version: ">= 2.0", Servers: []string{server}})
	require.ErrorContains(t, err, "yanked; pin one with ==")
}

// TestNativeDownload_IndexYanked pins the same rule for Download, which
// picks one file: the newest version not yanked, unless == pins one.
func TestNativeDownload_IndexYanked(t *testing.T) {
	t.Parallel()

	old := indexRock{name: "hello", version: "1.0-1"}
	yanked := indexRock{name: "hello", version: "2.0-1", yanked: true}
	server := indexDir(t, old, yanked)

	for version, want := range map[string]indexRock{"": old, "2.0-1": yanked} {
		r, _ := newIndexClient(t, nil)

		got, err := r.Download(context.Background(), "hello", client.DownloadOpts{
			Version: version, Servers: []string{server},
		})
		require.NoError(t, err, "Download hello %q", version)
		assert.Equal(t, want.rockName(), filepath.Base(got))
	}
}
