# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
and this project adheres to [Semantic
Versioning](http://semver.org/spec/v2.0.0.html) except to the first release.

## [Unreleased]

### Added

- fetch: `Options.SHA256` checks the sha256 of the fetched file before
  anything unpacks or keeps it, in the http and file backends and in `File`.
  A mismatch removes the file and fails with an error naming it and both
  digests, wrapping the new `rocks.ErrChecksumMismatch`; a git repository or
  a local directory, which no digest describes, fails instead of being
  fetched unverified.
- remote: A rock server may publish a `tt-rocks-index.json`
  (`remote.RocksIndexFile`), which `HTTPRemoteIndex`, `FileRemoteIndex` and
  `Search` read before any manifest. It names each file by an absolute https
  URL or one relative to the index, with its sha256, and each version's
  rockspec apart from its rocks, so the rocks may live on release pages. A
  server without one is read from its manifests as before; one whose index
  does not read, or is of a newer format (`rocks.ErrUnsupportedIndexFormat`),
  is in error rather than read through its manifests. `ReadRocksIndex`
  decodes one into `remote.RocksIndex`, which documents the rules; a rock
  that breaks them is refused with an error naming the entry. Native backend
  only: the lua backend reads manifests.
- rocks: `VersionedRock` carries what such a server says of a version: the
  chosen file's `Arch` and `SHA256`, every platform's file and digest in
  `Artifacts` (the new `rocks.Artifact`), the rockspec's `SpecURL` and
  `SpecSHA256`, and `Yanked`; `remote.Match` gains `SHA256` and `Yanked`.
- remote: `FileRemoteIndex.InsecureServers`, the hosts an index's absolute
  `http` urls may name; `IndexOptions.InsecureServers` now reaches it too.
  `ManifestCache` keeps the manifests read with different insecure servers
  apart.
- deps: `MatchRock` is `Match` for a row a server offers: a version the
  server yanked matches only when an `==` constraint pins it (PEP 592).
  `Resolve`, the native `Install` and `Download` choose by it, so a fresh
  resolve passes a yanked version over while a lock's pin still installs it,
  and a failure that only yanked versions would have met says so.
- client: The native `Install` resolves from the rockspec a server
  publishes apart from its rocks (`VersionedRock.SpecURL`), so no rock is
  downloaded until the whole closure is resolved, and checks every rockspec
  and rock it fetches against the sha256 the server lists before unpacking
  it; a rock must bundle the very rockspec its dependencies were read from.
  `Download` checks the file it saves the same way. `rocks.InstallStep`
  gains `SHA256`, which `deps.Resolve` fills from the row it picks.
- client: `InstallOpts.Artifacts` pins the requested rock's digests by
  arch, as a lock records them from `VersionedRock.Artifacts`: the file
  `Install` chooses for this platform must be pinned under its arch (else
  the new `rocks.ErrArtifactNotPinned`, before anything is fetched) and
  download with the pinned digest, against any server, one that publishes
  no digests too. The lua backend, which checks no digest, refuses pins with
  `rocks.ErrNotImplemented`.
- remote: The publishing side of a `tt-rocks-index.json`: `NewRocksIndex`,
  `RocksIndex.Add`, which refuses a version already listed
  (`ErrDuplicateVersion`) or an entry against the rules, `RocksIndex.Write`,
  whose JSON depends on the contents alone (keys sorted, artifacts sorted by
  arch, as `jq -S .` prints it), and `RocksIndex.Validate`, which reports
  every broken entry at once and, with `ValidateOptions.Fetch`, every file
  that is not there with its listed sha256 and size.

## [v0.1.0] - 2026-11-09

The first release: a pure-Go native engine and a vendored upstream LuaRocks
3.9.2 behind one `Rocks` facade, a `tree` package that owns a rock tree's
files and its manifest together, and a differential harness that holds the
two backends to the same deployed artifacts wherever they share semantics.

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
  `EvalSource` evaluates the contents of a rockspec the caller read itself.
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
  keeps only permission bits, never setuid, setgid or sticky: a zip entry
  made on Unix gets the bits it records, the umask not applied, as unzip
  restores them, and one made elsewhere the defaults, less the umask.
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
  backups — and `DeleteVersion` passes it on to the next provider,
  removing the directories it leaves empty up to the tree's root, as
  upstream's `rmdir -p` does, so the last command or C module takes `bin/`,
  `lib/tarantool/` and `lib/` with it. A
  command wrapper puts the module directories of the tree it is deployed to
  on `package.path` and `package.cpath`. `Deploy` and
  `DeleteVersion` read only the rock's sources and write, rename and remove
  only inside the tree, through `os.Root`, so a `build.install` key, a
  `copy_directories` entry, a `rock_manifest` entry or a rock name that
  leaves, by `..` or through a symlink pointing out, fails with
  `rocks.ErrUnsafePath` instead of reaching outside, where upstream follows
  it.
- tree, manif, client: The directories and files a change to a tree
  creates have the modes upstream LuaRocks gives them, less the process
  umask: directories, the tree's own and an unpacked rock's, 0777, commands
  and C modules 0755, and the other files, manifests and the lock file
  among them, 0644 or 0666, so a tree made under the usual umask, 022, is
  readable by every user, as one a system service runs from must be. So do
  the directories and files the lua backend creates, a tree's rocks dir and
  every file its LuaRocks writes, the manifests among them, included, which
  gopher-lua's `io.open` would create 0600.
- tree: `Tree.Update` changes a tree in a transaction, a `Tx`, which keeps
  the tree manifest in step with the files: `Tx.Deploy` deploys a rock and
  records it, `Tx.Remove` undeploys an installed version, a `RockRef`, and
  drops it, failing with `ErrNotInstalled` for one the manifest does not
  list, and undeploying the modules and commands its manifest entry names
  when its `rock_manifest` cannot be read, so the plain paths it held pass
  on; `Tx.View` shows the tree as the transaction has it. A change is all
  or nothing, as upstream rolls back a failed deploy, build or install: a
  `Tx` journals every file it creates, renames, overwrites or removes, and a
  method that fails, an `fn` that fails or a manifest that cannot be written
  undoes them — the files of other rocks it demoted or promoted, the
  `rock_manifest`s and the tree manifest are as they were, and the install
  dir a deploy function built in is removed. `Tx.Commit` keeps the changes
  so far whatever fails next. The native client deploys and removes rocks
  through it, so a failed `Install`, `Build` or `Make` leaves the tree as
  upstream's does: as it was, but for an installed copy of the very version,
  which, as upstream, it removes for good before it builds.
  `Tx.Deploy` copies the commands of a rock whose rockspec sets
  `deploy.wrap_bin_scripts = false` verbatim, whatever `Tree.BinWrap` says.
- tree: `Tx.InstallStaged` deploys a directory laid out as an unpacked
  binary rock as the native client installs a `.rock` — conflict rules,
  command wrappers, `rock_manifest` and tree manifest entry alike — its
  `spec.RawSource` becoming the installed rockspec; `StagedOpts.RequirePlain`
  refuses, with `ErrNotPlain` and the tree untouched, a rock a file of which
  would not take its plain path, `NoDisplace` one a file of which would take
  its plain path from another rock, which would keep it at its versioned
  path only, and `RemoveOtherVersions` removes the rock's other versions
  first. As upstream's `install_binary_rock`, which
  unpacks a rock whole, the files at the top of a binary rock land in its
  install dir, and no default docs are made up from them. A stage dir that
  is empty or does not exist is a rock with no files, its rockspec alone
  installed, as for a rock that only gathers its dependencies.
- tree: `Tx.Graft` installs the rocks another tree has on disk, such as an
  extracted package's `.rocks`, as their binary rocks would install: each is
  staged as the native `Pack` stages an installed rock, its `lua` and `lib`
  files taken from whichever of their plain and versioned paths has the
  digest its `rock_manifest` records, every staged file checked against it
  (`ErrIntegrity`); the other tree's manifest is neither trusted nor copied.
  A version installed already is skipped when its `rock_manifest` is the
  same, and fails the graft with `ErrConflict` otherwise, unless `Replace`.
  Nothing changes before every rock is staged and checked, so a symlink
  leading out of the source or a `rock_manifest` entry leaving its directory
  (`rocks.ErrUnsafePath`) leaves the tree as it was, and so does, with
  `GraftOpts.NoDisplace`, a graft that would take the plain path of a file
  from a rock installed here, and with `GraftOpts.RequirePlain` one a
  grafted rock of which would not hold its plain paths (`ErrNotPlain`). The
  `GraftReport` lists
  the files of the source's `share/`, `lib/` and `bin/` no rock of it lists.
- tree: `Tx.Rewrap` rewrites every command wrapper for the tree's root and
  `BinWrap`, for a tree moved, extracted elsewhere or run by another
  interpreter, where `Deploy` would write one; commands copied verbatim are
  left alone.
- tree: `Tree.Rebuild` rewrites the tree manifest from the rock dirs on
  disk, as `luarocks-admin make_manifest --local-tree` does, each rock's
  resolved dependencies its direct ones at the newest version of the rebuilt
  tree; a rock dir whose `rock_manifest` cannot be read is skipped and
  reported, or with `RebuildOpts.Strict` fails it, the manifest unchanged.
- tree: `Tree.Check` reports what is wrong with a tree without changing it:
  files missing, modified or at the wrong one of their plain and versioned
  paths, rock dirs the manifest does not list and entries without their
  rock dir, files under `share/`, `lib/` and `bin/` no rock owns, backups,
  wrappers other than the ones a `BinWrap` gives, and a manifest that does
  not parse. The other directories at the tree's root are a tool's own.
- tree, client: A change to a tree holds the tree's lock, an advisory
  `flock(2)` on `share/tarantool/rocks/.lock` (a no-op where there is no
  `flock`): `Tree.Update`, `Tree.Rebuild`, every write of the native backend
  and the typed writes of the lua backend, each creating the rocks dir and
  the lock file of a tree that does not exist yet, wait for it until their
  context ends, failing then with `tree.ErrLocked`, so concurrent changes of
  one tree, from goroutines or processes, no longer lose each other's
  manifest entries. `Tree.Check`, and `Tx.Graft` of its source, read under it,
  shared. `Paths.LockFile` names the lock file, which is the library's own:
  `Tx.Graft` does not take it from its source, `Tree.Check` and `View` look
  past it, and `Pack` and `PackStaged` never pack it, so a tool archiving a
  whole tree knows to leave it out.
- tree: `Inspect` reads a tree into a `View` without creating anything, an
  absent tree or manifest reading as empty, and `Tree.View` does the same
  for an open tree. `Open` creates nothing either, so a tree that does not
  exist is still absent after `Tree.View`, `Tree.Check`, which finds it
  healthy, and `Tree.Rebuild`, which finds no rocks, and after the client's
  `List`, `Show` and `Which`; each change creates the directories it writes
  into, and `Tree.Update` the rocks dir and its lock file. `Inspect` and
  `Tree.View` read under the tree's lock, shared, waiting for it
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
  rockspec sets no `CMAKE_INSTALL_PREFIX`. The build tools run in the
  caller's process group, so the terminal's Ctrl-C reaches them, and a
  cancelled build returns within about a second even if a process a tool
  started still holds its output. With `Config.IsolateBuilds` on unix each
  build tool runs in a process group of its own instead, which cancelling
  the build's context kills whole, the compilers a `make` started
  included; Ctrl-C no longer reaches it, so the caller must cancel the
  context on SIGINT and SIGTERM. The `builtin` backend
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
  constraints accept. A binary rock is deployed through
  `tree.Tx.InstallStaged`, and `BinWrapFor` returns the command wrapper
  inputs the native backend deploys with, so a `tree.Tree` given them writes
  the wrappers `Install` writes. `PackStaged` packs a directory laid out as
  an unpacked binary rock into the `.rock` the native `Pack` makes of the
  rock once installed from it, installing it into a temporary tree in
  `PackStagedOpts.TempDir`, or the system's temporary directory. A rock
  either packs records the permission bits of its files, as upstream's
  `zip` does, so upstream installs the commands it keeps in the rock's
  install dir executable, and `Unpack` restores them as `unzip` does; its
  entries carry no modification time, so the same files with the same
  modes pack to the same bytes.
- client: The lua backend runs each operation in its own VM, so no state
  carries over between calls, and one client may be used from several
  goroutines at once. It honours `Config.Servers`, reads
  `/etc/luarocks/config-5.1.lua` (`LUAROCKS_SYSCONFDIR` overrides it), and
  loads Lua modules only from its embedded copy and the rock trees, never
  from the current directory, `$LUA_PATH` or system directories. The error
  of a failed command carries the message LuaRocks printed. A command
  LuaRocks runs through `os.execute` writes to the standard output and error
  the process started with, where LuaRocks' own `io.stdout` and `io.stderr`
  write, whatever `os.Stdout` and `os.Stderr` hold later, so its errors stay
  out of the output even where `go test -json` makes `os.Stderr` the same
  file as `os.Stdout`. Where upstream splices a path into a shell command
  line unquoted, the engine quotes it: `fs.is_lua`, so a Lua command under a
  path holding a quote, a dollar sign or a backtick gets its wrapper, and the
  `-D` values of a `cmake` build, so CMake gets the tree's path as it is;
  nothing the path names runs. A rockspec's `$(PREFIX)`, `$(LUADIR)`, … are
  substituted once, as the native backend substitutes them, so a `$(NAME)`
  a path holds stays in it where upstream removed it. Its typed
  methods run over the rock trees `Config.Tree` and
  `Config.Tarantool.Prefix` whatever a config file sets, so `Config.Tree` is
  always one of them, as upstream assumes of the tree it installs into;
  `Exec` runs over the configured trees. Its methods and `Exec` honour their
  context: cancelling it stops the Lua code and kills the shell of each
  command LuaRocks runs through `os.execute` and `io.popen` (its whole
  process group with `Config.IsolateBuilds` on unix), LuaRocks' cleanup
  rolls a partial install back, the files left at the names `os.tmpname`
  gave out are removed, and the call returns at once with an error wrapping
  `ctx.Err()`.
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
