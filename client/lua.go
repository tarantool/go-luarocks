package client

// luaEngine is the gopher-lua backend (BackendLua). It boots the embedded
// Tarantool LuaRocks fork (3.9.2) into a fresh gopher-lua VM for every
// dispatch and runs write operations through extra/wrapper.lua's exec() entry
// point.
//
// Environment-override policy:
//
// The engine installs a custom os.getenv into the VM that consults an
// envOverride map first and falls through to the host process env for any
// other key. The override map serves the LuaRocks build-config keys
// used by the embedded VM — LUAROCKS_PREFIX, LUA_INCDIR, LUA_BINDIR,
// TARANTOOL_DIR and TT_CLI_TARANTOOL_VERSION. Every other os.getenv read (CC, CFLAGS, LDFLAGS, AR, RANLIB,
// LINK, MT, MAKE, WINDRES, CMAKE_*, PATH, HOME, USER, TMPDIR/TMP/TEMP, XDG_*,
// http_proxy/https_proxy/no_proxy, LUAROCKS_SYSCONFDIR,
// LUAROCKS_CROSS_COMPILING, LUA_PATH_/LUA_CPATH_, and Windows-only vars) falls
// through to the host process env by design — the build toolchain genuinely
// lives there. The engine never mutates the host env: it never calls
// os.Setenv.
//
// Config delivery:
//
// Everything the embedded LuaRocks needs from rocks.Config reaches it through
// luarocks.core.hardcoded, which is this module's own extra/hardcoded.lua
// preloaded from the embedded FS — never through a config file on disk. The
// dynamic values are served by Go globals the engine installs before preload:
// glr_getwd, glr_pwd_command and glr_servers. cfg.init consumes that module in
// make_defaults (core/cfg.lua) and, because our hardcoded sets FORCE_HARDCODED,
// deep-merges the whole table into cfg.variables afterwards. Keeping the engine
// free of on-disk state is why: a generated file needs a temp dir, an owner and
// a cleanup path, and the engine has no public Close to hang one on. The one
// setting a typed method takes from rocks.Config past cfg.init is
// cfg.rocks_trees, which extra/wrapper.lua's exec_typed replaces with what
// glr_rocks_trees serves.
//
// VM lifecycle: newLuaEngine creates no VM. Each dispatch takes a warm VM from
// the pool (or warms one inline), runs on it alone and closes it, so
// concurrent dispatches never share a VM; see the luaEngine type comment.
//
// Cancellation: a dispatch runs under the caller's context (luaVM.run), which
// the VM checks at every instruction and the engine's os.execute and io.popen
// run their processes under (lua_proc.go), so a cancelled call returns at
// once, with an error wrapping ctx.Err(), after LuaRocks' cleanup has run.
//
// Only Lua that arrived via the embedded FS is executed — embedded modules,
// compiled once per process and preloaded into each VM (compiledModules),
// plus the fixed wrapper dispatch string via DoString; no caller-supplied Lua
// source and no loadfile/dofile exposure.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	luarocksembed "github.com/tarantool/go-luarocks/internal/luarocks"
	"github.com/tarantool/go-luarocks/internal/treelock"
	"github.com/tarantool/go-luarocks/tree"
	lua "github.com/yuin/gopher-lua"
	"github.com/yuin/gopher-lua/parse"
)

// osExitSentinel prefixes the Lua error raised by the engine's os.exit
// replacement so call() can recover the requested exit code from the error
// message produced by DoString.
const osExitSentinel = "go-luarocks os.exit: "

const (
	// luaPoolSize is how many warm VMs the engine keeps parked. One keeps an
	// idle engine at the same footprint the old single-VM reuse had (a booted VM
	// is ~36 MB) while still hiding the warm-up of the next dispatch behind the
	// current one. A burst of concurrent calls beyond this warms VMs inline.
	luaPoolSize = 1

	// ioOpenModeArgIndex is the 1-based Lua argument position of io.open's mode
	// string; ioOpenMinArgsForMode is the argument count at or above which a
	// mode string is present (io.open(filename, mode)).
	ioOpenMinArgsForMode = 2

	// printWriteArgCount is the number of values pushed for the io.stdout write
	// call in the print shim (the file handle plus the line string).
	printWriteArgCount = 2

	// searchFieldsMin is the minimum tab-separated field count a --porcelain
	// search line must have to be a result record (name, version, arch, repo).
	searchFieldsMin = 4

	// decimalBase is the radix used when accumulating the exit-code digits.
	decimalBase = 10

	// globalArgsTreePair counts the two argv entries the mandatory --tree global
	// always contributes; per-server entries add two more apiece.
	globalArgsTreePair = 2
	argsPerServer      = 2
)

// luaEngine implements the Engine interface against an embedded LuaRocks VM.
//
// One VM per dispatch, never reused. Upstream LuaRocks is written for
// one-process-per-command, so its module-level state IS per-command state:
// cmd.lua prepends every --server onto cfg.rocks_servers and doubles it per
// --dev; sets cfg.no_manifest, cfg.verbose, cfg.connection_timeout and friends
// under "if args.X then" with no path back; accumulates VAR=VALUE into
// cfg.variables; util.run_scheduled_functions runs its list without clearing
// it, so rollbacks re-fire; fs/lua.lua's dir_stack survives an aborted command;
// and the manifest caches in core/manif.lua and manif.lua never invalidate,
// including for the local tree. Reusing one VM across dispatches inherits every
// one of those, and the list grows with each upstream release.
//
// Isolation is bought with a pool rather than a bare fresh VM because the
// expensive half is cfg.init (~45 ms on darwin/arm64), not the preload (~18 ms),
// and cfg.init only happens inside a dispatch. acquire therefore starts warming
// the next VM concurrently with the dispatch about to run, so that cost hides
// behind work the caller wanted anyway.
//
// What it hides behind is the caller's own command, which sets the terms:
// per-call cost is dispatch + max(0, warm - dispatch). A command that runs at
// least as long as a warm-up — install, build, a search that touches the
// network — pays nothing for isolation. A trivial one does not hide it:
// measured, a bare `help` costs ~24 ms of dispatch and ~69 ms end to end,
// which is what booting a VM per dispatch costs anyway.
//
// The alternative is to let the warm-up outlive the call, which measured ~24 ms
// per `help` — but warming shells out, so that leaves processes starting after
// the call returned, with nothing to stop them: the engine has no Close, and in
// tests it raced the framework's os.Stdout and ran `cd` into directories the
// test had already removed. release waits for the warm-up for that reason.
type luaEngine struct {
	cfg         rocks.Config
	store       rocks.ManifestStore
	logger      *slog.Logger
	envOverride map[string]string

	// pool holds VMs that are booted AND warmed (cfg.init already run), ready
	// to serve exactly one dispatch. A VM is never reused: see the type comment
	// for why. Capacity is luaPoolSize, so an idle engine holds the same single
	// VM the old reuse model did.
	pool chan *luaVM

	// callImpl, when non-nil, replaces callViaState as the dispatch backend.
	// It exists solely as a test seam so dispatch tests can assert the exact
	// argv a method builds (and feed a canned stdout to data-returning
	// methods) without booting the embedded VM. Production code leaves it nil.
	callImpl func(argv []string) (string, error)
}

// newLuaEngine constructs the engine and populates the env-override map from
// cfg.Tarantool. It creates no VM: the first dispatch warms one (acquire).
func newLuaEngine(cfg rocks.Config, store rocks.ManifestStore, logger *slog.Logger) *luaEngine {
	envOverride := map[string]string{}
	// Only add an entry when the source value is non-empty so the host-env
	// fallback can still apply for keys we have no opinion on.
	if cfg.Tarantool.Prefix != "" {
		envOverride["LUAROCKS_PREFIX"] = cfg.Tarantool.Prefix
	}

	// LUA_BINDIR follows the executable, not the prefix: upstream Lua code
	// (fs.is_lua, util.get_luajit_version) runs <LUA_BINDIR>/tarantool via
	// os.execute, and in a flat SDK the binary lives at the prefix root.
	if binDir := build.LuaBinDir(cfg); binDir != "" {
		envOverride["LUA_BINDIR"] = binDir
	}

	if cfg.Tarantool.IncludeDir != "" {
		envOverride["LUA_INCDIR"] = cfg.Tarantool.IncludeDir
		envOverride["TARANTOOL_DIR"] = filepath.Dir(cfg.Tarantool.IncludeDir)
	}

	if cfg.Tarantool.Version != "" {
		// The tarantool/luarocks fork registers `tarantool` as a provided
		// dependency from this env var (util.lua add_provided_versions) — there
		// is no `_TARANTOOL` global in the gopher-lua VM, so the env is the only
		// source. Without it, rockspecs with `dependencies = {"tarantool >= X"}`
		// fail resolution. Mirrors tt's TT_CLI_TARANTOOL_VERSION.
		envOverride["TT_CLI_TARANTOOL_VERSION"] = cfg.Tarantool.Version
	}

	e := &luaEngine{
		cfg:         cfg,
		store:       store,
		logger:      logger,
		envOverride: envOverride,
		pool:        make(chan *luaVM, luaPoolSize),
	}
	// Best-effort close of any warm VM still parked in the pool when the engine
	// is garbage-collected. The engine has no public Close (adding one would
	// burden every caller); it owns no on-disk state, so these are the only
	// resources a finalizer has to reclaim.
	runtime.SetFinalizer(e, func(e *luaEngine) { e.cleanup() })

	return e
}

// cleanup closes every VM parked in the pool. Safe to call when none was ever
// warmed. A VM currently serving a dispatch is not in the pool and is closed by
// release.
func (e *luaEngine) cleanup() {
	for {
		select {
		case vm := <-e.pool:
			vm.Close()
		default:
			return
		}
	}
}

// luaPreloadMap maps gopher-lua require names to their embedded-FS paths.
// Mirrors tt/cli/rocks/rocks.go's rocks_preload, with the path prefixes adapted
// to this module's embed layout: upstream modules live under "src/src/" (the
// git subtree placed the upstream repo root at internal/luarocks/src/), shims
// under "extra/". luarocks.cmd.path is a shim (extra/cmd_path.lua) over the
// upstream module, which is preloaded as extra.upstream.luarocks.cmd.path: the
// path command prints package.path, which newVM empties, so the shim shows the
// one a stock VM would have had. luarocks.fs.lua is a shim (extra/fs_lua.lua)
// over the upstream module, preloaded as extra.upstream.luarocks.fs.lua: it
// limits external-command discovery to the command being run, so a
// luarocks.cmd.external.<name> module is loaded only by `<name>` or
// `help <name>`, rather than by every command as upstream does.
var luaPreloadMap = func() map[string]string {
	rocksPath := "src/src/"
	extraPath := "extra/"

	return map[string]string{
		"extra.wrapper":           extraPath + "wrapper.lua",
		"luarocks.core.hardcoded": extraPath + "hardcoded.lua",
		"luarocks.core.util":      rocksPath + "luarocks/core/util.lua",
		"luarocks.core.persist":   rocksPath + "luarocks/core/persist.lua",
		"luarocks.core.sysdetect": rocksPath + "luarocks/core/sysdetect.lua",
		"luarocks.core.cfg":       rocksPath + "luarocks/core/cfg.lua",
		"luarocks.core.dir":       rocksPath + "luarocks/core/dir.lua",
		"luarocks.core.path":      rocksPath + "luarocks/core/path.lua",
		"luarocks.core.manif":     rocksPath + "luarocks/core/manif.lua",
		"luarocks.core.vers":      rocksPath + "luarocks/core/vers.lua",
		"luarocks.util":           rocksPath + "luarocks/util.lua",
		"luarocks.loader":         rocksPath + "luarocks/loader.lua",
		"luarocks.dir":            rocksPath + "luarocks/dir.lua",
		"luarocks.path":           rocksPath + "luarocks/path.lua",
		"luarocks.fs":             rocksPath + "luarocks/fs.lua",
		"luarocks.persist":        rocksPath + "luarocks/persist.lua",
		"luarocks.fun":            rocksPath + "luarocks/fun.lua",
		"luarocks.tools.patch":    rocksPath + "luarocks/tools/patch.lua",
		"luarocks.tools.zip":      rocksPath + "luarocks/tools/zip.lua",
		"luarocks.tools.tar":      rocksPath + "luarocks/tools/tar.lua",
		"luarocks.fs.unix":        rocksPath + "luarocks/fs/unix.lua",
		// luarocks.fs.macosx is INTENTIONALLY NOT preloaded: it is one of
		// luaAbsentModules. Its is_dir/is_file
		// (internal/luarocks/.../fs/macosx.lua) probe directory-ness via
		// io.open(path.."/.", "r") and inspect the PUC-Lua errno returned as the
		// third result (codes 2/13/20/21). gopher-lua's io.open does not surface
		// libc errno that way, so macosx.is_dir returns false for EVERY existing
		// directory — which breaks fs.check_command_permissions (the tree always
		// looks unwritable) and every downstream mkdir/copy. The absent-module
		// stub makes require("luarocks.fs.macosx") fail in load_platform_fns's
		// pcall, so fs.init falls through to the shell-out tools backend
		// (luarocks.fs.unix.tools → `test -d`), which works correctly in-VM.
		"luarocks.fs.unix.tools":           rocksPath + "luarocks/fs/unix/tools.lua",
		"luarocks.fs.lua":                  extraPath + "fs_lua.lua",
		"extra.upstream.luarocks.fs.lua":   rocksPath + "luarocks/fs/lua.lua",
		"luarocks.fs.tools":                rocksPath + "luarocks/fs/tools.lua",
		"luarocks.queries":                 rocksPath + "luarocks/queries.lua",
		"luarocks.type_check":              rocksPath + "luarocks/type_check.lua",
		"luarocks.type.rockspec":           rocksPath + "luarocks/type/rockspec.lua",
		"luarocks.rockspecs":               rocksPath + "luarocks/rockspecs.lua",
		"luarocks.signing":                 rocksPath + "luarocks/signing.lua",
		"luarocks.fetch":                   rocksPath + "luarocks/fetch.lua",
		"luarocks.type.manifest":           rocksPath + "luarocks/type/manifest.lua",
		"luarocks.manif":                   rocksPath + "luarocks/manif.lua",
		"luarocks.build.builtin":           rocksPath + "luarocks/build/builtin.lua",
		"luarocks.deps":                    rocksPath + "luarocks/deps.lua",
		"luarocks.deplocks":                rocksPath + "luarocks/deplocks.lua",
		"luarocks.cmd":                     rocksPath + "luarocks/cmd.lua",
		"luarocks.argparse":                rocksPath + "luarocks/argparse.lua",
		"luarocks.test.busted":             rocksPath + "luarocks/test/busted.lua",
		"luarocks.test.command":            rocksPath + "luarocks/test/command.lua",
		"luarocks.results":                 rocksPath + "luarocks/results.lua",
		"luarocks.search":                  rocksPath + "luarocks/search.lua",
		"luarocks.repos":                   rocksPath + "luarocks/repos.lua",
		"luarocks.cmd.show":                rocksPath + "luarocks/cmd/show.lua",
		"luarocks.cmd.path":                extraPath + "cmd_path.lua",
		"extra.upstream.luarocks.cmd.path": rocksPath + "luarocks/cmd/path.lua",
		"luarocks.cmd.write_rockspec":      rocksPath + "luarocks/cmd/write_rockspec.lua",
		"luarocks.manif.writer":            rocksPath + "luarocks/manif/writer.lua",
		"luarocks.remove":                  rocksPath + "luarocks/remove.lua",
		"luarocks.pack":                    rocksPath + "luarocks/pack.lua",
		"luarocks.build":                   rocksPath + "luarocks/build.lua",
		"luarocks.cmd.make":                rocksPath + "luarocks/cmd/make.lua",
		"luarocks.cmd.build":               rocksPath + "luarocks/cmd/build.lua",
		"luarocks.cmd.install":             rocksPath + "luarocks/cmd/install.lua",
		"luarocks.cmd.list":                rocksPath + "luarocks/cmd/list.lua",
		"luarocks.download":                rocksPath + "luarocks/download.lua",
		"luarocks.cmd.download":            rocksPath + "luarocks/cmd/download.lua",
		"luarocks.cmd.search":              rocksPath + "luarocks/cmd/search.lua",
		"luarocks.cmd.pack":                rocksPath + "luarocks/cmd/pack.lua",
		"luarocks.cmd.new_version":         rocksPath + "luarocks/cmd/new_version.lua",
		"luarocks.cmd.purge":               rocksPath + "luarocks/cmd/purge.lua",
		"luarocks.cmd.init":                rocksPath + "luarocks/cmd/init.lua",
		"luarocks.cmd.lint":                rocksPath + "luarocks/cmd/lint.lua",
		"luarocks.test":                    rocksPath + "luarocks/test.lua",
		"luarocks.cmd.test":                rocksPath + "luarocks/cmd/test.lua",
		"luarocks.cmd.which":               rocksPath + "luarocks/cmd/which.lua",
		"luarocks.cmd.remove":              rocksPath + "luarocks/cmd/remove.lua",
		"luarocks.upload.multipart":        rocksPath + "luarocks/upload/multipart.lua",
		"luarocks.upload.api":              rocksPath + "luarocks/upload/api.lua",
		"luarocks.cmd.upload":              rocksPath + "luarocks/cmd/upload.lua",
		"luarocks.cmd.doc":                 rocksPath + "luarocks/cmd/doc.lua",
		"luarocks.cmd.unpack":              rocksPath + "luarocks/cmd/unpack.lua",
		"luarocks.cmd.config":              rocksPath + "luarocks/cmd/config.lua",
		"luarocks.require":                 rocksPath + "luarocks/require.lua",
		"luarocks.build.cmake":             rocksPath + "luarocks/build/cmake.lua",
		"luarocks.build.make":              rocksPath + "luarocks/build/make.lua",
		"luarocks.build.command":           rocksPath + "luarocks/build/command.lua",
		"luarocks.fetch.cvs":               rocksPath + "luarocks/fetch/cvs.lua",
		"luarocks.fetch.svn":               rocksPath + "luarocks/fetch/svn.lua",
		"luarocks.fetch.sscm":              rocksPath + "luarocks/fetch/sscm.lua",
		"luarocks.fetch.git":               rocksPath + "luarocks/fetch/git.lua",
		"luarocks.fetch.git_file":          rocksPath + "luarocks/fetch/git_file.lua",
		"luarocks.fetch.git_http":          rocksPath + "luarocks/fetch/git_http.lua",
		"luarocks.fetch.git_https":         rocksPath + "luarocks/fetch/git_https.lua",
		"luarocks.fetch.git_ssh":           rocksPath + "luarocks/fetch/git_ssh.lua",
		"luarocks.fetch.hg":                rocksPath + "luarocks/fetch/hg.lua",
		"luarocks.fetch.hg_http":           rocksPath + "luarocks/fetch/hg_http.lua",
		"luarocks.fetch.hg_https":          rocksPath + "luarocks/fetch/hg_https.lua",
		"luarocks.fetch.hg_ssh":            rocksPath + "luarocks/fetch/hg_ssh.lua",
		"luarocks.admin.cache":             rocksPath + "luarocks/admin/cache.lua",
		"luarocks.admin.cmd.refresh_cache": rocksPath + "luarocks/admin/cmd/refresh_cache.lua",
		"luarocks.admin.index":             rocksPath + "luarocks/admin/index.lua",
		"luarocks.admin.cmd.add":           rocksPath + "luarocks/admin/cmd/add.lua",
		"luarocks.admin.cmd.remove":        rocksPath + "luarocks/admin/cmd/remove.lua",
		"luarocks.admin.cmd.make_manifest": rocksPath + "luarocks/admin/cmd/make_manifest.lua",
	}
}()

// luaAbsentModules are the modules LuaRocks probes for on its own, each under
// pcall, to pick up optional features — and that the engine therefore answers
// itself: newVM preloads a stub for each that fails the require the way a
// missing module does, so the fallback upstream has for it runs and nothing is
// looked up on disk. Without the stubs the probes reach the rock trees, which
// luarocks.loader puts on package.path, and a repository that commits
// .rocks/share/tarantool/lfs.lua would have it run by a plain `list`.
//
// The list comes from recording every require the preload did not satisfy —
// during help, list, path, config, show, which, remove, purge, the --help of
// every command and the builds of the integration suite, on Linux — and from
// reading the sources for what that run cannot reach: the platforms other than
// linux, ssl.https (probed only once socket.http has loaded) and mimetypes
// (upload only).
//
//   - luarocks.fs.<platform> and luarocks.fs.<platform>.tools (fs.init, for
//     each platform cfg detects; luaRocksPlatforms lists the ones it knows).
//     unix and unix.tools are preloaded and stay real. Of the rest upstream
//     ships only macosx, which is broken in gopher-lua (see luaPreloadMap),
//     and win32, which the engine never preloaded; for the others the stub
//     only settles the miss without a lookup.
//   - lfs, posix, md5, digest, bz2, zlib, socket.http, socket.ftp, ltn12 and
//     ssl.https (fs/lua.lua, tools/zip.lua, upload/api.lua): C libraries, or
//     Lua ones that need theirs, which gopher-lua cannot load.
//   - cjson, dkjson, json (util.lua, for config --json and upload) and
//     mimetypes (upload/multipart.lua): JSON and MIME helpers, some of them
//     pure Lua, kept off disk like the rest. A command that needs one fails
//     the way it does when none is installed.
//
// The plugin points stay open: luarocks.build.<type>, luarocks.test.<type>,
// luarocks.fetch.<protocol>, luarocks.cmd.external.<name> and
// luarocks.admin.cmd.external.<name> still load from the rock trees, because a
// backend or command installed there is how LuaRocks is extended. Each loads
// only when it is used: a build or test type a rockspec names, a protocol a
// source URL names, an external command argv names (see the luarocks.fs.lua
// shim in luaPreloadMap).
var luaAbsentModules = func() []string {
	mods := []string{
		"lfs", "posix", "md5", "digest", "bz2", "zlib",
		"socket.http", "socket.ftp", "ltn12", "ssl.https",
		"cjson", "dkjson", "json", "mimetypes",
	}

	for _, platform := range luaRocksPlatforms {
		if platform == "unix" {
			continue
		}

		mods = append(mods, "luarocks.fs."+platform, "luarocks.fs."+platform+".tools")
	}

	return mods
}()

// luaRocksPlatforms is platform_order from the vendored core/cfg.lua: every
// platform cfg.each_platform can yield, so every luarocks.fs.<platform> that
// fs.init can probe for.
var luaRocksPlatforms = []string{
	"unix", "bsd", "solaris", "netbsd", "openbsd", "freebsd", "dragonfly",
	"linux", "macosx", "cygwin", "msys", "haiku",
	"windows", "win32", "mingw", "mingw32", "msys2_mingw_w64",
}

// Compiled modules are shared by every VM. gopher-lua's *FunctionProto is
// produced without an LState (parse.Parse then lua.Compile) and installed into
// any of them with NewFunctionFromProto, which binds the proto to that VM's
// environment and leaves the proto itself read-only. So the embedded LuaRocks
// tree is parsed and compiled once per process instead of once per VM — which
// is what makes one-VM-per-dispatch affordable: compiling ~100 modules was the
// bulk of a boot.
var (
	luaProtoOnce sync.Once
	luaProtos    map[string]*lua.FunctionProto
	errLuaProto  error
)

// compiledModules parses and compiles every embedded module on first use and
// returns the shared result. Failures are cached with it: the embedded tree
// does not change between calls, so a retry could only fail the same way.
func compiledModules() (map[string]*lua.FunctionProto, error) {
	luaProtoOnce.Do(func() {
		protos := make(map[string]*lua.FunctionProto, len(luaPreloadMap))

		for modName, path := range luaPreloadMap {
			src, err := luarocksembed.FS.ReadFile(path)
			if err != nil {
				errLuaProto = fmt.Errorf("luaEngine: read embedded %s: %w", path, err)

				return
			}

			chunk, err := parse.Parse(bytes.NewReader(src), path)
			if err != nil {
				errLuaProto = fmt.Errorf("luaEngine: parse %s: %w", modName, err)

				return
			}

			proto, err := lua.Compile(chunk, path)
			if err != nil {
				errLuaProto = fmt.Errorf("luaEngine: compile %s: %w", modName, err)

				return
			}

			protos[modName] = proto
		}

		luaProtos = protos
	})

	return luaProtos, errLuaProto
}

// luaVM is a VM of the engine together with the processes its os.execute and
// io.popen start (lua_proc.go).
type luaVM struct {
	*lua.LState

	procs *luaProcs
}

// Close kills and collects the processes of the pipes the Lua code left open
// and closes the VM.
func (vm *luaVM) Close() {
	vm.procs.reap()
	vm.LState.Close()
}

// luaCleanupTimeout bounds the cleanup luaVM.run runs after a cancelled
// chunk.
const luaCleanupTimeout = 5 * time.Second

// luaCancelCleanup runs the functions LuaRocks scheduled to undo a command
// that did not finish — removing its temporary directories, rolling back a
// partial install — the way cmd.lua's die runs them when a command fails.
const luaCancelCleanup = `local util = package.loaded["luarocks.util"]
if util then util.run_scheduled_functions() end`

// run runs chunk on the VM under ctx: once ctx is done, the VM raises an
// error at its next instruction, and os.execute and io.popen kill their
// processes and raise it at once. A chunk the cancellation cut short gets
// LuaRocks' cleanup run on it, which the cancelled VM can no longer run
// itself, under a context of its own bounded by luaCleanupTimeout. Either
// way no process of the chunk outlives run (luaProcs.reap).
func (vm *luaVM) run(ctx context.Context, chunk string) error {
	err := vm.runUnder(ctx, chunk)
	if err != nil && ctx.Err() != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), luaCleanupTimeout)
		_ = vm.runUnder(cleanupCtx, luaCancelCleanup)

		cancel()
	}

	vm.procs.reap()

	return err
}

// runUnder runs chunk with the VM's context set to one derived from ctx,
// which run cancels once the chunk is done, so the contexts gopher-lua
// derives from it for the chunk's coroutines go with it, and removed
// afterwards, so a pooled VM holds none. A ctx that cannot be cancelled is not
// set at all: the VM checks a context at every instruction.
func (vm *luaVM) runUnder(ctx context.Context, chunk string) error {
	if ctx.Done() == nil {
		return vm.DoString(chunk)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	vm.SetContext(runCtx)
	defer vm.RemoveContext()

	return vm.DoString(chunk)
}

// newVM creates a gopher-lua VM with an empty package.path, installs the custom
// os.getenv, os.execute, io.popen, io.open and io.output and the glr_* globals,
// and preloads every embedded module. The caller owns the returned VM and must
// Close it; nothing is cached on the engine. cfg.init has NOT run yet at this
// point — that happens inside the first dispatch, which is what warmVM adds on
// top.
func (e *luaEngine) newVM() (*luaVM, error) {
	L := lua.NewState() // full stdlibs; do NOT skip OpenLibs

	pkgTbl, ok := L.GetField(L.Get(lua.EnvironIndex), "package").(*lua.LTable)
	if !ok {
		L.Close()

		return nil, errors.New("luaEngine: package table missing from Lua environment")
	}

	// Kept for the path command, which prints it (extra/cmd_path.lua).
	defaultPath := L.GetField(pkgTbl, "path").String()

	// Find modules only in package.preload and in the rock trees. gopher-lua
	// seeds package.path from $LUA_PATH or "./?.lua;/usr/local/share/lua/5.1/…",
	// and LuaRocks requires many modules we do not preload — luarocks.fs.linux,
	// lfs, socket.http, every luarocks.cmd.external.* it finds on the path — so
	// any file of that name in the current directory, in a system LuaRocks or on
	// $LUA_PATH would run inside the engine. The rock trees still get there:
	// luarocks.loader, which luarocks.cmd.which requires while cmd.run_command
	// loads the command table, appends cfg.rocks_trees to package.path
	// (cfg.init_package_paths), so a build backend installed into one of them
	// still loads; the optional modules LuaRocks probes for are stubbed in the
	// preload and never get that far (luaAbsentModules). package.cpath needs
	// nothing: gopher-lua leaves it empty and cannot load C modules.
	L.SetField(pkgTbl, "path", lua.LString(""))

	L.SetGlobal("glr_default_package_path", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(defaultPath))

		return 1
	}))

	// Install the custom os.getenv and the glr_* globals BEFORE preloading,
	// since hardcoded.lua reads them at require time.
	osTbl, ok := L.GetField(L.Get(lua.EnvironIndex), "os").(*lua.LTable)
	if !ok {
		L.Close()

		return nil, errors.New("luaEngine: os table missing from Lua environment")
	}

	// The functions a VM holds must not reference e: a warm VM waits in
	// e.pool, and a cycle through e, which carries a finalizer, is never
	// collected (runtime.SetFinalizer), so every engine would keep its VM.
	envOverride := e.envOverride

	L.SetField(osTbl, "getenv", L.NewFunction(func(L *lua.LState) int {
		key := L.CheckString(1)
		if v, ok := envOverride[key]; ok {
			L.Push(lua.LString(v))

			return 1
		}

		if v, ok := os.LookupEnv(key); ok {
			L.Push(lua.LString(v))

			return 1
		}

		L.Push(lua.LNil)

		return 1
	}))

	e.setConfigGlobals(L)

	ioTbl, ok := L.GetField(L.Get(lua.EnvironIndex), "io").(*lua.LTable)
	if !ok {
		L.Close()

		return nil, errors.New("luaEngine: io table missing from Lua environment")
	}

	// Run child processes under the VM's context; see lua_proc.go.
	procs := newLuaProcs(e.cfg.IsolateBuilds)
	if err := procs.install(L, osTbl, ioTbl); err != nil {
		L.Close()

		return nil, err
	}

	if err := installFileOpeners(L, ioTbl); err != nil {
		L.Close()

		return nil, err
	}

	// Intercept os.exit. The vendored LuaRocks CLI terminates every command
	// path with os.exit (luarocks/cmd.lua), which gopher-lua maps to Go's
	// os.Exit — fatal for our in-process VM, which would take the host
	// process down with it. Replace it with a function that raises a Lua error
	// carrying the requested exit code (prefixed with osExitSentinel) so the
	// surrounding DoString unwinds back into call() instead of killing the
	// process. call() maps code 0 to a nil error and any non-zero code to a
	// descriptive Go error.
	L.SetField(osTbl, "exit", L.NewFunction(func(L *lua.LState) int {
		code := L.OptInt(1, 0)
		L.RaiseError("%s%d", osExitSentinel, code)

		return 0
	}))

	// Route the base print() through the VM's io.stdout. gopher-lua's stock
	// print writes via fmt.Print to the host process stdout, bypassing the
	// io.stdout field that redirectIO swaps for an in-memory buffer. A
	// handful of LuaRocks commands emit user-facing data with print rather than
	// util.printout — notably `config <key>` (cmd/config.lua print_entry prints
	// string values via print). Without this shim that output would escape to
	// the real stdout and the captured string callViaState returns would be
	// empty, breaking the Config data parse. The shim tab-joins its args and
	// appends a newline exactly like PUC-Lua print, then writes to whatever
	// io.stdout currently is — so it honors the redirect and the no-Setenv
	// invariant (in-VM handle only, never the host stdout or os.Setenv).
	L.SetGlobal("print", L.NewFunction(func(L *lua.LState) int {
		ioTbl, ok := L.GetField(L.Get(lua.EnvironIndex), "io").(*lua.LTable)
		if !ok {
			return 0
		}

		out := L.GetField(ioTbl, "stdout")
		writeFn := L.GetField(out, "write")
		top := L.GetTop()

		parts := make([]string, top)

		for i := 1; i <= top; i++ {
			parts[i-1] = lua.LVAsString(L.Get(i))
		}

		line := strings.Join(parts, "\t") + "\n"

		L.Push(writeFn)
		L.Push(out)
		L.Push(lua.LString(line))
		L.Call(printWriteArgCount, 0)

		return 0
	}))

	protos, err := compiledModules()
	if err != nil {
		L.Close()

		return nil, err
	}

	preload := L.GetField(pkgTbl, "preload")

	for modName, proto := range protos {
		L.SetField(preload, modName, L.NewFunctionFromProto(proto))
	}

	// Fail every probe for an optional module the way a missing module fails,
	// without looking anywhere; see luaAbsentModules.
	absent := L.NewFunction(func(L *lua.LState) int {
		L.RaiseError("module %s not found: the go-luarocks engine does not provide it", L.OptString(1, "?"))

		return 0
	})

	for _, modName := range luaAbsentModules {
		L.SetField(preload, modName, absent)
	}

	return &luaVM{LState: L, procs: procs}, nil
}

// setConfigGlobals installs the glr_* globals extra/hardcoded.lua and
// extra/wrapper.lua read rocks.Config through.
func (e *luaEngine) setConfigGlobals(L *lua.LState) { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	// A copy, not e: see the comment on os.getenv in newVM.
	cfg := e.cfg

	L.SetGlobal("glr_getwd", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString(cfg.WorkingDir))

		return 1
	}))

	// glr_pwd_command backs hardcoded.PWD. The shell-out fs backend
	// (luarocks.fs.tools) resolves the base directory for every `cd <dir> && cmd`
	// it runs by RUNNING cfg.variables.PWD (default "pwd"), which would report
	// the host process cwd — wrong for an in-process engine whose logical cwd is
	// cfg.WorkingDir. Handing it an echo of WorkingDir makes relative build paths
	// (e.g. builtin `cp src/foo.lua`) resolve there, which replaces a host
	// os.Chdir (forbidden: the host process is shared) with a per-command cd.
	L.SetGlobal("glr_pwd_command", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LString("echo " + shellSingleQuote(cfg.WorkingDir)))

		return 1
	}))

	// glr_servers backs hardcoded.ROCKS_SERVERS. Returning nil for an empty
	// Config.Servers lets hardcoded.lua keep its own default visible in Lua
	// rather than duplicating it here.
	L.SetGlobal("glr_servers", L.NewFunction(func(L *lua.LState) int {
		if len(cfg.Servers) == 0 {
			L.Push(lua.LNil)

			return 1
		}

		servers := L.NewTable()
		for _, s := range cfg.Servers {
			servers.Append(lua.LString(s))
		}

		L.Push(servers)

		return 1
	}))

	// glr_rocks_trees backs cfg.rocks_trees for the typed methods (see
	// rocksTrees): extra/wrapper.lua's exec_typed puts it in place of the list
	// cfg.init built, so the --tree every typed method passes is always one of
	// the rock trees. Upstream assumes it is: a tree outside the list is
	// installed into, but never searched, by every deps mode but "one", so an
	// installed dependency is not found again (deps.lua fulfill_dependency's
	// assert). It also keeps a host config file's rocks_trees from changing
	// what a typed method does.
	L.SetGlobal("glr_rocks_trees", L.NewFunction(func(L *lua.LState) int {
		trees := L.NewTable()
		for _, tree := range rocksTrees(cfg) {
			trees.Append(lua.LString(tree))
		}

		L.Push(trees)

		return 1
	}))
}

// warmVM returns a VM that has both booted and run cfg.init, so a dispatch on
// it costs only the command itself. The warm-up is a real `help` dispatch
// rather than a direct cfg.init call: cmd.run_command initializes cfg with the
// values it detected, and cfg.initialized makes that a one-shot, so priming it
// any other way would leave the real dispatch with a config built from
// different inputs. `help` takes no flags, so it leaves none of the sticky
// cfg state behind that motivates the one-VM-per-dispatch rule. Its output is
// discarded rather than drained to the logger — nobody asked for it. It runs
// under ctx, and a warm-up ctx cut short yields no VM.
func (e *luaEngine) warmVM(ctx context.Context) (*luaVM, error) {
	vm, err := e.newVM() //nolint:contextcheck // the VM's functions take the context of the chunk they run in (luaVM.run), not one of their own
	if err != nil {
		return nil, err
	}

	restore := redirectIO(vm.LState, io.Discard, io.Discard)
	doErr := vm.run(ctx, dispatchString(wrapperExec, "go-luarocks", []string{"help"}))

	restore()

	if err := ctx.Err(); err != nil {
		vm.Close()

		return nil, fmt.Errorf("luaEngine: warm-up dispatch: %w", err)
	}

	// `help` unwinds through the os.exit shim, so a sentinel error carrying
	// exit code 0 is the expected outcome, exactly as in callViaState.
	if doErr != nil {
		if code, ok := parseOsExit(doErr.Error()); !ok || code != 0 {
			vm.Close()

			return nil, fmt.Errorf("luaEngine: warm-up dispatch: %w", doErr)
		}
	}

	return vm, nil
}

// acquire hands out a VM for exactly one dispatch, taking a warm one when the
// pool has it and warming inline otherwise (a burst of concurrent calls, or the
// very first call on a cold engine). It then starts warming the NEXT one in the
// background, so that cost overlaps the dispatch about to run instead of
// landing on the following call. warmed is closed once that warm-up is done;
// the caller hands it to release. Both warm-ups run under ctx, the caller's,
// so cancelling the call stops them too.
func (e *luaEngine) acquire(ctx context.Context) (*luaVM, <-chan struct{}, error) {
	var (
		vm  *luaVM
		err error
	)

	select {
	case vm = <-e.pool:
	default:
		vm, err = e.warmVM(ctx)
		if err != nil {
			return nil, nil, err
		}
	}

	return vm, e.startWarming(ctx), nil
}

// startWarming prepares a replacement VM concurrently with the dispatch that is
// about to run, and returns a channel closed once it is done. Failures are
// dropped: nothing is lost, because the next acquire warms inline and reports
// the error to whoever actually asked for work. So is a warm-up that ctx cut
// short: the VM it was warming never reaches the pool.
func (e *luaEngine) startWarming(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})

	go func() {
		defer close(done)

		warm, err := e.warmVM(ctx)
		if err != nil {
			// Nothing is lost: the next acquire warms inline and reports the
			// failure to whoever actually asked for work.
			return
		}

		select {
		case e.pool <- warm:
		default:
			warm.Close()
		}
	}()

	return done
}

// release retires the VM a dispatch ran on. The VM is closed rather than
// returned to the pool: whatever state the command left in it, a cancelled
// command included, is exactly what must not reach the next caller. It then
// waits for the background warm-up its acquire started, warmed, so a
// returned call leaves no work of ours still running. Each call waits for its own warm-up only: the calls of one client
// may run concurrently, and a WaitGroup shared by them was waited on by one
// while another added to it from zero, which the WaitGroup contract forbids
// and which panicked.
func (e *luaEngine) release(vm *luaVM, warmed <-chan struct{}) {
	vm.Close()
	<-warmed
}

// shellSingleQuote wraps s in single quotes for the POSIX shell. A single
// quote inside the value is escaped the standard way: close the quote, emit a
// backslash-escaped quote, reopen. The result is embedded in the command
// hardcoded.PWD hands to the shell-out fs backend, so a path containing a quote
// must not be able to end the argument.
func shellSingleQuote(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `'\''`) + `'`
}

// dispatch routes argv to the active dispatch backend. callImpl is a test seam
// (nil in production); when nil, the real embedded-VM path callViaState runs.
// It returns the stdout the LuaRocks command printed plus any error.
func (e *luaEngine) dispatch(ctx context.Context, argv []string) (string, error) {
	if e.callImpl != nil {
		return e.callImpl(argv)
	}

	return e.callViaState(ctx, argv)
}

// call is a thin error-only wrapper over callViaState, retained for the boot
// smoke tests in lua_test.go that predate the unified seam. New code uses
// dispatch/callViaState directly.
func (e *luaEngine) call(argv []string) error {
	_, err := e.callViaState(context.Background(), argv)

	return err
}

// callViaState dispatches argv through extra/wrapper.lua's exec_typed() — exec()
// over the engine's rock trees (glr_rocks_trees) — on a VM taken from the pool and retired afterwards, so no state this command leaves behind
// can reach the next one. progname is fixed to the default value. Each arg is
// single-quote-escaped for embedding in the Lua single-quoted dispatch string.
//
// stdout/stderr capture: before running the command, callViaState
// replaces the Lua VM's io.stdout and io.stderr fields with buffer-backed file
// tables (newWriterFile) pointing at in-memory Go buffers, restoring the
// originals afterward. LuaRocks emits all user-facing text through
// util.printout/printerr, which write to io.stdout/io.stderr, so this captures
// it. After the command finishes, the captured text is drained into e.logger at
// info level (slog.Default() if e.logger is nil); the no-Setenv invariant holds
// — we touch only the in-VM io handles, never the host process stdout/stderr or
// os.Setenv. The
// captured stdout string is returned so data-returning methods (e.g. Pack) can
// parse it.
//
// The command runs under ctx (luaVM.run); a command that fails once ctx is
// done fails with an error wrapping ctx.Err().
func (e *luaEngine) callViaState(ctx context.Context, argv []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", cancelledError(argv, err)
	}

	vm, warmed, err := e.acquire(ctx)
	if err != nil {
		return "", contextError(ctx, argv, err)
	}

	defer e.release(vm, warmed)

	var stdout, stderr bytes.Buffer

	restore := redirectIO(vm.LState, &stdout, &stderr)
	defer restore()

	doErr := vm.run(ctx, dispatchString(wrapperExecTyped, "go-luarocks", argv))

	// Drain whatever the command printed into the logger before interpreting
	// the result, so even a failing command's diagnostics reach the caller.
	restore()
	e.drainOutput(argv, stdout.String(), stderr.String())
	out := stdout.String()

	if doErr == nil {
		// The wrapper returned normally (no os.exit). Treat as success.
		return out, nil
	}
	// The CLI almost always unwinds via our os.exit replacement, surfacing as
	// a Lua error whose message embeds osExitSentinel + the exit code. A code
	// of 0 is success; any non-zero code (or a non-sentinel error) is a real
	// failure surfaced verbatim.
	if code, ok := parseOsExit(doErr.Error()); ok {
		if code == 0 {
			return out, nil
		}

		return out, contextError(ctx, argv,
			fmt.Errorf("luaEngine: %v exited with code %d%s", argv, code, failureMessage(stderr.String())))
	}

	return out, contextError(ctx, argv, doErr)
}

// contextError returns err, the failure of the command argv, or, when ctx is
// done, an error wrapping ctx.Err() instead: the command failed because it
// was cancelled, whatever it reported then.
func contextError(ctx context.Context, argv []string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return cancelledError(argv, ctxErr)
	}

	return err
}

// cancelledError is the failure of the command argv cancelled with ctxErr.
func cancelledError(argv []string, ctxErr error) error {
	return fmt.Errorf("luaEngine: %v: %w", argv, ctxErr)
}

// failureMessage returns, prefixed with ": ", what a failed LuaRocks command
// printed to explain the failure, so the error reporting it says why and not
// only that it failed — the output itself reaches no one but the logger. That
// is the text from its last "Error:" line on, the way cmd.die prints a
// message, or else its last stderr line; empty when it printed nothing.
func failureMessage(stderr string) string {
	text := strings.TrimSpace(stderr)
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	from := len(lines) - 1

	for i, line := range slices.Backward(lines) {
		if strings.HasPrefix(line, "Error:") {
			from = i

			break
		}
	}

	return ": " + strings.Join(lines[from:], " ")
}

// The entry points of extra/wrapper.lua a dispatch string calls.
const (
	// wrapperExec runs a command as the host's LuaRocks would: over the
	// configuration cfg.init read. It backs Exec and the warm-up.
	wrapperExec = "exec"
	// wrapperExecTyped runs a command for a typed method: over the rock
	// trees of the engine's Config instead of the configured ones.
	wrapperExecTyped = "exec_typed"
)

// dispatchString builds the Lua one-liner that invokes the entry point entry
// of extra/wrapper.lua (wrapperExec or wrapperExecTyped) with the given
// program name and the single-quote-escaped argv. It is shared by callViaState
// (capturing, with the default progname) and callRaw (passthrough,
// caller-supplied progname so `tt rocks` can show its own usage).
func dispatchString(entry, progname string, argv []string) string {
	parts := make([]string, 0, len(argv)+1)
	parts = append(parts, quoteLua(progname))

	for _, arg := range argv {
		parts = append(parts, quoteLua(arg))
	}

	return fmt.Sprintf("t=require('extra.wrapper').%s(%s)", entry, strings.Join(parts, ", "))
}

// quoteLua wraps s in single quotes, escaping embedded single quotes, for
// embedding in the dispatch string.
func quoteLua(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `\'`) + "'"
}

// callRaw dispatches argv through extra/wrapper.lua's exec() WITHOUT capturing
// io.stdout/io.stderr, so LuaRocks writes straight to the process's stdout and
// stderr. It backs the public Rocks.Exec escape hatch (tt's `tt rocks`), which
// must reproduce upstream's terminal output verbatim; the capturing
// callViaState exists for programmatic methods that parse the output.
//
// progname is the program name LuaRocks prints in its own usage/help, letting
// the caller (e.g. tt) show "tt rocks" instead of the default progname.
//
// Like callViaState it runs on a VM of its own from acquire, under ctx, and
// retires it afterwards.
// Exit-code handling matches callViaState: os.exit(0) and a clean return
// are success, any other code is surfaced as an error.
func (e *luaEngine) callRaw(ctx context.Context, progname string, argv []string) error {
	if err := ctx.Err(); err != nil {
		return cancelledError(argv, err)
	}

	vm, warmed, err := e.acquire(ctx)
	if err != nil {
		return contextError(ctx, argv, err)
	}

	defer e.release(vm, warmed)

	doErr := vm.run(ctx, dispatchString(wrapperExec, progname, argv))
	if doErr == nil {
		return nil
	}

	if code, ok := parseOsExit(doErr.Error()); ok {
		if code == 0 {
			return nil
		}

		return contextError(ctx, argv, fmt.Errorf("luaEngine: %v exited with code %d", argv, code))
	}

	return contextError(ctx, argv, doErr)
}

// redirectIO points the given VM's io.stdout and io.stderr at the supplied Go
// buffers by installing buffer-backed file userdata via io.output()/io.errput.
// It returns a function that restores the original handles; the function is
// idempotent so callViaState can both defer it and call it eagerly. Only the
// in-VM io handles are touched.
func redirectIO(L *lua.LState, stdout, stderr io.Writer) func() { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	ioTbl, ok := L.GetField(L.Get(lua.EnvironIndex), "io").(*lua.LTable)
	if !ok {
		return func() {}
	}

	origOut := L.GetField(ioTbl, "stdout")
	origErr := L.GetField(ioTbl, "stderr")

	L.SetField(ioTbl, "stdout", newWriterFile(L, stdout))
	L.SetField(ioTbl, "stderr", newWriterFile(L, stderr))

	restored := false

	return func() {
		if restored {
			return
		}

		restored = true

		L.SetField(ioTbl, "stdout", origOut)
		L.SetField(ioTbl, "stderr", origErr)
	}
}

// newWriterFile builds a Lua table that quacks like an io file handle for the
// subset of methods LuaRocks' util.printout/printerr use: write (called as
// f:write(...)) and the no-op flush/close. All bytes go to w.
func newWriterFile(L *lua.LState, w io.Writer) *lua.LTable { //nolint:gocritic // L is the conventional gopher-lua LState receiver name used throughout
	tbl := L.NewTable()
	write := L.NewFunction(func(L *lua.LState) int {
		// arg 1 is the file table (self); the rest are the strings to write.
		top := L.GetTop()
		for i := 2; i <= top; i++ {
			v := L.Get(i)
			if v == lua.LNil {
				continue
			}

			if _, err := io.WriteString(w, lua.LVAsString(v)); err != nil {
				L.RaiseError("go-luarocks io capture: %v", err)
			}
		}

		L.Push(tbl) // io files return self for chaining

		return 1
	})
	noop := L.NewFunction(func(L *lua.LState) int {
		L.Push(tbl)

		return 1
	})

	L.SetField(tbl, "write", write)
	L.SetField(tbl, "flush", noop)
	L.SetField(tbl, "close", noop)

	return tbl
}

// drainOutput logs captured stdout/stderr at info level so command diagnostics
// are not lost. e.logger is used when set; otherwise slog.Default().
func (e *luaEngine) drainOutput(argv []string, stdout, stderr string) {
	logger := e.logger
	if logger == nil {
		logger = slog.Default()
	}

	cmd := strings.Join(argv, " ")
	if s := strings.TrimRight(stdout, "\n"); s != "" {
		logger.Info("luaEngine stdout", "cmd", cmd, "output", s)
	}

	if s := strings.TrimRight(stderr, "\n"); s != "" {
		logger.Info("luaEngine stderr", "cmd", cmd, "output", s)
	}
}

// packedPathRE matches the line upstream luarocks pack prints on success:
// `Packed: <path>` (luarocks/pack.lua report_and_sign_local_file).
var packedPathRE = regexp.MustCompile(`(?m)^Packed:\s*(.+?)\s*$`)

// parsePackPath extracts the produced rock path from pack's captured stdout.
// It returns ("", false) when no `Packed:` line is present so the caller can
// raise a real error rather than fabricating a path (no silent fallback).
func parsePackPath(stdout string) (string, bool) {
	m := packedPathRE.FindStringSubmatch(stdout)
	if m == nil {
		return "", false
	}

	return m[1], true
}

// wrotePathRE matches the line LuaRocks prints after writing a rockspec file:
//   - new_version: `Wrote <path>` (cmd/new_version.lua).
//   - write_rockspec: `Wrote template at <path> -- you should now edit ...`
//     (cmd/write_rockspec.lua).
//
// Both share the `Wrote ` / `Wrote template at ` prefix; the alternation
// captures the path and stops the write_rockspec variant before its trailing
// " -- you should now edit and finish it." hint.
var wrotePathRE = regexp.MustCompile(`(?m)^Wrote (?:template at )?(.+?)(?: -- .*)?\s*$`)

// parseWrotePath extracts the written rockspec path from new_version /
// write_rockspec stdout. Returns ("", false) when no `Wrote` line is present so
// the caller raises a real error rather than fabricating a path.
func parseWrotePath(stdout string) (string, bool) {
	m := wrotePathRE.FindStringSubmatch(stdout)
	if m == nil {
		return "", false
	}

	return m[1], true
}

// parseSearchResults parses the --porcelain listing search.print_result_tree
// emits: one tab-separated record per match,
// "<name>\t<version>\t<arch>\t<repo>\t<namespace>". Title lines are suppressed
// under --porcelain, so every non-blank line with at least four tab fields is
// a result; lines that do not match that shape are skipped (defensive against
// stray diagnostics that may share the buffer).
//
// The namespace field is present only when the match carries one:
// util.printout concatenates its arguments and a nil namespace ends the
// argument list, so an un-namespaced match prints four fields, not five.
func parseSearchResults(stdout string) []SearchResult {
	// Non-nil so a search that matched nothing returns an empty slice rather
	// than nil, which is what the native backend returns and what Search's
	// contract promises.
	out := []SearchResult{}

	for line := range strings.SplitSeq(stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields := strings.Split(line, "\t")
		if len(fields) < searchFieldsMin {
			continue
		}

		res := SearchResult{
			Name:    fields[0],
			Version: fields[1],
			Arch:    fields[2],
			Server:  fields[3],
		}
		if len(fields) > searchFieldsMin {
			res.Namespace = fields[searchFieldsMin]
		}

		out = append(out, res)
	}

	return out
}

// normalizeOpenMode rewrites a PUC-Lua-5.1 file mode string into the form
// gopher-lua's io.open accepts. gopher-lua rejects the "<mode>+b" ordering
// (r+b, w+b, a+b) but accepts the equivalent "<mode>b+" ordering (rb+, wb+,
// ab+); both denote the same update-binary mode. Any other mode (including
// already-normalized or non-binary modes) is returned unchanged.
func normalizeOpenMode(mode string) string {
	switch mode {
	case "r+b":
		return "rb+"
	case "w+b":
		return "wb+"
	case "a+b":
		return "ab+"
	default:
		return mode
	}
}

// parseOsExit extracts the exit code from a DoString error message produced by
// the engine's os.exit replacement. The gopher-lua error string embeds the
// sentinel somewhere after a position prefix; returns (code, true) on a match.
func parseOsExit(msg string) (int, bool) {
	_, after, ok := strings.Cut(msg, osExitSentinel)
	if !ok {
		return 0, false
	}

	rest := after
	// The code runs up to the first non-digit (gopher-lua may append a
	// stack traceback after the message).
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}

	if end == 0 {
		return 0, false
	}

	code := 0
	for i := range end {
		code = code*decimalBase + int(rest[i]-'0')
	}

	return code, true
}

// --- Engine interface implementation ---
//
// The five methods the native backend also serves (Install, Build, Make, Pack,
// Unpack) build the upstream argv and dispatch; the remaining methods map to
// upstream-only commands.

// globalArgs returns the LuaRocks global options that must precede the
// subcommand name. --tree is ALWAYS emitted so the lua backend installs into
// the SAME tree the native engine uses (backend parity); it reads the engine's
// cfg.Tree. servers, when non-empty, append a --server <s> per entry (the
// global option lives on the main parser, before the subcommand).
func (e *luaEngine) globalArgs(servers []string) []string {
	argv := make([]string, 0, globalArgsTreePair+argsPerServer*len(servers))
	argv = append(argv, "--tree", e.cfg.Tree)

	for _, s := range servers {
		argv = append(argv, "--server", s)
	}

	return argv
}

// dispatchLocked runs argv as dispatch does, holding the exclusive lock of
// e.cfg.Tree that the native backend and tree.Tree.Update take (see package
// internal/treelock), so a typed write of the lua backend and a change to the
// tree through them never run at once. It waits for the lock until ctx ends,
// and fails then with an error wrapping tree.ErrLocked.
//
// When create is set and the tree's root exists, its rocks dir is created
// first, as the write would. A tree whose rocks dir does not exist then, or
// whose lock cannot be taken for another reason, such as a read-only file
// system, is written unlocked: upstream reports what the write cannot do.
func (e *luaEngine) dispatchLocked(ctx context.Context, create bool, argv []string) error {
	dir := tree.Paths{Tree: e.cfg.Tree}.RocksDir()

	if _, err := os.Stat(e.cfg.Tree); err == nil && create {
		_ = os.MkdirAll(dir, luaTreeDirMode)
	}

	if _, err := os.Stat(dir); err == nil {
		lock, err := treelock.Exclusive(ctx, dir)

		switch {
		case err == nil:
			defer lock.Unlock()
		case errors.Is(err, treelock.ErrBusy):
			return fmt.Errorf("luaEngine: %w: %w", tree.ErrLocked, err)
		}
	}

	_, err := e.dispatch(ctx, argv)

	return err
}

// luaTreeDirMode is the mode dispatchLocked creates a missing rocks dir with:
// 0777, which the umask narrows, as the mkdir -p upstream runs does and as
// the native backend creates the directories of a tree.
const luaTreeDirMode = 0o777

// depsModeArg maps a DepsPolicy to upstream's --deps-mode choices
// {all, one, order, none}. DepsAll→"all", DepsNone→"none". DepsOnlyNew has no
// exact upstream equivalent; "order" (use the current tree plus those below it
// on rocks_trees) is the closest match to the "only new" documented intent.
// No policy maps to "one", upstream's default, which Build and Make run in,
// since they pass no --deps-mode.
func depsModeArg(p DepsPolicy) string {
	switch p {
	case DepsNone:
		return "none"
	case DepsOnlyNew:
		// Closest upstream match to "only new" — see DepsPolicy doc.
		return "order"
	case DepsAll:
		fallthrough
	default:
		return "all"
	}
}

func (e *luaEngine) Install(ctx context.Context, name string, opts InstallOpts) error {
	argv := e.globalArgs(opts.Servers)

	argv = append(argv, "install", name)

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	argv = append(argv, "--deps-mode", depsModeArg(opts.Deps))

	return e.dispatchLocked(ctx, true, argv)
}

// Build runs `luarocks build`. BuildOpts.Keep is passed as --keep, which
// upstream takes to keep the other installed versions of the rock; the
// native backend takes it to keep its staging dir.
func (e *luaEngine) Build(ctx context.Context, specPath string, opts BuildOpts) error {
	argv := e.globalArgs(nil)

	argv = append(argv, "build", specPath)

	if opts.Keep {
		argv = append(argv, "--keep")
	}

	return e.dispatchLocked(ctx, true, argv)
}

func (e *luaEngine) Make(ctx context.Context, opts MakeOpts) error {
	argv := e.globalArgs(nil)
	argv = append(argv, "make")
	// Upstream make searches the current dir (glr_getwd) when no rockspec
	// positional is given; only append it when explicitly set.
	if opts.RockspecPath != "" {
		argv = append(argv, opts.RockspecPath)
	}

	return e.dispatchLocked(ctx, true, argv)
}

func (e *luaEngine) Pack(ctx context.Context, target string, opts PackOpts) (string, error) {
	// opts.SrcOnly: upstream pack has no dedicated flag — it produces a
	// .src.rock when target is a rockspec and a binary .rock when target is an
	// installed rock name (luarocks/cmd/pack.lua). There is no clean argv
	// expression to force src-only for an installed-rock target through this
	// path, so SrcOnly is honored implicitly by the target kind. We do NOT
	// silently swallow it: when SrcOnly is set against a non-rockspec target it
	// simply has no effect here, left for a follow-up rather than guessed.
	argv := e.globalArgs(nil)
	argv = append(argv, "pack", target)

	out, err := e.dispatch(ctx, argv)
	if err != nil {
		return "", err
	}

	path, ok := parsePackPath(out)
	if !ok {
		return "", fmt.Errorf("luaEngine: pack %s succeeded but no 'Packed:' path line found in output: %q", target, out)
	}

	return path, nil
}

func (e *luaEngine) Unpack(ctx context.Context, archive, destDir string) error {
	// Upstream unpack has no destination flag: it creates a directory derived
	// from the rock name inside the current working dir (luarocks/cmd/unpack.lua
	// run_unpacker → fs.make_dir(dir_name)). The VM's cwd is reported by
	// glr_getwd, which returns cfg.WorkingDir. destDir is therefore not
	// expressible as an argv flag here; the caller controls the extraction root
	// via cfg.WorkingDir. We do not invent a flag or silently drop destDir —
	// the parameter's effect is documented as bounded by cfg.WorkingDir.
	_ = destDir
	argv := e.globalArgs(nil)
	argv = append(argv, "unpack", archive)
	_, err := e.dispatch(ctx, argv)

	return err
}

// Remove runs `luarocks remove`. It is tree-scoped: the --tree global precedes
// the subcommand. name is the rock; opts.Version narrows to one version.
func (e *luaEngine) Remove(ctx context.Context, name string, opts RemoveOpts) error {
	argv := e.globalArgs(nil)

	argv = append(argv, "remove", name)

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if opts.Force {
		argv = append(argv, "--force")
	}

	if opts.ForceFast {
		argv = append(argv, "--force-fast")
	}

	argv = append(argv, "--deps-mode", depsModeArg(opts.Deps))

	return e.dispatchLocked(ctx, false, argv)
}

// Purge runs `luarocks purge` against the engine's tree (the mandatory --tree
// global is always emitted).
func (e *luaEngine) Purge(ctx context.Context, opts PurgeOpts) error {
	argv := e.globalArgs(nil)

	argv = append(argv, "purge")

	if opts.OldVersions {
		argv = append(argv, "--old-versions")
	}

	if opts.Force {
		argv = append(argv, "--force")
	}

	if opts.ForceFast {
		argv = append(argv, "--force-fast")
	}

	return e.dispatchLocked(ctx, false, argv)
}

// Search runs `luarocks search --porcelain` and parses the tab-separated
// listing into []SearchResult. --porcelain is always passed so the output is
// machine-parseable. Errors loud if the command fails.
func (e *luaEngine) Search(ctx context.Context, pattern string, opts SearchOpts) ([]SearchResult, error) {
	argv := e.globalArgs(opts.Servers)

	argv = append(argv, "search", pattern)

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if opts.Source {
		argv = append(argv, "--source")
	}

	if opts.Binary {
		argv = append(argv, "--binary")
	}

	if opts.All {
		argv = append(argv, "--all")
	}

	argv = append(argv, "--porcelain")

	out, err := e.dispatch(ctx, argv)
	if err != nil {
		return nil, err
	}
	// An empty result set is a legitimate outcome (a successful search that
	// matched nothing prints no listing lines under --porcelain); return an
	// empty slice, not an error.
	return parseSearchResults(out), nil
}

// Download runs `luarocks download` and returns the path of the downloaded
// file. NOTE: the embedded LuaRocks 3.9.2 does not print the saved path —
// cmd/download.lua emits nothing on success and copies the file into the
// current directory (= cfg.WorkingDir via glr_getwd). Rather than fabricate a
// path (no silent fallback), Download diffs the download directory across
// the dispatch and returns the file that appeared; on ambiguity (overwrite or
// >1 new file) it returns the containing directory. Success returns a nil error.
func (e *luaEngine) Download(ctx context.Context, name string, opts DownloadOpts) (string, error) {
	argv := e.globalArgs(opts.Servers)

	argv = append(argv, "download", name)

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if opts.All {
		argv = append(argv, "--all")
	}

	if opts.Source {
		argv = append(argv, "--source")
	}

	if opts.Rockspec {
		argv = append(argv, "--rockspec")
	}

	if opts.Arch != "" {
		argv = append(argv, "--arch", opts.Arch)
	}
	// The embedded LuaRocks 3.9.2 (download.lua) prints no saved path, so we can't
	// parse one. Instead, snapshot the download directory (fs.current_dir() ==
	// cfg.WorkingDir) before and after and report the file that appeared. This
	// is a real path, never fabricated; on success the error is always nil.
	before := dirFiles(e.cfg.WorkingDir)

	if _, err := e.dispatch(ctx, argv); err != nil {
		return "", err
	}

	var added []string

	for name := range dirFiles(e.cfg.WorkingDir) {
		if !before[name] {
			added = append(added, name)
		}
	}

	if len(added) == 1 {
		return filepath.Join(e.cfg.WorkingDir, added[0]), nil
	}
	// Zero new files (an existing file was overwritten on re-download) or more
	// than one (ambiguous): the exact filename can't be uniquely identified.
	// Return the directory the rock was downloaded into — honest, not fabricated.
	return e.cfg.WorkingDir, nil
}

// dirFiles returns the set of regular-file names directly in dir. A missing or
// empty dir yields an empty set (no error) — used by Download to diff the
// download directory across a dispatch.
func dirFiles(dir string) map[string]bool {
	out := map[string]bool{}
	if dir == "" {
		return out
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}

	for _, ent := range entries {
		if ent.Type().IsRegular() {
			out[ent.Name()] = true
		}
	}

	return out
}

// Lint runs `luarocks lint <specPath>`. lint takes only the rockspec
// positional. Non-zero exit (syntax error) surfaces as a real error.
func (e *luaEngine) Lint(ctx context.Context, specPath string, opts LintOpts) error {
	_ = opts // lint has no tunable flags
	argv := []string{"lint", specPath}
	_, err := e.dispatch(ctx, argv)

	return err
}

// NewVersion runs `luarocks new_version <specPath>` and returns the path of the
// written rockspec parsed from the `Wrote <path>` line. Fails loud if that line
// is absent.
func (e *luaEngine) NewVersion(ctx context.Context, specPath string, opts NewVersionOpts) (string, error) {
	argv := []string{"new_version", specPath}
	if opts.NewVersion != "" {
		argv = append(argv, opts.NewVersion)
	}

	if opts.NewURL != "" {
		argv = append(argv, opts.NewURL)
	}

	if opts.Dir != "" {
		argv = append(argv, "--dir", opts.Dir)
	}

	if opts.Tag != "" {
		argv = append(argv, "--tag", opts.Tag)
	}

	out, err := e.dispatch(ctx, argv)
	if err != nil {
		return "", err
	}

	path, ok := parseWrotePath(out)
	if !ok {
		return "", fmt.Errorf("luaEngine: new_version %s succeeded but no 'Wrote' path line found in output: %q", specPath, out)
	}

	return path, nil
}

// WriteRockspec runs `luarocks write_rockspec` and returns the path of the
// written rockspec parsed from the `Wrote template at <path>` line. Fails loud
// if that line is absent. The wrapper command name uses an underscore.
func (e *luaEngine) WriteRockspec(ctx context.Context, url string, opts WriteRockspecOpts) (string, error) {
	argv := []string{"write_rockspec"}
	// Positionals are name, version, location in that order; upstream infers
	// missing leading ones. Emit name/version only when set, then the url
	// (location) last.
	if opts.Name != "" {
		argv = append(argv, opts.Name)
	}

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if url != "" {
		argv = append(argv, url)
	}

	if opts.Output != "" {
		argv = append(argv, "--output", opts.Output)
	}

	if opts.License != "" {
		argv = append(argv, "--license", opts.License)
	}

	if opts.Summary != "" {
		argv = append(argv, "--summary", opts.Summary)
	}

	if opts.Detailed != "" {
		argv = append(argv, "--detailed", opts.Detailed)
	}

	if opts.Homepage != "" {
		argv = append(argv, "--homepage", opts.Homepage)
	}

	if opts.LuaVersions != "" {
		argv = append(argv, "--lua-versions", opts.LuaVersions)
	}

	if opts.RockspecFormat != "" {
		argv = append(argv, "--rockspec-format", opts.RockspecFormat)
	}

	if opts.Tag != "" {
		argv = append(argv, "--tag", opts.Tag)
	}

	if opts.Lib != "" {
		argv = append(argv, "--lib", opts.Lib)
	}

	out, err := e.dispatch(ctx, argv)
	if err != nil {
		return "", err
	}

	path, ok := parseWrotePath(out)
	if !ok {
		return "", fmt.Errorf("luaEngine: write_rockspec %s succeeded but no 'Wrote' path line found in output: %q", url, out)
	}

	return path, nil
}

// Doc runs `luarocks doc`. It is tree-scoped (it resolves an installed rock),
// so the --tree global precedes the subcommand.
func (e *luaEngine) Doc(ctx context.Context, name string, opts DocOpts) error {
	argv := e.globalArgs(nil)

	argv = append(argv, "doc", name)

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if opts.Home {
		argv = append(argv, "--home")
	}

	if opts.List {
		argv = append(argv, "--list")
	}

	_, err := e.dispatch(ctx, argv)

	return err
}

// Test runs `luarocks test <specPath>`. It is tree-scoped (test installs deps
// into the tree), so the --tree global precedes the subcommand. opts.Args are
// passed through as the trailing suite arguments.
func (e *luaEngine) Test(ctx context.Context, specPath string, opts TestOpts) error {
	argv := e.globalArgs(nil)

	argv = append(argv, "test", specPath)

	if opts.Prepare {
		argv = append(argv, "--prepare")
	}

	if opts.TestType != "" {
		argv = append(argv, "--test-type", opts.TestType)
	}

	argv = append(argv, opts.Args...)
	_, err := e.dispatch(ctx, argv)

	return err
}

// Config runs `luarocks config` and returns the captured stdout (the printed
// value for a key, or the whole config when no key is given). When the command
// writes (Value or Unset set) there is no value to print and the trimmed empty
// output is returned. Fails loud on command error.
func (e *luaEngine) Config(ctx context.Context, opts ConfigOpts) (string, error) {
	argv := []string{"config"}
	if opts.Key != "" {
		argv = append(argv, opts.Key)
	}

	if opts.Value != "" {
		argv = append(argv, opts.Value)
	}

	if opts.Unset {
		argv = append(argv, "--unset")
	}

	if opts.Scope != "" {
		argv = append(argv, "--scope", opts.Scope)
	}

	if opts.JSON {
		argv = append(argv, "--json")
	}

	out, err := e.dispatch(ctx, argv)
	if err != nil {
		return "", err
	}

	return strings.TrimRight(out, "\n"), nil
}

// Upload runs `luarocks upload <specPath>`. Void: success is an exit-0 dispatch.
func (e *luaEngine) Upload(ctx context.Context, specPath string, opts UploadOpts) error {
	argv := []string{"upload", specPath}
	if opts.SrcRock != "" {
		argv = append(argv, opts.SrcRock)
	}

	if opts.SkipPack {
		argv = append(argv, "--skip-pack")
	}

	if opts.APIKey != "" {
		argv = append(argv, "--api-key", opts.APIKey)
	}

	if opts.TempKey != "" {
		argv = append(argv, "--temp-key", opts.TempKey)
	}

	if opts.Force {
		argv = append(argv, "--force")
	}

	if opts.Sign {
		argv = append(argv, "--sign")
	}

	_, err := e.dispatch(ctx, argv)

	return err
}

// InitProject runs `luarocks init` in cfg.WorkingDir. Void.
func (e *luaEngine) InitProject(ctx context.Context, opts InitProjectOpts) error {
	argv := []string{"init"}
	if opts.Name != "" {
		argv = append(argv, opts.Name)
	}

	if opts.Version != "" {
		argv = append(argv, opts.Version)
	}

	if opts.Reset {
		argv = append(argv, "--reset")
	}

	_, err := e.dispatch(ctx, argv)

	return err
}

// Admin runs `luarocks admin <subCmd> <args...>`. The wrapper.lua exec()
// dispatches a leading "admin" arg to the admin command table, so argv begins
// with "admin" followed by the subcommand and its verbatim args. Admin
// commands are server-scoped, not tree-scoped, so no --tree global is emitted;
// opts.Server appends --server when set. Void.
func (e *luaEngine) Admin(ctx context.Context, subCmd string, args []string, opts AdminOpts) error {
	argv := []string{"admin", subCmd}

	argv = append(argv, args...)

	if opts.Server != "" {
		argv = append(argv, "--server", opts.Server)
	}

	if opts.Force {
		argv = append(argv, "--force")
	}

	_, err := e.dispatch(ctx, argv)

	return err
}
