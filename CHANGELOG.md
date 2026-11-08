# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

Requires Go 1.26 or later, which `golang.org/x/crypto` v0.56.0 declares; that
version carries the ssh fixes for GO-2026-6354 and GO-2026-6355.

### Added

- deps: Version parsing, dependency constraint matching, and a transitive
  dependency resolver.
- rockspec: A sandboxed `gopher-lua`-based evaluator for `.rockspec` files,
  including conditional (platform-guarded) rockspecs.
- manif: A manifest reader and writer compatible with the upstream LuaRocks
  manifest format. A `rock_manifest` entry outside its `lua`, `lib`, `bin`,
  `conf` and `doc` sections is kept in `RockManifest.Other`.
- fetch: Source fetch backends for `http(s)`, `git`/`git+https`/`git+ssh`,
  and `file://` sources. `Sources` returns a `Result` that says whether the
  path is already the source root. `File` retrieves a single file into a
  directory without unpacking it (upstream `download.lua`'s `get_file`).
- fetch, remote: HTTP requests share process-wide transports, cloned from
  `http.DefaultTransport`, so connections are reused and `HTTP_PROXY`,
  `HTTPS_PROXY` and `NO_PROXY` are honoured.
- tree: On-disk rock tree layout, deployment, conflict detection, and
  `which`-style lookup. A `lua`/`lib`/`bin` subtree installed by any build
  backend is deployed like the rockspec's own sections, `build.modules` is
  read for `builtin` rocks only, and the default `copy_directories` and docs
  of upstream's `install_files` apply. A rock's `rock_manifest` lists every
  file of its install dir under its plain path, and `ModuleIndex` and
  `CommandIndex` derive its modules and commands from it, as upstream
  `repos.package_modules` and `package_commands` do. When several rocks
  provide a file, `Tree.Providers` names them per `ItemKind` (module or
  command), and `Deploy` gives the plain path to the one upstream would —
  demoting both forms of a module, and backing up a file in its way that
  another rock or version owns, or none does, without overwriting earlier
  backups — and `DeleteVersion` passes it on to the next provider. A
  command wrapper puts the module directories of the tree it is deployed to
  on `package.path` and `package.cpath`.
- build: Build backends — `builtin`, `cmake`, `make`, and `command`.
  `LUA_BINDIR` is derived from `Tarantool.Executable` when it has a
  directory component, so a flat Tarantool SDK with no `bin/` works. The
  `make` and `cmake` backends install into the rock's install dir, as
  upstream's `$(PREFIX)`, `$(LUADIR)`, … name it; `cmake` builds out of tree
  in `build.luarocks` and installs under the rock's `$(PREFIX)` when the
  rockspec sets no `CMAKE_INSTALL_PREFIX`.
- remote: Indexes for rock servers that are HTTP(S) endpoints or local
  directories (`/srv/rocks` or `file:///srv/rocks`) — the offline case
  upstream serves with `luarocks --only-server=/path/to/repo`. `NewIndex`
  picks the index a server's form implies, `NewIndexes` does it for a whole
  server list, and `OrderedIndex` queries any mix of the two in order,
  first-found-wins. A `ManifestCache`, set through `IndexOptions.Cache`,
  shares fetched manifests between indexes.
- remote: `Search`, a port of upstream `search.search_repos` over the same
  manifest loaders the indexes use — substring or exact name matching,
  version constraints, the arch filter, a cross-server merge, the
  exact-version early stop, and the rocks the Lua VM provides.
- client: A `Rocks` facade with a pure-Go native engine (the default) and a
  vendored upstream LuaRocks 3.9.2 engine (driven through `gopher-lua`),
  selectable per client, exposing `Install`, `Build`, `Make`, `Pack`,
  `Unpack`, `Remove`, `List`, `Show`, and `Which` on both. The lua engine
  also serves `Purge`, `Lint`, `NewVersion`, `WriteRockspec`, `Doc`,
  `Test`, `Config`, `Upload`, `InitProject`, `Admin`, and `Exec` (the full
  upstream CLI); the native engine returns `rocks.ErrNotImplemented` for
  them.
- client: The native engine keeps go-luarocks' own semantics where they
  differ from upstream's, which the lua engine runs, as the README's
  "Backend Parity" lists: `Install`, `Build` and `Make` keep the other
  installed versions of a rock; `Build` and `Make` install no dependencies,
  `build_dependencies` included; `Remove` does not check whether another
  rock depends on the one it removes, so `RemoveOpts.Force` and `ForceFast`
  apply to the lua engine only; `Which` looks a module up by its plain paths
  in `Config.Tree`; and `BuildOpts.Keep` keeps the native staging build
  directory, while the lua engine passes it to upstream as `--keep`.
- client: `InstallOpts.Deps` and `RemoveOpts.Deps` take a `DepsPolicy`:
  `DepsAll`, the zero value, `DepsNone` and `DepsOnlyNew`. The native
  engine resolves a rock's transitive dependencies and installs, in
  topological order, every one a rock in `Config.Tree` does not satisfy,
  unless `DepsNone`, which installs the rock alone; it consults no other
  rock tree. The lua engine passes the policy to upstream as `--deps-mode`
  `all`, `none` or `order` on `Install` and `Remove`.
- client: `Config.Servers` and `InstallOpts.Servers` accept a local
  directory wherever they accept an HTTP(S) URL, so `Install` can work
  entirely offline from a mirror on disk. A server list is queried in
  configuration order, first-found-wins, and a client fetches the manifest
  of each server once for its lifetime.
- client: The native backend installs a rock from the artifact the index
  chose, as upstream `install` does: a `.all.rock` or host-arch `.rock` is
  deployed as built, a `.src.rock` is built from its bundled sources, and
  only a bare `.rockspec` fetches `source.url`. The tree manifest's
  `modules` and `commands` indexes are derived from each rock's deployed
  `rock_manifest`, so they list the modules and commands of `cmake`, `make`
  and `command` rocks too, and their provider lists keep upstream's order.
  Each rock's resolved `dependencies` are recorded when it is deployed, from
  its direct dependencies.
- client: The lua backend runs each operation in its own VM, so no state
  carries over between calls. It honours `Config.Servers`, reads
  `/etc/luarocks/config-5.1.lua` (`LUAROCKS_SYSCONFDIR` overrides it), and
  loads Lua modules only from its embedded copy and the rock trees, never
  from the current directory, `$LUA_PATH` or system directories. The error
  of a failed command carries the message LuaRocks printed. Its typed
  methods run over the rock trees `Config.Tree` and
  `Config.Tarantool.Prefix` whatever a config file sets, so `Config.Tree` is
  always one of them, as upstream assumes of the tree it installs into;
  `Exec` runs over the configured trees.
- client: `Search` on the native backend (upstream `luarocks search`), with
  `--source` / `--binary` / `--all` and results identical to the lua
  backend's. `SearchResult` carries `Arch` and `Namespace`, so a caller can
  tell a rockspec from an installable binary rock.
- client: `Download` on the native backend (upstream `luarocks download`),
  with `--source` / `--rockspec` / `--arch` / `--all` and the same files,
  bytes and returned paths as the lua backend. The three arch flags are
  mutually exclusive, as upstream's parser makes them.
- Runnable, godoc-rendered examples across all public packages.
