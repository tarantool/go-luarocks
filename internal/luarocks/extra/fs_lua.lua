-- luarocks.fs.lua, with external-command discovery limited to the command
-- being run, and with fs.is_lua quoting the file name it checks.
--
-- cmd.run_command asks fs.modules for every module under luarocks.cmd.external
-- (luarocks.admin.cmd.external for admin) in the directories of package.path
-- and requires each one to build its parser, so an external command a rock
-- tree carries — a repository's committed .rocks among them — ran during every
-- command. For those two namespaces this shim's fs.modules returns only the
-- names the dispatch asks for as a command (extra.wrapper's requested set), so
-- `<name>` and `help <name>` still reach an external command, and nothing else
-- loads it; plain help no longer lists it.
--
-- The upstream module is preloaded as extra.upstream.luarocks.fs.lua and runs
-- unchanged. fs.init drops both modules from package.loaded when it reloads
-- the fs modules (its pattern, "luarocks%.fs%.", is unanchored), so a reload
-- gets a fresh upstream module, as it does without the shim.

local upstream = require("extra.upstream.luarocks.fs.lua")

local fs_lua = {}
for k, v in pairs(upstream) do
    fs_lua[k] = v
end

local external_namespaces = {
    ["luarocks.cmd.external"] = true,
    ["luarocks.admin.cmd.external"] = true,
}

function fs_lua.modules(at)
    local found = upstream.modules(at)
    if not external_namespaces[at] then
        return found
    end

    local requested = require("extra.wrapper").requested
    local wanted = {}
    for _, name in ipairs(found) do
        if requested[name] then
            table.insert(wanted, name)
        end
    end
    return wanted
end

-- lua_string returns s as a single-quoted Lua string literal that every Lua
-- reads back as s.
local function lua_string(s)
    s = s:gsub("\\", "\\\\"):gsub("'", "\\'"):gsub("\n", "\\n"):gsub("\r", "\\r")
    return "'" .. s .. "'"
end

-- is_lua is upstream's, which decides whether a command script is Lua by
-- running the configured interpreter on a chunk that loads it, with both
-- layers of that command line quoted. Upstream splices the file name into the
-- chunk, `loadfile('<name>')`, and the chunk into the shell command line in
-- double quotes, neither escaped, so under a path holding a single quote the
-- chunk did not parse, and under one holding a dollar sign the shell expanded
-- it, $(...) running as a command: the script was taken for a binary and
-- deployed unwrapped. For any other name the interpreter runs the same chunk.
-- Upstream also turns every backslash of the name into a slash, against the
-- escaping it lacks; that is kept for Windows, where a backslash separates
-- the name's directories, and dropped elsewhere, where it is part of a name.
function fs_lua.is_lua(filename)
    local fs = require("luarocks.fs")
    local dir = require("luarocks.dir")
    local cfg = require("luarocks.core.cfg")

    if cfg.is_platform("windows") then
        filename = filename:gsub([[%\]], "/")
    end
    local lua = fs.Q(dir.path(cfg.variables["LUA_BINDIR"], cfg.lua_interpreter))
    local chunk = "if loadfile(" .. lua_string(filename) .. ") then os.exit(0) else os.exit(1) end"
    return fs.execute_string(lua .. " -e " .. fs.Q(chunk)) == true
end

return fs_lua
