# Vendored LuaRocks

`src/` holds the **Tarantool fork** of LuaRocks 3.9.2
([tarantool/luarocks](https://github.com/tarantool/luarocks), branch
`luarocks-3.9.2-tarantool`), vendored via `git subtree` (do not edit it
directly). This is the same source `tt` ships (`tt/cli/rocks/third_party`), not
stock upstream — the fork registers `tarantool` as a provided dependency
(via `TT_CLI_TARANTOOL_VERSION`), reads the build/install layout from
`hardcoded.lua`, uses Tarantool's `digest` module for md5, and adds embedded-use
guards (`rawget(_G,'arg')`, `cfg.initialized`). Stock upstream lacks all of
these, so tarantool-dependent rockspecs fail there. Because the subtree placed
the repo root here, the Lua modules live under `src/src/luarocks/`.

Bump with (track the fork's branch, not upstream tags):

```
git subtree pull --prefix=internal/luarocks/src \
    https://github.com/tarantool/luarocks.git luarocks-3.9.2-tarantool --squash
```

`extra/` holds the only files we control: shims preloaded ahead of upstream
modules. A shim over an upstream module preloads that module as
`extra.upstream.<name>`, copies it and replaces what it changes, so the rest
of the module runs unchanged; behaviour changes to upstream go there, never
into `src/`.

- `wrapper.lua` — dispatch entry point (`exec(bin, ...)`), maps subcommands to
  `luarocks.cmd.*` and runs `cmd.run_command`.
- `hardcoded.lua` — LuaRocks build/install settings. Reads `LUAROCKS_PREFIX`,
  `LUA_BINDIR`, `LUA_INCDIR` via the lua engine's custom
  `os.getenv` (backed by a Go map from `Config.Tarantool`), and the current
  working dir via the Go-bound `glr_getwd()` global.
- `cmd_path.lua` — `luarocks.cmd.path`, printing the `package.path` a stock
  interpreter would have.
- `fs_lua.lua` — `luarocks.fs.lua`, loading an external command only when it
  is the one run, and quoting the file name `fs.is_lua` puts on a shell
  command line.
- `util.lua` — `luarocks.util`, changed in place rather than copied, as other
  modules set fields its own functions read: `variable_substitutions` keeps a
  `$(NAME)` a substituted value holds.
- `build_cmake.lua` — `luarocks.build.cmake`, escaping the rockspec variables
  it substitutes into the double-quoted `-D` values of its cmake command line.

The embedded FS is declared in `embed.go`.
