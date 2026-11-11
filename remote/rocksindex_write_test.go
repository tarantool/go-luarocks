package remote_test

// Tests for the writing side of a tt-rocks-index.json: Add, Write and
// Validate.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/remote"
)

func artifact(arch, url, body string) remote.IndexArtifact {
	return remote.IndexArtifact{Arch: arch, URL: url, SHA256: digestOf(body), Size: int64(len(body))}
}

// TestRocksIndex_WriteIsDeterministic pins that the bytes Write produces
// depend on the index alone, whatever order it was built in, and read back
// to the same index.
func TestRocksIndex_WriteIsDeterministic(t *testing.T) {
	t.Parallel()

	const release, signed = "https://example.org/dl/", "?asset=1&sig=2"

	linux := artifact("linux-x86_64", release+"hello-1.0-1.linux-x86_64.rock"+signed, "linux")
	macos := artifact("macosx-aarch64", release+"hello-1.0-1.macosx-aarch64.rock"+signed, "macos")
	world := remote.IndexVersion{Rockspec: &remote.IndexFile{URL: "world-2.0-1.rockspec", SHA256: digestOf("w")}}

	build := func(arts []remote.IndexArtifact, worldFirst bool) []byte {
		idx := remote.NewRocksIndex()
		add := []func(){
			func() { require.NoError(t, idx.Add("hello", "1.0-1", remote.IndexVersion{Artifacts: arts})) },
			func() { require.NoError(t, idx.Add("world", "2.0-1", world)) },
		}

		if worldFirst {
			add[0], add[1] = add[1], add[0]
		}

		for _, f := range add {
			f()
		}

		var buf bytes.Buffer
		require.NoError(t, idx.Write(&buf))

		return buf.Bytes()
	}

	first := build([]remote.IndexArtifact{linux, macos}, false)
	assert.Equal(t, string(first), string(build([]remote.IndexArtifact{macos, linux}, true)))
	assert.True(t, bytes.HasSuffix(first, []byte("}\n")), "a final newline")
	assert.Contains(t, string(first), signed, "no HTML escaping")
	assert.Less(t, strings.Index(string(first), "linux-x86_64"), strings.Index(string(first), "macosx-aarch64"),
		"artifacts sorted by arch")

	idx, err := remote.ReadRocksIndex(bytes.NewReader(first))
	require.NoError(t, err)

	var again bytes.Buffer
	require.NoError(t, idx.Write(&again))
	assert.Equal(t, string(first), again.String(), "a written index reads back to itself")
}

func TestRocksIndex_WriteRefusesUnknownFormat(t *testing.T) {
	t.Parallel()

	for _, format := range []int{0, remote.RocksIndexFormat + 1} {
		err := (&remote.RocksIndex{Format: format}).Write(&bytes.Buffer{})
		require.ErrorContains(t, err, "not one this package writes", "format %d", format)
	}
}

// TestRocksIndex_Add pins what Add refuses — a version listed already,
// however spelled, and an entry against the rules — and that it keeps a
// copy of what it was given.
func TestRocksIndex_Add(t *testing.T) {
	t.Parallel()

	idx := &remote.RocksIndex{Format: remote.RocksIndexFormat}
	v := remote.IndexVersion{Artifacts: []remote.IndexArtifact{artifact("all", "hello-1.0-1.all.rock", "x")}}
	v.Artifacts[0].SHA256 = strings.ToUpper(v.Artifacts[0].SHA256)
	require.NoError(t, idx.Add("hello", "1.0-1", v), "an index without a repository takes a version")

	v.Artifacts[0].URL = "changed-afterwards.rock"
	assert.Equal(t, "hello-1.0-1.all.rock", idx.Repository["hello"]["1.0-1"].Artifacts[0].URL, "Add keeps a copy")
	assert.Equal(t, digestOf("x"), idx.Repository["hello"]["1.0-1"].Artifacts[0].SHA256, "digests are lowercased")

	same := remote.IndexVersion{Artifacts: []remote.IndexArtifact{artifact("all", "hello-1.0.0-1.all.rock", "x")}}
	require.ErrorIs(t, idx.Add("hello", "1.0.0-1", same), remote.ErrDuplicateVersion, "the same version spelled otherwise")

	for name, bad := range map[string]remote.IndexVersion{
		"no files":   {},
		"plain http": {Artifacts: []remote.IndexArtifact{artifact("all", "http://rocks.example/hello-2.0-1.all.rock", "x")}},
		"wrong name": {Artifacts: []remote.IndexArtifact{artifact("all", "hello-2.0-1.src.rock", "x")}},
	} {
		require.Error(t, idx.Add("hello", "2.0-1", bad), name)
	}

	assert.Len(t, idx.Repository["hello"], 1, "nothing refused was added")
}

// TestRocksIndex_ValidateFetches pins Validate's online check against a
// GitHub-like server: every listed file is fetched, through the release
// redirect, and every problem is reported at once, naming its entry.
func TestRocksIndex_ValidateFetches(t *testing.T) {
	t.Parallel()

	gh := testenv.NewGitHub(t)
	server := gh.URL() + "/" + indexDir
	gh.Put(indexDir+"/hello-1.0-1.rockspec", []byte("spec"))

	idx := remote.NewRocksIndex()
	require.NoError(t, idx.Add("hello", "1.0-1", remote.IndexVersion{
		Rockspec: &remote.IndexFile{URL: "hello-1.0-1.rockspec", SHA256: digestOf("spec")},
		Artifacts: []remote.IndexArtifact{
			artifact("all", gh.Release("org/rocks-repo", "hello-1.0-1", "hello-1.0-1.all.rock", []byte("rock")), "rock"),
		},
	}))

	opts := remote.ValidateOptions{Fetch: true, Base: server, InsecureServers: gh.InsecureServers()}
	require.NoError(t, idx.Validate(context.Background(), opts))
	assert.True(t, gh.Requested("hello-1.0-1.all.rock"))
	assert.True(t, gh.Requested("hello-1.0-1.rockspec"))

	require.NoError(t, idx.Add("bad", "1.0-1", remote.IndexVersion{
		Rockspec: &remote.IndexFile{URL: "bad-1.0-1.rockspec", SHA256: digestOf("missing")},
		Artifacts: []remote.IndexArtifact{
			artifact("all", gh.Release("org/rocks-repo", "bad-1.0-1", "bad-1.0-1.all.rock", []byte("other")), "rock"),
			{
				Arch:   "src",
				URL:    gh.Release("org/rocks-repo", "bad-1.0-1", "bad-1.0-1.src.rock", []byte("src")),
				SHA256: digestOf("src"),
				Size:   99,
			},
		},
	}))

	err := idx.Validate(context.Background(), opts)
	require.ErrorContains(t, err, "bad 1.0-1: all: ")
	require.ErrorContains(t, err, "has sha256 "+digestOf("other")+", the index lists "+digestOf("rock"))
	require.ErrorContains(t, err, "bad 1.0-1: rockspec: GET "+server+"/bad-1.0-1.rockspec: status 404")
	require.ErrorContains(t, err, "bad 1.0-1: src: ")
	require.ErrorContains(t, err, "has 3 bytes, the index lists 99")
	assert.NotContains(t, err.Error(), "hello", "the good rock has no problem")
}

// TestRocksIndex_ValidateFetchesLocal pins Validate against an index in a
// local directory, its relative urls resolving there, and that a relative
// url with no base to resolve it is a problem, not a guess.
func TestRocksIndex_ValidateFetchesLocal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello-1.0-1.all.rock"), []byte("rock"), 0o600))

	idx := remote.NewRocksIndex()
	require.NoError(t, idx.Add("hello", "1.0-1", remote.IndexVersion{
		Artifacts: []remote.IndexArtifact{artifact("all", "hello-1.0-1.all.rock", "rock")},
	}))

	for _, base := range []string{dir, "file://" + dir} {
		require.NoError(t, idx.Validate(context.Background(), remote.ValidateOptions{Fetch: true, Base: base}), base)
	}

	err := idx.Validate(context.Background(), remote.ValidateOptions{Fetch: true})
	require.ErrorContains(t, err, "no base to resolve it against")
	require.NoError(t, idx.Validate(context.Background(), remote.ValidateOptions{}), "offline, nothing is resolved")
}
