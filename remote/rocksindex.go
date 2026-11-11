package remote

// The tt-rocks-index.json a rock server may publish beside, or instead of, a
// LuaRocks manifest: the format, how one is read and checked, and how a read
// one becomes the projection a manifest becomes, so the indexes, Search and
// the resolver need no second code path.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"path"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// RocksIndexFile is the name of the rock index a server may publish in the
// directory its manifests would be in. HTTPRemoteIndex, FileRemoteIndex and
// Search read it before any manifest: when the server has one it is the
// server's whole catalogue, and no manifest is probed; when it has none — an
// HTTP 403, 404 or 410, or no such file in a directory — the manifests are
// probed as before; and when it is there but cannot be read, the server is in
// error rather than read through its manifests.
//
// Only the native backend reads it. Upstream LuaRocks, which the lua backend
// runs, knows only manifests, so a server that publishes nothing else serves
// no rocks there.
const RocksIndexFile = "tt-rocks-index.json"

// RocksIndexFormat is the format of tt-rocks-index.json this package reads
// and writes. Reading an index of a newer format fails with an error wrapping
// rocks.ErrUnsupportedIndexFormat; fields a format does not define are
// ignored, so a later format may add some without breaking this one.
const RocksIndexFormat = 1

// RocksIndex is a tt-rocks-index.json: for each rock version, where its files
// are and their sha256. Unlike a manifest, it names every file by URL, so the
// files may live apart from the index — on release pages, say — and it
// publishes a version's rockspec apart from its rocks, so a resolver reads
// the dependencies without downloading a rock.
//
// The rules the indexes read it by, and Validate checks:
//
//   - a rock name and a version are single path elements; the version parses
//     (deps.ParseVersion) and carries a revision ("0.25.1-1");
//   - a version lists a rockspec, artifacts or both; an artifact's arch is a
//     single path element, appears once, and is neither "rockspec" — the
//     rockspec field is that — nor "installed";
//   - every sha256 is 64 hex digits, and a size is not negative;
//   - a url is absolute https — plain http only for a host among the insecure
//     servers, as IndexOptions.InsecureServers lists them — or relative: a
//     path below the directory of the index, without "." or ".." segments, a
//     query, a fragment or a percent escape, which resolves the same way on
//     an HTTP server and in a local directory;
//   - the last segment of a url's path is the file's name:
//     <name>-<version>.<arch>.rock for an artifact and
//     <name>-<version>.rockspec for the rockspec, the name a download is
//     saved under and that tells what kind of file it is.
//
// A rock that breaks a rule is refused whole: asking the server for it is an
// error naming the entry, and the server's other rocks are unaffected. An
// index that is not JSON of this shape fails whole.
//
// Only https and http urls are accepted today. The format does not tie a url
// to a scheme: one with an other scheme, such as an oci:// reference to a
// blob by digest, can be accepted later, with its own rule for the name in
// place of the last rule above, without a new format.
type RocksIndex struct {
	// Format is the format of the index, RocksIndexFormat for one this
	// package writes.
	Format int `json:"format"`
	// Repository maps a rock name to its versions, each a version-revision
	// string such as "0.25.1-1".
	Repository map[string]map[string]IndexVersion `json:"repository"`
}

// IndexVersion is what a RocksIndex lists for one rock version. Its fields,
// like those of IndexFile and IndexArtifact, are declared in the order of
// their JSON names, so Write puts the keys of every object in sorted order.
type IndexVersion struct {
	// Artifacts are the packaged rocks of the version, one per arch: an
	// "<os>-<cpu>" for a platform's, "all" for a pure-Lua one and "src" for
	// a source rock.
	Artifacts []IndexArtifact `json:"artifacts,omitempty"`
	// Rockspec is the version's rockspec, published apart from its rocks.
	// Without it, resolving reads the rockspec out of the artifact it picks,
	// which takes downloading that; with no artifacts, it is what installs,
	// built from source.url.
	Rockspec *IndexFile `json:"rockspec,omitempty"`
	// Yanked withdraws the version: rocks.VersionedRock.Yanked says what
	// that does to a resolve.
	Yanked bool `json:"yanked,omitempty"`
}

// IndexFile is one file a RocksIndex lists: where it is and its digest.
type IndexFile struct {
	// SHA256 is the file's sha256 in hex.
	SHA256 string `json:"sha256"`
	// Size is the file's size in bytes; 0, and left out of the JSON, when
	// not given.
	Size int64 `json:"size,omitempty"`
	// URL locates the file, absolute or relative to the index's directory.
	URL string `json:"url"`
}

// IndexArtifact is one packaged rock a RocksIndex lists.
type IndexArtifact struct {
	// Arch is the manifest arch of the rock.
	Arch string `json:"arch"`
	// SHA256 is the rock's sha256 in hex.
	SHA256 string `json:"sha256"`
	// Size is the rock's size in bytes; 0, and left out of the JSON, when
	// not given.
	Size int64 `json:"size,omitempty"`
	// URL locates the rock, absolute or relative to the index's directory.
	URL string `json:"url"`
}

// ReadRocksIndex decodes a tt-rocks-index.json. It checks the format — an
// index of a format newer than RocksIndexFormat fails with an error wrapping
// rocks.ErrUnsupportedIndexFormat — and that the JSON has the documented
// shape, ignoring fields it does not know; whether the entries keep the
// rules of RocksIndex is Validate's to check.
func ReadRocksIndex(r io.Reader) (*RocksIndex, error) {
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("remote: read rock index: %w", err)
	}

	// The format is read on its own first: an index of a newer format is
	// reported as one, not as whatever its new shape fails to decode as.
	var head struct {
		Format json.RawMessage `json:"format"`
	}

	if err := json.Unmarshal(body, &head); err != nil {
		return nil, fmt.Errorf("remote: decode rock index: %w", err)
	}

	if head.Format == nil {
		return nil, errors.New("remote: rock index has no format")
	}

	var format int
	if err := json.Unmarshal(head.Format, &format); err != nil || format < 1 {
		return nil, fmt.Errorf("remote: rock index format %s is not a positive integer", head.Format)
	}

	if format > RocksIndexFormat {
		return nil, fmt.Errorf("remote: %w: the index has format %d, newer than format %d this tt reads; upgrade tt",
			rocks.ErrUnsupportedIndexFormat, format, RocksIndexFormat)
	}

	var idx RocksIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("remote: decode rock index: %w", err)
	}

	if idx.Repository == nil {
		return nil, errors.New("remote: rock index has no repository")
	}

	return &idx, nil
}

// indexedFile is one file of an index version whose entry keeps the rules.
type indexedFile struct {
	// arch is the artifact's arch, or archRockspec for the rockspec.
	arch string
	// ref is the url as the index lists it, and parsed its parsed form.
	ref    string
	parsed *url.URL
	// sha256 is the digest, lowercased.
	sha256 string
	size   int64
}

// checkIndexVersion checks one version of the rock name against the rules of
// RocksIndex and returns its files, sorted by arch, the rockspec under
// archRockspec. insecure lists the hosts an absolute http url may name.
func checkIndexVersion(name, version string, v IndexVersion, insecure []string) ([]indexedFile, error) {
	for _, part := range []string{name, version} {
		if err := safepath.Name(part); err != nil {
			return nil, err
		}
	}

	parsed, err := deps.ParseVersion(version)
	if err != nil {
		return nil, err
	}

	// The revision is part of every file name, and without it two spellings
	// of one version would be told apart by nothing a resolver compares.
	if !parsed.HasRevision {
		return nil, fmt.Errorf("version %q has no revision", version)
	}

	if v.Rockspec == nil && len(v.Artifacts) == 0 {
		return nil, errors.New("the version lists no file")
	}

	files := make([]indexedFile, 0, len(v.Artifacts)+1)

	if s := v.Rockspec; s != nil {
		f, err := checkIndexFile(archRockspec, name+"-"+version+".rockspec", s.URL, s.SHA256, s.Size, insecure)
		if err != nil {
			return nil, fmt.Errorf("rockspec: %w", err)
		}

		files = append(files, f)
	}

	seen := map[string]bool{}

	for i, a := range v.Artifacts {
		if err := checkArtifactArch(a.Arch, seen); err != nil {
			return nil, fmt.Errorf("artifact %d: %w", i, err)
		}

		f, err := checkIndexFile(a.Arch, name+"-"+version+"."+a.Arch+".rock", a.URL, a.SHA256, a.Size, insecure)
		if err != nil {
			return nil, fmt.Errorf("artifact %s: %w", a.Arch, err)
		}

		files = append(files, f)
	}

	slices.SortFunc(files, func(a, b indexedFile) int { return strings.Compare(a.arch, b.arch) })

	return files, nil
}

// checkArtifactArch checks the arch of an artifact, which must not be one of
// those seen before, and records it.
func checkArtifactArch(arch string, seen map[string]bool) error {
	switch arch {
	case "":
		return errors.New("no arch")
	case archRockspec, archInstalled:
		return fmt.Errorf("arch %q is not one of a packaged rock", arch)
	}

	if err := safepath.Name(arch); err != nil {
		return fmt.Errorf("arch: %w", err)
	}

	if seen[arch] {
		return fmt.Errorf("arch %q is listed twice", arch)
	}

	seen[arch] = true

	return nil
}

// checkIndexFile checks the entry of one file, which must be called file.
func checkIndexFile(arch, file, ref, digest string, size int64, insecure []string) (indexedFile, error) {
	sum := strings.ToLower(digest)
	if _, err := hex.DecodeString(sum); err != nil || len(sum) != sha256HexLen {
		return indexedFile{}, fmt.Errorf("sha256 %q is not 64 hex digits", digest)
	}

	if size < 0 {
		return indexedFile{}, fmt.Errorf("size %d is negative", size)
	}

	parsed, err := checkIndexURL(ref, file, insecure)
	if err != nil {
		return indexedFile{}, err
	}

	return indexedFile{arch: arch, ref: ref, parsed: parsed, sha256: sum, size: size}, nil
}

// sha256HexLen is the length of a sha256 digest in hex.
const sha256HexLen = 64

// checkIndexURL checks ref, the url an index lists for the file called file,
// and returns it parsed.
func checkIndexURL(ref, file string, insecure []string) (*url.URL, error) {
	if ref == "" {
		return nil, errors.New("no url")
	}

	u, err := url.Parse(ref)
	if err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}

	if u.Scheme == "" {
		err = checkRelativeURL(ref, u)
	} else {
		err = checkAbsoluteURL(ref, u, insecure)
	}

	if err != nil {
		return nil, err
	}

	// A download is saved under the last segment of the url's path, and that
	// name says what kind of file it is: a .rockspec is built, a .src.rock
	// built from what it bundles, any other .rock deployed as it is. So it
	// must be the file's own name, not just any name a server might give.
	if got := path.Base(u.Path); got != file {
		return nil, fmt.Errorf("url %q names the file %q, want %q", ref, got, file)
	}

	return u, nil
}

// checkAbsoluteURL accepts an https url, and an http one whose host is among
// insecure — compared as the transport compares a request's host with
// InsecureServers, so a host:port matches only with its port — since an index
// that may send a client to any host must not send it there in the clear.
func checkAbsoluteURL(ref string, u *url.URL, insecure []string) error {
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		listed := slices.ContainsFunc(insecure, func(h string) bool { return strings.EqualFold(h, u.Host) })
		if !listed {
			return fmt.Errorf("url %q is plain http, allowed only for a host among the insecure servers", ref)
		}
	default:
		return fmt.Errorf("url %q has the scheme %q, not https", ref, u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("url %q has no host", ref)
	}

	return nil
}

// checkRelativeURL accepts a plain path below the index's directory: one
// that resolves to the same file whether the index is read over HTTP or off
// disk, and never above that directory.
func checkRelativeURL(ref string, u *url.URL) error {
	if u.Host != "" || u.User != nil || u.Opaque != "" || strings.ContainsAny(ref, `?#%\`) {
		return fmt.Errorf("relative url %q is not a plain path", ref)
	}

	for seg := range strings.SplitSeq(u.Path, "/") {
		if err := safepath.Name(seg); err != nil {
			return fmt.Errorf("relative url %q is not a path below the index's directory: %w", ref, err)
		}
	}

	return nil
}

// indexBase is where the relative urls of one index resolve, and which hosts
// its absolute http urls may name.
type indexBase struct {
	// web is the URL of the index's directory, ending in "/", for an index
	// read over HTTP; nil for one read off disk.
	web *url.URL
	// dir is the directory of an index read off disk, and prefix the
	// "file://" the server's form carries, if it does (filePrefixOf).
	dir, prefix string
	// insecure lists the hosts an absolute http url may name.
	insecure []string
}

// locate returns where f is: its url as listed when that is absolute, and
// otherwise resolved against the index's directory, as a URL on its server or
// a path in it.
func (b indexBase) locate(f indexedFile) string {
	switch {
	case f.parsed.Scheme != "":
		return f.ref
	case b.web != nil:
		return b.web.ResolveReference(f.parsed).String()
	default:
		return b.prefix + filepath.Join(b.dir, filepath.FromSlash(f.parsed.Path))
	}
}

// indexedVersion is what an index says about one rock version beyond the
// arches it lists, shared by the archEntry of each of its files.
type indexedVersion struct {
	specURL, specSHA256 string
	yanked              bool
	// artifacts are the version's files, the rockspec among them, sorted by
	// arch, as rocks.VersionedRock.Artifacts lists them.
	artifacts []rocks.Artifact
}

// decodeRocksIndex reads the index at, of server, from body and projects it
// for Query and Search. A rock that breaks a rule of RocksIndex is refused,
// not projected.
func decodeRocksIndex(server, at string, body []byte, base indexBase) (*remoteManifest, error) {
	idx, err := ReadRocksIndex(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", at, err)
	}

	out := &remoteManifest{
		server:     server,
		repository: map[string]map[string][]archEntry{},
		refused:    map[string]error{},
	}

	for name, versions := range idx.Repository {
		rock, err := projectIndexedRock(name, versions, base)
		if err != nil {
			out.refused[name] = fmt.Errorf("%s: refusing rock %q: %w", at, name, err)

			continue
		}

		out.repository[name] = rock
	}

	return out, nil
}

// projectIndexedRock turns the versions of one rock into the manifest
// projection: an archEntry per file, the rockspec included under
// archRockspec as a manifest lists a bare rockspec, each carrying where the
// file is and its digest.
func projectIndexedRock(name string, versions map[string]IndexVersion, base indexBase) (map[string][]archEntry, error) {
	out := make(map[string][]archEntry, len(versions))

	// In order, so that of two broken versions the same one is reported.
	for _, version := range slices.Sorted(maps.Keys(versions)) {
		v := versions[version]

		files, err := checkIndexVersion(name, version, v, base.insecure)
		if err != nil {
			return nil, fmt.Errorf("version %q: %w", version, err)
		}

		info := &indexedVersion{yanked: v.Yanked, artifacts: make([]rocks.Artifact, 0, len(files))}
		entries := make([]archEntry, 0, len(files))

		for _, f := range files {
			at := base.locate(f)

			if f.arch == archRockspec {
				info.specURL, info.specSHA256 = at, f.sha256
			}

			info.artifacts = append(info.artifacts, rocks.Artifact{Arch: f.arch, URL: at, SHA256: f.sha256, Size: f.size})
			entries = append(entries, archEntry{arch: f.arch, url: at, sha256: f.sha256, version: info})
		}

		out[version] = entries
	}

	return out, nil
}
