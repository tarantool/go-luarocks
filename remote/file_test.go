package remote_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // parity with upstream source.md5
	"encoding/hex"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/remote"
)

// statManifest is a byte-for-byte copy of the LuaRocks manifest in tt's
// offline fixture repository (test/integration/rocks/repo/manifest): two
// versions of `stat`, each offered only as the "all" arch. Copied rather than
// read from the tt checkout so this suite has no dependency outside its own
// module.
const statManifest = `commands = {}
modules = {}
repository = {
   stat = {
      ["0.3.1-1"] = {
         {
            arch = "all"
         }
      },
      ["0.3.2-1"] = {
         {
            arch = "all"
         }
      }
   }
}
`

const (
	statRock  = "stat"
	statOld   = "0.3.1-1"
	statNew   = "0.3.2-1"
	rockOldFn = "stat-0.3.1-1.all.rock"
	rockNewFn = "stat-0.3.2-1.all.rock"
)

// statRockspec is the rockspec bundled inside the fixture's .all.rock,
// trimmed to the fields an index consumer can observe.
const statRockspec = `package = 'stat'
version = '%s'
source = { url = 'git+https://github.com/tarantool/stat.git' }
build = { type = 'none' }
`

// newStatRepo materializes the tt fixture's shape in a temp directory: a
// LuaRocks `manifest` plus one real (zip-format) .all.rock per version, so a
// resolved URL can actually be fetched.
func newStatRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest"), statManifest)
	writeRock(t, filepath.Join(dir, rockOldFn), "stat-0.3.1-1.rockspec", statOld)
	writeRock(t, filepath.Join(dir, rockNewFn), "stat-0.3.2-1.rockspec", statNew)

	return dir
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750), "mkdir %q", filepath.Dir(path))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600), "write %q", path)
}

// writeRock writes a .rock archive — a zip under another extension, exactly
// as upstream packs them — holding a single rockspec entry.
func writeRock(t *testing.T, path, specName, version string) {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	w, err := zw.Create(specName)
	require.NoError(t, err, "zip entry")

	_, err = w.Write([]byte(strings.ReplaceAll(statRockspec, "%s", version)))
	require.NoError(t, err, "zip write")
	require.NoError(t, zw.Close(), "zip close")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600), "write %q", path)
}

// serveManifestOverHTTP is the counterpart of newStatRepo for a member that
// must be HTTP: it serves body at the bare `manifest` probe, which is the one
// probe both index implementations reach.
func serveManifestOverHTTP(t *testing.T, body string) string {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/manifest" {
			_, _ = w.Write([]byte(body))

			return
		}

		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	return srv.URL
}

// versionsOf projects a Query result down to its version strings.
func versionsOf(got []rocks.VersionedRock) []string {
	out := make([]string, 0, len(got))
	for _, r := range got {
		out = append(out, r.Version.Raw)
	}

	return out
}

// urlOf returns the resolved URL for one version of a Query result.
func urlOf(t *testing.T, got []rocks.VersionedRock, version string) string {
	t.Helper()

	for _, r := range got {
		if r.Version.Raw == version {
			return r.URL
		}
	}

	t.Fatalf("version %q not among %v", version, versionsOf(got))

	return ""
}

func TestFileRemoteIndex_ResolvesFixtureRepo(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)
	idx := &remote.FileRemoteIndex{Server: dir}

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "Query")
	assert.Equal(t, []string{statOld, statNew}, versionsOf(got), "both fixture versions")

	// The "all" arch is a ready-to-install rock, so path.make_url's default
	// branch applies: <dir>/<name>-<version>.<arch>.rock.
	assert.Equal(t, filepath.Join(dir, rockOldFn), urlOf(t, got, statOld), "URL of 0.3.1-1")
	assert.Equal(t, filepath.Join(dir, rockNewFn), urlOf(t, got, statNew), "URL of 0.3.2-1")

	for _, r := range got {
		_, err := os.Stat(r.URL)
		require.NoError(t, err, "resolved artifact %q must exist on disk", r.URL)
	}
}

func TestFileRemoteIndex_PicksNewestUnderConstraint(t *testing.T) {
	t.Parallel()

	idx := &remote.FileRemoteIndex{Server: newStatRepo(t)}

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "Query")

	constraints, err := deps.ParseConstraints(">= 0.3.1")
	require.NoError(t, err, "ParseConstraints")

	best, ok := newest(got, constraints)
	require.True(t, ok, "some version must satisfy >= 0.3.1")
	assert.Equal(t, statNew, best.Version.Raw, "the newer version wins under >=")
	assert.Equal(t, rockNewFn, filepath.Base(best.URL), "the winning URL is the newer rock")
}

// TestFileRemoteIndex_NoVersionSatisfiesConstraint pins the classification
// the tt resolver depends on: "nothing matches the constraint" is NOT an
// index error. Query succeeds with candidates and the caller's constraint
// filter comes up empty, exactly as against an HTTP server.
func TestFileRemoteIndex_NoVersionSatisfiesConstraint(t *testing.T) {
	t.Parallel()

	idx := &remote.FileRemoteIndex{Server: newStatRepo(t)}

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "Query must succeed even when no version can satisfy the caller")
	require.Len(t, got, 2, "candidates are still reported")

	constraints, err := deps.ParseConstraints(">= 9.9")
	require.NoError(t, err, "ParseConstraints")

	_, ok := newest(got, constraints)
	assert.False(t, ok, "no fixture version satisfies >= 9.9")
}

// newest mirrors the caller-side selection (tt's pickNewest): the highest
// version among the candidates that satisfies every constraint.
func newest(
	candidates []rocks.VersionedRock, constraints []rocks.VersionConstraint,
) (rocks.VersionedRock, bool) {
	var best rocks.VersionedRock

	found := false

	for _, c := range candidates {
		if !deps.Match(c.Version, constraints) {
			continue
		}

		if !found || deps.Compare(c.Version, best.Version) > 0 {
			best = c
			found = true
		}
	}

	return best, found
}

// TestFileRemoteIndex_ServerForms covers both accepted spellings of a local
// server: the bare filesystem path and the file:// URL. The resolved artifact
// keeps the form it was configured with, so a lockfile records what the
// operator wrote.
func TestFileRemoteIndex_ServerForms(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)

	cases := []struct {
		name       string
		server     string
		wantPrefix string
	}{
		{name: "bare path", server: dir, wantPrefix: ""},
		{name: "file URL", server: "file://" + dir, wantPrefix: "file://"},
		{name: "uppercase file URL", server: "FILE://" + dir, wantPrefix: "file://"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := &remote.FileRemoteIndex{Server: c.server}

			got, err := idx.Query(context.Background(), statRock, "")
			require.NoError(t, err, "Query")
			assert.Equal(t, []string{statOld, statNew}, versionsOf(got), "both forms see both versions")
			assert.Equal(t, c.wantPrefix+filepath.Join(dir, rockNewFn), urlOf(t, got, statNew), "URL")
		})
	}
}

// TestFileRemoteIndex_ResolvedURLIsFetchable closes the handoff to the fetch
// layer end to end: whatever the index resolves to must be something
// fetch.Fetch accepts, for both server spellings. Asserting the unpacked
// rockspec (rather than just a nil error) proves the .rock was recognized as
// an archive and extracted, not merely copied.
func TestFileRemoteIndex_ResolvedURLIsFetchable(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)

	for _, server := range []string{dir, "file://" + dir} {
		t.Run(server, func(t *testing.T) {
			t.Parallel()

			idx := &remote.FileRemoteIndex{Server: server}

			got, err := idx.Query(context.Background(), statRock, "")
			require.NoError(t, err, "Query")

			dest := t.TempDir()

			out, err := fetch.Fetch(context.Background(), urlOf(t, got, statNew), dest)
			require.NoError(t, err, "fetch the resolved artifact")

			body, err := os.ReadFile(filepath.Join(out, "stat-0.3.2-1.rockspec"))
			require.NoError(t, err, "the .rock must be unpacked into the destination")
			assert.Contains(t, string(body), "version = '0.3.2-1'", "unpacked rockspec")
		})
	}
}

// TestFileRemoteIndex_ResolvedURLHonoursMD5 — the index carries no checksum
// logic of its own (source.md5 comes from the rockspec and is verified by the
// fetch layer), so the only thing that could break locally is the handoff:
// fetch must apply Options.MD5 to a resolved local path the same way it does
// to a download. A wrong digest must fail, a right one must pass — the second
// half matters, since a check that never runs also "passes".
func TestFileRemoteIndex_ResolvedURLHonoursMD5(t *testing.T) {
	t.Parallel()

	idx := &remote.FileRemoteIndex{Server: newStatRepo(t)}

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "Query")

	artifact := urlOf(t, got, statNew)

	raw, err := os.ReadFile(artifact)
	require.NoError(t, err, "read the resolved artifact")

	sum := md5.Sum(raw) //nolint:gosec // parity with upstream source.md5

	_, err = fetch.FetchWith(context.Background(), artifact, t.TempDir(),
		fetch.Options{MD5: hex.EncodeToString(sum[:])})
	require.NoError(t, err, "the artifact's real md5 must verify")

	_, err = fetch.FetchWith(context.Background(), artifact, t.TempDir(),
		fetch.Options{MD5: strings.Repeat("0", 32)})
	require.Error(t, err, "a wrong md5 must be refused")
	assert.Contains(t, err.Error(), "MD5 check")
}

// TestFileRemoteIndex_ParityWithHTTP serves one manifest two ways and
// requires the same answer from both indexes, down to the artifact filename.
// This is the property the whole type exists for: a local mirror of a server
// resolves to the same rock the server would have.
func TestFileRemoteIndex_ParityWithHTTP(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest"), statManifest)

	origin := serveManifestOverHTTP(t, statManifest)

	local, err := (&remote.FileRemoteIndex{Server: dir}).Query(context.Background(), statRock, "")
	require.NoError(t, err, "local Query")

	over, err := (&remote.HTTPRemoteIndex{Servers: []string{origin}}).
		Query(context.Background(), statRock, "")
	require.NoError(t, err, "HTTP Query")

	require.Equal(t, versionsOf(over), versionsOf(local), "same versions")

	for i := range over {
		assert.Equal(t, filepath.Base(over[i].URL), filepath.Base(local[i].URL),
			"same artifact filename for %s", over[i].Version.Raw)
	}
}

// TestFileRemoteIndex_RockNotInManifest is the local twin of
// TestHTTPRemoteIndex_NameNotInManifest: a rock the manifest never mentions
// is an empty, error-free result, which is what lets the caller report "not
// found" rather than a transport failure.
func TestFileRemoteIndex_RockNotInManifest(t *testing.T) {
	t.Parallel()

	idx := &remote.FileRemoteIndex{Server: newStatRepo(t)}

	got, err := idx.Query(context.Background(), "does-not-exist", "")
	require.NoError(t, err, "an absent rock must not be an error")
	assert.Empty(t, got, "unknown rock name must yield an empty result")
}

// TestFileRemoteIndex_UnusableServerErrors covers every way a local server
// can fail to produce a manifest. All of them must land where the HTTP index
// lands for the equivalent case: a non-nil error out of Query, never an empty
// success that the caller would misreport as "rock not found".
func TestFileRemoteIndex_UnusableServerErrors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		server       func(t *testing.T) string
		wantNotExist bool
		wantErr      string
	}{
		{
			name: "no such directory",
			server: func(t *testing.T) string {
				t.Helper()

				return filepath.Join(t.TempDir(), "absent")
			},
			wantNotExist: true,
			wantErr:      "stat",
		},
		{
			name: "server is a regular file",
			server: func(t *testing.T) string {
				t.Helper()

				p := filepath.Join(t.TempDir(), "repo")
				writeFile(t, p, "not a directory")

				return p
			},
			wantErr: "is not a directory",
		},
		{
			name: "directory without a manifest",
			server: func(t *testing.T) string {
				t.Helper()

				return t.TempDir()
			},
			wantErr: "no manifest variant found",
		},
		{
			name: "manifest is unreadable",
			server: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				// A directory named `manifest` makes every read fail with EISDIR
				// without depending on the test not running as root.
				require.NoError(t, os.Mkdir(filepath.Join(dir, "manifest"), 0o750), "mkdir manifest")

				return dir
			},
			wantErr: "no manifest variant found",
		},
		{
			name: "manifest does not parse",
			server: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "manifest"), "this is not = lua {{{")

				return dir
			},
			wantErr: "parse manifest at",
		},
		{
			name: "manifest has no repository table",
			server: func(t *testing.T) string {
				t.Helper()

				dir := t.TempDir()
				writeFile(t, filepath.Join(dir, "manifest"), "commands = {}\n")

				return dir
			},
			wantErr: "missing `repository` key",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := &remote.FileRemoteIndex{Server: c.server(t)}

			got, err := idx.Query(context.Background(), statRock, "")
			require.Error(t, err, "an unusable server must be an error, not an empty result")
			assert.Empty(t, got, "no rows alongside the error")
			assert.Contains(t, err.Error(), c.wantErr, "error text")

			if c.wantNotExist {
				assert.ErrorIs(t, err, fs.ErrNotExist,
					"a missing directory must stay recognizable as fs.ErrNotExist")
			}
		})
	}
}

// TestFileRemoteIndex_MisconfiguredServer covers the two ways the Server
// field itself can be wrong, mirroring HTTPRemoteIndex's "no servers
// configured" guard.
func TestFileRemoteIndex_MisconfiguredServer(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		server  string
		wantErr string
	}{
		{name: "empty", server: "", wantErr: "no server directory configured"},
		{name: "http server", server: "https://rocks.example.org", wantErr: "is not a local directory"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			idx := &remote.FileRemoteIndex{Server: c.server}

			_, err := idx.Query(context.Background(), statRock, "")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantErr)
		})
	}
}

// TestFileRemoteIndex_UnparsableVersionKeyErrors is the local twin of the
// HTTP case: when no version key for the rock parses, Query surfaces the
// parse error rather than an empty success.
func TestFileRemoteIndex_UnparsableVersionKeyErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest"),
		`repository = { foo = { [""] = { { arch = "src" } } } }`)

	idx := &remote.FileRemoteIndex{Server: dir}

	_, err := idx.Query(context.Background(), "foo", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse version")
}

// TestFileRemoteIndex_ProbeOrderAndFormats pins that the local index probes
// the same filenames in the same order as the HTTP one, and decodes each
// format the same way — the zipped versioned manifest, the plain versioned
// manifest, the bare `manifest`, and the JSON variant.
func TestFileRemoteIndex_ProbeOrderAndFormats(t *testing.T) {
	t.Parallel()

	const jsonBody = `{"repository":{"stat":{"0.3.2-1":[{"arch":"all"}]}}}`

	cases := []struct {
		name  string
		files map[string]string
		zip   string
		want  []string
	}{
		{name: "bare manifest", files: map[string]string{"manifest": statManifest},
			want: []string{statOld, statNew}},
		{name: "versioned manifest", files: map[string]string{"manifest-5.1": statManifest},
			want: []string{statOld, statNew}},
		{name: "json manifest", files: map[string]string{"manifest-5.1.json": jsonBody},
			want: []string{statNew}},
		{name: "zipped versioned manifest", zip: "manifest-5.1.zip", want: []string{statOld, statNew}},
		{
			name: "versioned wins over bare",
			files: map[string]string{
				"manifest-5.1": statManifest,
				"manifest":     `repository = { stat = { ["9.9.9-1"] = { { arch = "all" } } } }`,
			},
			want: []string{statOld, statNew},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			for name, body := range c.files {
				writeFile(t, filepath.Join(dir, name), body)
			}

			if c.zip != "" {
				writeZippedManifest(t, filepath.Join(dir, c.zip), "manifest-5.1", statManifest)
			}

			idx := &remote.FileRemoteIndex{Server: dir}

			got, err := idx.Query(context.Background(), statRock, "")
			require.NoError(t, err, "Query")
			assert.Equal(t, c.want, versionsOf(got))
		})
	}
}

func writeZippedManifest(t *testing.T, path, inner, body string) {
	t.Helper()

	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	w, err := zw.Create(inner)
	require.NoError(t, err, "zip entry")

	_, err = w.Write([]byte(body))
	require.NoError(t, err, "zip write")
	require.NoError(t, zw.Close(), "zip close")
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o600), "write %q", path)
}

// TestFileRemoteIndex_NamespacedQuery pins the same layout the HTTP index
// requests: the per-namespace manifest under manifests/<ns>/, with artifacts
// still resolved against the base directory.
func TestFileRemoteIndex_NamespacedQuery(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)
	writeFile(t, filepath.Join(dir, "manifests", "myuser", "manifest"),
		`repository = { stat = { ["0.3.2-1"] = { { arch = "all" } } } }`)

	idx := &remote.FileRemoteIndex{Server: dir}

	got, err := idx.Query(context.Background(), statRock, "myuser")
	require.NoError(t, err, "Query")
	require.Len(t, got, 1, "only the namespaced manifest's version")
	assert.Equal(t, filepath.Join(dir, rockNewFn), got[0].URL,
		"artifacts resolve against the base directory, not the namespace subdir")
}

// TestFileRemoteIndex_CachesManifest — the parsed manifest is memoized per
// namespace for the life of the value, so a second Query answers without
// touching the disk again. Removing the file between the calls is the only
// way to observe that from outside the package.
func TestFileRemoteIndex_CachesManifest(t *testing.T) {
	t.Parallel()

	dir := newStatRepo(t)
	idx := &remote.FileRemoteIndex{Server: dir}

	_, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "first Query")
	require.NoError(t, os.Remove(filepath.Join(dir, "manifest")), "remove manifest")

	got, err := idx.Query(context.Background(), statRock, "")
	require.NoError(t, err, "second Query must be served from the cache")
	assert.Len(t, got, 2)
}

// TestFileRemoteIndex_ArchSelection pins that arch acceptance and artifact
// preference are the shared code paths, not an HTTP-only concern: a
// foreign-arch-only version is excluded, and among the accepted ones a
// host binary beats src which beats a bare rockspec.
func TestFileRemoteIndex_ArchSelection(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		manifest string
		want     string
	}{
		{
			name:     "foreign arch only is excluded",
			manifest: `repository = { foo = { ["1.0-1"] = { { arch = "macosx-x86_64" } } } }`,
		},
		{
			name:     "host binary beats src",
			manifest: `repository = { foo = { ["1.0-1"] = { { arch = "src" }, { arch = "linux-x86_64" } } } }`,
			want:     "foo-1.0-1.linux-x86_64.rock",
		},
		{
			name:     "src beats rockspec",
			manifest: `repository = { foo = { ["1.0-1"] = { { arch = "rockspec" }, { arch = "src" } } } }`,
			want:     "foo-1.0-1.src.rock",
		},
		{
			name:     "rockspec arch resolves to a .rockspec",
			manifest: `repository = { foo = { ["1.0-1"] = { { arch = "rockspec" } } } }`,
			want:     "foo-1.0-1.rockspec",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "manifest"), c.manifest)

			idx := &remote.FileRemoteIndex{Server: dir, Arch: "linux-x86_64"}

			got, err := idx.Query(context.Background(), "foo", "")
			require.NoError(t, err, "Query")

			if c.want == "" {
				assert.Empty(t, got, "foreign-arch-only version must be excluded")

				return
			}

			require.Len(t, got, 1)
			assert.Equal(t, filepath.Join(dir, c.want), got[0].URL)
		})
	}
}

// TestFileRemoteIndex_InstalledArchURL pins the nested layout
// path.make_url uses for the "installed" pseudo-arch.
func TestFileRemoteIndex_InstalledArchURL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "manifest"),
		`repository = { foo = { ["1.0-1"] = { { arch = "installed" } } } }`)

	idx := &remote.FileRemoteIndex{Server: dir}

	got, err := idx.Query(context.Background(), "foo", "")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, filepath.Join(dir, "foo", "1.0-1", "foo-1.0-1.rockspec"), got[0].URL)
}

func TestFileRemoteIndex_ContextCanceled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	idx := &remote.FileRemoteIndex{Server: newStatRepo(t)}

	_, err := idx.Query(ctx, statRock, "")
	require.ErrorIs(t, err, context.Canceled)
}
