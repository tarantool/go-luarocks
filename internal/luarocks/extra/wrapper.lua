-- requested holds the names this dispatch asks for as a command: the command
-- and, for help, the command it asks about. The luarocks.fs.lua shim
-- (extra/fs_lua.lua) lets external-command discovery yield only these, so an
-- external command is loaded only when it is the one being run.
local wrapper = { requested = {} }

-- Options of the main parser that take a value (get_parser in cmd.lua),
-- hidden aliases included. Everything else that starts with "-" is a flag.
local valued_options = {
    ["--server"] = true, ["--from"] = true,
    ["--only-server"] = true, ["--only-from"] = true,
    ["--only-sources"] = true, ["--only-sources-from"] = true,
    ["--namespace"] = true,
    ["--lua-dir"] = true,
    ["--lua-version"] = true,
    ["--tree"] = true, ["--to"] = true,
    ["--timeout"] = true,
    ["--project-tree"] = true,
}

-- requested_commands finds the command in argv the way the main parser does:
-- the first positional, skipping global options with their values and the
-- VAR=VALUE assignments cmd.run_command strips before parsing. For help it
-- adds the next positional, the command help is asked about.
local function requested_commands(...)
    local args = { ... }
    local positionals = {}
    local i = 1
    while i <= #args and #positionals < 2 do
        local a = args[i]
        if a == "--" then
            for j = i + 1, #args do
                table.insert(positionals, args[j])
            end
            break
        elseif a:sub(1, 1) == "-" then
            if valued_options[a] then
                i = i + 1
            end
        elseif not a:match("^[^-][^=]*=") then
            table.insert(positionals, a)
        end
        i = i + 1
    end

    local requested = {}
    if positionals[1] then
        requested[positionals[1]] = true
        if positionals[1] == "help" and positionals[2] then
            requested[positionals[2]] = true
        end
    end
    return requested
end

function wrapper.exec(bin, ...)
    local cfg = require("luarocks.core.cfg")
    local util = require("luarocks.util")

    local arg = ...

    -- Tweak help messages.
    if arg == "admin" then
        util.this_program = function(default) -- luacheck: no unused args
            return bin .. " admin"
        end
    else
        util.this_program = function(default) -- luacheck: no unused args
            return bin
        end
    end
    -- luarocks.cmd freezes the program name at require time
    -- (cmd.lua: `local program = util.this_program("luarocks")`), and a pooled
    -- VM has already loaded it during warm-up under the default progname.
    -- Drop it from package.loaded so this dispatch gets a module whose frozen
    -- program matches THIS caller's progname, exactly as a fresh upstream
    -- process would. Reloaded modules keep cached references to the old table,
    -- but the frozen value is a module local that nothing outside cmd.lua can
    -- read, so stale references cannot observe the wrong name.
    package.loaded["luarocks.cmd"] = nil
    local cmd = require("luarocks.cmd")

    if arg == "admin" then
        local description = "LuaRocks repository administration interface"
        local admin_args = {...}

        table.remove(admin_args, 1)
        wrapper.requested = requested_commands(unpack(admin_args))

        local commands = {
           make_manifest = "luarocks.admin.cmd.make_manifest",
           add = "luarocks.admin.cmd.add",
           remove = "luarocks.admin.cmd.remove",
           refresh_cache = "luarocks.admin.cmd.refresh_cache",
        }

        cmd.run_command(description, commands, "luarocks.admin.cmd.external", unpack(admin_args))
        return
    end

    local rocks_commands = {
        build = "luarocks.cmd.build",
        config = "luarocks.cmd.config",
        doc = "luarocks.cmd.doc",
        download = "luarocks.cmd.download",
        init = "luarocks.cmd.init",
        install = "luarocks.cmd.install",
        lint = "luarocks.cmd.lint",
        list = "luarocks.cmd.list",
        make = "luarocks.cmd.make",
        make_manifest = "luarocks.admin.cmd.make_manifest",
        new_version = "luarocks.cmd.new_version",
        pack = "luarocks.cmd.pack",
        path = "luarocks.cmd.path",
        purge = "luarocks.cmd.purge",
        remove = "luarocks.cmd.remove",
        search = "luarocks.cmd.search",
        show = "luarocks.cmd.show",
        test = "luarocks.cmd.test",
        unpack = "luarocks.cmd.unpack",
        upload = "luarocks.cmd.upload",
        which = "luarocks.cmd.which",
        write_rockspec = "luarocks.cmd.write_rockspec",
    }

    wrapper.requested = requested_commands(...)
    cmd.run_command('LuaRocks package manager', rocks_commands, 'luarocks.cmd.external', ...)
end

-- use_rocks_trees puts the rock trees the engine hands over (glr_rocks_trees)
-- in place of the cfg.rocks_trees cfg.init built from the config files.
-- Nothing cfg.init derives depends on that list, so replacing it afterwards
-- is the same as a config file setting it, except for package.path:
-- luarocks.loader, which a warm VM has loaded, appended the old trees to it
-- (cfg.init_package_paths), and it gets the new ones instead. package.path
-- holds nothing else (client/lua.go newVM empties it).
local function use_rocks_trees()
    local cfg = require("luarocks.core.cfg")
    cfg.init()
    cfg.rocks_trees = glr_rocks_trees()
    package.path = ""
    cfg.init_package_paths()
end

-- exec_typed is exec for the typed methods of the Go client: the command runs
-- over the engine's rock trees rather than the configured ones.
function wrapper.exec_typed(bin, ...)
    use_rocks_trees()
    return wrapper.exec(bin, ...)
end

return wrapper
