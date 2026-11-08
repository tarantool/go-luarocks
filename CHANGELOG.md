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
  including conditional (platform-guarded) rockspecs. `Validate` rejects a
  path-valued field that is absolute or holds a `..` element — `source.file`,
  `source.dir`, the `build.install` keys and values, `build.modules` paths
  and sources, `copy_directories` — with an error wrapping the new
  `rocks.ErrUnsafePath`, a deliberate strictness beyond upstream.
- manif: A manifest reader and writer compatible with the upstream LuaRocks
  manifest format. A `rock_manifest` entry outside its `lua`, `lib`, `bin`,
  `conf` and `doc` sections is kept in `RockManifest.Other`. `FileStore`
  writes a manifest through a temp file of its own, flushed to stable
  storage before it is renamed into place, so two writers at once never
  clobber each other's write half-way.
- fetch: Source fetch backends for `http(s)`, `git`/`git+https`/`git+ssh`,
  and `file://` sources. `Sources` returns a `Result` that says whether the
  path is already the source root. `File` retrieves a single file into a
  directory without unpacking it (upstream `download.lua`'s `get_file`).
- fetch, remote: HTTP requests share process-wide transports, cloned from
  `http.DefaultTransport`, so connections are reused and `HTTP_PROXY`,
  `HTTPS_PROXY` and `NO_PROXY` are honoured. A host in `InsecureServers`
  skips certificate verification for its own requests only, each redirect
  hop judged by its own host, and a redirect from https to http fails.
- fetch: Everything a fetch writes stays inside its destination: a
  `source.file`, a URL file name or a clone directory that leaves it fails
  with `rocks.ErrUnsafePath`, and so does an archive entry reaching above it
  or beneath a symlink pointing out of it. Extraction creates no symlinks and
  keeps only permission bits, never setuid, setgid or sticky.
- tree: On-disk rock tree layout, deployment, conflict detection, and
  `which`-style lookup. A `lua`/`lib`/`bin` subtree installed by any build
  backend is deployed like the rockspec's own sections, `build.modules` is
  read for `builtin` rocks only, and the default `copy_directories` and docs
  of upstream's `install_files` apply, a default doc that is a symlink
  staying inside the sources copied as the file it points to. A rock's
  `rock_manifest` lists every
  file of its install dir under its plain path, and `ModuleIndex` and
  `CommandIndex` derive its modules and commands from it, as upstream
  `repos.package_modules` and `package_commands` do. When several rocks
  provide a file, `Tree.Providers` names them per `ItemKind` (module or
  command), and `Deploy` gives the plain path to the one upstream would —
  demoting both forms of a module, and backing up a file in its way that
  another rock or version owns, or none does, without overwriting earlier
  backups — and `DeleteVersion` passes it on to the next provider. A
  command wrapper puts the module directories of the tree it is deployed to
  on `package.path` and `package.cpath`. `Deploy` and
  `DeleteVersion` read only the rock's sources and write, rename and remove
  only inside the tree, through `os.Root`, so a `build.install` key, a
  `copy_directories` entry, a `rock_manifest` entry or a rock name that
  leaves, by `..` or through a symlink pointing out, fails with
  `rocks.ErrUnsafePath` instead of reaching outside, where upstream follows
  it.
- tree: `Tree.Update` changes a tree in a transaction, a `Tx`, which keeps
  the tree manifest in step with the files: `Tx.Deploy` deploys a rock and
  records it, `Tx.Remove` undeploys an installed version, a `RockRef`, and
  drops it, failing with `ErrNotInstalled` for one the manifest does not
  list, and `Tx.View` shows the tree as the transaction has it. The manifest
  is written even when the change fails half-way, so it never lags behind
  the files. The native client deploys and removes rocks through it.
- tree, client: A change to a tree holds the tree's lock, an advisory
  `flock(2)` on `share/tarantool/rocks/.lock` (a no-op where there is no
  `flock`): `Tree.Update`, every write of the native backend and the typed
  writes of the lua backend wait for it until their context ends, failing
  then with `tree.ErrLocked`, so concurrent changes of one tree, from
  goroutines or processes, no longer lose each other's manifest entries.
- tree: `Inspect` reads a tree into a `View` without creating anything, an
  absent tree or manifest reading as empty, and `Tree.View` does the same
  for an open tree; both read under the tree's lock, shared, waiting for it
  until their context ends, failing then with `ErrLocked`. A `View`
  answers `Rocks`, `Versions` and `Installed`, the `Providers` of a module or
  command, the `Owner` of a deployed file, a rock's `Files` and where each is
  now (its plain or versioned path, and a command's real script), its
  declared `Dependencies`, the resolved ones it `Requires`, its `Dependents`,
  and a `Command`'s script and entry; `Verify` checks a rock's files against
  its `rock_manifest` (`ErrIntegrity`), and `CheckPlain` that its files hold
  their plain paths (`ErrNotPlain`). A manifest that does not parse fails
  with `ErrCorrupt`.
- tree: `Tx.Prune` removes, in one pass, every installed rock version a
  keep function does not keep — `KeepVersions` keeps a set of pins — in a
  fixed order, whatever order the rocks were installed in, and with
  `PruneOpts.Orphans` also the install dirs the tree manifest does not
  list.
- build: Build backends — `builtin`, `cmake`, `make`, and `command`.
  `LUA_BINDIR` is derived from `Tarantool.Executable` when it has a
  directory component, so a flat Tarantool SDK with no `bin/` works. The
  `make` and `cmake` backends install into the rock's install dir, as
  upstream's `$(PREFIX)`, `$(LUADIR)`, … name it; `cmake` builds out of tree
  in `build.luarocks` and installs under the rock's `$(PREFIX)` when the
  rockspec sets no `CMAKE_INSTALL_PREFIX`. On unix every build tool runs in
  a process group of its own, which cancelling the build's context kills
  whole, the compilers a `make` started included. The `builtin` backend
  reads only inside the sources and writes only inside its build dir.
  Before any backend runs, a rockspec's `build.extra_files` are written
  into the sources and its `build.patches` applied, by a port of upstream's
  `luarocks/tools/patch.lua` checked against it, letting a patch create and
  delete files for `rockspec_format` 3.0, and the sources are marked so
  that a second build of them applies nothing again; a name that leaves
  the sources fails with `rocks.ErrUnsafePath`. The `cfg.variables`
  upstream sets on unix expand to their values: `$(LD)` (the compiler),
  `$(AR)`, `$(RANLIB)`, `$(LDFLAGS)`, `$(LIB_EXTENSION)` (`so`),
  `$(OBJ_EXTENSION)` (`o`), `$(TEST)` and the tool names (`$(CP)`,
  `$(MKDIR)`, …), and `$(LUA)` without a configured executable to
  `tarantool` in `LUA_BINDIR`. Each of a rockspec's `external_dependencies`
  is looked up as upstream's `check_external_deps` does, under
  `/usr/local`, `/usr`, `/` and the Tarantool prefix, and its
  `$(<NAME>_DIR)`, `$(<NAME>_INCDIR)`, `$(<NAME>_LIBDIR)` and
  `$(<NAME>_BINDIR)` name where it was found; one found nowhere does not
  fail the build, unlike upstream, and gets the directories under
  `/usr/local`.
- remote: Indexes for rock servers that are HTTP(S) endpoints or local
  directories (`/srv/rocks` or `file:///srv/rocks`) — the offline case
  upstream serves with `luarocks --only-server=/path/to/repo`. `NewIndex`
  picks the index a server's form implies, `NewIndexes` does it for a whole
  server list, and `OrderedIndex` queries any mix of the two in order,
  first-found-wins. A `ManifestCache`, set through `IndexOptions.Cache`,
  shares fetched manifests between indexes. A local server's row whose name,
  version or arch would lead its artifact path out of the directory is
  dropped.
- remote: `MakeManifest`, a native `luarocks-admin make_manifest` for a rock
  server: it indexes the `.rockspec`, `.src.rock` and `.<arch>.rock` files of
  a directory into `manifest` and `manifest-<version>` for each of
  `MakeManifestOpts.LuaVersions` (5.1 to 5.4 by default, leaving out the
  versions whose rockspec rules that Lua out), and `index.html` with `Index`,
  byte-equal to upstream's and each written atomically. `Zip` also writes the
  zipped manifests, and a stale `manifest-<version>.zip` is always rewritten.
  It reads only inside the directory, and fails, writing nothing, on a rock
  file that is a symlink leading out of it (`rocks.ErrUnsafePath`), is not a
  readable regular file, or on an input upstream fails on.
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
  its direct dependencies, each at the newest installed version its
  constraints accept.
- client: The lua backend runs each operation in its own VM, so no state
  carries over between calls, and one client may be used from several
  goroutines at once. It honours `Config.Servers`, reads
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
- client: The native backend runs `Tarantool.Executable`, in the command
  wrappers it deploys and in the LuaJIT probe of `Search`, when it names a
  file, else `<Tarantool.Prefix>/bin/tarantool`, so both work in a flat
  Tarantool SDK and with an executable outside the prefix.
- client: On the native backend the VM provides a `tarantool` rock at the
  x.y.z part of `Tarantool.Version`, everything before its first dash, at
  revision 1, alike to the dependency resolution of `Install`, to `Search`
  and to `Download`, so every form tt passes provides it: `3.9.0-0-g1a2b3c4`,
  `3.9.0` and `3.9.0-1` as `3.9.0-1`, `2.11.1-0-g57f5f6e-r579` as
  `2.11.1-1`, `3.3.0-entrypoint-31-g9eb3974` as `3.3.0-1`. A version whose
  part does not parse provides none and is logged as a warning.
- client: The native backend keeps every path a rock names inside its
  directory, a deliberate strictness beyond upstream and the lua backend:
  `source.dir` and `source.file` stay inside the fetched sources, `Unpack`
  extracts inside its destination, keeping permission bits only, and `Pack`
  copies only inside the tree and its staging dir; anything else fails with
  `rocks.ErrUnsafePath`.
- Runnable, godoc-rendered examples across all public packages.
