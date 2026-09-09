package client

// The native backend's `luarocks download`. The server walk is remote.Search
// (search.search_repos) and the retrieval is fetch.File (download.lua's
// get_file); what this file adds is upstream's cmd/download.lua plus the two
// search.lua helpers between them — find_suitable_rock, which turns a query
// into ONE artifact, and pick_latest_version, which decides which of a
// version's offerings that artifact is.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/remote"
)

const (
	// archSrc is the manifest arch of a packaged source rock (`.src.rock`).
	archSrc = "src"
	// archRockspec is the manifest arch of a bare `.rockspec`.
	archRockspec = "rockspec"
	// archInstalled is the manifest arch of a row that names an installed
	// rock rather than a downloadable artifact — what a tree manifest and the
	// VM-provided pseudo-server carry. download.lua skips it explicitly.
	archInstalled = "installed"
)

// Download fetches one rock file from the configured rock servers into
// e.cfg.WorkingDir and returns the path it wrote, reproducing upstream
// `luarocks download` (cmd/download.lua over download.download).
//
// Which file is fetched is find_suitable_rock's answer: the query must match
// exactly one rock name, its highest matching version is taken, and among
// that version's offerings a binary rock wins over a source rock, which wins
// over a bare rockspec (pick_latest_version, search.lua:195-217). opts.Source,
// opts.Rockspec and opts.Arch each narrow the accepted arches instead, and
// upstream makes them mutually exclusive — so more than one set is an error
// here rather than a silent pick.
//
// opts.All downloads EVERY match rather than one, which is also what makes an
// empty name legal: upstream turns `--all` with no name into a substring
// query for "" (download.lua:26). A failed member does not abort the rest;
// the error at the end lists them all. Rows the VM provides — arch
// "installed" — are skipped, since they name no artifact.
//
// The return value under opts.All follows the lua backend's rule, so the two
// are comparable: the single file that appeared in the working directory when
// exactly one did, and the working directory itself otherwise (nothing new,
// or several). That backend has no better answer available — the embedded
// LuaRocks prints no path and it can only diff the directory — and matching
// it is worth more than being marginally more precise here.
//
// One deliberate difference from the lua backend, in the single-file case:
// this returns the path it wrote even when that overwrote an existing file,
// where the directory diff sees no new file and answers with the directory.
// The native backend knows the name it saved and does not have to guess.
func (e *nativeEngine) Download(ctx context.Context, name string, opts DownloadOpts) (string, error) {
	arch, err := downloadArch(opts)
	if err != nil {
		return "", err
	}

	// Upstream rejects a MISSING name positional ("Argument missing"); the Go
	// API cannot tell that from an empty string, and --all is what asks for
	// everything — the same reading Search documents.
	if name == "" && !opts.All {
		return "", errors.New(
			"rocks.Download: enter a rock name, or set DownloadOpts.All to download every match")
	}

	if e.cfg.WorkingDir == "" {
		return "", errors.New("rocks.Download: no working directory configured")
	}

	rockName, namespace := splitNamespacedName(name)

	cs, err := deps.ParseConstraints(opts.Version)
	if err != nil {
		return "", fmt.Errorf("rocks.Download: parse version %q: %w", opts.Version, err)
	}

	servers := searchServers(opts.Servers, e.cfg.Servers)
	if len(servers) == 0 {
		return "", errors.New("rocks.Download: no servers configured")
	}

	q := remote.Query{
		Name:      rockName,
		Namespace: namespace,
		// download.lua:26 — `--all` with a name still matches that exact name;
		// only `--all` with no name at all widens to a substring query.
		Substring:   opts.All && rockName == "",
		Constraints: cs,
		Arch:        arch,
		// No Provided: a VM-provided row could only match the name queried
		// here (Substring is false whenever a name was given), and that name
		// is refused outright by find_suitable_rock below. Under --all with no
		// name the rows would match and then be dropped for their "installed"
		// arch, as download.download drops them.
		Provided: nil,
	}

	if opts.All {
		return e.downloadAll(ctx, servers, q, opts)
	}

	return e.downloadOne(ctx, servers, q, opts)
}

// downloadArch is cmd/download.lua's flag-to-arch mapping. The three flags
// form a parser mutex upstream (cmd:mutex, cmd/download.lua:19-22), so a
// caller that sets two of them is asking for something upstream's argv cannot
// express.
//
// The nil return for "none set" is the default accepted arch set, NOT the
// empty set — see remote.Query.Arch.
func downloadArch(opts DownloadOpts) ([]string, error) {
	set := 0

	for _, on := range []bool{opts.Source, opts.Rockspec, opts.Arch != ""} {
		if on {
			set++
		}
	}

	if set > 1 {
		return nil, errors.New(
			"rocks.Download: DownloadOpts.Source, Rockspec and Arch are mutually exclusive")
	}

	switch {
	case opts.Source:
		return []string{archSrc}, nil
	case opts.Rockspec:
		return []string{archRockspec}, nil
	case opts.Arch != "":
		return []string{opts.Arch}, nil
	default:
		return nil, nil
	}
}

// downloadOne is search.find_rock_checking_lua_versions over
// find_suitable_rock: resolve the query to a single artifact URL and fetch it.
// check_lua_versions is not offered, so the "notfound" case always carries the
// line upstream appends when it is off.
func (e *nativeEngine) downloadOne(
	ctx context.Context, servers []string, q remote.Query, opts DownloadOpts,
) (string, error) {
	// find_suitable_rock refuses a rock the VM already supplies before it
	// consults any server, and it does so regardless of the version asked for
	// (search.lua:248-252 tests presence, not the constraint).
	//nolint:contextcheck // rocksProvided probes the interpreter under its own short deadline by design: the answer is memoized for the engine's life, and a cancelled download must not cache a failure. See luajitVersion.
	if version, ok := e.rocksProvided()[q.Name]; ok {
		return "", downloadNotFound(q, opts, fmt.Sprintf(
			"Rock %s %s is already provided by VM or via 'rocks_provided' in the config file.",
			q.Name, version))
	}

	matches, err := remote.Search(ctx, servers, remote.IndexOptions{
		InsecureServers: e.cfg.InsecureServers,
	}, q)
	if err != nil {
		return "", fmt.Errorf("rocks.Download: %w", err)
	}

	if len(matches) == 0 {
		return "", downloadNotFound(q, opts,
			"No results matching query were found for Lua "+luaDialect+".\n"+
				"To check if it is available for other Lua versions, use --check-lua-versions.")
	}

	// find_suitable_rock indexes its result tree by name and refuses more than
	// one key. An exact-name query cannot normally produce two, which is why
	// upstream calls it a shouldn't-happen.
	for _, m := range matches {
		if m.Name != matches[0].Name {
			return "", downloadNotFound(q, opts, "Several rocks matched query.")
		}
	}

	pick, ok := pickLatestVersion(matches)
	if !ok {
		return "", downloadNotFound(q, opts,
			"No results matching query were found for Lua "+luaDialect+".")
	}

	path, err := fetch.File(ctx, pick.URL, e.cfg.WorkingDir, fetch.Options{
		InsecureServers: e.cfg.InsecureServers,
	})
	if err != nil {
		return "", fmt.Errorf("rocks.Download: %w", err)
	}

	e.logger.Info("rocks.Download: downloaded",
		"name", pick.Name, "version", pick.Version, "arch", pick.Arch, "path", path)

	return path, nil
}

// downloadAll is download.download's `all` branch: fetch every match, keep
// going past a failure, and report the failures together at the end.
func (e *nativeEngine) downloadAll(
	ctx context.Context, servers []string, q remote.Query, opts DownloadOpts,
) (string, error) {
	matches, err := remote.Search(ctx, servers, remote.IndexOptions{
		InsecureServers: e.cfg.InsecureServers,
	}, q)
	if err != nil {
		return "", fmt.Errorf("rocks.Download: %w", err)
	}

	// The directory snapshot is how the lua backend answers this call, and
	// matching its answer is the point — see the method doc.
	before := dirFiles(e.cfg.WorkingDir)

	var (
		found    bool
		failures []string
	)

	for _, m := range matches {
		// "Ignore provided rocks" (download.lua:38): an installed row names no
		// artifact to fetch.
		if m.Arch == archInstalled {
			continue
		}

		found = true

		if _, err := fetch.File(ctx, m.URL, e.cfg.WorkingDir, fetch.Options{
			InsecureServers: e.cfg.InsecureServers,
		}); err != nil {
			failures = append(failures, err.Error())
		}
	}

	if !found {
		return "", downloadNotFound(q, opts,
			"No results matching query were found for Lua "+luaDialect+".\n"+
				"To check if it is available for other Lua versions, use --check-lua-versions.")
	}

	if len(failures) > 0 {
		return "", fmt.Errorf("rocks.Download: %s", strings.Join(failures, "\n"))
	}

	var added []string

	for file := range dirFiles(e.cfg.WorkingDir) {
		if !before[file] {
			added = append(added, filepath.Join(e.cfg.WorkingDir, file))
		}
	}

	if len(added) == 1 {
		return added[0], nil
	}

	return e.cfg.WorkingDir, nil
}

// pickLatestVersion is search.lua's function of the same name (195-217),
// applied to the rows of one rock: take the highest version, then pick one of
// that version's offerings.
//
// The pick rule reads oddly and is copied deliberately. A binary arch —
// anything that is neither "src" nor "rockspec" — always wins, and among
// several the LAST one in manifest order does; a source rock only displaces a
// bare rockspec. So the preference is binary > src > rockspec, with manifest
// order breaking a tie between binaries.
//
// The highest version is computed here rather than read off remote.Search's
// descending order: the two agree, and not depending on that is what keeps
// this correct if the ordering the search imposes ever changes.
func pickLatestVersion(matches []remote.Match) (remote.Match, bool) {
	best := ""

	// Never read before best is set, so the zero value is inert.
	var bestParsed rocks.Version

	for _, m := range matches {
		v, err := deps.ParseVersion(m.Version)
		if err != nil {
			continue
		}

		if best == "" || deps.Compare(v, bestParsed) > 0 {
			best, bestParsed = m.Version, v
		}
	}

	if best == "" {
		return remote.Match{}, false
	}

	pick := -1

	for i, m := range matches {
		if m.Version != best {
			continue
		}

		if pick < 0 {
			pick = i

			continue
		}

		if (m.Arch == archSrc && matches[pick].Arch == archRockspec) ||
			(m.Arch != archSrc && m.Arch != archRockspec) {
			pick = i
		}
	}

	return matches[pick], true
}

// downloadNotFound wraps a search failure the way download.download does:
// "Could not find a result named <namespace/name version>: <reason>"
// (download.lua:59-60, over util.format_rock_name).
func downloadNotFound(q remote.Query, opts DownloadOpts, reason string) error {
	rock := q.Name
	if q.Namespace != "" {
		rock = q.Namespace + "/" + rock
	}

	if opts.Version != "" {
		rock += " " + opts.Version
	}

	return fmt.Errorf("rocks.Download: could not find a result named %s: %s", rock, reason)
}
