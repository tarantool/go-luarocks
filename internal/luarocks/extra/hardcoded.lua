-- This file contains LuaRocks hardcoded settings.
-- Adapted from tt/cli/rocks/extra/hardcoded.lua. Env var names align with
-- upstream LuaRocks conventions; the go-luarocks lua engine serves them via a
-- custom os.getenv backed by a Go map populated from Config.Tarantool.
--
-- This module is the ONLY channel through which rocks.Config reaches the
-- embedded LuaRocks: the engine writes no config file. core/cfg.lua reads the
-- keys below in make_defaults, and FORCE_HARDCODED (set at the bottom) makes it
-- deep-merge this whole table into cfg.variables afterwards, which is how PWD
-- lands there. Values that vary per engine or per host come from globals the
-- engine installs before this module is required: glr_getwd, glr_pwd_command,
-- glr_servers, glr_host_arch.

local function get_tarantool_path()
    return os.getenv('LUA_BINDIR') or "/usr/bin"
end

local function get_tarantool_include_path()
    return os.getenv('LUA_INCDIR') or "/usr/include/tarantool"
end

local function get_tarantool_prefix_path()
    return os.getenv('LUAROCKS_PREFIX') or "/usr"
end

local cwd = glr_getwd()
local system, processor = glr_host_arch()

return {
    -- The host's system and processor, which cfg.init makes cfg.arch of, as
    -- the native backend names them (see glr_host_arch in client/lua.go).
    -- Without them cfg.init takes what luarocks.core.sysdetect reads off the
    -- first architecture of /bin/sh, which is x86_64 on macOS on Apple
    -- Silicon.
    SYSTEM = system,
    PROCESSOR = processor,
    PREFIX = get_tarantool_prefix_path(),
    LUA_BINDIR = get_tarantool_path(),
    LUA_INCDIR = get_tarantool_include_path(),
    LUA_MODULES_LIB_SUBDIR = [[/lib/tarantool]],
    LUA_MODULES_LUA_SUBDIR = [[/share/tarantool]],
    LUA_INTERPRETER = [[tarantool]],
    ROCKS_SUBDIR = [[/share/tarantool/rocks]],
    -- Where the system config-5.1.lua lives, the same as an upstream
    -- installer writes into its hardcoded.lua. Without it cfg.init falls
    -- back to detect_sysconfdir(), which derives the directory from cfg.lua's
    -- own chunk name — for the embedded tree that is the relative embed path
    -- ("src/src"), so /etc/luarocks was never read. LUAROCKS_SYSCONFDIR in
    -- the environment still overrides this.
    SYSCONFDIR = [[/etc/luarocks]],
    -- Config.Servers when the caller configured any, else the Tarantool
    -- default. cfg.lua reads this as cfg.rocks_servers, so a configured list
    -- REPLACES the default rather than stacking onto it.
    ROCKS_SERVERS = glr_servers() or {
        [[http://rocks.tarantool.org/]],
    },
    LOCALDIR = cwd,

    -- Anchors the shell-out fs backend to Config.WorkingDir; see
    -- glr_pwd_command in client/lua.go for why this is not "pwd".
    PWD = glr_pwd_command(),

    HOME_TREE_SUBDIR = [[/.rocks]],
    EXTERNAL_DEPS_SUBDIRS = {
        bin = "bin",
        lib = {"lib", [[lib64]]},
        include = "include",
    },
    RUNTIME_EXTERNAL_DEPS_SUBDIRS = {
        bin = "bin",
        lib = {"lib", [[lib64]]},
        include = "include",
    },
    LOCAL_BY_DEFAULT = true,
    FORCE_HARDCODED = true,
}
