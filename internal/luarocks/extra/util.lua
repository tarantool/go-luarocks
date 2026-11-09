-- luarocks.util, with util.variable_substitutions substituting once.
--
-- Upstream substitutes the rockspec variables, $(PREFIX), $(LUADIR) and the
-- rest, into a value and then warns about every $(NAME) the result still
-- holds and removes it, as a variable the rockspec names but nobody defines.
-- The result holds the substituted values too, so a $(NAME) in one of them —
-- in a path, the tree's or the working dir's, as a directory may be named —
-- was taken for the rockspec's own and cut out of the path. This
-- variable_substitutions warns about and removes only the $(NAME)s the
-- rockspec writes that name no variable, as the native backend's
-- substitution does (build/vars.go). A value without a $( in a variable is
-- substituted as before.
--
-- The upstream module is preloaded as extra.upstream.luarocks.util, and the
-- shim changes its table in place rather than copying it: util's own
-- functions read fields of it that other modules set, util.this_program
-- among them (extra/wrapper.lua).

local util = require("extra.upstream.luarocks.util")

-- var_format_pattern is upstream's, a $(NAME) of a rockspec.
local var_format_pattern = "%$%((%a[%w_]*)%)"

--- Perform make-style variable substitutions on string values of a table:
-- every substring "$(XYZ)" of a string value of tbl is replaced by vars.XYZ,
-- a string or a number, or else, with a warning, by nothing. Only string
-- values are processed; subtables are not scanned.
-- @param tbl table: Table to have its string values modified.
-- @param vars table: Table containing string-string key-value pairs
-- representing variables to replace in the strings values of tbl.
function util.variable_substitutions(tbl, vars)
   assert(type(tbl) == "table")
   assert(type(vars) == "table")

   local updated = {}
   for k, v in pairs(tbl) do
      if type(v) == "string" then
         updated[k] = v:gsub(var_format_pattern, function(name)
            local value = vars[name]
            if type(value) == "string" or type(value) == "number" then
               return tostring(value)
            end
            util.warning("unmatched variable " .. name)
            return ""
         end)
      end
   end
   for k, v in pairs(updated) do
      tbl[k] = v
   end
end

return util
