# go-luarocks

A Go library for managing rocks inside a [Tarantool](https://www.tarantool.io/)
installation. It ships two backends: vendored upstream
[LuaRocks](https://luarocks.org/) 3.9.2 driven through `gopher-lua`, and a
pure-Go reimplementation of the subset Tarantool needs. Targets Tarantool only —
no PUC-Lua or LuaJIT-as-distinct, no Windows. Linux + macOS, amd64 + arm64.

## Quick start

```go
import (
    rocks "github.com/tarantool/go-luarocks"
    "github.com/tarantool/go-luarocks/client"
)

r, err := client.New(rocks.Config{
    Tree:       "/path/to/tarantool-tree",
    WorkingDir: ".",
    Tarantool: rocks.TarantoolConfig{
        Prefix:     "/opt/tarantool",
        IncludeDir: "/opt/tarantool/include/tarantool",
    },
    Servers: []string{"http://rocks.tarantool.org/"},
})
// r.Install / r.Build / r.Make / r.List / r.Show / r.Which / r.Pack / r.Unpack
```

## Backends

Most callers want the fast path, so `client.New(cfg)` defaults to the native
backend. Two backends exist:

- **native** (default) — pure-Go, no Lua VM. Supports the five write ops
  (`Install`/`Build`/`Make`/`Pack`/`Unpack`) plus `List`/`Show`/`Which`, and
  returns `rocks.ErrNotImplemented` for the other upstream commands.
- **lua** — `client.New(cfg, client.WithBackend(client.BackendLua))`. Runs
  vendored upstream LuaRocks 3.9.2 through `gopher-lua` (the `LState` boots
  lazily on first use), giving full upstream command coverage. This is the
  REFERENCE for correct behavior.

Reads (`List`/`Show`/`Which`) always go through the native tree reader
regardless of backend.

### Backend parity

The native backend is built to be a behavioral drop-in for the lua reference —
**zero divergence in deployed artifacts** — so a consumer like `tt` can prefer
`tt → lua → go` and treat the backend choice as a pure performance decision.

A differential harness verifies this: it installs the same fixture through BOTH
backends into two separate trees and diffs the deployed trees against each
other (upstream LuaRocks is its own oracle). The contract is that every
DEPLOYED ARTIFACT — any file NOT under `share/tarantool/rocks/` — is byte-equal
between backends. Run it (needs a real `tarantool` on `PATH` plus its `lua.h`):

```
go test ./client/ -tags integration_luaengine -run TestBackendParity -v
```

Documented, non-failing differences: the manifest format
(`share/tarantool/rocks/manifest`), the per-rock bookkeeping subtree under
`share/tarantool/rocks/` (native does not copy the `.rockspec` into the install
dir), and compiled `.so` modules (not byte-reproducible across two
compilations — asserted by presence, not bytes).

## Why

Tarantool's `tt` CLI currently runs LuaRocks by embedding ~70 Lua files in
[`gopher-lua`](https://github.com/yuin/gopher-lua). This works but couples
build/pack/run subsystems to a Lua runtime, makes errors hard to type, and
leaks process-wide state. This project replaces that with a narrow, pure-Go
library that other Go tooling can compose without spinning up a Lua VM — except
for sandboxed `.rockspec` evaluation, which still needs gopher-lua to handle
conditional rockspecs.

## Packages

- `deps` — version parsing, dependency constraints, transitive resolver
- `rockspec` — sandboxed gopher-lua rockspec evaluator
- `manif` — upstream-compatible manifest reader/writer
- `fetch` — source fetch backends (`http(s)`, `git`/`git+https`/`git+ssh`, `file://`)
- `tree` — on-disk tree layout, deploy, conflicts, `which`
- `build` — build backends (`builtin`, `cmake`, `make`, `command`)
- `remote` — HTTP remote index
- `client` — the Rocks facade + native and lua engines

## License

[BSD-2-Clause](LICENSE). © 2026 Eugene Blikh.
