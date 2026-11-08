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
| Resolved dependencies in the tree manifest | a rock's direct ones, recorded at its deploy (the newest installed version its constraints accept); the other entries are left as they are | every rock's, transitively, recomputed on each write |
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
and the tree `manifest` record the same contents. Run it (needs a real
`tarantool` on `PATH` plus its `lua.h`, and `cmake`):

```bash
go test ./client/ -tags integration_luaengine -run 'TestBackendParity|TestBackendDivergence' -v
```

Documented, non-failing differences: the formatting of the manifest files
(`share/tarantool/rocks/manifest` and each `rock_manifest`), the `build/`
staging directory the native backend keeps in each rock's install dir under
`share/tarantool/rocks/` (not listed in its `rock_manifest`), and compiled
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
  setuid, setgid or sticky), and extracts an absolute entry name inside the
  destination, as `tar` and `unzip` do.

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

### Packages

- `deps` — version parsing, dependency constraints, transitive resolver
- `rockspec` — sandboxed `gopher-lua` rockspec evaluator
- `manif` — upstream-compatible manifest reader/writer
- `fetch` — source fetch backends (`http(s)`, `git`/`git+https`/`git+ssh`, `file://`)
- `tree` — on-disk tree layout, deploy, conflicts, `which`
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
