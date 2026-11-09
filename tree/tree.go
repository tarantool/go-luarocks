package tree

import (
	"errors"
	"os"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/manif"
)

// File and directory permission bits used across the tree package when
// materializing deployed rocks. Each is the mode a file or directory is
// created with, which the process umask then narrows, as upstream LuaRocks
// creates them: under the usual umask, 022, a tree is readable by everyone.
const (
	// dirPerm is applied to directories created in the rock tree.
	dirPerm os.FileMode = 0o777
	// filePerm is the default mode for regular deployed files (non-executable),
	// upstream's "read" permissions.
	filePerm os.FileMode = 0o644
	// execPerm is the mode for executable deployed files: install.bin scripts,
	// C modules and the generated command wrappers, upstream's "exec"
	// permissions.
	execPerm os.FileMode = 0o755
)

// Tree is the in-memory handle on a Tarantool rocks tree on disk. It
// composes the path scheme (Paths) and a ManifestStore. Operations
// that mutate the tree — Deploy chiefly — go through this type.
type Tree struct {
	Paths

	// Store reads/writes the tree-level and per-rock manifest files. The
	// default is manif.FileStore. Tests inject fakes.
	Store rocks.ManifestStore

	// BinWrap, when non-nil, makes Deploy reproduce upstream LuaRocks'
	// command-script handling (repos.deploy_files → fs.wrap_script): the real
	// script is installed under the per-rock dir and the public <tree>/bin entry
	// becomes a generated /bin/sh launcher. When nil, build.install.bin entries
	// are copied verbatim (the pre-parity behavior; kept for callers that
	// construct a Tree without a Tarantool target).
	BinWrap *BinWrap

	// Providers lists the installed rocks that provide a deploy item — a
	// module (ModuleItem) for a lua/lib file, a command (CommandItem) for a bin
	// file — as "name/version" tokens in the order of the tree manifest's
	// modules or commands index: index 0 is the provider holding the plain
	// path, index 1 the one that takes it over when index 0 goes away. It backs
	// Deploy's check_spot_if_available (repos.lua:248) and DeleteVersion's
	// promotion of the next provider (repos.lua:585). When nil, no item has a
	// provider: every deployed file takes its plain path. The facade sets this
	// from the tree manifest as it stands before the operation.
	Providers func(kind ItemKind, item string) []string

	// j records the changes of the tree's files, to undo them when the
	// change they are part of fails: a Tx's journal in the Tree a Tx method
	// works on, the journal of the call in Deploy and DeleteVersion called
	// on their own.
	j *journal
}

// BinWrap carries the two environment-dependent inputs upstream bakes into a
// command wrapper (fs/unix.lua wrap_script): the absolute interpreter path
// (cfg.variables.LUA_BINDIR + cfg.lua_interpreter) and cfg.sysconfdir.
type BinWrap struct {
	// Interpreter is the absolute path to the Lua interpreter the wrapper
	// execs, e.g. "/opt/tarantool/bin/tarantool".
	Interpreter string
	// Sysconfdir is the value exported as LUAROCKS_SYSCONFDIR, matching
	// upstream's cfg.sysconfdir (default "/etc/luarocks").
	Sysconfdir string
}

// Open returns a Tree handle backed by cfg.Tree on disk. It creates
// nothing, and the tree need not exist: each change creates the directories
// it writes into, Update the rocks dir and its lock file, so a tree that does
// not exist is still absent after it is viewed (View), checked (Check) or
// rebuilt (Rebuild), each of which takes it for an empty one.
//
// cfg.Tarantool fields are not consulted: tree management is independent
// of the Tarantool target (which only matters at build time).
func Open(cfg rocks.Config) (*Tree, error) {
	if cfg.Tree == "" {
		return nil, errors.New("tree.Open: Config.Tree is empty")
	}

	return &Tree{
		Paths: Paths{Tree: cfg.Tree},
		Store: manif.FileStore{},
	}, nil
}
