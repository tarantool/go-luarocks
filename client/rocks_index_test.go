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
	"github.com/tarantool/go-luarocks/internal/testenv"
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

// requireNotDeployed checks that the tree holds no module of r.
func requireNotDeployed(t *testing.T, tree string, r indexRock) {
	t.Helper()

	assert.NoFileExists(t, filepath.Join(tree, "share", "tarantool", r.name+".lua"))
}

// ghIndexDir is the branch directory the fake GitHub serves the index from.
const ghIndexDir = "org/rocks-repo/index"

// githubPlace places the files of an index as rocks-repo does: a rockspec
// beside the index on its branch, listed by a relative url, and a rock as
// the asset of its own release, listed by the absolute download url.
func githubPlace(gh *testenv.GitHub) func(name string, body []byte) string {
	return func(name string, body []byte) string {
		if strings.HasSuffix(name, ".rockspec") {
			gh.Put(ghIndexDir+"/"+name, body)

			return name
		}

		return gh.Release("org/rocks-repo", strings.TrimSuffix(name, ".all.rock"), name, body)
	}
}

// publishIndex serves idx as the fake GitHub's index and returns the server
// URL, the index's directory.
func publishIndex(t *testing.T, gh *testenv.GitHub, idx remote.RocksIndex) string {
	t.Helper()

	body, err := json.Marshal(idx)
	require.NoError(t, err)
	gh.Put(ghIndexDir+"/"+remote.RocksIndexFile, body)

	return gh.URL() + "/" + ghIndexDir
}

// TestNativeInstall_IndexResolvesWithoutRocks pins the point of the index's
// rockspecs: Install reads the dependencies of every rock from the rockspec
// the server publishes apart, so no rock is downloaded until the whole
// closure is resolved, and each is downloaded once, from its release page,
// through the redirect.
func TestNativeInstall_IndexResolvesWithoutRocks(t *testing.T) {
	t.Parallel()

	gh := testenv.NewGitHub(t)
	hello := indexRock{name: "hello", version: "1.0-1", deps: []string{"dep >= 1.0"}}
	dep := indexRock{name: "dep", version: "1.0-1"}
	server := publishIndex(t, gh, rocksIndex(t, githubPlace(gh), hello, dep))

	r, tree := newIndexClient(t, gh.InsecureServers())
	require.NoError(t, r.Install(context.Background(), "hello", client.InstallOpts{Servers: []string{server}}))
	requireDeployedVersion(t, tree, hello)
	requireDeployedVersion(t, tree, dep)

	var order []string

	for _, req := range gh.Requests() {
		for _, file := range []string{hello.specName(), dep.specName(), hello.rockName(), dep.rockName()} {
			if strings.HasPrefix(req, "front ") && strings.HasSuffix(req, "/"+file) {
				order = append(order, file)
			}
		}
	}

	assert.Equal(t, []string{hello.specName(), dep.specName(), dep.rockName(), hello.rockName()}, order,
		"both rockspecs are read before any rock is downloaded, and each file once: %v", gh.Requests())
	assert.False(t, slices.ContainsFunc(gh.Requests(), func(r string) bool { return strings.Contains(r, "manifest") }),
		"a server with an index has no manifest probed")
}

// TestNativeInstall_IndexChecksums pins that every file is checked against
// the digest the index lists before anything uses it: a rockspec that does
// not match fails the install before any rock is fetched, and a rock that
// does not match fails it before it is deployed.
func TestNativeInstall_IndexChecksums(t *testing.T) {
	t.Parallel()

	hello := indexRock{name: "hello", version: "1.0-1"}

	for name, tc := range map[string]struct {
		edit func(v *remote.IndexVersion)
		file string
	}{
		"rockspec": {edit: func(v *remote.IndexVersion) { v.Rockspec.SHA256 = sha256Of([]byte("other")) }, file: hello.specName()},
		"rock":     {edit: func(v *remote.IndexVersion) { v.Artifacts[0].SHA256 = sha256Of([]byte("other")) }, file: hello.rockName()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gh := testenv.NewGitHub(t)
			idx := rocksIndex(t, githubPlace(gh), hello)
			v := idx.Repository["hello"]["1.0-1"]
			tc.edit(&v)
			idx.Repository["hello"]["1.0-1"] = v
			server := publishIndex(t, gh, idx)

			r, tree := newIndexClient(t, gh.InsecureServers())
			err := r.Install(context.Background(), "hello", client.InstallOpts{Servers: []string{server}})
			require.ErrorIs(t, err, rocks.ErrChecksumMismatch)
			require.ErrorContains(t, err, tc.file)
			require.ErrorContains(t, err, sha256Of([]byte("other")))
			requireNotDeployed(t, tree, hello)

			if name == "rockspec" {
				assert.False(t, gh.Requested(hello.rockName()), "no rock is fetched past a bad rockspec")
			}
		})
	}
}

// TestNativeInstall_IndexBundledRockspec pins that a rock installs only when
// it bundles the very rockspec its server publishes for it, the one its
// dependencies were resolved from.
func TestNativeInstall_IndexBundledRockspec(t *testing.T) {
	t.Parallel()

	gh := testenv.NewGitHub(t)
	hello := indexRock{name: "hello", version: "1.0-1"}
	idx := rocksIndex(t, githubPlace(gh), hello)

	other := append(hello.spec(), []byte("-- edited\n")...)
	gh.Put(ghIndexDir+"/"+hello.specName(), other)

	v := idx.Repository["hello"]["1.0-1"]
	v.Rockspec.SHA256 = sha256Of(other)
	idx.Repository["hello"]["1.0-1"] = v
	server := publishIndex(t, gh, idx)

	r, tree := newIndexClient(t, gh.InsecureServers())
	err := r.Install(context.Background(), "hello", client.InstallOpts{Servers: []string{server}})
	require.ErrorContains(t, err, hello.rockName()+" bundles a rockspec other than the one its server publishes")
	requireNotDeployed(t, tree, hello)
}

// TestNativeInstall_Pins pins InstallOpts.Artifacts, a lock's digests: the
// file chosen must be pinned under its arch and download with the pinned
// digest, which the server's must agree with — against an index, and against
// a server that publishes no digests at all.
func TestNativeInstall_Pins(t *testing.T) {
	t.Parallel()

	hello := indexRock{name: "hello", version: "1.0-1"}
	rock := hello.rock(t)
	other := sha256Of([]byte("other"))

	classic := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(classic, hello.rockName()), rock, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(classic, "manifest"),
		[]byte(`repository = { hello = { ["1.0-1"] = { { arch = "all" } } } }`), 0o600))

	for name, tc := range map[string]struct {
		classic bool
		pins    map[string]string
		want    error
		// early is set when the pin fails before anything is fetched.
		early bool
	}{
		"index, pinned":          {pins: map[string]string{"all": strings.ToUpper(sha256Of(rock))}},
		"index, not pinned":      {pins: map[string]string{"linux-x86_64": sha256Of(rock)}, want: rocks.ErrArtifactNotPinned, early: true},
		"index, pin disagrees":   {pins: map[string]string{"all": other}, want: rocks.ErrChecksumMismatch, early: true},
		"index, rockspec pinned": {pins: map[string]string{"all": sha256Of(rock), "rockspec": other}, want: rocks.ErrChecksumMismatch, early: true},
		"classic, pinned":        {classic: true, pins: map[string]string{"all": sha256Of(rock)}},
		"classic, pin differs":   {classic: true, pins: map[string]string{"all": other}, want: rocks.ErrChecksumMismatch},
		"classic, not pinned":    {classic: true, pins: map[string]string{}, want: rocks.ErrArtifactNotPinned},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gh := testenv.NewGitHub(t)

			server := classic
			if !tc.classic {
				server = publishIndex(t, gh, rocksIndex(t, githubPlace(gh), hello))
			}

			r, tree := newIndexClient(t, gh.InsecureServers())
			err := r.Install(context.Background(), "hello", client.InstallOpts{
				Version: "1.0-1", Servers: []string{server}, Artifacts: tc.pins,
			})

			if tc.want == nil {
				require.NoError(t, err)
				requireDeployedVersion(t, tree, hello)

				return
			}

			require.ErrorIs(t, err, tc.want)
			requireNotDeployed(t, tree, hello)

			if tc.early {
				assert.False(t, gh.Requested(hello.specName()) || gh.Requested(hello.rockName()),
					"nothing is fetched past a pin that fails: %v", gh.Requests())
			}
		})
	}
}

// TestLuaInstall_PinsNotImplemented pins that the lua backend, which checks
// no digest, refuses pins rather than install unverified.
func TestLuaInstall_PinsNotImplemented(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir(), WorkingDir: t.TempDir()}, client.WithBackend(client.BackendLua))
	require.NoError(t, err)

	err = r.Install(context.Background(), "hello", client.InstallOpts{Artifacts: map[string]string{}})
	require.ErrorIs(t, err, rocks.ErrNotImplemented)
}

// TestNativeDownload_IndexChecksum pins Download against an index: the rock
// is saved under its own name, not the redirect's, once it has the digest
// the index lists, and one that does not is not saved at all.
func TestNativeDownload_IndexChecksum(t *testing.T) {
	t.Parallel()

	hello := indexRock{name: "hello", version: "1.0-1"}

	gh := testenv.NewGitHub(t)
	good := publishIndex(t, gh, rocksIndex(t, githubPlace(gh), hello))

	r, _ := newIndexClient(t, gh.InsecureServers())
	got, err := r.Download(context.Background(), "hello", client.DownloadOpts{Servers: []string{good}})
	require.NoError(t, err)
	assert.Equal(t, hello.rockName(), filepath.Base(got))
	requireFile(t, got, string(hello.rock(t)))

	bad := testenv.NewGitHub(t)
	idx := rocksIndex(t, githubPlace(bad), hello)
	v := idx.Repository["hello"]["1.0-1"]
	v.Artifacts[0].SHA256 = sha256Of([]byte("other"))
	idx.Repository["hello"]["1.0-1"] = v

	r, _ = newIndexClient(t, bad.InsecureServers())
	got, err = r.Download(context.Background(), "hello", client.DownloadOpts{Servers: []string{publishIndex(t, bad, idx)}})
	require.ErrorIs(t, err, rocks.ErrChecksumMismatch)
	assert.Empty(t, got)
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
