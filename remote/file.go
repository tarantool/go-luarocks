package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// fileURLPrefix is the scheme separator of a file URL. It is trimmed rather
// than parsed with net/url so this package and fetch's file backend agree on
// what a `file://` server means: fetch.fileBackend does the same
// strings.TrimPrefix, which keeps `file:///tmp/repo` → `/tmp/repo` and
// performs no percent-decoding on either side.
const fileURLPrefix = "file://"

// LocalServerPath reports whether server names a local directory rather than
// an HTTP(S) rock server, returning the filesystem path it designates.
//
// The rule, taken from upstream dir.split_url (core/dir.lua:41-51), which is
// what makes `luarocks --only-server=/path/to/repo` work:
//
//   - a `file://` prefix (case-insensitively) is a local directory, and the
//     path is whatever follows it — so `file:///tmp/repo` is `/tmp/repo`;
//   - any other string containing `://` is a remote URL and NOT local, so
//     `https://rocks.tarantool.org` keeps going through HTTPRemoteIndex;
//   - any other non-empty string has no protocol at all and is therefore a
//     path, exactly as split_url's `if not protocol then protocol = "file"`
//     branch decides. This is deliberate and load-bearing in two directions:
//     a Windows path (`C:\rocks\repo`, `C:/rocks/repo`) has no `://` and is
//     never mistaken for a host, and a scheme-less `rocks.example.org/dist`
//     is a *directory name*, not a server — upstream reads it that way too,
//     so refusing it here would diverge from luarocks for no gain;
//   - the empty string names nothing and is not local, so a misconfigured
//     empty server keeps failing where it already failed.
//
// The returned path is the raw string: it is neither cleaned nor made
// absolute, because a relative rock server is legal and the caller's working
// directory is the one that must resolve it.
func LocalServerPath(server string) (string, bool) {
	if server == "" {
		return "", false
	}

	if len(server) >= len(fileURLPrefix) &&
		strings.EqualFold(server[:len(fileURLPrefix)], fileURLPrefix) {
		return server[len(fileURLPrefix):], true
	}

	if strings.Contains(server, "://") {
		return "", false
	}

	return server, true
}

// FileRemoteIndex is the rocks.RemoteIndex backed by a rock server that is a
// local directory — the offline/air-gapped case upstream serves with
// `luarocks --only-server=/path/to/repo`.
//
// It is the exact counterpart of HTTPRemoteIndex and differs only in where
// the bytes come from: the manifest is read from `<dir>/manifest…` instead of
// fetched with GET, and a resolved artifact is a path under the same
// directory instead of a URL on the same host. Manifest probe order, decoding
// (zip / Lua-source / JSON), arch acceptance, artifact preference
// (.rock > .src.rock > .rockspec) and per-(server, namespace) caching are the
// shared code paths, so a local mirror and its HTTP original resolve to the
// same version. Mirrors upstream manif.load_manifest, whose `protocol ==
// "file"` branch reads dir.path(repodir, filename) off disk while the else
// branch fetches (manif.lua:90-116).
//
// Error classification matches HTTPRemoteIndex exactly, because tt's resolver
// branches on it: a rock the manifest does not mention is an empty result and
// a nil error (the caller turns that into "not found"), while an unusable
// server — missing directory, no readable manifest, unparsable manifest — is
// a non-nil error. Selecting a version out of the returned candidates is the
// caller's job, so "no version satisfies the constraint" is likewise an empty
// match over a successful, error-free Query.
//
// Like HTTPRemoteIndex, a value is not safe for concurrent use: Query
// populates the manifest cache without locking.
type FileRemoteIndex struct {
	// Server is the rock server as configured — a bare directory path or a
	// `file://` URL (see LocalServerPath). Empty is an error from Query.
	Server string

	// LuaVersion is the Lua dialect string used to build the manifest
	// filename. Defaults to "5.1" if empty (Tarantool case).
	LuaVersion string

	// Arch is the host arch string (LuaRocks cfg.arch, e.g. "linux-x86_64")
	// used to accept host-built binary rocks during Query. Empty defaults to
	// the detected host arch (hostArch).
	Arch string

	// cache memoizes parsed manifests across calls, keyed by namespace.
	cache map[string]*remoteManifest
}

// Compile-time check that FileRemoteIndex satisfies rocks.RemoteIndex.
var _ rocks.RemoteIndex = (*FileRemoteIndex)(nil)

// Query implements rocks.RemoteIndex against the local directory. It returns
// every VersionedRock listed under `name` in the directory's manifest, each
// carrying the on-disk path of its preferred artifact.
func (f *FileRemoteIndex) Query(
	ctx context.Context, name, namespace string,
) ([]rocks.VersionedRock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if f.Server == "" {
		return nil, errors.New("remote.FileRemoteIndex: no server directory configured")
	}

	dir, ok := LocalServerPath(f.Server)
	if !ok {
		return nil, fmt.Errorf(
			"remote.FileRemoteIndex: server %q is not a local directory", f.Server)
	}

	mf, err := f.load(dir, namespace)
	if err != nil {
		// A single index has no sibling server to fall back on, so unlike
		// HTTPRemoteIndex's per-server loop this is terminal — but it lands in
		// the same place: Query returns a non-nil error and no rows.
		return nil, fmt.Errorf("remote.FileRemoteIndex: load %s: %w", f.Server, err)
	}

	merged := map[string][]mergedItem{}

	for verStr, arches := range mf.repository[name] {
		for _, a := range acceptedArches(arches, f.Arch) {
			merged[verStr] = append(merged[verStr], mergedItem{server: f.Server, arch: a.arch})
		}
	}

	makeURL := func(_, rockName, version, arch string) string {
		return localRockURL(dir, filePrefixOf(f.Server), rockName, version, arch)
	}

	out, parseErr := mergedRocks("remote.FileRemoteIndex", name, merged, makeURL)
	if len(out) == 0 && parseErr != nil {
		return nil, parseErr
	}

	return out, nil
}

// load reads and parses the manifest for the directory, using the cache if
// present. A non-empty namespace reads the per-namespace manifest under
// <dir>/manifests/<namespace>/, matching the layout HTTPRemoteIndex requests
// over the wire (upstream manifest_search, search.lua:107-109); the artifacts
// still live at the base directory.
func (f *FileRemoteIndex) load(dir, namespace string) (*remoteManifest, error) {
	if f.cache == nil {
		f.cache = map[string]*remoteManifest{}
	}

	if m, ok := f.cache[namespace]; ok {
		return m, nil
	}

	lv := f.LuaVersion
	if lv == "" {
		lv = "5.1"
	}

	base := dir
	if namespace != "" {
		base = filepath.Join(dir, "manifests", namespace)
	}

	// Stat the directory up front. Probing four filenames inside a directory
	// that does not exist would report the LAST probe's ENOENT, naming a file
	// nobody configured; this names the directory the user actually wrote and
	// still wraps fs.ErrNotExist for callers that test for it.
	st, err := os.Stat(base)
	if err != nil {
		return nil, fmt.Errorf("stat %q: %w", base, err)
	}

	if !st.IsDir() {
		return nil, fmt.Errorf("%q is not a directory", base)
	}

	// Two failure kinds, kept apart on purpose. A read error means the variant
	// simply is not there (the normal case for three of the four probes), while
	// a decode error means a manifest IS there and is broken — the one thing the
	// operator can act on. HTTPRemoteIndex keeps a single lastErr, so the final
	// probe's 404 overwrites an earlier decode failure; against a directory the
	// user is looking at, reporting "manifest-5.1.json: no such file" for a
	// malformed `manifest` sitting next to it is a dead end, so the decode error
	// wins here. Only the message differs — an unusable server is a non-nil
	// error from Query either way, which is the part callers branch on.
	var readErr, decodeErr error

	for _, p := range manifestProbes(lv) {
		path := filepath.Join(base, p.name)

		// The path is built from the operator-configured server directory and a
		// fixed manifest filename, which is the documented purpose of this index.
		body, err := os.ReadFile(path)
		if err != nil {
			readErr = err

			continue
		}

		m, err := decodeManifest(f.Server, path, p, body)
		if err != nil {
			if decodeErr == nil {
				decodeErr = err
			}

			continue
		}

		f.cache[namespace] = m

		return m, nil
	}

	if decodeErr != nil {
		return nil, decodeErr
	}

	return nil, fmt.Errorf("no manifest variant found in %s: %w", base, readErr)
}

// filePrefixOf returns the `file://` prefix to put back on a constructed
// artifact path when the server was configured as a file URL, and "" when it
// was configured as a bare path. Preserving the form the caller wrote keeps
// the URL recognizable in a lockfile; both forms reach fetch's file backend,
// which strips the prefix and treats a scheme-less string as a path
// (fetch.FetchWith, mirroring dir.split_url).
func filePrefixOf(server string) string {
	if len(server) >= len(fileURLPrefix) &&
		strings.EqualFold(server[:len(fileURLPrefix)], fileURLPrefix) {
		return fileURLPrefix
	}

	return ""
}

// localRockURL is makeRockURL for a local directory: the same
// path.make_url shapes, joined with the OS separator so the result is a path
// the fetch layer can stat directly.
//
//	arch == archRockspec → <dir>/<name>-<version>.rockspec
//	arch == "installed"  → <dir>/<name>/<version>/<name>-<version>.rockspec
//	otherwise            → <dir>/<name>-<version>.<arch>.rock
func localRockURL(dir, prefix, name, version, arch string) string {
	switch arch {
	case archRockspec:
		return prefix + filepath.Join(dir, name+"-"+version+".rockspec")
	case archInstalled:
		return prefix + filepath.Join(dir, name, version, name+"-"+version+".rockspec")
	default:
		return prefix + filepath.Join(dir, name+"-"+version+"."+arch+".rock")
	}
}
