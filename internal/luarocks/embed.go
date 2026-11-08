// Package luarocksembed exposes the vendored Tarantool LuaRocks fork (luarocks-3.9.2-tarantool)
// (under src/) and the extra shims (under extra/) as an embedded
// filesystem. The lua engine in client/lua.go preloads these modules into a
// gopher-lua VM. Do not edit src/ directly; it is the vendored Tarantool LuaRocks fork.
// Shim behavior lives in extra/.
//
// Only the Lua modules (src/src/luarocks/) and the fork's license
// (src/COPYING) are embedded. The rest of the subtree — the Windows installer
// and its bundled .exe/.dll tools, the test suite, build scripts — is about
// 11 MB that every binary importing this module would otherwise carry, for
// code the engine never loads.
package luarocksembed

import "embed"

//go:embed src/src/luarocks extra src/COPYING
var FS embed.FS

// ReadFile reads a file from the embedded FS by its slash-separated path
// relative to the internal/luarocks directory (e.g. "extra/wrapper.lua" or
// "src/src/luarocks/core/cfg.lua").
func ReadFile(name string) ([]byte, error) {
	return FS.ReadFile(name)
}
