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
- Backend Parity: the native backend is a byte-equal drop-in for the lua
  reference in all deployed artifacts, verified by a differential harness
- Rock Operations: `Install`, `Build`, `Make`, `Pack`, `Unpack`, plus
  `List`, `Show`, and `Which`
- Dependency Resolution: version parsing, dependency constraints, and a
  transitive resolver
- Sandboxed Rockspecs: `gopher-lua`-based evaluator for conditional rockspecs
- Source Fetching: `http(s)`, `git`/`git+https`/`git+ssh`, and `file://`
  source backends
- Build Backends: `builtin`, `cmake`, `make`, and `command`
- Upstream Compatibility: manifest reader/writer compatible with upstream
  LuaRocks and an HTTP remote index client

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

- **native** (default) — pure-Go, no Lua VM. Supports the five write ops
  (`Install`/`Build`/`Make`/`Pack`/`Unpack`) plus `List`/`Show`/`Which`, and
  returns `rocks.ErrNotImplemented` for the other upstream commands.
- **lua** — `client.New(cfg, client.WithBackend(client.BackendLua))`. Runs
  vendored upstream LuaRocks 3.9.2 through `gopher-lua` (the `LState` boots
  lazily on first use), giving full upstream command coverage. This is the
  reference for correct behavior.

Reads (`List`/`Show`/`Which`) always go through the native tree reader
regardless of backend.

#### Backend Parity

The native backend is built to be a behavioral drop-in for the lua reference —
**zero divergence in deployed artifacts** — so a consumer like `tt` can prefer
`tt → lua → go` and treat the backend choice as a pure performance decision.

A differential harness verifies this: it installs the same fixture through
both backends into two separate trees and diffs the deployed trees against each
other (upstream LuaRocks is its own oracle). The contract is that every
deployed artifact — any file NOT under `share/tarantool/rocks/` — is byte-equal
between backends. Run it (needs a real `tarantool` on `PATH` plus its `lua.h`):

```bash
go test ./client/ -tags integration_luaengine -run TestBackendParity -v
```

Documented, non-failing differences: the manifest format
(`share/tarantool/rocks/manifest`), the per-rock bookkeeping subtree under
`share/tarantool/rocks/` (native does not copy the `.rockspec` into the install
dir), and compiled `.so` modules (not byte-reproducible across two
compilations — asserted by presence, not bytes).

### Packages

- `deps` — version parsing, dependency constraints, transitive resolver
- `rockspec` — sandboxed `gopher-lua` rockspec evaluator
- `manif` — upstream-compatible manifest reader/writer
- `fetch` — source fetch backends (`http(s)`, `git`/`git+https`/`git+ssh`, `file://`)
- `tree` — on-disk tree layout, deploy, conflicts, `which`
- `build` — build backends (`builtin`, `cmake`, `make`, `command`)
- `remote` — HTTP remote index
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

[ci-badge]: https://github.com/tarantool/go-luarocks/actions/workflows/ci.yml/badge.svg?branch=develop
[ci-url]: https://github.com/tarantool/go-luarocks/actions/workflows/ci.yml
[godoc-badge]: https://pkg.go.dev/badge/github.com/tarantool/go-luarocks.svg
[godoc-url]: https://pkg.go.dev/github.com/tarantool/go-luarocks
[coverage-badge]: https://coveralls.io/repos/github/tarantool/go-luarocks/badge.svg?branch=develop
[coverage-url]: https://coveralls.io/github/tarantool/go-luarocks?branch=develop
[telegram-badge]: https://img.shields.io/badge/Telegram-join%20chat-blue.svg
[telegram-en-url]: http://telegram.me/tarantool
[telegram-ru-url]: http://telegram.me/tarantoolru
