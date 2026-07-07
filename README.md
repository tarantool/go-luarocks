# go-luarocks

A Go interface to [LuaRocks](https://luarocks.org/) for managing rocks inside
a [Tarantool](https://www.tarantool.io/) installation. Targets Tarantool only —
Linux + macOS, amd64 + arm64.

## Status

This commit lands the first working layer: the vendored upstream **LuaRocks
3.9.2** (tarantool fork) is embedded and driven from Go through
[`gopher-lua`](https://github.com/yuin/gopher-lua). The `client` package boots
the embedded LuaRocks VM lazily and dispatches every operation
(`Install`/`Build`/`Make`/`Pack`/`Unpack`/`Remove`/`Search`/…) into it.

```go
import (
    rocks "github.com/tarantool/go-luarocks"
    "github.com/tarantool/go-luarocks/client"
)

r, err := client.New(rocks.Config{
    Tree:       "/path/to/tarantool-tree",
    WorkingDir: ".",
    Servers:    []string{"http://rocks.tarantool.org/"},
})
_ = r.Install(ctx, "metrics", client.InstallOpts{})
```

A pure-Go reimplementation of the LuaRocks subset needed by Tarantool is layered
on top in subsequent commits, package by package (`deps`, `rockspec`, `manif`,
`fetch`, `tree`, `build`, `remote`, and finally a native `client` engine).
