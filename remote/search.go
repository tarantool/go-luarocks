package remote

import (
	"context"
	"fmt"
	"sort"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
)

// ProvidedRepo is the pseudo-server upstream records for rocks the Lua VM
// supplies rather than a rock server (search.lua's `provided_repo`). A Match
// carrying it did not come off any configured server, so its URL is empty:
// there is no artifact to fetch.
const ProvidedRepo = "provided by VM or rocks_provided"

// Query is one search over the configured rock servers, the Go analogue of
// upstream's query object (queries.lua) as consumed by search.search_repos.
type Query struct {
	// Name is the rock name to look for. With Substring it is matched as a
	// plain (non-pattern, non-glob) substring of each manifest name, so an
	// empty Name plus Substring matches every rock — which is how upstream
	// implements `luarocks search --all`.
	Name string

	// Namespace restricts the search to one namespace. Non-empty makes each
	// server's manifest be read from <server>/manifests/<namespace>/ and
	// excludes VM-provided rocks, which have no namespace (results.lua:57).
	Namespace string

	// Substring selects substring matching over exact-name matching
	// (results.lua match_name). It is what `luarocks search` sets and what
	// `luarocks install` does not.
	Substring bool

	// Constraints are AND'd version constraints every match must satisfy. An
	// empty list accepts every version.
	Constraints []rocks.VersionConstraint

	// Arch is the set of accepted manifest arch values. Empty means the
	// upstream default set — the pseudo-arches src, all, rockspec and
	// installed plus the host arch (queries.lua:17-23 together with
	// query_mt.arch[cfg.arch] = true).
	//
	// A non-empty list means EXACTLY those arches. That is deliberate and
	// matches upstream: queries.new only adds cfg.arch to the fallback table
	// on the metatable, never to a set built by arch_to_table, so
	// `queries.new(name, ns, ver, false, "src|rockspec")` accepts src and
	// rockspec and nothing else.
	Arch []string

	// Provided maps the name of a rock the VM supplies to its
	// version-revision string (upstream util.get_rocks_provided). Matching
	// entries are appended to the result under ProvidedRepo with arch
	// "installed", exactly as search_repos does after walking the servers.
	// Nil means "the VM provides nothing", which is the right value for a
	// caller that only wants what the servers hold.
	Provided map[string]string
}

// Match is one (name, version, arch, server) row of a search result — the
// same tuple upstream's result tree stores and print_result_tree prints under
// --porcelain.
type Match struct {
	// Name is the rock name as the manifest spells it.
	Name string
	// Version is the version-revision string as the manifest spells it.
	Version string
	// Arch is the manifest arch of this particular offering: "rockspec" for a
	// bare .rockspec, "src" for a source rock, "all" for a pure-Lua binary
	// rock, a concrete "<os>-<cpu>" for a host-built one, or "installed" for
	// a VM-provided rock.
	Arch string
	// Server is the server the row came from, normalized the way
	// dir.normalize does for the porcelain listing: a local directory loses
	// its file:// prefix and any trailing slash, an HTTP server keeps its
	// scheme and loses the trailing slash. For a VM-provided rock it is
	// ProvidedRepo.
	Server string
	// Namespace is the namespace the row was searched under (empty when the
	// query carried none), mirroring manifest_search's `query.namespace`.
	Namespace string
	// URL locates the artifact, built by the same path.make_url rules the
	// indexes use for Query: an https:// URL for an HTTP server, an on-disk
	// path (or file:// URL, preserving the configured form) for a local
	// directory. Empty for a VM-provided rock, which has no artifact.
	URL string
}

// HostArch returns the LuaRocks cfg.arch string for the running host
// ("<os>-<cpu>", e.g. "linux-x86_64"). It is exported because the caller that
// splits a search result into "source" and "binary" halves needs the same
// string this package filters manifests with — upstream's
// split_source_and_binary_results compares each item's arch against cfg.arch.
func HostArch() string { return hostArch() }

// Search walks every server in order and returns every manifest row matching
// q, merged across ALL servers — upstream store_result appends rather than
// stopping at the first server that has the rock (search.lua:26-30), which is
// what makes `luarocks search` list the same rock from two mirrors. That is
// deliberately NOT the first-found-wins rule the install path applies through
// OrderedIndex: a search reports what exists, an install picks one.
//
// Each server is dispatched by its own form, exactly as NewIndex does: a
// local directory is read off disk, anything else is fetched over HTTP.
//
// The walk stops early once an exact-version match for q.Name is in hand
// ("stop searching repos if exact match was found", search.lua:151-155):
// "exact" means q has an `==` constraint and some row for the rock named
// exactly q.Name carries that version string. Servers after that point are
// never contacted.
//
// Error policy matches HTTPRemoteIndex.Query, because callers branch on it: a
// server that cannot be read is skipped so a sibling can still answer, and
// the failure is surfaced only when NOTHING was found and at least one server
// failed. A search that simply matches nothing is an empty, error-free
// result.
//
// The returned slice is ordered as print_result_tree prints it: names
// ascending, versions DESCENDING (util.sortedpairs with vers.compare_versions
// sorts newest first), and within one (name, version) the manifest's own item
// order, servers in the order given. Two distinct version strings that parse
// equal keep their insertion order.
func Search(
	ctx context.Context, servers []string, opts IndexOptions, q Query,
) ([]Match, error) {
	accepted := acceptedArchSet(q.Arch, opts.Arch)
	exact := exactVersion(q.Constraints)

	var (
		found   []Match
		loadErr error
	)

	for _, server := range servers {
		mf, dir, isLocal, err := loadServerManifest(ctx, server, q.Namespace, opts)
		if err != nil {
			if loadErr == nil {
				loadErr = fmt.Errorf("remote.Search: load %s: %w", server, err)
			}

			continue
		}

		found = append(found, matchManifest(mf, server, dir, isLocal, q, accepted)...)

		if exact != "" && hasExactMatch(found, q.Name, exact) {
			break
		}
	}

	found = append(found, matchProvided(q, accepted)...)

	// Only surface a failure when nothing was found AND at least one server
	// hard-failed; a rock genuinely absent everywhere is an empty result.
	if len(found) == 0 && loadErr != nil {
		return nil, loadErr
	}

	sortMatches(found)

	return found, nil
}

// loadServerManifest reads one server's manifest, dispatching on the server's
// form the way NewIndex does. It reports whether the server is a local
// directory and, if so, which directory, so the caller can build on-disk
// artifact paths.
func loadServerManifest(
	ctx context.Context, server, namespace string, opts IndexOptions,
) (*remoteManifest, string, bool, error) {
	if dir, ok := LocalServerPath(server); ok {
		idx := &FileRemoteIndex{Server: server, LuaVersion: opts.LuaVersion, Arch: opts.Arch}

		mf, err := idx.load(dir, namespace)
		if err != nil {
			return nil, "", true, err
		}

		return mf, dir, true, nil
	}

	idx := &HTTPRemoteIndex{
		Servers:         []string{server},
		InsecureServers: opts.InsecureServers,
		UserAgent:       opts.UserAgent,
		LuaVersion:      opts.LuaVersion,
		Arch:            opts.Arch,
	}

	mf, err := idx.load(ctx, server, namespace)
	if err != nil {
		return nil, "", false, err
	}

	return mf, "", false, nil
}

// matchManifest turns one server's manifest into the rows matching q. dir is
// the local directory the server names when isLocal is set.
func matchManifest(
	mf *remoteManifest, server, dir string, isLocal bool, q Query, accepted map[string]bool,
) []Match {
	normalized := NormalizeServer(server)

	// Names are walked in sorted order so the rows this server contributes
	// are already grouped deterministically; the final ordering is imposed by
	// sortMatches regardless, but a deterministic intermediate keeps the
	// insertion order (the tiebreak) reproducible run to run — Go map
	// iteration is not.
	names := make([]string, 0, len(mf.repository))
	for name := range mf.repository {
		names = append(names, name)
	}

	sort.Strings(names)

	var out []Match

	for _, name := range names {
		if !matchName(q, name) {
			continue
		}

		versions := mf.repository[name]

		vers := make([]string, 0, len(versions))
		for version := range versions {
			vers = append(vers, version)
		}

		sort.Strings(vers)

		for _, version := range vers {
			v, err := deps.ParseVersion(version)
			if err != nil {
				// An unparsable version key is skipped, not fatal: it must not
				// hide the versions of the same rock that do parse.
				continue
			}

			if !deps.Match(v, q.Constraints) {
				continue
			}

			for _, entry := range versions[version] {
				if !accepted[entry.arch] {
					continue
				}

				out = append(out, Match{
					Name:      name,
					Version:   version,
					Arch:      entry.arch,
					Server:    normalized,
					Namespace: q.Namespace,
					URL:       artifactURL(server, dir, isLocal, name, version, entry.arch),
				})
			}
		}
	}

	return out
}

// artifactURL builds the artifact location for one row, by the same
// path.make_url rules the indexes use: an on-disk path (keeping whichever of
// the bare-path / file:// forms the server was configured as) for a local
// directory, a URL on the server otherwise.
func artifactURL(server, dir string, isLocal bool, name, version, arch string) string {
	if isLocal {
		return localRockURL(dir, filePrefixOf(server), name, version, arch)
	}

	return makeRockURL(server, name, version, arch)
}

// matchProvided returns the rows for rocks the VM supplies, appended by
// search_repos after every server has been walked. They carry arch
// "installed" and no namespace, so a namespaced query never matches one
// (results.lua:57) and an explicit Arch set that omits "installed" filters
// them out — both of which upstream gets from the same satisfies() call.
func matchProvided(q Query, accepted map[string]bool) []Match {
	if len(q.Provided) == 0 || !accepted[archInstalled] || q.Namespace != "" {
		return nil
	}

	names := make([]string, 0, len(q.Provided))
	for name := range q.Provided {
		names = append(names, name)
	}

	sort.Strings(names)

	var out []Match

	for _, name := range names {
		if !matchName(q, name) {
			continue
		}

		version := q.Provided[name]

		v, err := deps.ParseVersion(version)
		if err != nil {
			continue
		}

		if !deps.Match(v, q.Constraints) {
			continue
		}

		out = append(out, Match{
			Name:    name,
			Version: version,
			Arch:    archInstalled,
			Server:  ProvidedRepo,
		})
	}

	return out
}

// matchName is results.lua match_name: a plain substring test when the query
// asks for one (string.find with plain=true — NOT a Lua pattern and not a
// glob), an exact comparison otherwise.
func matchName(q Query, name string) bool {
	if q.Substring {
		return strings.Contains(name, q.Name)
	}

	return name == q.Name
}

// exactVersion returns the version string of the query's first `==`
// constraint, or "" when it has none. It is what search_repos compares
// against the result tree to decide it can stop contacting servers.
func exactVersion(cs []rocks.VersionConstraint) string {
	for _, c := range cs {
		if c.Op == "==" {
			return c.Version.Raw
		}
	}

	return ""
}

// hasExactMatch reports whether the rows gathered so far include the rock
// named exactly name at exactly version. Upstream indexes its result tree by
// the query name, so a substring query that matched several rocks only stops
// the walk when the rock named exactly as queried is among them.
func hasExactMatch(found []Match, name, version string) bool {
	if name == "" {
		return false
	}

	for _, m := range found {
		if m.Name == name && m.Version == version {
			return true
		}
	}

	return false
}

// acceptedArchSet builds the set of manifest arch values a query accepts.
// An empty want yields the upstream default set; a non-empty one is taken
// verbatim (see Query.Arch for why cfg.arch is not added to it).
func acceptedArchSet(want []string, hostOverride string) map[string]bool {
	if len(want) > 0 {
		out := make(map[string]bool, len(want))
		for _, a := range want {
			out[a] = true
		}

		return out
	}

	host := hostOverride
	if host == "" {
		host = hostArch()
	}

	return map[string]bool{
		archSrc:       true,
		"all":         true,
		archRockspec:  true,
		archInstalled: true,
		host:          true,
	}
}

// sortMatches imposes print_result_tree's porcelain order in place: names
// ascending, versions descending (newest first — vers.compare_versions is a
// `>` comparison), insertion order preserved within one (name, version).
func sortMatches(matches []Match) {
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		if a.Version == b.Version {
			return false
		}

		av, aerr := deps.ParseVersion(a.Version)
		bv, berr := deps.ParseVersion(b.Version)

		if aerr != nil || berr != nil {
			return false
		}

		return deps.Compare(av, bv) > 0
	})
}

// NormalizeServer renders a server string the way dir.normalize does for the
// porcelain listing, which is what makes a Match.Server comparable with the
// lua backend's output: forward slashes, no trailing slash, no `.` segments,
// `..` resolved, and the `file://` scheme dropped so a local directory reads
// as the plain path it is. Any other scheme is kept.
func NormalizeServer(server string) string {
	proto, path := splitURL(server)

	path = strings.ReplaceAll(path, "\\", "/")

	// gsub("(.)/*$", "%1"): drop trailing slashes but never the whole string.
	for len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}

	path = strings.ReplaceAll(path, "//", "/")

	// A Windows drive letter is held aside so it is not mistaken for a path
	// segment (dir.normalize does the same with its `^.:` match).
	drive := ""
	if len(path) >= 2 && path[1] == ':' {
		drive, path = path[:2], path[2:]
	}

	// strings.Split(path, "/") yields exactly the chunks Lua's
	// gmatch("(.-)/") does over path.."/", including the leading empty one
	// that stands for the root.
	var pieces []string

	for piece := range strings.SplitSeq(path, "/") {
		n := len(pieces)

		switch {
		case piece == ".":
			// A "." segment contributes nothing.
		case piece != "..":
			pieces = append(pieces, piece)
		case n == 0 || pieces[n-1] == "..":
			pieces = append(pieces, piece)
		case pieces[n-1] != "":
			pieces = pieces[:n-1]
		default:
			// ".." at the root cancels itself: upstream neither pops the
			// empty root segment nor keeps the "..".
		}
	}

	switch {
	case len(pieces) == 0:
		path = drive + "."
	case len(pieces) == 1 && pieces[0] == "":
		path = drive + "/"
	default:
		path = drive + strings.Join(pieces, "/")
	}

	// The scheme test is case-insensitive where upstream's is not, so it
	// agrees with LocalServerPath: this package already reads a `FILE://`
	// server off disk, and reporting it with a scheme upstream could not have
	// resolved in the first place would only make the two disagree.
	if !strings.EqualFold(proto, "file") {
		path = proto + "://" + path
	}

	return path
}

// splitURL is dir.split_url: it returns the protocol and the path, defaulting
// the protocol to "file" for a string that carries none, and unquoting a
// value wrapped in matching quotes.
func splitURL(url string) (string, string) {
	url = unquote(url)

	if i := strings.Index(url, "://"); i >= 0 && !strings.Contains(url[:i], ":") {
		return url[:i], url[i+len("://"):]
	}

	return "file", url
}

// quotedMinLen is the shortest string that can carry a matching pair of
// quotes: the two quotes themselves, around an empty value.
const quotedMinLen = 2

// unquote strips one layer of matching single or double quotes, as
// dir.split_url does before parsing.
func unquote(s string) string {
	if len(s) >= quotedMinLen {
		first, last := s[0], s[len(s)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return s[1 : len(s)-1]
		}
	}

	return s
}
