-- luarocks.cmd.path, with the output a stock interpreter would print.
--
-- The engine starts every VM with an empty package.path (newVM in
-- client/lua.go), so the VM never looks for modules in the current directory,
-- in /usr/local/share/lua/5.1 or on $LUA_PATH. Upstream `path` prints
-- package.path as part of LUA_PATH, though, so that evaluating its output keeps
-- what the user's Lua searched before. For the duration of the command this
-- shim puts back the package.path the VM would have held without the emptying:
-- the one gopher-lua computed (glr_default_package_path) with the rock trees
-- luarocks.loader appended through cfg.init_package_paths. The upstream module
-- is preloaded as extra.upstream.luarocks.cmd.path and runs unchanged.

local upstream = require("extra.upstream.luarocks.cmd.path")
local cfg = require("luarocks.core.cfg")
local util = require("luarocks.core.util")

local path_cmd = {}
for k, v in pairs(upstream) do
    path_cmd[k] = v
end

-- stock_package_path returns what cfg.init_package_paths makes of `default`
-- (util.cleanup_path(default .. ";" .. trees)), given what it made of the
-- empty string, `engine`: an empty first entry followed by the trees, already
-- deduplicated and normalized. The trees are appended as they are rather than
-- cleaned again, because cleaning is not idempotent ("///" becomes "//").
local function stock_package_path(engine, default)
    if engine == "" then
        -- luarocks.loader has not run: package.path is as newVM left it.
        return default
    end

    local parts = util.split_string(util.cleanup_path(default, ";", cfg.lua_version, true), ";")
    local seen = {}
    for _, part in ipairs(parts) do
        seen[part] = true
    end

    local trees = util.split_string(engine, ";")
    local first = trees[1] == "" and 2 or 1
    for i = first, #trees do
        if not seen[trees[i]] then
            seen[trees[i]] = true
            table.insert(parts, trees[i])
        end
    end

    return table.concat(parts, ";")
end

local function restore(engine, ok, ...)
    package.path = engine
    if not ok then
        error((...), 0)
    end
    return ...
end

function path_cmd.command(args)
    local engine = package.path
    package.path = stock_package_path(engine, glr_default_package_path())
    return restore(engine, pcall(upstream.command, args))
end

return path_cmd
