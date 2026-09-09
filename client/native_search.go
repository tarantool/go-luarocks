package client

// The native backend's `luarocks search`. The server walk itself lives in
// remote.Search (a port of search.search_repos); what this file adds is the
// command layer of upstream's cmd/search.lua — argument handling, the
// source/binary split, and the set of rocks the Lua VM provides.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/remote"
)

// luaDialect is the Lua version the Tarantool fork targets. It is the "5.1"
// in `lua 5.1-1`, the version of the `lua` rock the VM provides.
const luaDialect = "5.1"

// archAll is the manifest arch of a rock that is binary but portable (pure
// Lua). Together with the host arch it is what makes an item a "binary"
// result rather than a source one (cmd/search.lua
// split_source_and_binary_results).
const archAll = "all"

// defaultLuaBinDir is hardcoded.lua's fallback for LUA_BINDIR when no
// Tarantool prefix is configured.
const defaultLuaBinDir = "/usr/bin"

// luajitProbeTimeout bounds the one interpreter invocation used to learn the
// LuaJIT version. It is deliberately short: the answer is a nicety (it names
// the version of the `luajit`/`luabitop` rocks the VM provides), never a
// prerequisite for searching a server.
const luajitProbeTimeout = 10 * time.Second

// jitVersionPrefixLen is the length of the "LuaJIT " prefix `jit.version`
// carries, which upstream strips with jit.version:sub(8).
const jitVersionPrefixLen = 7

// namespacedNameRE is util.split_namespace's pattern: exactly one slash, with
// a non-empty name on each side.
var namespacedNameRE = regexp.MustCompile(`^([^/]+)/([^/]+)$`)

// Search queries the configured rock servers for rocks matching pattern,
// reproducing upstream `luarocks search` (cmd/search.lua) over the native
// index.
//
// pattern is matched as a plain substring of each rock's name — not a glob
// and not a regular expression — and is lower-cased along with any
// `namespace/name` prefix, exactly as util.namespaced_name_action does before
// the query is built. opts.Version adds an `==` constraint (a full constraint
// expression such as ">= 1.0" is accepted too, which is a superset of what
// upstream's positional takes).
//
// Results are ordered as `search --porcelain` prints them: the rockspec and
// source-rock block first, then the binary block, each with names ascending
// and versions descending. opts.Source keeps only the first block,
// opts.Binary only the second; setting both keeps neither, which is what
// upstream's two independent `if` guards do.
//
// A search that matches nothing is an empty slice and a nil error. A server
// that cannot be read is skipped so a sibling can still answer, and the
// failure surfaces only when nothing at all was found.
//
// Two deliberate differences from the lua backend, both documented because
// they are visible in the results:
//
//   - An empty pattern without opts.All is an error here, where upstream
//     rejects only a MISSING positional. The Go API cannot tell an omitted
//     argument from an empty string, and the alternative reading — listing
//     every rock on every server — is what opts.All exists to ask for.
//   - The `luajit` and `luabitop` versions this reports for the VM come from
//     probing the configured interpreter, as upstream does; when no
//     interpreter can be run they are omitted entirely rather than guessed.
func (e *nativeEngine) Search(
	ctx context.Context, pattern string, opts SearchOpts,
) ([]SearchResult, error) {
	name, version := pattern, opts.Version

	// --all searches for everything: upstream blanks both the name and the
	// version before building the query (cmd/search.lua:57-59).
	if opts.All {
		name, version = "", ""
	} else if pattern == "" {
		return nil, errors.New(
			"rocks.Search: enter a name and version, or set SearchOpts.All to list everything")
	}

	name, namespace := splitNamespacedName(name)

	cs, err := deps.ParseConstraints(version)
	if err != nil {
		return nil, fmt.Errorf("rocks.Search: parse version %q: %w", version, err)
	}

	servers := searchServers(opts.Servers, e.cfg.Servers)
	if len(servers) == 0 {
		return nil, errors.New("rocks.Search: no servers configured")
	}

	matches, err := remote.Search(ctx, servers, remote.IndexOptions{
		InsecureServers: e.cfg.InsecureServers,
	}, remote.Query{
		Name:        name,
		Namespace:   namespace,
		Substring:   true,
		Constraints: cs,
		//nolint:contextcheck // rocksProvided probes the interpreter under its own short deadline by design: the answer is memoized for the engine's life, and a cancelled search must not cache a failure. See luajitVersion.
		Provided: e.rocksProvided(),
	})
	if err != nil {
		return nil, fmt.Errorf("rocks.Search: %w", err)
	}

	return splitSourceAndBinaryResults(matches, opts), nil
}

// searchServers is upstream's --server handling: the caller's servers are
// searched BEFORE the configured ones, not instead of them (cmd.lua:130
// concatenates them onto the front of cfg.rocks_servers). See the comment on
// SearchOpts.Servers for why this differs from InstallOpts.Servers.
func searchServers(override, configured []string) []string {
	out := make([]string, 0, len(override)+len(configured))
	out = append(out, override...)
	out = append(out, configured...)

	return out
}

// splitNamespacedName is util.namespaced_name_action: an argument that names
// a file is taken verbatim, anything else is split on a single slash into
// namespace and name and lower-cased.
func splitNamespacedName(arg string) (string, string) {
	if strings.HasSuffix(arg, ".rockspec") || strings.HasSuffix(arg, ".rock") {
		return arg, ""
	}

	if m := namespacedNameRE.FindStringSubmatch(arg); m != nil {
		return strings.ToLower(m[2]), strings.ToLower(m[1])
	}

	return strings.ToLower(arg), ""
}

// splitSourceAndBinaryResults is cmd/search.lua's
// split_source_and_binary_results plus the two guards that print the halves:
// a match is "binary" when its arch is portable ("all") or the host's, and
// --source / --binary each suppress the other half. Both blocks keep the
// order remote.Search established, so the concatenation is still sorted
// within each block — which is exactly what the porcelain listing shows.
func splitSourceAndBinaryResults(matches []remote.Match, opts SearchOpts) []SearchResult {
	host := remote.HostArch()

	sources := make([]SearchResult, 0, len(matches))
	binaries := make([]SearchResult, 0, len(matches))

	for _, m := range matches {
		res := SearchResult{
			Name:      m.Name,
			Version:   m.Version,
			Arch:      m.Arch,
			Server:    m.Server,
			Namespace: m.Namespace,
		}

		if m.Arch == archAll || m.Arch == host {
			binaries = append(binaries, res)

			continue
		}

		sources = append(sources, res)
	}

	out := make([]SearchResult, 0, len(matches))

	if !opts.Binary {
		out = append(out, sources...)
	}

	if !opts.Source {
		out = append(out, binaries...)
	}

	return out
}

// rocksProvided is util.get_rocks_provided for the Tarantool fork: the `lua`
// rock the dialect stands for, the LuaJIT-versioned `luajit`/`luabitop` pair
// when the interpreter can be asked, and `tarantool` itself when a version is
// configured. Upstream appends these to every search result under a pseudo
// server, so a user searching for `lua` is told the VM already provides it
// instead of being offered a rock to install.
func (e *nativeEngine) rocksProvided() map[string]string {
	provided := map[string]string{"lua": luaDialect + "-1"}

	if ljv := e.luajitVersion(); ljv != "" {
		provided["luabitop"] = ljv + "-1"
		provided["luajit"] = ljv + "-1"
	}

	// Upstream reads TT_CLI_TARANTOOL_VERSION and keeps everything before the
	// first dash, so a bare "3.9.0" with no revision yields no entry at all.
	if before, _, ok := strings.Cut(e.cfg.Tarantool.Version, "-"); ok && before != "" {
		provided["tarantool"] = before + "-1"
	}

	return provided
}

// luajitVersion asks the configured interpreter for its LuaJIT version, the
// way util.get_luajit_version does — `jit.version` minus its "LuaJIT "
// prefix. It returns "" when there is no interpreter to ask or it is not
// LuaJIT-based, which is upstream's outcome too (it drops the luajit and
// luabitop entries rather than inventing a version).
//
// The result is memoized for the life of the engine: the interpreter cannot
// change under a running client, and a search must not pay for a subprocess
// twice. The probe runs under its own short deadline rather than the caller's
// context, so a cancelled search cannot poison the cached answer.
func (e *nativeEngine) luajitVersion() string {
	e.luajitOnce.Do(func() {
		binDir := defaultLuaBinDir
		if e.cfg.Tarantool.Prefix != "" {
			binDir = filepath.Join(e.cfg.Tarantool.Prefix, "bin")
		}

		interp := filepath.Join(binDir, luaInterpreter)

		ctx, cancel := context.WithTimeout(context.Background(), luajitProbeTimeout)
		defer cancel()

		// `os.exit()` is what upstream appends for a tarantool interpreter,
		// which otherwise drops into its interactive console after -e.
		cmd := exec.CommandContext(ctx, interp, "-e",
			"io.write(tostring(jit and jit.version)) os.exit()")

		// Only stdout is captured: tarantool writes its own diagnostics to
		// stderr, and folding them into the value would make a warning look
		// like a version.
		out, err := cmd.Output()
		if err != nil {
			e.logger.Debug("rocks.Search: no LuaJIT version from interpreter",
				"interpreter", interp, "err", err)

			return
		}

		v := strings.TrimSpace(string(out))
		if !strings.HasPrefix(v, "LuaJIT ") || len(v) <= jitVersionPrefixLen {
			return
		}

		e.luajit = v[jitVersionPrefixLen:]
	})

	return e.luajit
}
