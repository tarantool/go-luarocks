-- luarocks.build.cmake, with the rockspec's variables escaped where upstream
-- splices them into its cmake command line.
--
-- Upstream substitutes $(PREFIX), $(LUADIR) and the other rockspec variables
-- into the values of build.variables and passes each to sh as
-- -D<name>="<value>", in double quotes. A path in those variables — the
-- tree's, the build dir's — that holds a $, a `, a " or a \ was read by the
-- shell: under a tree whose path holds a dollar sign, sh expanded $HOME and ran
-- $(...) as a command, CMake got another prefix and the build installed beside
-- the tree. This shim runs upstream's cmake.run unchanged but for that
-- substitution, which takes each variable's value escaped for a double-quoted
-- word of sh. What a rockspec writes in a value itself still reaches the
-- shell as upstream passes it: a $ there is the rockspec's own, as it is in a
-- value a make build passes to make. Windows' cmd.exe expands none of these
-- in double quotes, so there the values are left as they are.
--
-- The upstream module is preloaded as extra.upstream.luarocks.build.cmake.

local upstream = require("extra.upstream.luarocks.build.cmake")
local util = require("luarocks.util")
local cfg = require("luarocks.core.cfg")

local cmake = {}
for k, v in pairs(upstream) do
    cmake[k] = v
end

-- double_quoted returns vars with each string value escaped for a
-- double-quoted word of sh, which reads it back as the value.
local function double_quoted(vars)
    local escaped = {}
    for k, v in pairs(vars) do
        if type(v) == "string" then
            v = v:gsub('[\\"$`]', "\\%0")
        end
        escaped[k] = v
    end
    return escaped
end

function cmake.run(rockspec, no_install)
    if cfg.is_platform("windows") then
        return upstream.run(rockspec, no_install)
    end

    -- upstream.run substitutes once, into build.variables, the values it
    -- passes as -D<name>="<value>".
    local substitute = util.variable_substitutions
    util.variable_substitutions = function(tbl, vars)
        return substitute(tbl, double_quoted(vars))
    end
    local ok, res, err = pcall(upstream.run, rockspec, no_install)
    util.variable_substitutions = substitute
    if not ok then
        error(res, 0)
    end
    return res, err
end

return cmake
