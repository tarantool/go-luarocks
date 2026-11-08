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
// Deploy and DeleteVersion keep every file they write, rename or remove inside
// the tree, a deliberate strictness beyond upstream, which joins the names a
// rockspec or a rock_manifest gives as they come: a build.install key, a
// copy_directories entry, a rock_manifest entry, or a rock name or version
// that leaves its directory, absolute, through "..", or through a symlink
// pointing out, fails the operation with an error wrapping
// rocks.ErrUnsafePath, and the sources are read only inside srcDir and
// buildDir. Names that stay inside behave as upstream's do.
//
// This package uses forward-slash Unix paths exclusively via
// path/filepath; no Windows-specific handling.
package tree
