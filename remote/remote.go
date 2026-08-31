package remote

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/manif"
)

// Manifest arch values for non-binary rock entries: archSrc is a packaged
// source rock, archRockspec is a bare .rockspec (source spec with no packaged
// rock). Both rank below a host-built binary arch during selection.
// archInstalled is the pseudo-arch of an already-installed rock, whose
// rockspec lives under a per-version subdirectory.
const (
	archSrc       = "src"
	archRockspec  = "rockspec"
	archInstalled = "installed"
)

// HTTPRemoteIndex is the default rocks.RemoteIndex backed by one or more
// rock servers reachable over HTTP/HTTPS.
//
// Manifest filename probe order, per server:
//
//  1. manifest-<lua_version>.json   (parsed as JSON)
//  2. manifest-<lua_version>         (Lua-source via manif.Parse)
//  3. manifest                        (Lua-source)
//
// HTTPRemoteIndex caches the parsed manifest per (server, lua-version)
// pair for the lifetime of the struct so the resolver does not re-fetch
// for every Query call.
type HTTPRemoteIndex struct {
	// Servers is the ordered list of base URLs (with or without trailing
	// slash) to consult. Empty is an error from Query.
	Servers []string

	// InsecureServers contains hostnames whose TLS certificates should not
	// be verified. Pulled from Config.InsecureServers by the facade.
	InsecureServers []string

	// UserAgent overrides the default User-Agent header for outbound GETs.
	UserAgent string

	// LuaVersion is the Lua dialect string used to build the manifest
	// filename. Defaults to "5.1" if empty (Tarantool case).
	LuaVersion string

	// Arch is the host arch string (LuaRocks cfg.arch, e.g. "linux-x86_64")
	// used to accept host-built binary rocks during Query. Empty defaults to
	// the detected host arch (hostArch). A version listed only under a foreign
	// binary arch is excluded, mirroring results:satisfies (results.lua:57).
	Arch string

	// cache memoizes parsed manifests across calls. Keyed by the server
	// string as passed to load (the raw, un-normalized entry from Servers).
	cache map[string]*remoteManifest
}

// Compile-time check that HTTPRemoteIndex satisfies rocks.RemoteIndex.
var _ rocks.RemoteIndex = (*HTTPRemoteIndex)(nil)

const (
	// httpClientTimeout bounds a single manifest GET.
	httpClientTimeout = 2 * time.Minute

	// httpStatusErrorThreshold is the lowest HTTP status code treated as an
	// error (4xx and 5xx).
	httpStatusErrorThreshold = 400
)

// remoteManifest is the in-memory projection of a parsed rock server
// manifest needed to satisfy Query. The full upstream shape carries more
// data (modules, commands, dependencies) but the resolver only needs the
// (name → versions → arch) triplet to construct URLs.
type remoteManifest struct {
	server     string
	repository map[string]map[string][]archEntry
}

type archEntry struct {
	arch string
}

// Query implements rocks.RemoteIndex. For each known server, it returns
// every VersionedRock listed under `name` in the manifest, with the URL
// pre-computed using path.make_url-equivalent rules (see makeRockURL).
func (h *HTTPRemoteIndex) Query(ctx context.Context, name, namespace string) ([]rocks.VersionedRock, error) {
	if len(h.Servers) == 0 {
		return nil, errors.New("remote.HTTPRemoteIndex: no servers configured")
	}

	// Merge the accepted arch items for each version across ALL servers into one
	// shared list (upstream store_result appends every server's item into
	// result_tree[name][version], search.lua:26-30), then pick a single arch per
	// version across that merged list — so a version offered as rockspec on one
	// server and src on another resolves to the src item, not the first server's.
	merged := map[string][]mergedItem{}

	var loadErr error

	for _, srv := range h.Servers {
		// A single unreachable or malformed server is non-fatal: mirror
		// search.lua search_repos, which warns and continues so a rock
		// available on ANY configured server still resolves.
		mf, err := h.load(ctx, srv, namespace)
		if err != nil {
			if loadErr == nil {
				loadErr = fmt.Errorf("remote.HTTPRemoteIndex: load %s: %w", srv, err)
			}

			continue
		}

		versions, ok := mf.repository[name]
		if !ok {
			continue
		}

		for verStr, arches := range versions {
			// Filter to arches the host query accepts, mirroring
			// results:satisfies (query.arch[self.arch] or "any"). The default
			// query arch set is {src,all,rockspec,installed} plus the host arch
			// (queries.lua:17-23,72).
			for _, a := range acceptedArches(arches, h.Arch) {
				merged[verStr] = append(merged[verStr], mergedItem{server: srv, arch: a.arch})
			}
		}
	}

	out, parseErr := mergedRocks("remote.HTTPRemoteIndex", name, merged, makeRockURL)
	if loadErr == nil {
		loadErr = parseErr
	}

	// Only surface a failure when nothing was found AND at least one server
	// hard-failed; a rock genuinely absent everywhere returns an empty slice.
	if len(out) == 0 && loadErr != nil {
		return nil, loadErr
	}

	return out, nil
}

// mergedItem is one (server, arch) offering of a rock version, accumulated
// across all servers before arch selection.
type mergedItem struct {
	server string
	arch   string
}

// pickMergedItem chooses one item from the merged cross-server list, reproducing
// upstream pick_latest_version (search.lua:207-213): a binary/all/installed arch
// (neither "src" nor "rockspec") wins, last-one-wins among those; "src" beats
// "rockspec". So a prebuilt .rock is preferred over source, and a bare .rockspec
// is the last resort — considered across every server's offering, not per-server.
func pickMergedItem(items []mergedItem) int {
	pick := 0

	for i, it := range items {
		if (it.arch == archSrc && items[pick].arch == archRockspec) ||
			(it.arch != archSrc && it.arch != archRockspec) {
			pick = i
		}
	}

	return pick
}

// load fetches and parses the manifest for one server, using the cache if
// present. A non-empty namespace fetches the per-namespace manifest under
// /manifests/<namespace>/ (upstream manifest_search, search.lua:107-109); the
// .rock artifacts still live at the base server, so the projected manifest
// keeps `server` as the base for URL construction.
func (h *HTTPRemoteIndex) load(ctx context.Context, server, namespace string) (*remoteManifest, error) {
	if h.cache == nil {
		h.cache = map[string]*remoteManifest{}
	}

	// Cache key includes the namespace so the base and namespaced manifests of
	// the same server don't collide.
	cacheKey := server + "\x00" + namespace
	if m, ok := h.cache[cacheKey]; ok {
		return m, nil
	}

	lv := h.LuaVersion
	if lv == "" {
		lv = "5.1"
	}

	base := strings.TrimRight(server, "/") + "/"
	if namespace != "" {
		base += "manifests/" + namespace + "/"
	}

	var lastErr error

	for _, p := range manifestProbes(lv) {
		path := base + p.name

		body, err := h.get(ctx, path)
		if err != nil {
			lastErr = err

			continue
		}

		m, err := decodeManifest(server, path, p, body)
		if err != nil {
			lastErr = err

			continue
		}

		h.cache[cacheKey] = m

		return m, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no manifest variant found at %s", server)
	}

	return nil, lastErr
}

// manifestProbe is one candidate manifest filename plus how to decode it.
// The name is relative to the server base, so an HTTP index joins it with "/"
// and a file index with the OS separator.
type manifestProbe struct {
	name   string
	isJSON bool
	isZip  bool
	// inner is the entry name inside a .zip probe (the un-suffixed manifest
	// name, per manif.lua:132 nozip = pathname:match("(.*)%.zip$")).
	inner string
}

// manifestProbes returns the manifest filenames to try, in order, for the
// given Lua version.
//
// Upstream manif.load_manifest probes [manifest-<lv>.zip, manifest-<lv>,
// manifest] with the versioned (Lua-filtered) forms first (manif.lua:90-133).
// The manifest-<lv>.json variant is a Go-specific extra kept LAST so it never
// preempts the upstream versioned manifests.
func manifestProbes(luaVersion string) []manifestProbe {
	return []manifestProbe{
		{name: "manifest-" + luaVersion + ".zip", isZip: true, inner: "manifest-" + luaVersion},
		{name: "manifest-" + luaVersion, isJSON: false},
		{name: "manifest", isJSON: false},
		{name: "manifest-" + luaVersion + ".json", isJSON: true},
	}
}

// decodeManifest turns the raw bytes of one manifest probe into the projected
// (name → version → arches) shape. path is used only for error messages, and
// server is recorded on the result so URL construction can reach it.
//
// Shared by HTTPRemoteIndex and FileRemoteIndex: the two differ in where the
// bytes come from, never in how a manifest is decoded.
func decodeManifest(
	server, path string, p manifestProbe, body []byte,
) (*remoteManifest, error) {
	if p.isZip {
		unzipped, err := unzipManifest(body, p.inner)
		if err != nil {
			return nil, fmt.Errorf("unzip manifest at %s: %w", path, err)
		}

		body = unzipped
	}

	var raw map[string]any

	if p.isJSON {
		err := json.Unmarshal(body, &raw)
		if err != nil {
			return nil, fmt.Errorf("decode JSON manifest at %s: %w", path, err)
		}
	} else {
		v, err := manif.Parse(body)
		if err != nil {
			return nil, fmt.Errorf("parse manifest at %s: %w", path, err)
		}

		rawMap, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("manifest at %s: top-level is %T, want map", path, v)
		}

		raw = rawMap
	}

	m, err := projectManifest(server, raw)
	if err != nil {
		return nil, fmt.Errorf("project manifest from %s: %w", path, err)
	}

	return m, nil
}

// mergedRocks turns the accumulated (version → items) map into one
// rocks.VersionedRock per version, in sorted version-string order for
// deterministic output (the resolver re-sorts by parsed version regardless).
// makeURL builds the artifact URL for the winning item of each version.
//
// A version key that fails deps.ParseVersion is skipped and reported as the
// returned error, which callers surface only when nothing resolved at all —
// one unparsable key must not hide the versions that did parse.
func mergedRocks(
	errPrefix, name string,
	merged map[string][]mergedItem,
	makeURL func(server, name, version, arch string) string,
) ([]rocks.VersionedRock, error) {
	vers := make([]string, 0, len(merged))
	for verStr := range merged {
		vers = append(vers, verStr)
	}

	sort.Strings(vers)

	out := []rocks.VersionedRock{}

	var parseErr error

	for _, verStr := range vers {
		items := merged[verStr]

		v, err := deps.ParseVersion(verStr)
		if err != nil {
			if parseErr == nil {
				parseErr = fmt.Errorf("%s: parse version %q for %q: %w", errPrefix, verStr, name, err)
			}

			continue
		}

		win := items[pickMergedItem(items)]
		out = append(out, rocks.VersionedRock{
			Name:    name,
			Version: v,
			URL:     makeURL(win.server, name, verStr, win.arch),
		})
	}

	return out, parseErr
}

// unzipManifest extracts the manifest bytes from a zipped versioned manifest.
// It prefers the entry named inner (the un-suffixed manifest-<lv> name); if
// absent, it falls back to the single entry in the archive.
func unzipManifest(body []byte, inner string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}

	if len(zr.File) == 0 {
		return nil, errors.New("empty zip archive")
	}

	f := zr.File[0]

	for _, e := range zr.File {
		if e.Name == inner {
			f = e

			break
		}
	}

	rc, err := f.Open()
	if err != nil {
		return nil, err
	}

	defer func() { _ = rc.Close() }()

	return io.ReadAll(rc)
}

// get performs a single HTTP GET with sensible defaults. It returns the
// response body for any status below 400; a status of 400 or above is an
// error. (Redirects are followed by the underlying http.Client, so 3xx
// responses are not normally seen here.)
func (h *HTTPRemoteIndex) get(ctx context.Context, rawURL string) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}

	tr := &http.Transport{}

	for _, host := range h.InsecureServers {
		if strings.EqualFold(host, u.Host) {
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in

			break
		}
	}

	client := &http.Client{Transport: tr, Timeout: httpClientTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}

	ua := h.UserAgent
	if ua == "" {
		ua = "go-luarocks/0.1"
	}

	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= httpStatusErrorThreshold {
		return nil, fmt.Errorf("GET %s: status %d", rawURL, resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// projectManifest extracts the (name → version → arches) shape from the
// raw parsed manifest. We only consume `repository`.
func projectManifest(server string, raw map[string]any) (*remoteManifest, error) {
	repoRaw, ok := raw["repository"]
	if !ok {
		return nil, errors.New("missing `repository` key")
	}

	repoMap, ok := repoRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("repository is %T, want map", repoRaw)
	}

	out := &remoteManifest{
		server:     server,
		repository: map[string]map[string][]archEntry{},
	}

	for name, vAny := range repoMap {
		versions, ok := vAny.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("repository.%s is %T, want map", name, vAny)
		}

		inner := map[string][]archEntry{}

		for ver, entry := range versions {
			arr, err := toArchArr(entry)
			if err != nil {
				return nil, fmt.Errorf("repository.%s.%s: %w", name, ver, err)
			}

			inner[ver] = arr
		}

		out.repository[name] = inner
	}

	return out, nil
}

func toArchArr(v any) ([]archEntry, error) {
	switch arr := v.(type) {
	case []any:
		out := make([]archEntry, 0, len(arr))

		for i, e := range arr {
			em, ok := e.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("entry %d is %T, want map", i, e)
			}

			arch, _ := em["arch"].(string)
			out = append(out, archEntry{arch: arch})
		}

		return out, nil
	case map[string]any:
		// JSON serialization of the upstream manifest can flatten the single-
		// element arch array into a bare map. Accept that shape too.
		arch, _ := arr["arch"].(string)

		return []archEntry{{arch: arch}}, nil
	default:
		return nil, fmt.Errorf("expected array or map, got %T", v)
	}
}

// acceptedArches keeps only entries whose arch is in the default query arch
// set: the pseudo-arches src/all/rockspec/installed plus the host arch. This
// is the Go analogue of results:satisfies' query.arch membership test. An
// empty want falls back to the detected host arch.
func acceptedArches(arches []archEntry, want string) []archEntry {
	host := want
	if host == "" {
		host = hostArch()
	}

	out := make([]archEntry, 0, len(arches))

	for _, a := range arches {
		switch a.arch {
		case archSrc, "all", archRockspec, archInstalled, host:
			out = append(out, a)
		}
	}

	return out
}

// hostArch returns the LuaRocks cfg.arch string for the running host
// ("<os>-<cpu>", e.g. "linux-x86_64"), used as the default accepted binary
// arch. Best-effort mapping of Go's GOOS/GOARCH onto LuaRocks' uname-derived
// names.
func hostArch() string {
	return luaOSName(runtime.GOOS) + "-" + luaCPUName(runtime.GOARCH)
}

func luaOSName(goos string) string {
	switch goos {
	case "darwin":
		return "macosx"
	default:
		return goos
	}
}

func luaCPUName(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "386":
		return "x86"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

// makeRockURL constructs the resource URL for one rock entry, matching
// upstream `path.make_url(repo, name, version, arch)`.
//
//	arch == archRockspec   → <server>/<name>-<version>.rockspec
//	arch == "installed"  → <server>/<name>/<version>/<name>-<version>.rockspec
//	otherwise            → <server>/<name>-<version>.<arch>.rock
func makeRockURL(server, name, version, arch string) string {
	base := strings.TrimRight(server, "/") + "/"

	switch arch {
	case archRockspec:
		return base + name + "-" + version + ".rockspec"
	case archInstalled:
		return base + name + "/" + version + "/" + name + "-" + version + ".rockspec"
	default:
		return base + name + "-" + version + "." + arch + ".rock"
	}
}
