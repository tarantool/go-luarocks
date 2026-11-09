package tree

import (
	"path/filepath"

	"github.com/tarantool/go-luarocks/internal/treelock"
)

// Paths computes the conventional subdirectories under a tree root.
//
// All methods are pure functions of Tree; they do not touch disk. The
// changes of a Tree create the directories they write into.
type Paths struct {
	// Tree is the tree root (e.g. ".rocks" in a project, or a system
	// tarantool prefix). Always an absolute or project-relative path.
	Tree string
}

// RocksDir returns <tree>/share/tarantool/rocks — the rocks_dir upstream
// luarocks refers to.
func (p Paths) RocksDir() string {
	return filepath.Join(p.Tree, "share", "tarantool", "rocks")
}

// LockFile returns <tree>/share/tarantool/rocks/.lock — the file whose
// advisory lock orders the changes of the tree (see Tree.Update). It is the
// library's own, not the tree's contents: Graft does not take it from its
// source, Check and View look past it, and Pack never packs it. A tool that
// archives or copies a whole tree leaves it out; the next change of the copy
// creates its own. Removing it from a tree in use lets two changes run at
// once.
func (p Paths) LockFile() string {
	return filepath.Join(p.RocksDir(), treelock.Name)
}

// InstallDir returns <tree>/share/tarantool/rocks/<name>/<ver> — the
// per-rock install directory holding rockspec, rock_manifest, build/, doc/.
func (p Paths) InstallDir(name, ver string) string {
	return filepath.Join(p.RocksDir(), name, ver)
}

// DeployLuaDir returns <tree>/share/tarantool — where dotted .lua modules
// are flattened to slashed paths during Deploy.
func (p Paths) DeployLuaDir() string {
	return filepath.Join(p.Tree, "share", "tarantool")
}

// DeployLibDir returns <tree>/lib/tarantool — where compiled .so modules
// are flattened to slashed paths during Deploy.
func (p Paths) DeployLibDir() string {
	return filepath.Join(p.Tree, "lib", "tarantool")
}

// BinDir returns <tree>/bin — destination for build.install.bin entries.
func (p Paths) BinDir() string {
	return filepath.Join(p.Tree, "bin")
}

// ConfDir returns <tree>/share/tarantool/rocks/<name>/<ver>/conf — the
// per-rock configuration directory. Upstream installs build.install.conf via
// install_to into path.conf_dir(name,version) (a per-rock dir) and never
// relocates conf to a shared etc dir (repos.deploy_files handles only
// bin/lua/lib), so conf files reside permanently under the install dir.
func (p Paths) ConfDir(name, ver string) string {
	return filepath.Join(p.InstallDir(name, ver), "conf")
}

// DocDir returns <tree>/share/tarantool/rocks/<name>/<ver>/doc — the
// per-rock documentation directory.
func (p Paths) DocDir(name, ver string) string {
	return filepath.Join(p.InstallDir(name, ver), "doc")
}
