-- luarocks.fs.lua, with external-command discovery limited to the command
-- being run.
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

return fs_lua
