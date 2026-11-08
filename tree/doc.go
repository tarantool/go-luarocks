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
// Deploy and DeleteVersion change the files of a tree only; the tree manifest,
// <tree>/share/tarantool/rocks/manifest, which lists the installed rocks and
// the providers of each module and command, is kept in step with them by
// Update: it runs a change as a transaction, a Tx, under the tree's lock,
// whose Deploy, Remove and Prune change the files and the manifest together.
// Inspect, which creates nothing, and Tree.View read a tree into a View, a
// snapshot of its manifests that answers what is installed, which rock owns
// a deployed file and where each of a rock's files is, and checks the files
// against their rock_manifest. Which of a module's or command's providers
// holds its plain path follows the manifest's provider lists, as Deploy and
// DeleteVersion decide it.
//
// The changes a tool used to make to a tree by hand have a Tx method each:
// InstallStaged deploys a directory laid out as an unpacked binary rock as
// the native client installs a .rock, Graft installs the rocks another tree
// has on disk as their binary rocks would install, without trusting its
// manifest, and Rewrap rewrites the command wrappers for the tree's root and
// BinWrap. Tree.Rebuild rewrites the tree manifest from the rocks on disk,
// and Tree.Check reports, without changing anything, what is wrong with a
// tree: files missing, modified or at the wrong spot, rock dirs and manifest
// entries without each other, files no rock owns, and stale wrappers.
//
// This package uses forward-slash Unix paths exclusively via
// path/filepath; no Windows-specific handling.
package tree
