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
- Runnable, godoc-rendered examples across all public packages.

### Changed

### Fixed
