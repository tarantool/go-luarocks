#!/usr/bin/env bash
# gen_goldens.sh — regenerate manif/testdata/persist/out/<name> from in/<name>
# using upstream luarocks' persist.save_from_table_to_string.
#
# Idempotent: running twice leaves the working tree unchanged.
#
# Requires:
#   - tarantool (LuaJIT 5.1) on PATH or at /opt/homebrew/bin/tarantool
#   - upstream luarocks source tree at $LUAROCKS_SRC
#     (defaults to ../../luarocks/src relative to this script)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IN_DIR="${SCRIPT_DIR}/testdata/persist/in"
OUT_DIR="${SCRIPT_DIR}/testdata/persist/out"

TARANTOOL="${TARANTOOL:-/opt/homebrew/bin/tarantool}"
if ! command -v "${TARANTOOL}" >/dev/null 2>&1; then
   if command -v tarantool >/dev/null 2>&1; then
      TARANTOOL="$(command -v tarantool)"
   else
      echo "gen_goldens.sh: tarantool not found (set TARANTOOL=)" >&2
      exit 1
   fi
fi

# Resolve the vendored upstream luarocks source. We assume it lives two
# directories above this worktree (the typical layout described in the task
# header). Override with LUAROCKS_SRC if needed.
DEFAULT_SRC="$(cd "${SCRIPT_DIR}/../../.." && pwd)/luarocks/src"
LUAROCKS_SRC="${LUAROCKS_SRC:-${DEFAULT_SRC}}"
if [[ ! -f "${LUAROCKS_SRC}/luarocks/persist.lua" ]]; then
   echo "gen_goldens.sh: cannot find persist.lua at ${LUAROCKS_SRC}/luarocks/persist.lua" >&2
   echo "  set LUAROCKS_SRC to point at the upstream luarocks src/ directory" >&2
   exit 1
fi

mkdir -p "${OUT_DIR}"

# Drive the generator entirely from a single Lua script piped on stdin.
# We stub the few luarocks.* modules that persist.lua requires at load time
# but does NOT use from save_from_table_to_string, so we don't have to drag
# in the full luarocks runtime (cfg, fs, dir, ...). The real upstream
# util.sortedpairs / util.keys are aliased through luarocks.core.util.
LUAROCKS_SRC="${LUAROCKS_SRC}" IN_DIR="${IN_DIR}" OUT_DIR="${OUT_DIR}" \
   "${TARANTOOL}" -e '
local src = os.getenv("LUAROCKS_SRC")
package.path = src .. "/?.lua;" .. src .. "/?/init.lua;" .. package.path

-- Stub modules that persist.lua require()s at top level but does not invoke
-- from save_from_table_to_string. Loud failure if anything ever calls them.
local function loud_stub(name)
   return setmetatable({}, { __index = function(_, k)
      error("gen_goldens stub: "..name.."."..k.." was called unexpectedly", 2)
   end })
end
package.preload["luarocks.dir"]      = function() return loud_stub("luarocks.dir") end
package.preload["luarocks.fs"]       = function() return loud_stub("luarocks.fs") end
package.preload["luarocks.core.cfg"] = function() return loud_stub("luarocks.core.cfg") end

-- luarocks.util pulls in many transitive deps; we only need a thin wrapper
-- around luarocks.core.util that exposes sortedpairs (the only entry point
-- save_from_table_to_string actually calls).
package.preload["luarocks.util"] = function()
   local core_util = require("luarocks.core.util")
   return setmetatable({}, { __index = core_util })
end

-- luarocks.core.persist is required at the top of persist.lua only so that
-- persist.run_file / persist.load_into_table can be re-exported. They are
-- not used by save_from_table_to_string. Persist.lua reads (not invokes)
-- the fields at load time, so we expose harmless functions that raise if
-- ever actually called.
package.preload["luarocks.core.persist"] = function()
   local function never(name)
      return function() error("gen_goldens stub: "..name.." was called unexpectedly", 2) end
   end
   return {
      run_file        = never("luarocks.core.persist.run_file"),
      load_into_table = never("luarocks.core.persist.load_into_table"),
   }
end

local persist = require("luarocks.persist")

local function read_file(p)
   local f, err = io.open(p, "rb")
   if not f then error(err) end
   local s = f:read("*a"); f:close(); return s
end

local function write_file(p, s)
   local f, err = io.open(p, "wb")
   if not f then error(err) end
   f:write(s); f:close()
end

local function list_dir(dirpath)
   local names = {}
   local pf = io.popen("ls -1 " .. ("%q"):format(dirpath))
   if not pf then error("cannot list "..dirpath) end
   for line in pf:lines() do table.insert(names, line) end
   pf:close()
   table.sort(names)
   return names
end

local in_dir  = os.getenv("IN_DIR")
local out_dir = os.getenv("OUT_DIR")

for _, name in ipairs(list_dir(in_dir)) do
   if name:match("%.lua$") then
      local in_path  = in_dir .. "/" .. name
      local out_path = out_dir .. "/" .. name:gsub("%.lua$", ".out")

      local chunk, err = loadstring(read_file(in_path), in_path)
      if not chunk then error("loadstring "..in_path..": "..tostring(err)) end
      local value = chunk()
      if type(value) ~= "table" then
         error("input "..in_path.." did not return a table")
      end

      local s, perr = persist.save_from_table_to_string(value)
      if not s then error("persist "..in_path..": "..tostring(perr)) end
      write_file(out_path, s)
      io.stderr:write("ok: " .. name .. " -> " .. out_path:match("[^/]+$") .. "\n")
   end
end
'

echo "gen_goldens.sh: wrote $(ls -1 "${OUT_DIR}" | wc -l | tr -d " ") golden(s) to ${OUT_DIR}"
