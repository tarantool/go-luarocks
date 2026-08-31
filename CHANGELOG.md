# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

### Added

- deps: Version parsing, dependency constraint matching, and a transitive
  dependency resolver.
- rockspec: A sandboxed `gopher-lua`-based evaluator for `.rockspec` files,
  including conditional (platform-guarded) rockspecs.
- manif: A manifest reader and writer compatible with the upstream LuaRocks
  manifest format.
- fetch: Source fetch backends for `http(s)`, `git`/`git+https`/`git+ssh`,
  and `file://` sources.
- tree: On-disk rock tree layout, deployment, conflict detection, and
  `which`-style lookup.
- build: Build backends — `builtin`, `cmake`, `make`, and `command`.
- remote: An HTTP client for the LuaRocks remote manifest index.
- client: A `Rocks` facade with a pure-Go native engine and a vendored
  upstream LuaRocks 3.9.2 engine (driven through `gopher-lua`), selectable
  per client, exposing `Install`, `Build`, `Make`, `Pack`, `Unpack`, `List`,
  `Show`, and `Which`.
- remote: A file-backed registry index, so a rock server may be a local
  directory (`/srv/rocks` or `file:///srv/rocks`) as well as an HTTP(S)
  endpoint — the offline case upstream serves with
  `luarocks --only-server=/path/to/repo`. `NewIndex` picks the index a
  server's form implies, `NewIndexes` does it for a whole configured server
  list, and `OrderedIndex` queries any mix of the two in order.
- client: `Config.Servers` and `InstallOpts.Servers` accept a local directory
  wherever they accept an HTTP(S) URL, so `Install` can resolve and install
  entirely offline from a mirror on disk.
- Runnable, godoc-rendered examples across all public packages.

### Changed

- fetch: `Backend.Fetch` returns a `Result` (the path plus a `SourceRoot`
  flag) instead of a bare path, and `fetch.Sources` exposes it. `Fetch` and
  `FetchWith` keep their path-only signatures.
- client: A multi-server `Config.Servers` / `InstallOpts.Servers` list is now
  queried in configuration order, first-found-wins, instead of merging every
  server's offering of a version before picking an arch. A merge cannot span
  transports, and first-found-wins is the rule tt's resolver already applies
  over the same list. Behaviour for a single configured server is unchanged.

### Fixed

- tree: Relocation of a backend-installed `lua`/`lib` subtree now runs for every
  `build.type`, not only `make`. A `cmake` or `command` rock installing into
  `$(LUADIR)`/`$(LIBDIR)` left its modules under
  `share/tarantool/rocks/<name>/<version>/lua/`, where no loader looks, and
  recorded nothing in the `rock_manifest`.
- client: The tree manifest's `modules` index is derived from the deployed
  `rock_manifest` (`tree.ModuleIndex`, mirroring upstream
  `repos.package_modules`) instead of the rockspec's `build.modules`, which only
  the `builtin` backend populates. A `cmake`, `make` or `command` rock was
  written to the manifest with `modules = {}`, so neither this library nor
  `luarocks` could tell what it provided. A `build.install.lua` entry is now
  indexed as a module too, matching upstream.
- client: Building a rock whose source is a git repository no longer descends
  into a subdirectory of the checkout. The archive base-directory heuristic
  (upstream `fetch.find_base_dir`) was applied to every fetch result, so a
  clone holding a directory named like the repository — `tarantool/checks`
  ships `checks/` next to its `CMakeLists.txt` — was entered one level too
  deep and the build failed. Backends now report whether the path they return
  is already the source root, which also covers a `file://` source that copies
  a local tree.
