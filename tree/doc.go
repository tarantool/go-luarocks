// Package tree implements the on-disk Tarantool-rocks layout: the path
// scheme under <tree>/share/tarantool, <tree>/lib/tarantool, <tree>/bin,
// and the Deploy operation that copies a built rock's artifacts into it.
//
// Paths is the pure path calculator — every method derives a location from
// the Tree root without touching disk. Open wraps a rocks.Config into a Tree,
// which adds the disk-facing operations: Deploy moves a staged build into the
// per-rock install dir and the shared deploy dirs, Which resolves a dotted
// module name to its deployed file, and the conflict helpers (MungedPath)
// compute the versioned fallback names used when two rock versions deploy
// the same target.
//
// The layout is fixed for Tarantool:
//
//	<tree>/share/tarantool/rocks/<name>/<ver>/   — per-rock install dir
//	<tree>/share/tarantool/                       — deploy_lua_dir
//	<tree>/lib/tarantool/                         — deploy_lib_dir
//	<tree>/bin/                                   — bin scripts
//
// This package uses forward-slash Unix paths exclusively via
// path/filepath; no Windows-specific handling.
package tree
