package tree

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/yuin/gopher-lua/parse"
)

// isLua reports whether the file at path parses as Lua source, mirroring
// upstream fs.is_lua (fs/lua.lua:1179), which probes the file with the
// configured interpreter's loadfile. A compiled binary or a shell script fails
// to parse and is deployed verbatim rather than wrapped in a Lua-interpreter
// launcher.
//
// loadfile skips a first line that starts with '#' (luaL_loadfile; LuaJIT's
// lexer, which Tarantool's loadfile uses, skips the "#!" line the same way), so
// a "#!/usr/bin/env tarantool" script is Lua and upstream wraps it. The parser
// here sees the same chunk, as gopher-lua's own LoadFile does.
//
// The parser is gopher-lua's, for Lua 5.1, not the interpreter's: a script
// that only LuaJIT's loadfile accepts — LuaJIT bytecode, a UTF-8 BOM, \z
// escapes, hex floats, LL/ULL/i number literals — is copied verbatim here
// where upstream wraps it. Running the configured interpreter instead, as
// upstream does, would make every deploy of a command depend on it.
func isLua(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	r := bufio.NewReader(f)

	if first, err := r.Peek(1); err == nil && first[0] == '#' {
		// Drop the line up to, not including, its newline, so the parser keeps
		// the line count loadfile reports.
		for {
			b, err := r.ReadByte()
			if err != nil {
				break // the whole file was the '#' line: an empty chunk
			}

			if b == '\n' {
				_ = r.UnreadByte()

				break
			}
		}
	}

	_, err = parse.Parse(r, path)

	return err == nil
}

// Module kinds, matching the LuaRocks install layout: pure-Lua modules go
// under the "lua" tree, native libraries under the "lib" tree.
const (
	kindLua = "lua"
	kindLib = "lib"
)

// Deploy copies the artifacts of a built rock into the tree, producing
// the per-rock RockManifest (path → md5 hex) that the caller then writes
// to <install-dir>/rock_manifest via t.Store.WriteRock.
//
// Two source directories are consulted:
//   - srcDir holds the original rockspec source tree (the output of
//     fetch.Fetch). Deploy reads .lua modules, prebuilt .so/.dylib
//     modules, install.{lua,lib,bin,conf} files, and copy_directories
//     from srcDir at the rockspec-relative paths.
//   - buildDir holds compiled artifacts produced by the build subsystem.
//     Deploy reads .c-source-derived .so files and table-form module
//     artifacts from buildDir at the canonical slashed path
//     `<dotted/slashed>.so`.
//
// For callers without a separate build phase (pure-.lua rocks, or unit
// tests where srcDir already contains every artifact), pass srcDir as
// both arguments — the lookup is layered and both forms work.
//
// What gets copied where:
//
//   - build.modules: dotted module name → slashed path; .lua → DeployLuaDir,
//     .so → DeployLibDir.
//   - build.install.lua / .lib: the KEY is a dotted module name (upstream
//     is_module_path=true) → module_to_path(key) subdir; a .lua source is
//     renamed to <last-segment>.lua. See installDest.
//   - build.install.conf: the KEY is a literal path joined under the per-rock
//     ConfDir (<install-dir>/conf); conf is never deployed to a shared dir.
//   - build.install.bin: the KEY is a literal path. The real script lands in
//     the per-rock <install-dir>/bin; when BinWrap is set and it is Lua,
//     BinDir gets an executable launcher for it (0o755), and otherwise a
//     verbatim copy (0o755).
//   - build.copy_directories: each entry is a directory under srcDir copied
//     recursively into the per-rock install dir; unset, it defaults to a doc/
//     dir, and with no directory copied the source root's README, LICENSE,
//     COPYING and *.md files go to the per-rock doc dir.
//   - lua/, lib/ and bin/ subtrees a build backend installed into buildDir or
//     the per-rock install dir are deployed like the sections above.
//
// The rock_manifest's conf and doc sections, and its Other entries, list every
// file the per-rock install dir ends up holding outside lua/, lib/ and bin/, as
// upstream make_rock_manifest walks that dir (see recordInstallDir).
//
// Collisions with previously-deployed versions are handled via munged
// filenames; see conflicts.go. A file more than one stage reaches — a
// build.modules entry the builtin backend also staged under buildDir/lua — is
// deployed once, by the first stage to reach it (see spots), except that
// build.install.lua/lib overwrites a build.modules file at the same path, as
// upstream's install_files overwrites the backend's.
//
// Every path the spec names stays where it belongs, which upstream does not
// check: a source path inside srcDir or buildDir, a target inside its deploy
// dir, the per-rock install dir or its conf dir, and the rock's name and
// version a single directory each. A name that leaves them — absolute, through
// "..", or through a symlink pointing out — fails Deploy with an error
// wrapping rocks.ErrUnsafePath, before anything is written there. A symlink
// that stays inside them is followed.
//
// Deploy is a linear pipeline: each numbered stage below handles one rock
// artifact section (rockspec, build.modules, build.install.*, copy_directories,
// installed lua/lib/bin subtrees) in the same order upstream build.lua does. The
// per-section logic lives in the deploy* helpers so this orchestrator stays a
// readable sequence.
//
// A Deploy that fails undoes what it changed: the files it wrote are removed,
// and those it renamed or moved aside are put back.
func (t *Tree) Deploy(spec *rocks.Rockspec, srcDir, buildDir string) (*rocks.RockManifest, error) {
	var rm *rocks.RockManifest

	err := t.journaled(func(t *Tree) error {
		var err error

		rm, err = t.deploy(spec, srcDir, buildDir, true)

		return err
	})
	if err != nil {
		return nil, err
	}

	return rm, nil
}

// deploy is Deploy, with defaultDocs saying whether stage 3 installs the
// default docs when no directory is copied, as a build does and the install of
// a binary rock does not.
func (t *Tree) deploy(spec *rocks.Rockspec, srcDir, buildDir string, defaultDocs bool) (*rocks.RockManifest, error) {
	if spec == nil {
		return nil, errors.New("tree.Deploy: nil spec")
	}

	if spec.Package == "" || spec.Version == "" {
		return nil, errors.New("tree.Deploy: spec missing Package/Version")
	}

	if err := checkNameVersion(spec.Package, spec.Version); err != nil {
		return nil, fmt.Errorf("tree.Deploy: %w", err)
	}

	if buildDir == "" {
		buildDir = srcDir
	}

	rm := &rocks.RockManifest{
		Lua:   map[string]string{},
		Lib:   map[string]string{},
		Bin:   map[string]string{},
		Conf:  map[string]string{},
		Doc:   map[string]string{},
		Other: map[string]string{},
	}

	if err := t.deployRockspec(spec, rm); err != nil {
		return nil, err
	}

	// Every lua/lib/bin target is resolved once per Deploy, however many stages
	// reach it (see spots).
	claimed := newSpots()

	if err := t.deployModules(spec, srcDir, buildDir, rm, claimed); err != nil {
		return nil, err
	}

	if err := t.deployInstallModulePaths(spec, srcDir, rm, claimed); err != nil {
		return nil, err
	}

	if err := t.deployInstallConf(spec, srcDir); err != nil {
		return nil, err
	}

	if err := t.deployInstallBin(spec, srcDir, rm, claimed); err != nil {
		return nil, err
	}

	if err := t.deployCopyDirectories(spec, srcDir, defaultDocs); err != nil {
		return nil, err
	}

	if err := t.deployInstalledSubtrees(spec, buildDir, rm, claimed); err != nil {
		return nil, err
	}

	if err := t.recordInstallDir(spec, rm); err != nil {
		return nil, err
	}

	return rm, nil
}

// recordInstallDir (stage 5) lists in rm every file the per-rock install dir
// holds outside lua/, lib/ and bin/: conf/ files under rm.Conf, doc/ files
// under rm.Doc and the rest under rm.Other, each keyed by its path relative to
// that directory, as upstream make_rock_manifest walks the whole install dir
// (manif/writer.lua:258-288). That covers build.install.conf, copy_directories
// and the default docs as well as anything a backend installed into $(CONFDIR),
// $(DOCDIR) or its prefix.
//
// lua/ and lib/ are emptied by stage 4 and bin/ is recorded as it is deployed.
// Skipped too are the rockspec, recorded by stage 0, and two entries upstream
// never has: the rock_manifest of an earlier deploy of this version, and the
// build/ staging dir the native backends keep under the install dir (see
// deployInstalledSubtrees).
func (t *Tree) recordInstallDir(spec *rocks.Rockspec, rm *rocks.RockManifest) error {
	installDir := t.InstallDir(spec.Package, spec.Version)

	skip := map[string]bool{
		kindLua: true, kindLib: true, "bin": true, buildStagingDir: true,
		"rock_manifest": true, spec.Package + "-" + spec.Version + ".rockspec": true,
	}

	err := filepath.Walk(installDir, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}

		rel, err := filepath.Rel(installDir, p)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)
		top, rest, nested := strings.Cut(rel, "/")

		if skip[top] {
			if fi.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if fi.IsDir() {
			return nil
		}

		sum, err := fileMD5(p)
		if err != nil {
			return err
		}

		switch {
		case nested && top == "conf":
			rm.Conf[rest] = sum
		case nested && top == "doc":
			rm.Doc[rest] = sum
		default:
			rm.Other[rel] = sum
		}

		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("tree.Deploy: list install dir: %w", err)
	}

	return nil
}

// deployRockspec (stage 0) copies the rockspec verbatim into the per-rock
// install dir as <name>-<version>.rockspec and records it in the rock_manifest
// under that versioned key (upstream make_rock_manifest walks the install dir
// where the rockspec was copied — build.lua:330). Skipped for hand-constructed
// specs that carry no source bytes.
func (t *Tree) deployRockspec(spec *rocks.Rockspec, rm *rocks.RockManifest) error {
	if len(spec.RawSource) == 0 {
		return nil
	}

	rockspecName := spec.Package + "-" + spec.Version + ".rockspec"
	dst := filepath.Join(t.InstallDir(spec.Package, spec.Version), rockspecName)

	if err := t.j.mkdirAll(t.RocksDir(), filepath.Dir(dst)); err != nil {
		return fmt.Errorf("tree.Deploy: mkdir install dir: %w", err)
	}

	sum := md5.Sum(spec.RawSource)
	if err := t.j.writeFile(t.RocksDir(), dst, spec.RawSource, filePerm); err != nil {
		return fmt.Errorf("tree.Deploy: write rockspec: %w", err)
	}

	rm.RockspecFile = rockspecName
	rm.Rockspec = hex.EncodeToString(sum[:])

	return nil
}

// deployModules (stage 1) copies each build.modules entry into the shared
// lua/lib deploy dirs, resolving collisions and recording each md5 in rm.
//
// build.modules is the builtin backend's input: upstream build/builtin.lua is
// the only reader, and the make, cmake, command and none backends never look
// at it (build/make.lua, cmake.lua, command.lua; build.lua run_build_driver
// returns early for "none"). A rock of those types deploys only what its build
// installed, whatever modules it lists.
func (t *Tree) deployModules(spec *rocks.Rockspec, srcDir, buildDir string, rm *rocks.RockManifest, claimed *spots) error {
	if !isBuiltin(spec.Build.Type) {
		return nil
	}

	for modName, mod := range spec.Build.Modules {
		srcRoot, srcPath, dstPath, kind, err := resolveModule(srcDir, buildDir, modName, mod, t.Paths)
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(t.deployDirOf(kind), dstPath)
		if err != nil {
			return err
		}

		finalPath, _, err := claimed.claim(t, kind, rel, spec.Package, spec.Version)
		if err != nil {
			return err
		}

		// Upstream deploys .so modules with perms="exec" (move_lib →
		// fs.move(...,"exec") → 0755); pure-Lua modules stay "read"/0644.
		mode := filePerm
		if kind == kindLib {
			mode = execPerm
		}

		sum, err := t.j.copyFile(srcRoot, srcPath, t.deployDirOf(kind), finalPath, mode)
		if err != nil {
			return fmt.Errorf("tree.Deploy: module %q: %w", modName, err)
		}

		// The rock_manifest lists the plain path even when the file took its
		// versioned one: upstream's is a listing of the per-rock install dir.
		switch kind {
		case kindLua:
			rm.Lua[filepath.ToSlash(rel)] = sum
		case kindLib:
			rm.Lib[filepath.ToSlash(rel)] = sum
		}
	}

	return nil
}

// isBuiltin reports whether a build.type selects the builtin backend: an empty
// type defaults to it (rockspec_format 3.0, build.lua build_rockspec).
func isBuiltin(buildType string) bool {
	return buildType == "" || buildType == "builtin"
}

// deployInstallModulePaths (stage 2) handles build.install.lua and
// build.install.lib.
//
// Upstream's install_files (luarocks/build.lua) interprets the string KEYS
// of each section differently depending on the section (prepare_install_dirs
// sets is_module_path per section):
//
//   - lua, lib  → is_module_path=true:  the key is a DOTTED MODULE NAME. The
//     destination subdir is module_to_path(key) (dots→slashes, minus the last
//     segment); a .lua source is renamed to <last-segment>.lua, anything else
//     keeps the source basename.
//   - bin, conf → is_module_path=false: the key is a literal slash path; the
//     destination is dir_name(key)/base_name(key), which filepath.Join
//     reproduces directly (see deployInstallConf / deployInstallBin).
func (t *Tree) deployInstallModulePaths(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest, claimed *spots) error {
	type instGroup struct {
		entries      map[string]string
		kind         string
		mode         os.FileMode
		dst          map[string]string
		isModulePath bool
	}

	groups := []instGroup{
		{spec.Build.Install.Lua, kindLua, filePerm, rm.Lua, true},
		{spec.Build.Install.Lib, kindLib, execPerm, rm.Lib, true},
	}
	for _, g := range groups {
		dir := t.deployDirOf(g.kind)

		for key, srcRel := range g.entries {
			src, err := safepath.JoinEntry(srcDir, srcRel)
			if err != nil {
				return fmt.Errorf("tree.Deploy: install %q: %w", key, err)
			}

			rel, err := safepath.Rel(dir, installDest(dir, key, srcRel, g.isModulePath))
			if err != nil {
				return fmt.Errorf("tree.Deploy: install %q: %w", key, err)
			}

			// A target stage 1 already deployed is overwritten in place: upstream
			// install_files runs after the backend and wins (build.lua:412-423).
			dst, _, err := claimed.claim(t, g.kind, rel, spec.Package, spec.Version)
			if err != nil {
				return fmt.Errorf("tree.Deploy: install %q: %w", key, err)
			}

			sum, err := t.j.copyFile(srcDir, src, dir, dst, g.mode)
			if err != nil {
				return fmt.Errorf("tree.Deploy: install %q: %w", key, err)
			}

			g.dst[filepath.ToSlash(rel)] = sum
		}
	}

	return nil
}

// deployInstallConf installs build.install.conf into the per-rock conf dir and
// does NOT deploy to a shared location (upstream repos.deploy_files handles only
// bin/lua/lib). Since these files reside permanently in the per-rock dir, they
// never collide across rocks, so resolveSpot is not used. recordInstallDir lists
// them in rock_manifest.conf, keyed relative to that conf dir.
func (t *Tree) deployInstallConf(spec *rocks.Rockspec, srcDir string) error {
	confDir := t.ConfDir(spec.Package, spec.Version)
	for key, srcRel := range spec.Build.Install.Conf {
		src, err := safepath.JoinEntry(srcDir, srcRel)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install conf %q: %w", key, err)
		}

		dst, err := safepath.JoinEntry(confDir, key)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install conf %q: %w", key, err)
		}

		if _, err := t.j.copyFile(srcDir, src, confDir, dst, filePerm); err != nil {
			return fmt.Errorf("tree.Deploy: install conf %q: %w", key, err)
		}
	}

	return nil
}

// deployInstallBin (stage 2b) installs build.install.bin — is_module_path=false,
// like conf: the key is the path under the bin dir. Each entry goes through
// deployBinFile, which also deploys the commands a build backend installed
// itself (stage 4).
func (t *Tree) deployInstallBin(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest, claimed *spots) error {
	for key, srcRel := range spec.Build.Install.Bin {
		src, err := safepath.JoinEntry(srcDir, srcRel)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
		}

		// is_module_path=false: dir_name/base_name == the key path.
		target, err := safepath.JoinEntry(t.BinDir(), key)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
		}

		rel, err := safepath.Rel(t.BinDir(), target)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
		}

		if err := t.deployBinFile(srcDir, src, rel, spec, rm, claimed); err != nil {
			return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
		}
	}

	return nil
}

// deployBinFile deploys the command script src as <tree>/bin/<rel>, the way
// upstream repos.deploy_files deploys each entry of the rock_manifest's bin
// section. The real script lands in the per-rock <install-dir>/bin/<rel>
// first, where upstream install_files copies every build.install.bin entry
// (build.lua install_files) and where a backend that installed into
// $(BINDIR) has put it already, and it stays there. Then, when t.BinWrap is
// set AND the script parses as Lua, <tree>/bin gets a launcher for it
// (should_wrap_bin_scripts → fs.wrap_script); a non-Lua script (compiled
// binary, shell script) fails is_lua and is copied there verbatim, as
// install_binary does (repos.lua:452-458), and so is every script when
// BinWrap is unset (legacy behavior).
//
// A command is deployed once per Deploy, by the first stage to reach it:
// build.install.bin (stage 2b) runs before the backend-installed bin subtrees
// (stage 4), as upstream install_files overwrites what the backend installed at
// the same path before the single deploy pass. A repeat visit is a no-op.
//
// Either way the rock_manifest lists the command under its plain path, which
// is also where the real script lives in the per-rock bin dir, whether the
// command took the plain or the versioned name in <tree>/bin.
//
// src is read inside srcRoot.
func (t *Tree) deployBinFile(srcRoot, src, rel string, spec *rocks.Rockspec, rm *rocks.RockManifest, claimed *spots) error {
	if claimed.seen(t, deployBin, rel) {
		return nil
	}

	scriptDir := filepath.Join(t.InstallDir(spec.Package, spec.Version), "bin")

	realScript, err := safepath.JoinEntry(scriptDir, rel)
	if err != nil {
		return err
	}

	var sum string

	if filepath.Clean(src) == realScript {
		sum, err = fileMD5(src)
	} else {
		sum, err = t.j.copyFile(srcRoot, src, scriptDir, realScript, execPerm)
	}

	if err != nil {
		return err
	}

	rm.Bin[filepath.ToSlash(rel)] = sum

	dst, _, err := claimed.claim(t, deployBin, rel, spec.Package, spec.Version)
	if err != nil {
		return err
	}

	if t.BinWrap == nil || !isLua(realScript) {
		_, err := t.j.copyFile(scriptDir, realScript, t.BinDir(), dst, execPerm)

		return err
	}

	// Public launcher → <tree>/bin/<rel>, execing the interpreter on the
	// real script (matches fs/unix.lua wrap_script byte-for-byte).
	body := t.binWrapperScript(realScript, spec.Package, spec.Version)
	if err := t.j.writeFile(t.BinDir(), dst, []byte(body), execPerm); err != nil {
		return fmt.Errorf("write bin wrapper: %w", err)
	}

	return nil
}

// deployCopyDirectories (stage 3) recursively copies each build.copy_directories
// entry into the per-rock install dir, with upstream install_files' defaults
// (build.lua:299-321): a rockspec that sets no copy_directories copies a doc/
// directory if the source has one, and when no directory was copied at all the
// source root's README, LICENSE, COPYING and *.md files go to the per-rock doc
// dir instead (installDefaultDocs). A listed directory that does not exist is an
// error; the default doc/ is not. With defaultDocs unset, as for a binary
// rock, which upstream install_binary_rock unpacks whole, no default docs are
// installed.
func (t *Tree) deployCopyDirectories(spec *rocks.Rockspec, srcDir string, defaultDocs bool) error {
	installDir := t.InstallDir(spec.Package, spec.Version)

	dirs := spec.Build.CopyDirectories
	copyingDefault := dirs == nil

	if copyingDefault {
		dirs = []string{"doc"}
	}

	anyDocs := false

	for _, dir := range dirs {
		src, err := safepath.Join(srcDir, dir)
		if err != nil {
			return fmt.Errorf("tree.Deploy: copy_directories[%q]: %w", dir, err)
		}

		if st, err := os.Stat(src); copyingDefault && (err != nil || !st.IsDir()) {
			continue
		}

		if err := t.j.copyTree(srcDir, src, installDir, filepath.Join(installDir, dir)); err != nil {
			return fmt.Errorf("tree.Deploy: copy_directories[%q]: %w", dir, err)
		}

		anyDocs = true
	}

	if anyDocs || !defaultDocs {
		return nil
	}

	return t.installDefaultDocs(srcDir, t.DocDir(spec.Package, spec.Version))
}

// defaultDocPatterns are the prefixes upstream install_default_docs matches a
// lowercased source-root entry name against ("^readme", "^license",
// "^copying"); its fourth pattern, "^.*%.md", is unanchored at the end, so any
// name containing ".md" matches too.
var defaultDocPatterns = []string{"readme", "license", "copying"}

// installDefaultDocs mirrors upstream install_default_docs (build.lua:271-287):
// every file at the top of srcDir whose lowercased name matches one of its
// patterns is copied, read-only, into docDir. A symlink is copied as the file
// it points to when that stays inside srcDir, and fails with
// rocks.ErrUnsafePath when it leads out; a dangling one is skipped, as
// upstream's failed fs.copy is. Upstream hands a matching directory to
// fs.copy too, which copies no tree; it is skipped here, and so is a link to
// one.
func (t *Tree) installDefaultDocs(srcDir, docDir string) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return fmt.Errorf("tree.Deploy: default docs: %w", err)
	}

	for _, e := range entries {
		if !isDefaultDoc(e.Name()) {
			continue
		}

		switch {
		case e.Type().IsRegular():
		case e.Type()&os.ModeSymlink != 0:
			if st, err := os.Stat(filepath.Join(srcDir, e.Name())); err != nil || !st.Mode().IsRegular() {
				continue
			}
		default:
			continue
		}

		if _, err := t.j.copyFile(srcDir, filepath.Join(srcDir, e.Name()), docDir, filepath.Join(docDir, e.Name()), filePerm); err != nil {
			return fmt.Errorf("tree.Deploy: default docs: %w", err)
		}
	}

	return nil
}

// isDefaultDoc reports whether a source-root file name is one upstream
// install_default_docs copies.
func isDefaultDoc(name string) bool {
	lower := strings.ToLower(name)

	for _, p := range defaultDocPatterns {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}

	return strings.Contains(lower, ".md")
}

// deployInstalledSubtrees (stage 4) relocates files a build backend installed
// straight into a lua/lib/bin subtree instead of declaring them in the
// rockspec.
//
// This runs for EVERY build.type, matching upstream: build.build_rock invokes
// the backend and then hands the whole per-rock install dir to
// make_rock_manifest, whose lua/lib/bin sections repos.deploy_files deploys. There
// is no make-specific gate upstream, and gating here left cmake/command rocks
// with their modules stranded off every loader path.
//
// Two roots are scanned because two conventions are in play:
//
//   - <buildDir>/lua and <buildDir>/lib — where the builtin backend stages
//     build.modules and build.install.lua/lib, and where a cmake build of a
//     hand-built spec without a rock name installs.
//   - <installDir>/lua and <installDir>/lib — where $(LUADIR)/$(LIBDIR) point
//     for the make, cmake and command backends (build/vars.go, mirroring
//     upstream configure_paths). A cmake rock passing
//     TARANTOOL_INSTALL_LUADIR="$(LUADIR)" — vshard does exactly this —
//     installs there.
//
// The bin/ subtree of each root is deployed too (deployInstalledBin).
//
// The two roots deliberately differ in what happens to the source file:
//
//   - installDir files are MOVED, and an emptied lua/lib dir is pruned. A .lua
//     left under share/tarantool/rocks/<name>/<ver>/lua/ sits on no loader path
//     and is precisely the layout this stage exists to undo; upstream's
//     repos.deploy_files moves too (fs.move).
//   - buildDir files are COPIED. <installDir>/build is a staging area the design
//     keeps on purpose, so its contents survive the deploy.
//
// A missing subtree is a no-op — the backend installed nothing there. lib files
// are deployed executable (0755).
func (t *Tree) deployInstalledSubtrees(
	spec *rocks.Rockspec,
	buildDir string,
	rm *rocks.RockManifest,
	claimed *spots,
) error {
	roots := []struct {
		dir  string
		disp srcDisposition
	}{
		{buildDir, keepSource},
		{t.InstallDir(spec.Package, spec.Version), moveSource},
	}

	seen := map[string]bool{}

	for _, root := range roots {
		if root.dir == "" || seen[filepath.Clean(root.dir)] {
			continue
		}

		seen[filepath.Clean(root.dir)] = true

		for _, sub := range []struct {
			kind  string
			mode  os.FileMode
			rmMap map[string]string
		}{
			{kindLua, filePerm, rm.Lua},
			{kindLib, execPerm, rm.Lib},
		} {
			src := filepath.Join(root.dir, sub.kind)

			err := t.deployInstalledSubtree(src, sub.kind, sub.mode, sub.rmMap, spec, root.disp, claimed)
			if err != nil {
				return fmt.Errorf("tree.Deploy: scan installed %s subtree: %w", sub.kind, err)
			}
		}

		if err := t.deployInstalledBin(filepath.Join(root.dir, "bin"), spec, rm, claimed); err != nil {
			return fmt.Errorf("tree.Deploy: scan installed bin subtree: %w", err)
		}
	}

	return nil
}

// deployInstalledBin deploys every command a build backend installed under
// binDir — $(BINDIR), which is <installDir>/bin, or bin/ under the cmake
// install prefix — as upstream
// repos.deploy_files deploys the rock_manifest's bin section: through
// deployBinFile, as build.install.bin entries are. Unlike lua/lib files the
// source is left in place from either root: upstream keeps the real script in
// the per-rock bin dir, where a command wrapper points (repos.lua:452-458). A
// missing subtree is a no-op.
func (t *Tree) deployInstalledBin(binDir string, spec *rocks.Rockspec, rm *rocks.RockManifest, claimed *spots) error {
	info, err := os.Stat(binDir)
	if err != nil || !info.IsDir() {
		return nil //nolint:nilerr // a missing subtree simply means nothing was installed there
	}

	return filepath.Walk(binDir, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil || fi.IsDir() {
			return werr
		}

		rel, err := filepath.Rel(binDir, p)
		if err != nil {
			return err
		}

		return t.deployBinFile(binDir, p, rel, spec, rm, claimed)
	})
}

// srcDisposition says what deployInstalledSubtree does with a source file once
// it has been deployed: keepSource leaves it where the backend put it,
// moveSource removes it (and prunes the directories it emptied). See
// deployInstalledSubtrees for why the two roots differ.
type srcDisposition bool

const (
	keepSource srcDisposition = false
	moveSource srcDisposition = true
)

// deployInstalledSubtree walks a backend-installed subtree (<root>/lua or
// <root>/lib) and deploys every file into the deploy dir of kind under its
// subtree-relative path, resolving collisions and recording each md5 in rmMap.
// With disp == moveSource the source file is removed afterwards and the emptied
// directories are pruned. A missing subtree is a no-op (the backend installed
// nothing there).
func (t *Tree) deployInstalledSubtree(
	srcDir, kind string,
	mode os.FileMode,
	rmMap map[string]string,
	spec *rocks.Rockspec,
	disp srcDisposition,
	claimed *spots,
) error {
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		return nil //nolint:nilerr // a missing subtree simply means nothing was installed there
	}

	err = filepath.Walk(srcDir, func(p string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}

		if fi.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}

		dst, first, err := claimed.claim(t, kind, rel, spec.Package, spec.Version)
		if err != nil {
			return err
		}

		// A target an earlier stage deployed is that stage's: the builtin
		// backend staged the very file stage 1 or 2 deployed from the source
		// tree, and upstream's install_files overwrites the backend's copy
		// before the single deploy pass (build.lua:412-423, repos.lua:488-541).
		if first {
			sum, err := t.j.copyFile(srcDir, p, t.deployDirOf(kind), dst, mode)
			if err != nil {
				return err
			}

			rmMap[filepath.ToSlash(rel)] = sum
		}

		if disp == moveSource {
			if err := t.j.remove(srcDir, p); err != nil {
				return fmt.Errorf("remove relocated %q: %w", p, err)
			}
		}

		return nil
	})
	if err != nil {
		return err
	}

	if disp == moveSource {
		t.pruneEmptyDirs(srcDir)
	}

	return nil
}

// pruneEmptyDirs removes dir and every subdirectory left empty by a moveSource
// deploy, bottom-up. Best-effort: a directory still holding files the deploy did
// not claim is left alone, and a failed removal is not an error — the files are
// already in place, and a stray empty directory harms nothing.
func (t *Tree) pruneEmptyDirs(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if e.IsDir() {
			t.pruneEmptyDirs(filepath.Join(dir, e.Name()))
		}
	}

	if rest, err := os.ReadDir(dir); err == nil && len(rest) == 0 {
		_ = t.j.remove(t.Tree, dir)
	}
}

// resolveModule maps a single build.modules entry to (srcRoot, srcPath,
// dstPath, kind): srcPath is read inside srcRoot, srcDir or buildDir, and fails
// to resolve when mod.Path leaves srcDir. kind is "lua" or "lib"; the caller
// writes into rm.Lua / rm.Lib accordingly.
//
// Source-directory layering:
//   - srcDir for .lua module paths and prebuilt .so/.dylib paths from the
//     rockspec (these live in the original source tree).
//   - buildDir for compiled artifacts the build subsystem produced
//     (C-source modules and table-form modules emit `<slashName>.so` into
//     buildDir).
//
// Dotted module names become slashed paths; the last segment becomes the
// filename. So "foo.bar.baz" with a .lua source becomes "foo/bar/baz.lua"
// under DeployLuaDir, and a .so module of the same name becomes
// "foo/bar/baz.so" under DeployLibDir.
func resolveModule(srcDir, buildDir, modName string, mod rocks.Module, p Paths) (srcRoot, src, dst, kind string, err error) {
	slashName := strings.ReplaceAll(modName, ".", string(filepath.Separator))
	srcRoot = srcDir

	switch {
	case mod.Path != "":
		ext := strings.ToLower(filepath.Ext(mod.Path))

		if ext == ".lua" || ext == ".so" || ext == ".dylib" {
			if _, err := safepath.JoinEntry(srcDir, mod.Path); err != nil {
				return "", "", "", "", fmt.Errorf("module %q: %w", modName, err)
			}
		}

		switch ext {
		case ".lua":
			// An init.lua source keeps its name under the module's directory
			// unless the module name already ends in ".init" (builtin.lua:298-
			// 306): ["foo"] = "src/foo/init.lua" deploys foo/init.lua, not
			// foo.lua. The builtin backend stages it there too (build/builtin.go
			// installLuaModule), so both copies resolve to the same target.
			src = filepath.Join(srcDir, mod.Path)
			dst = filepath.Join(p.DeployLuaDir(), slashName+".lua")

			if filepath.Base(mod.Path) == "init.lua" && !strings.HasSuffix(modName, ".init") {
				dst = filepath.Join(p.DeployLuaDir(), slashName, "init.lua")
			}

			kind = kindLua
		case ".so", ".dylib":
			// Tarantool/upstream luarocks deploy compiled modules with the
			// canonical platform suffix `.so`. Sources with `.dylib` on
			// macOS are renamed at deploy time to `.so` to match upstream's
			// install_files behavior. Prebuilt artifacts live in srcDir.
			src = filepath.Join(srcDir, mod.Path)
			dst = filepath.Join(p.DeployLibDir(), slashName+".so")
			kind = kindLib
		case ".c", ".cpp", ".cxx", ".cc":
			// A C source listed as Path implies the build step compiled it
			// to buildDir/lib/<slashName>.so. The "lib/" prefix matches
			// build/builtin's compileModule output layout.
			srcRoot = buildDir
			src = filepath.Join(buildDir, kindLib, slashName+".so")
			dst = filepath.Join(p.DeployLibDir(), slashName+".so")
			kind = kindLib
		default:
			err = fmt.Errorf("module %q: unsupported source extension %q", modName, ext)

			return srcRoot, src, dst, kind, err
		}
	case len(mod.Sources) > 0:
		// Table-form: the build subsystem produced the .so under
		// buildDir/lib/<slashName>.so (matching compileModule output).
		srcRoot = buildDir
		src = filepath.Join(buildDir, kindLib, slashName+".so")
		dst = filepath.Join(p.DeployLibDir(), slashName+".so")
		kind = kindLib
	default:
		err = fmt.Errorf("module %q: neither Path nor Sources set", modName)
	}

	return srcRoot, src, dst, kind, err
}

// installDest computes the on-disk destination for one build.install.<section>
// entry, mirroring upstream's install_to. For module-path sections (lua, lib)
// the key is a dotted module name: the subdir is module_to_path(key) and a .lua
// source is renamed to <last-segment>.lua. For literal sections (bin, conf) the
// key is a slash path joined directly under dir.
func installDest(dir, key, srcRel string, isModulePath bool) string {
	if !isModulePath {
		return filepath.Join(dir, key)
	}

	sub := moduleToPathDir(key)

	filename := filepath.Base(srcRel)
	if strings.HasSuffix(strings.ToLower(filename), ".lua") {
		filename = moduleLastSegment(key) + ".lua"
	}

	return filepath.Join(dir, filepath.FromSlash(sub), filename)
}

// moduleToPathDir mirrors upstream luarocks path.module_to_path: it drops the
// last dot-delimited segment of a module name and converts the remaining dots
// to forward slashes, yielding the destination SUBDIRECTORY (slash-delimited,
// possibly empty). E.g. "withinstall.helper" → "withinstall/", "a.b.c" →
// "a/b/", "foo" → "".
func moduleToPathDir(mod string) string {
	if i := strings.LastIndex(mod, "."); i >= 0 {
		mod = mod[:i+1] // keep through the final dot, drop the last segment
	} else {
		mod = "" // no dot: the whole string is the last segment, removed
	}

	return strings.ReplaceAll(mod, ".", "/")
}

// moduleLastSegment returns the final dot-delimited segment of a module name
// (upstream's `modname:match("([^.]+)$")`). E.g. "withinstall.helper" →
// "helper", "foo" → "foo".
func moduleLastSegment(mod string) string {
	if i := strings.LastIndex(mod, "."); i >= 0 {
		return mod[i+1:]
	}

	return mod
}

// binWrapperScript reproduces fs/unix.lua wrap_script byte-for-byte for the
// single-tree deps case: a /bin/sh launcher that sets package.path/cpath to the
// tree's deploy dirs, attempts to load luarocks.loader (guarded — a native tree
// has none, the pcall absorbs it), registers the rock context, then execs the
// interpreter on the real script. Path entries mirror path.package_paths:
// "<lua_dir>/?.lua;<lua_dir>/?/init.lua" and "<lib_dir>/?.so". Only the tree
// deployed to is listed, where upstream lists every rock tree its deps mode
// selects.
func (t *Tree) binWrapperScript(realScript, name, version string) string {
	lua := filepath.ToSlash(t.DeployLuaDir())
	lib := filepath.ToSlash(t.DeployLibDir())
	lpath := lua + "/?.lua;" + lua + "/?/init.lua"
	lcpath := lib + "/?.so"

	luainit := "package.path=" + luaQuote(lpath+";") + "..package.path" +
		";package.cpath=" + luaQuote(lcpath+";") + "..package.cpath" +
		";local k,l,_=pcall(require," + luaQuote("luarocks.loader") + ") _=k and l.add_context(" +
		luaQuote(name) + "," + luaQuote(version) + ")"

	return "#!/bin/sh\n\nLUAROCKS_SYSCONFDIR=" + shellQuote(t.BinWrap.Sysconfdir) +
		" exec " + shellQuote(t.BinWrap.Interpreter) +
		" -e " + shellQuote(luainit) +
		" " + shellQuote(realScript) + ` "$@"` + "\n"
}

// luaQuote mirrors util.LQ — Lua's string.format("%q", s). For the wrapper's
// path/identifier inputs (no embedded quotes, backslashes, or newlines) this is
// a plain double-quoted string; the escapes below keep it faithful regardless.
func luaQuote(s string) string {
	var b strings.Builder

	b.WriteByte('"')

	for i := range len(s) {
		switch c := s[i]; c {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0:
			b.WriteString(`\0`)
		default:
			b.WriteByte(c)
		}
	}

	b.WriteByte('"')

	return b.String()
}

// shellQuote mirrors fs/unix.lua unix.Q: wrap in single quotes, escaping any
// embedded single quote as '\”.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// copyFile writes src, read inside srcRoot, to dst inside dstRoot with mode,
// mkdir-p the parent, and returns the md5 hex of the bytes written. A symlink
// on the way that leaves its root fails the copy (see safepath). It writes
// outside any tree's journal, as into a stage dir.
func copyFile(srcRoot, src, dstRoot, dst string, mode os.FileMode) (string, error) {
	var j *journal

	return j.copyFile(srcRoot, src, dstRoot, dst, mode)
}

// copyFile is copyFile recording what it changes in j.
func (j *journal) copyFile(srcRoot, src, dstRoot, dst string, mode os.FileMode) (string, error) {
	in, err := safepath.Open(srcRoot, src)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	if err := j.mkdirAll(dstRoot, filepath.Dir(dst)); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", filepath.Dir(dst), err)
	}

	out, err := j.create(dstRoot, dst, mode)
	if err != nil {
		return "", fmt.Errorf("create %q: %w", dst, err)
	}

	h := md5.New()

	w := io.MultiWriter(out, h)
	if _, err := io.Copy(w, in); err != nil {
		_ = out.Close()

		return "", fmt.Errorf("copy %q->%q: %w", src, dst, err)
	}

	if err := out.Close(); err != nil {
		return "", fmt.Errorf("close %q: %w", dst, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// fileMD5 returns the md5 hex of the file at p.
func fileMD5(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}

	defer func() { _ = f.Close() }()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %q: %w", p, err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeFile writes data to path inside root with perm, creating its
// directory, recording what it changes in j.
func (j *journal) writeFile(root, path string, data []byte, perm os.FileMode) error {
	if err := j.mkdirAll(root, filepath.Dir(path)); err != nil {
		return err
	}

	f, err := j.create(root, path, perm)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}

// copyTree recursively copies src, inside srcRoot, to dst, inside dstRoot,
// keeping each file's permission bits. recordInstallDir lists the copies in
// the rock_manifest.
func copyTree(srcRoot, src, dstRoot, dst string) error {
	var j *journal

	return j.copyTree(srcRoot, src, dstRoot, dst)
}

// copyTree is copyTree recording what it changes in j.
func (j *journal) copyTree(srcRoot, src, dstRoot, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("stat %q: %w", src, err)
	}

	if !st.IsDir() {
		return fmt.Errorf("%q is not a directory", src)
	}

	return filepath.Walk(src, func(p string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}

		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return j.mkdirAll(dstRoot, target)
		}

		_, err = j.copyFile(srcRoot, p, dstRoot, target, info.Mode().Perm())

		return err
	})
}
