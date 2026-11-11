[![CI][ci-badge]][ci-url]
[![Go Reference][godoc-badge]][godoc-url]
[![Code Coverage][coverage-badge]][coverage-url]
[![Telegram EN][telegram-badge]][telegram-en-url]
[![Telegram RU][telegram-badge]][telegram-ru-url]

# go-luarocks: library to manage LuaRocks in a Tarantool installation

### About

<a href="http://tarantool.org">
    <img align="right" src="assets/logo.png" width="250" alt="Tarantool Logo">
</a>

**go-luarocks** is a Go library for managing rocks inside a
[Tarantool](https://www.tarantool.io/) installation. It ships two backends: a
pure-Go reimplementation of the subset Tarantool needs, and vendored upstream
[LuaRocks](https://luarocks.org/) 3.9.2 driven through `gopher-lua` as the
behavioral reference.

### Overview

Tarantool's `tt` CLI currently runs LuaRocks by embedding ~70 Lua files in
[`gopher-lua`](https://github.com/yuin/gopher-lua). This works but couples the
build/pack/run subsystems to a Lua runtime, makes errors hard to type, and
leaks process-wide state. **go-luarocks** replaces that with a narrow, pure-Go
library that other Go tooling can compose without spinning up a Lua VM — except
for sandboxed `.rockspec` evaluation, which still needs `gopher-lua` to handle
conditional rockspecs. The library targets Tarantool only — no PUC-Lua or
LuaJIT-as-distinct, no Windows. Linux + macOS, amd64 + arm64.

### Features

- Two Backends: a pure-Go `native` engine (default) and a vendored upstream
  LuaRocks `lua` engine, selectable per client
- Backend Parity: the native backend deploys the same artifacts, byte for
  byte, as the lua reference wherever the two share semantics, verified by a
  differential harness; where it keeps go-luarocks' own semantics, the
  differences are documented
- Rock Operations: `Install`, `Build`, `Make`, `Pack`, `Unpack`, plus
  `List`, `Show`, and `Which`
- Dependency Resolution: version parsing, dependency constraints, and a
  transitive resolver
- Sandboxed Rockspecs: `gopher-lua`-based evaluator for conditional rockspecs
- Source Fetching: `http(s)`, `git`/`git+https`/`git+ssh`, and `file://`
  source backends
- Build Backends: `builtin`, `cmake`, `make`, and `command`
- Upstream Compatibility: manifest reader/writer compatible with upstream
  LuaRocks, an HTTP remote index client, and a rock server indexer
  byte-equal to `luarocks-admin make_manifest`

### Requirements

- Go >= 1.26
- No CGO — the module and its dependencies (`go-git`, `gopher-lua`) are pure
  Go
- Linux or macOS
- Network access is only needed for remote/fetch operations against LuaRocks
  servers or git

### Installation

```bash
go get github.com/tarantool/go-luarocks
```

### Quick Start

```go
package main

import (
    "log"

    rocks "github.com/tarantool/go-luarocks"
    "github.com/tarantool/go-luarocks/client"
)

func main() {
    r, err := client.New(rocks.Config{
        Tree:       "/path/to/tarantool-tree",
        WorkingDir: ".",
        Tarantool: rocks.TarantoolConfig{
            Prefix:     "/opt/tarantool",
            IncludeDir: "/opt/tarantool/include/tarantool",
        },
        Servers: []string{"http://rocks.tarantool.org/"},
    })
    if err != nil {
        log.Fatal(err)
    }

    // r.Install / r.Build / r.Make / r.List / r.Show / r.Which / r.Pack / r.Unpack
    _ = r
}
```

### Backends

Most callers want the fast path, so `client.New(cfg)` defaults to the native
backend. Two backends exist:

- **native** (default) — pure-Go, no Lua VM. Supports the eight write ops
  (`Install`/`Build`/`Make`/`Pack`/`Unpack`/`Remove`/`Search`/`Download`)
  plus `List`/`Show`/`Which`, and returns `rocks.ErrNotImplemented` for the
  other upstream commands.
- **lua** — `client.New(cfg, client.WithBackend(client.BackendLua))`. Runs
  vendored upstream LuaRocks 3.9.2 through `gopher-lua`, running each
  operation in a fresh VM that is discarded afterwards (the embedded modules
  are compiled once per process and the next VM is warmed in the background),
  giving full upstream command coverage. This is the reference for correct
  behavior.

Reads (`List`/`Show`/`Which`) always go through the native tree reader
regardless of backend. `List` and `Show` answer as upstream's `list` and
`show` do: `Show` reports the newest installed version and its modules by
name. `Which` keeps go-luarocks' own lookup: the module's plain paths in
`Config.Tree` alone, where upstream's `which` (which `Exec` runs) asks the
LuaRocks loader over the manifests of every rock tree.

`InstallOpts.Deps` and `RemoveOpts.Deps` take a `client.DepsPolicy`:
`DepsAll` (the zero value), `DepsNone` or `DepsOnlyNew`. The native backend
reads it as go-luarocks always has: `Install` resolves the transitive closure
of the rock and installs, in topological order, every dependency a rock in
`Config.Tree` does not satisfy, unless `DepsNone`, which installs the rock
alone; `Build` and `Make` install no dependency at all. It consults and
writes `Config.Tree` alone: no other rock tree satisfies a dependency, and a
deployed command wrapper puts only `Config.Tree` on `package.path`. The lua
backend passes the policy to upstream as `--deps-mode` on `Install` and
`Remove` (`all`, `none` and `order` for `DepsAll`, `DepsNone` and
`DepsOnlyNew`); `Build` and `Make` pass none, so the configured `deps_mode`,
`one` by default, applies. Its typed methods run over the rock trees
`Config.Tree`, then `Config.Tarantool.Prefix`, whatever a LuaRocks config file
sets, so the tree they install into is always one of them, as upstream
assumes; `Exec` runs the command as the host's LuaRocks would, over the
configured trees.

The native backend runs a rock's build tools (`make`, `cmake`, the C
compiler, the shell of a `command` build) in the caller's process group, so
the terminal's Ctrl-C stops them with the caller. Cancelling the context
kills the running tool, and the call returns within about a second even if a
process the tool started still holds its output. A caller that cancels the
context on SIGINT and SIGTERM, e.g. with `signal.NotifyContext`, can set
`Config.IsolateBuilds`: on unix each tool then runs in a process group of its
own, which the cancellation kills whole, the compilers a `make` started
included. Ctrl-C no longer reaches that group, so without the cancellation
the tools would outlive the caller.

The lua backend honours the context of every method and of `Exec` too.
Cancelling it stops the Lua code at its next instruction and kills the shell
of each command LuaRocks runs through `os.execute` and `io.popen` — the
downloader, `unzip`, the build tools — in the same two ways: in the caller's
process group the shell alone, and with `Config.IsolateBuilds` its whole
process group. LuaRocks' own cleanup then runs, as it does when a command
fails, removing its temporary directories and the partial install of a
cancelled build, and the call fails with an error wrapping `ctx.Err()`. The
VM it ran on is discarded, as every VM is after its call.

#### Backend Parity

The native backend reproduces what upstream LuaRocks — the lua backend —
deploys: for the same operation, wherever the two backends share semantics,
every deployed artifact is byte-equal between them, and each rock's
`rock_manifest` and the tree `manifest` record the same contents. That is
not zero divergence overall. In a few places the native backend keeps
go-luarocks' own semantics, by decision: `tt`, its main consumer, drives it
as a deterministic executor — exact pins, `DepsNone`, a topological order and
its own pruning of the tree — and relies on them, while the lua backend stays
upstream. Switching backends on the same calls can therefore change the
outcome in these areas:

| Area | native backend (go-luarocks) | lua backend (upstream) |
| --- | --- | --- |
| Dependencies of `Install` | `DepsAll`/`DepsOnlyNew` resolve the closure and install what `Config.Tree` lacks; `DepsNone` installs the rock alone | `--deps-mode all`/`order`/`none`: a rock in the prefix's tree satisfies a dependency |
| Dependencies of `Build`, `Make` | none installed, `build_dependencies` included | the missing ones are installed first |
| Other versions of the rock on `Install`, `Build`, `Make` | kept | removed unless `--keep`, which only `BuildOpts.Keep` passes (on the native backend it keeps the staging build dir) |
| `Remove` of a rock another depends on | removed; `Force` and `ForceFast` change nothing | refused unless forced, or another version in the rock trees satisfies the dependent |
| A rock, or a dependency, that several servers offer, on `Install` | taken from the first server in the list holding a matching version, first-found-wins | the newest matching version across every server |
| Rock trees consulted, and listed by a command wrapper | `Config.Tree` alone | `Config.Tree`, then `Config.Tarantool.Prefix`, as the deps mode selects them |
| Resolved dependencies in the tree manifest | a rock's direct ones, recorded at its deploy (the newest installed version its constraints accept), or by `tree.Tree.Rebuild` against the whole tree; the other entries are left as they are | every rock's, transitively, recomputed on each write and by `make_manifest` |
| An `external_dependencies` entry found on no search path | the build runs, its `$(<NAME>_INCDIR)`, … naming the directories under `/usr/local` | the build fails before it starts |
| `tarantool` rock the VM provides | the part of `Config.Tarantool.Version` before its first dash, at revision 1, so a bare `3.9.0` provides `3.9.0-1`; a part that does not parse is logged and provides none | the part before the first dash, at revision 1, and none for a version with no dash, such as a bare `3.9.0` |
| `Which` | plain paths in `Config.Tree` | (`Exec` `which`) the loader, over every rock tree's manifest |

A differential harness verifies the parity: it builds the same fixture
through both backends, with `Make`, with `Build` and with `Install`, into
two separate trees and diffs the trees against each other (upstream LuaRocks
is its own oracle). The fixtures cover the `builtin`, `make`, `cmake` and
`command` build types, and trees holding several versions of a rock, or
several rocks providing the same module. Where a scenario meets one of the
differences above, it is listed as a known divergence with its reason
(`client/parity_divergence_test.go`) and checks what each backend does
instead — every version kept on the native backend and the expected ones on
the lua backend, the manifests equal but for the resolved dependencies — and
`TestBackendDivergence_*` runs the dependency policies and the removal of a
dependency through both backends and checks each one's outcome. The contract
is that every deployed artifact — any file NOT under `share/tarantool/rocks/`
— is byte-equal between backends, a command wrapper once the prefix's tree
upstream's `install` adds is left out, and that each rock's `rock_manifest`
and the tree `manifest` record the same contents. The two trees must also
have the same shape: the same directories, empty ones included, the same
permission bits on every file and directory (the suite runs under umask
022), and the same symlinks, compared by their target and never followed;
the differences found there that wait for a fix are named, with their
reasons, in `client/parity_shape_test.go`. The fixtures of every build type
also run with `Config.Tree` and `Config.WorkingDir` under a path holding a
space, a single quote, a dollar sign, non-ASCII letters or a 200-byte
component, or reached through a symlink (`TestBackendParity_HostilePaths`),
and the command wrappers are run. Where upstream splices such a path into a
shell command line unquoted, the lua backend's shims over its modules
(`internal/luarocks/extra`) quote it — `fs.is_lua`, which decides whether a
command script gets a wrapper, and the `-D` values of a `cmake` build — and
they substitute a rockspec's `$(PREFIX)`, `$(LUADIR)`, … once, as the native
backend does, where upstream then removed every `$(NAME)` the substituted
paths held. Under a dollar sign the backends end otherwise than alike, each
case named in `client/parity_paths_test.go`: make expands a `$` in the path,
and sh one in the double-quoted words of a `command` build and of a
Makefile's recipes, before any build script sees it, through either backend,
as both hand make its variables as arguments and substitute the paths into
the rockspec's own shell code as text; and the Makefiles CMake generates do
not escape a `$` of the build dir, so a `cmake` build fails in one: the
native backend's, which it stages in `Config.WorkingDir`, and both backends'
`Make`, which builds in it, where upstream's `Build` and `Install` stage under
`TMPDIR` and deploy the rock. Run it
(needs a real `tarantool` on `PATH` plus its `lua.h`, and `cmake`):

```bash
go test ./client/ -tags integration_luaengine -run 'TestBackendParity|TestBackendDivergence' -v
```

The same holds for what a failure leaves. `TestBackendParity_Failures`
(`client/parity_fail_test.go`) injects one into a build or a deploy — a C
module that does not compile, a `make install` that fails after it wrote
into `$(LUADIR)`, a command script the rockspec names that is missing, a
regular file where a module directory must go, an upgrade and a reinstall
that fail — and runs it through both backends, with `Make` and with
`Install`, on trees set up alike: both must fail and leave the tree exactly
as it was, every file, mode and manifest, as upstream rolls back, but for
the copy of the reinstalled version, which both remove for good before they
build. A build whose context is cancelled and a deploy into a read-only
directory run through the native backend alone. `Tree.Check` finds nothing
wrong with any tree afterwards.

Documented, non-failing differences: the formatting of the manifest files
(`share/tarantool/rocks/manifest` and each `rock_manifest`), the `build/`
staging directory the native backend keeps in each rock's install dir under
`share/tarantool/rocks/` (not listed in its `rock_manifest`, and not packed
by the native `Pack` nor grafted by `Tx.Graft`, where upstream's `pack`, which
copies the install dir whole, packs it), and compiled
`.so` modules (not byte-reproducible across two compilations — asserted by
presence, not bytes).

Besides the differences of semantics above, the native backend departs from
upstream deliberately in what it deploys and in the paths it follows, each
documented where it is made: a `cmake` rockspec that sets no
`CMAKE_INSTALL_PREFIX` installs under the rock's `$(PREFIX)` rather than
CMake's `/usr/local` (`build/cmake.go`); a rock
shipping both `foo.lua` and `foo.so` is indexed by its `.so`
(`tree.ModuleIndex`); and whether a command script is Lua, and so gets a
wrapper, is decided by parsing it as Lua 5.1 rather than by the Tarantool
interpreter's `loadfile`, which also accepts LuaJIT bytecode and LuaJIT's
syntax extensions (`tree/deploy.go` `isLua`); and `Pack` copies a demoted
file in a subdirectory back from the versioned name the deploy gave it, where
upstream looks for one the deploy never writes and packs another rock's file
(`client/native.go` `copyBackFiles`).

The native backend also keeps every file it reads, writes, renames, chmods or
removes inside the directory the name belongs to, where upstream LuaRocks —
and so the lua backend — follows any name it is given. It is checked in two
layers, and a name that would leave fails with an error wrapping
`rocks.ErrUnsafePath`:

- `rockspec.Validate` rejects a path-valued field that is absolute or holds a
  `..` element, or that names a file and is empty or `.`: `source.file`,
  `source.dir`, the `build.install` keys and values, `build.modules` paths and
  sources, `copy_directories`, and their platform overrides
  (`rockspec/paths.go`). A rock or rockspec from a server could otherwise
  write or delete outside the tree without running any build code.
- Every site that joins such a name checks it again, for the names that never
  pass Validate — a planted `rock_manifest`, an archive entry, the tree
  manifest, a server's manifest or URL: `tree.Deploy` and `DeleteVersion`
  stay in the tree and read only the sources, the builtin backend stays in its
  sources and build dir, a fetch and an archive extraction stay in their
  destination, the source root stays in the fetched sources, `Pack` in the
  tree and the pack dir, and a local server's artifacts in its directory. The
  file operations go through `os.Root` (`internal/safepath`), so a symlink on
  the way that points out fails them too; one that stays inside is followed.
  Archive extraction creates no symlinks, keeps only permission bits (never
  setuid, setgid or sticky) — those a zip entry made on Unix records exactly,
  as `unzip` restores them — and extracts an absolute entry name inside the
  destination, as `tar` and `unzip` do. A rock the native `Pack` or
  `client.PackStaged` packs records the permission bits of its files, made
  on Unix, as upstream's `zip` does, and no modification time.

Legitimate rockspecs are unaffected; the parity harness and the rock corpus
install the same trees as before.

#### Backend Interoperability

The parity harness compares two trees; a second one,
`TestBackendInterop_*`, has both backends work on the same tree, as `tt`
does when it switches backends on an existing project. One backend installs,
builds or makes a rock, and the other lists and shows it and resolves its
modules — through the native readers and through upstream's own `list`,
`show`, `which` and `path` — installs it again, upgrades or downgrades it,
removes it with and without dependents, packs it, or rewrites the tree
manifest by installing an unrelated rock. Every mixed run must end where a
run of either backend alone ends, and the two readers must agree on every
tree. Where the backends differ by design (the table above), the two
single-backend runs end differently, and a mixed run must end where the run
of the backend whose semantics decide it ends — the one that made the last
version, removed the dependency, or last recomputed the resolved
dependencies — or, where neither does, is skipped as a named known
divergence.

Run it as the parity harness (packing also needs `zip`, which upstream packs
with):

```bash
go test ./client/ -tags integration_luaengine -run TestBackendInterop -v
```

#### Rock Servers

`remote.MakeManifest(ctx, dir, opts)` turns a directory of `.rockspec`,
`.src.rock` and `.<arch>.rock` files into a rock server without a Lua VM:
it writes `manifest` and `manifest-5.1` … `manifest-5.4` (and, with
`MakeManifestOpts.Index`, `index.html`) byte-equal to what
`luarocks-admin make_manifest <dir>` writes, each through a temporary file
and a rename. It is checked against upstream twice: offline against the
vendored upstream code run by `internal/luaref`
(`TestMakeManifest_MatchesUpstream`, `FuzzMakeManifest`), and in the
sandbox against the lua backend's `Admin("make_manifest", dir)`:

```bash
go test ./client/ -tags integration_luaengine -run TestBackendParity_MakeManifest -v
```

Where upstream follows any name it lists, `MakeManifest` reads only inside
`dir` (a rock file that is a symlink leading out fails with
`rocks.ErrUnsafePath`), requires every listed name to be a readable regular
file, writes nothing when anything fails (upstream writes the manifests
before it fails in `index.html`), and rewrites a stale
`manifest-<version>.zip`, which clients read before `manifest-<version>`.
`MakeManifestOpts.Zip` writes the zips `luarocks-admin add` does.

#### Rock Index

A server may publish a `tt-rocks-index.json` where its manifests would be,
for instance from a git branch through `raw.githubusercontent.com`, with the
rocks on release pages:

```json
{
  "format": 1,
  "repository": {
    "emmylua": {
      "0.25.1-1": {
        "artifacts": [
          {
            "arch": "linux-x86_64",
            "sha256": "…",
            "size": 19777374,
            "url": "https://github.com/tarantool-contrib/rocks-repo/releases/download/emmylua-0.25.1-1/emmylua-0.25.1-1.linux-x86_64.rock"
          }
        ],
        "rockspec": {
          "sha256": "…",
          "url": "emmylua-0.25.1-1.rockspec"
        }
      }
    }
  }
}
```

The native backend reads it before any manifest, over HTTP and from a local
directory alike, in `Install`, `Search` and `Download`: when a server has
one, it is the server's catalogue; when it has none (403, 404, 410, or no
such file), the manifests are read as before; when it is there but does not
read, or has a newer `format`, the server is in error. A `url` is absolute
`https` (`http` only for a host in `InsecureServers`) or relative to the
index's directory, and its last segment must be the file's own name; a rock
that breaks a rule is refused with an error naming the entry.
`remote.RocksIndex` documents the rules. Each row a query returns
(`rocks.VersionedRock`) carries the chosen file's `SHA256`, the digests of
every platform's file (`Artifacts`), the rockspec's `SpecURL` and
`SpecSHA256`, and `Yanked`. The lua backend, which runs upstream LuaRocks,
reads manifests only: a server that publishes nothing but an index serves no
rocks there.

From such a server the native `Install` reads every rockspec it resolves
from the rockspec's own URL, so no rock is downloaded until the closure is
resolved, and checks each file against its sha256 before unpacking it
(`rocks.ErrChecksumMismatch`); a rock must bundle the rockspec it was
resolved from. A yanked version is passed over unless `==` pins it
(`deps.MatchRock`). `InstallOpts.Artifacts` takes a lock's digests by arch:
the file chosen for this platform must be among them
(`rocks.ErrArtifactNotPinned` otherwise) and have its digest, whatever
server serves it.

To publish one, `remote.ReadRocksIndex` reads an index (or
`remote.NewRocksIndex` starts one), `RocksIndex.Add` lists a version and
refuses one already listed (`remote.ErrDuplicateVersion`: a published
version never changes, a rebuild is a new revision), `RocksIndex.Validate`
reports every entry that breaks the rules — and, with
`ValidateOptions.Fetch`, every file that is not there with its listed
digest and size — and `RocksIndex.Write` writes JSON that depends on the
contents alone: keys sorted, artifacts sorted by arch, two-space indent, as
`jq -S .` prints it.

### Rock Trees

The `tree` package owns a tree's files and its manifest together, so a
tool that manages a tree itself, as `tt` does, need not edit it behind the
library's back:

```go
v, err := tree.Inspect(ctx, root) // read-only; creates nothing
ref, ok := v.Installed("luatest", "1.3.1-1")
script, entry, ok := v.Command("luatest")

t, err := tree.Open(rocks.Config{Tree: root})
err = t.Update(ctx, func(tx *tree.Tx) error {
    _, err := tx.Prune(tree.KeepVersions(pins), tree.PruneOpts{Orphans: true})
    return err
})
```

`tree.Open` creates nothing either: a tree that does not exist reads as an
empty one, and `View`, `Check` and `Rebuild` leave it absent, where `Update`
creates the rocks dir and its lock file.

A `View` tells which rock versions are installed, which provider of a
module or command holds its plain path, who owns a deployed file, where
each file of a rock is and whether it matches the rock's `rock_manifest`.
`Tx.Remove` and `Tx.Prune` undeploy rocks as the native `Remove` does —
without a dependents check, a plain path passing to the next provider — and
update the tree manifest in the same transaction. A rock whose
`rock_manifest` cannot be read, which upstream drops from the manifest
alone, is undeployed by the modules and commands its manifest entry names.

The other changes a tool makes to a tree by hand have one each, every one
writing the files and the manifest together:

```go
t.BinWrap = client.BinWrapFor(cfg) // the wrappers client.Install writes
err = t.Update(ctx, func(tx *tree.Tx) error {
    // A guest package's .rocks, as its binary rocks would install.
    report, err := tx.Graft(guestRocks, tree.GraftOpts{Select: notPinnedHere})
    if err != nil {
        return err
    }
    // A project's own files, laid out as an unpacked binary rock.
    _, err = tx.InstallStaged(spec, stageDir, tree.StagedOpts{
        RequirePlain: true, NoDisplace: true, RemoveOtherVersions: true,
    })
    return err
})
```

- `Tx.InstallStaged` deploys a directory laid out as an unpacked binary rock
  (`lua/`, `lib/`, `bin/`, and the rest into the rock's install dir) as the
  native client installs a `.rock`: conflict rules, wrappers,
  `rock_manifest` and manifest entry alike. Its `spec.RawSource` becomes the
  installed rockspec, which the lua backend reads; a directory that is empty
  or does not exist is a rock with no files. `RequirePlain` refuses,
  with `ErrNotPlain` and the tree untouched, a rock a file of which another
  rock holds the plain path of, and `NoDisplace` one a file of which would
  take the plain path from another rock, so that with both the rock's files
  are at plain paths no other rock had. `client.PackStaged` packs such a
  directory into the `.rock` the native `Pack` makes of the rock once
  installed.
- `Tx.Graft` installs the rocks another tree has on disk — an extracted
  package's `.rocks` — as their binary rocks would install. Each rock is
  staged as the native `Pack` stages an installed rock, its `lua` and `lib`
  files taken from whichever of their plain and versioned paths has the
  digest its `rock_manifest` records; the other tree's manifest is neither
  trusted nor copied. A version installed already is skipped when its
  `rock_manifest` is the same, and is `ErrConflict` otherwise, unless
  `Replace`. A file that is not what its `rock_manifest` records
  (`ErrIntegrity`), a symlink leading out of the source or an entry leaving
  its directory (`rocks.ErrUnsafePath`), or a conflict leaves the tree as it
  was, and so does, with `NoDisplace`, a graft that would take a plain path
  from a rock installed here, and with `RequirePlain` one a grafted rock of
  which would not hold its plain paths (`ErrNotPlain`). The report lists the
  files of the source's `share/`, `lib/` and `bin/` no rock of it lists,
  which are not copied.
- `Tx.Rewrap` rewrites every command wrapper for the tree's root and
  `BinWrap`, for a tree moved, extracted elsewhere or run by another
  interpreter; `client.BinWrapFor(cfg)` gives the inputs the native backend
  deploys with.
- `Tree.Rebuild` rewrites the manifest from the `rock_manifest`s and
  rockspecs on disk, as `luarocks-admin make_manifest --local-tree` does, the
  manifest there was, broken or gone, aside. Each rock's resolved
  dependencies are its direct ones, at the newest version of the rebuilt tree
  its constraints accept, as a native install in dependency order records
  them, where upstream resolves them transitively (the table above).
- `Tree.Check` is a read-only fsck: missing and modified files, files at the
  wrong one of their plain and versioned paths, rock dirs the manifest does
  not list and entries without their rock dir, files under `share/`, `lib/`
  and `bin/` no rock owns, backups, and wrappers other than the ones
  `BinWrap` gives. The other directories at the tree's root are a tool's
  own, as `tt` keeps its metadata in one. Problems are the report; the error
  is for I/O.

A change to a tree is all or nothing, as upstream rolls back a deploy, a
build or an install that fails (`repos.deploy_files`' rollback, the
functions `build.lua` and `cmd/install.lua` schedule): a `Tx` journals every
file it creates, overwrites, renames or removes, keeping a removed or
overwritten one aside in a `.tx-*` directory of the rocks dir until the
change ends, and a `Tx` method or an `Update` that fails undoes them all —
the files of every rock, demoted or promoted ones included, the
`rock_manifest`s and the tree manifest are as they were, and the install
dir a build wrote into is removed. `Tx.Commit` keeps the changes made so
far whatever fails next. The native `Install`, `Build` and `Make` deploy
each rock in one, so a failed one leaves the tree as upstream's does: as it
was, but for an installed copy of the very version, which both remove for
good before they build, and the dependencies an `Install` installed before
the rock that failed. What `fn` changes by itself, other than through
the `Tx`, is not undone, and a process killed half-way undoes nothing: it
leaves the tree as it was then, the files it set aside in the `.tx-*`
directory.

A change to a tree takes the tree's lock: an advisory `flock(2)` on
`share/tarantool/rocks/.lock` (Linux, macOS and the BSDs; elsewhere the lock
guards nothing). `tree.Tree.Update`, `Tree.Rebuild`, every write of the
native backend, and the typed writes of the lua backend (`Install`, `Build`,
`Make`, `Remove`, `Purge`) take it exclusive, and `Inspect`, `Tree.View`,
`Tree.Check` and `Tx.Graft`, of its source, shared; each waits for it until
its context ends, failing then with `tree.ErrLocked`, so two processes
changing one tree run one after the other instead of losing each other's
manifest entries. An external `luarocks` and `Exec` do not take it. The
manifests themselves are written through a temp file of each writer's own,
flushed and renamed into place.

The lock file, `tree.Paths.LockFile`, stays in a tree once a change has
taken the lock, and is the library's, not the tree's contents: `Tx.Graft`
does not take it from its source, `Check` and `View` look past it, and
`Pack` and `client.PackStaged` never pack it. A tool that archives or copies
a whole tree leaves it out, as the next change of the copy creates its own;
removing it from a tree in use would let two changes run at once.

Every directory and file a change to a tree creates has the mode upstream
LuaRocks gives it, less the process umask: directories 0777, commands and C
modules 0755, every other file 0644 or 0666. Under the usual umask, 022, a
tree is readable by every user — a system-wide tree a service runs from as
another user than the one installing into it needs nothing more — and under
077 by its owner alone.

### Packages

- `deps` — version parsing, dependency constraints, transitive resolver
- `rockspec` — sandboxed `gopher-lua` rockspec evaluator
- `manif` — upstream-compatible manifest reader/writer
- `fetch` — source fetch backends (`http(s)`, `git`/`git+https`/`git+ssh`, `file://`)
- `tree` — on-disk tree layout, deploy, conflicts, `which`, the tree
  manifest kept in step through `Update` transactions and read through
  `Inspect`, and the grafts, staged installs, rewraps, rebuilds and checks of
  a tree
- `build` — build backends (`builtin`, `cmake`, `make`, `command`)
- `remote` — rock server indexes (HTTP and local directories), search, and
  `MakeManifest`, the native `luarocks-admin make_manifest`
- `client` — the Rocks facade + native and lua engines

### Examples

Runnable, godoc-rendered examples live beside each package in
`examples_test.go` files. They double as executable documentation — the
deterministic ones assert their own output under `go test`.

- Version & dependency math: [`deps/examples_test.go`](deps/examples_test.go)
- Rockspec evaluation: [`rockspec/examples_test.go`](rockspec/examples_test.go)
- The `Rocks` facade: [`client/examples_test.go`](client/examples_test.go)
- Manifest read/write: [`manif/examples_test.go`](manif/examples_test.go)
- Tree layout & lookup: [`tree/examples_test.go`](tree/examples_test.go)
- Source fetching: [`fetch/examples_test.go`](fetch/examples_test.go)
- Build backends: [`build/examples_test.go`](build/examples_test.go)
- Remote index: [`remote/examples_test.go`](remote/examples_test.go)

Run them all:

```bash
go test -v -run Example ./...
```

### Contributing

Contributions are welcome! Please open an issue to discuss your ideas or submit
a pull request. See [`CONTRIBUTING.md`](CONTRIBUTING.md) for the issue and
pull-request process, commit-message conventions, and how to run the tests
and linters locally.

### License

This project is licensed under the BSD 2-Clause License – see the
[LICENSE](LICENSE) file for details.

[ci-badge]: https://github.com/tarantool/go-luarocks/actions/workflows/ci.yml/badge.svg?branch=master
[ci-url]: https://github.com/tarantool/go-luarocks/actions/workflows/ci.yml
[godoc-badge]: https://pkg.go.dev/badge/github.com/tarantool/go-luarocks.svg
[godoc-url]: https://pkg.go.dev/github.com/tarantool/go-luarocks
[coverage-badge]: https://coveralls.io/repos/github/tarantool/go-luarocks/badge.svg?branch=master
[coverage-url]: https://coveralls.io/github/tarantool/go-luarocks?branch=master
[telegram-badge]: https://img.shields.io/badge/Telegram-join%20chat-blue.svg
[telegram-en-url]: http://telegram.me/tarantool
[telegram-ru-url]: http://telegram.me/tarantoolru
