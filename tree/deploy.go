package tree

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/yuin/gopher-lua/parse"
)

// isLua reports whether the file at path parses as Lua source, mirroring
// upstream fs.is_lua (which probes the file with the interpreter's loadfile).
// A compiled binary or a shell script fails to parse and is deployed verbatim
// rather than wrapped in a Lua-interpreter launcher.
func isLua(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}

	defer func() { _ = f.Close() }()

	_, err = parse.Parse(f, path)

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
//   - build.install.bin: the KEY is a literal path. When BinWrap is set the
//     real script lands in the per-rock <install-dir>/bin and BinDir gets an
//     executable launcher (0o755); when nil the script is copied verbatim
//     (0o755).
//   - build.copy_directories: each entry is a directory under srcDir copied
//     recursively into the per-rock install dir (only a "doc" dir is recorded
//     in the rock_manifest; see copyTree).
//
// Collisions with previously-deployed versions are handled via munged
// filenames; see conflicts.go.
//
// Deploy is a linear pipeline: each numbered stage below handles one rock
// artifact section (rockspec, build.modules, build.install.*, copy_directories,
// make subtree) in the same order upstream build.lua does. The per-section logic
// lives in the deploy* helpers so this orchestrator stays a readable sequence.
func (t *Tree) Deploy(spec *rocks.Rockspec, srcDir, buildDir string) (*rocks.RockManifest, error) {
	if spec == nil {
		return nil, errors.New("tree.Deploy: nil spec")
	}

	if spec.Package == "" || spec.Version == "" {
		return nil, errors.New("tree.Deploy: spec missing Package/Version")
	}

	if buildDir == "" {
		buildDir = srcDir
	}

	rm := &rocks.RockManifest{
		Lua:  map[string]string{},
		Lib:  map[string]string{},
		Bin:  map[string]string{},
		Conf: map[string]string{},
		Doc:  map[string]string{},
	}

	if err := t.deployRockspec(spec, rm); err != nil {
		return nil, err
	}

	if err := t.deployModules(spec, srcDir, buildDir, rm); err != nil {
		return nil, err
	}

	if err := t.deployInstallModulePaths(spec, srcDir, rm); err != nil {
		return nil, err
	}

	if err := t.deployInstallConf(spec, srcDir, rm); err != nil {
		return nil, err
	}

	if err := t.deployInstallBin(spec, srcDir, rm); err != nil {
		return nil, err
	}

	if err := t.deployCopyDirectories(spec, srcDir, rm); err != nil {
		return nil, err
	}

	if err := t.deployMakeSubtrees(spec, buildDir, rm); err != nil {
		return nil, err
	}

	return rm, nil
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

	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return fmt.Errorf("tree.Deploy: mkdir install dir: %w", err)
	}

	sum := md5.Sum(spec.RawSource)
	if err := os.WriteFile(dst, spec.RawSource, filePerm); err != nil {
		return fmt.Errorf("tree.Deploy: write rockspec: %w", err)
	}

	rm.RockspecFile = rockspecName
	rm.Rockspec = hex.EncodeToString(sum[:])

	return nil
}

// deployModules (stage 1) copies each build.modules entry into the shared
// lua/lib deploy dirs, resolving collisions and recording each md5 in rm.
func (t *Tree) deployModules(spec *rocks.Rockspec, srcDir, buildDir string, rm *rocks.RockManifest) error {
	for modName, mod := range spec.Build.Modules {
		srcPath, dstPath, kind, err := resolveModule(srcDir, buildDir, modName, mod, t.Paths)
		if err != nil {
			return err
		}

		deployDir := deployDirForKind(t.Paths, kind)

		plainRel, err := filepath.Rel(deployDir, dstPath)
		if err != nil {
			return err
		}

		item := pathToModule(filepath.ToSlash(plainRel))

		finalPath, err := t.resolveSpot(deployDir, dstPath, item, spec.Package, spec.Version)
		if err != nil {
			return err
		}

		// Upstream deploys .so modules with perms="exec" (move_lib →
		// fs.move(...,"exec") → 0755); pure-Lua modules stay "read"/0644.
		mode := filePerm
		if kind == kindLib {
			mode = execPerm
		}

		sum, err := copyFile(srcPath, finalPath, mode)
		if err != nil {
			return fmt.Errorf("tree.Deploy: module %q: %w", modName, err)
		}

		rel, err := filepath.Rel(deployDir, finalPath)
		if err != nil {
			return err
		}

		rel = filepath.ToSlash(rel)

		switch kind {
		case kindLua:
			rm.Lua[rel] = sum
		case kindLib:
			rm.Lib[rel] = sum
		}
	}

	return nil
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
func (t *Tree) deployInstallModulePaths(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest) error {
	type instGroup struct {
		entries      map[string]string
		dir          string
		mode         os.FileMode
		dst          map[string]string
		isModulePath bool
	}

	groups := []instGroup{
		{spec.Build.Install.Lua, t.DeployLuaDir(), filePerm, rm.Lua, true},
		{spec.Build.Install.Lib, t.DeployLibDir(), execPerm, rm.Lib, true},
	}
	for _, g := range groups {
		for key, srcRel := range g.entries {
			src := filepath.Join(srcDir, srcRel)
			dst := installDest(g.dir, key, srcRel, g.isModulePath)

			plainRel, err := filepath.Rel(g.dir, dst)
			if err != nil {
				return err
			}

			item := pathToModule(filepath.ToSlash(plainRel))

			dst, err = t.resolveSpot(g.dir, dst, item, spec.Package, spec.Version)
			if err != nil {
				return err
			}

			sum, err := copyFile(src, dst, g.mode)
			if err != nil {
				return fmt.Errorf("tree.Deploy: install %q: %w", key, err)
			}

			rel, err := filepath.Rel(g.dir, dst)
			if err != nil {
				return err
			}

			g.dst[filepath.ToSlash(rel)] = sum
		}
	}

	return nil
}

// deployInstallConf installs build.install.conf into the per-rock conf dir and
// does NOT deploy to a shared location (upstream repos.deploy_files handles only
// bin/lua/lib). Since these files reside permanently in the per-rock dir, they
// never collide across rocks, so resolveSpot is not used. The rock_manifest.conf
// entry is keyed relative to that conf dir.
func (t *Tree) deployInstallConf(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest) error {
	confDir := t.ConfDir(spec.Package, spec.Version)
	for key, srcRel := range spec.Build.Install.Conf {
		src := filepath.Join(srcDir, srcRel)
		dst := filepath.Join(confDir, filepath.Clean(key))

		sum, err := copyFile(src, dst, filePerm)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install conf %q: %w", key, err)
		}

		rel, err := filepath.Rel(confDir, dst)
		if err != nil {
			return err
		}

		rm.Conf[filepath.ToSlash(rel)] = sum
	}

	return nil
}

// deployInstallBin (stage 2b) installs build.install.bin — is_module_path=false,
// like conf, but upstream additionally deploys the public <tree>/bin entry as a
// launcher (repos.deploy_files with should_wrap_bin_scripts → fs.wrap_script).
// When t.BinWrap is set AND the source parses as Lua we reproduce that two-step
// layout: the real script lands in the per-rock <install-dir>/bin and <tree>/bin
// gets the wrapper. A non-Lua source (compiled binary, shell script) fails
// is_lua and is copied verbatim, matching upstream install_binary's fs.is_lua
// gate (repos.lua:452). When BinWrap is unset we also copy verbatim (legacy
// behavior).
func (t *Tree) deployInstallBin(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest) error {
	installDir := t.InstallDir(spec.Package, spec.Version)

	for key, srcRel := range spec.Build.Install.Bin {
		src := filepath.Join(srcDir, srcRel)
		rel := filepath.Clean(key) // is_module_path=false: dir_name/base_name == the key path

		if t.BinWrap == nil || !isLua(src) {
			dst, err := t.resolveSpot(t.BinDir(), filepath.Join(t.BinDir(), rel), filepath.ToSlash(rel), spec.Package, spec.Version)
			if err != nil {
				return err
			}

			sum, err := copyFile(src, dst, execPerm)
			if err != nil {
				return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
			}

			r, err := filepath.Rel(t.BinDir(), dst)
			if err != nil {
				return err
			}

			rm.Bin[filepath.ToSlash(r)] = sum

			continue
		}

		// Wrapped: real script → per-rock <install-dir>/bin/<rel>.
		realScript := filepath.Join(installDir, "bin", rel)

		sum, err := copyFile(src, realScript, execPerm)
		if err != nil {
			return fmt.Errorf("tree.Deploy: install bin %q: %w", key, err)
		}

		rm.Bin[filepath.ToSlash(rel)] = sum

		// Public launcher → <tree>/bin/<rel>, execing the interpreter on the
		// real script (matches fs/unix.lua wrap_script byte-for-byte).
		wrapperDst, err := t.resolveSpot(t.BinDir(), filepath.Join(t.BinDir(), rel), filepath.ToSlash(rel), spec.Package, spec.Version)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(wrapperDst), dirPerm); err != nil {
			return fmt.Errorf("tree.Deploy: mkdir bin %q: %w", key, err)
		}

		body := t.binWrapperScript(realScript, spec.Package, spec.Version)
		if err := os.WriteFile(wrapperDst, []byte(body), execPerm); err != nil {
			return fmt.Errorf("tree.Deploy: write bin wrapper %q: %w", key, err)
		}
	}

	return nil
}

// deployCopyDirectories (stage 3) recursively copies each build.copy_directories
// entry into the per-rock install dir.
func (t *Tree) deployCopyDirectories(spec *rocks.Rockspec, srcDir string, rm *rocks.RockManifest) error {
	installDir := t.InstallDir(spec.Package, spec.Version)

	for _, dir := range spec.Build.CopyDirectories {
		src := filepath.Join(srcDir, dir)
		dst := filepath.Join(installDir, dir)

		if err := copyTree(src, dst, rm, dir); err != nil {
			return fmt.Errorf("tree.Deploy: copy_directories[%q]: %w", dir, err)
		}
	}

	return nil
}

// deployMakeSubtrees (stage 4) handles the make backend, which has no
// build.modules list — the Makefile installs straight into the per-rock lua/lib
// subtree (INST_LUADIR=$(LUADIR), INST_LIBDIR=$(LIBDIR), which the make backend
// points at buildDir/lua|lib). Scan those subtrees and relocate every installed
// file to the shared deploy dirs, mirroring repos.deploy_files. lib files are
// executable (0755).
func (t *Tree) deployMakeSubtrees(spec *rocks.Rockspec, buildDir string, rm *rocks.RockManifest) error {
	if spec.Build.Type != "make" {
		return nil
	}

	if err := t.deployInstalledSubtree(filepath.Join(buildDir, "lua"), t.DeployLuaDir(), filePerm, rm.Lua, spec); err != nil {
		return fmt.Errorf("tree.Deploy: scan make lua subtree: %w", err)
	}

	if err := t.deployInstalledSubtree(filepath.Join(buildDir, "lib"), t.DeployLibDir(), execPerm, rm.Lib, spec); err != nil {
		return fmt.Errorf("tree.Deploy: scan make lib subtree: %w", err)
	}

	return nil
}

// DeleteVersion undeploys a single installed rock version: it deletes the files
// this version deployed (recorded in its rock_manifest, at their actual — plain
// or versioned — paths), removes the per-rock install directory, and prunes the
// rock's name directory when no other version remains. It is the file-removal
// half of upstream repos.delete_version (repos.lua:585-695).
//
// Promotion of a next-highest kept version back to the active (unversioned)
// name is the caller's responsibility via the tree manifest; it only matters
// when multiple versions of the same rock are installed (the --keep path).
func (t *Tree) DeleteVersion(name, version string) error {
	installDir := t.InstallDir(name, version)

	// Best-effort: read the rock_manifest to know which deployed files to delete.
	// A missing/unreadable manifest still lets us remove the install dir.
	if rm, err := t.Store.ReadRock(filepath.Join(installDir, "rock_manifest")); err == nil && rm != nil {
		for _, g := range []struct {
			dir   string
			files map[string]string
		}{
			{t.DeployLuaDir(), rm.Lua},
			{t.DeployLibDir(), rm.Lib},
			{t.BinDir(), rm.Bin},
		} {
			for rel := range g.files {
				_ = os.Remove(filepath.Join(g.dir, filepath.FromSlash(rel)))
			}
		}
	}

	if err := os.RemoveAll(installDir); err != nil {
		return fmt.Errorf("tree.DeleteVersion: remove install dir: %w", err)
	}

	// Prune the rock's name dir when no versions remain.
	nameDir := filepath.Join(t.RocksDir(), name)
	if entries, err := os.ReadDir(nameDir); err == nil && len(entries) == 0 {
		_ = os.Remove(nameDir)
	}

	return nil
}

// deployInstalledSubtree walks a make-installed subtree (buildDir/lua or /lib)
// and copies every file into deployDir under its subtree-relative path,
// resolving collisions and recording each md5 in rmMap. A missing subtree is a
// no-op (the Makefile installed nothing there).
func (t *Tree) deployInstalledSubtree(srcDir, deployDir string, mode os.FileMode, rmMap map[string]string, spec *rocks.Rockspec) error {
	info, err := os.Stat(srcDir)
	if err != nil || !info.IsDir() {
		return nil //nolint:nilerr // a missing subtree simply means nothing was installed there
	}

	return filepath.Walk(srcDir, func(p string, fi os.FileInfo, werr error) error {
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

		item := pathToModule(filepath.ToSlash(rel))

		dst, err := t.resolveSpot(deployDir, filepath.Join(deployDir, rel), item, spec.Package, spec.Version)
		if err != nil {
			return err
		}

		sum, err := copyFile(p, dst, mode)
		if err != nil {
			return err
		}

		r, err := filepath.Rel(deployDir, dst)
		if err != nil {
			return err
		}

		rmMap[filepath.ToSlash(r)] = sum

		return nil
	})
}

// resolveModule maps a single build.modules entry to (srcPath, dstPath, kind).
// kind is "lua" or "lib"; the caller writes into rm.Lua / rm.Lib accordingly.
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
func resolveModule(srcDir, buildDir, modName string, mod rocks.Module, p Paths) (src, dst, kind string, err error) {
	slashName := strings.ReplaceAll(modName, ".", string(filepath.Separator))

	switch {
	case mod.Path != "":
		ext := strings.ToLower(filepath.Ext(mod.Path))
		switch ext {
		case ".lua":
			src = filepath.Join(srcDir, mod.Path)
			dst = filepath.Join(p.DeployLuaDir(), slashName+".lua")
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
			src = filepath.Join(buildDir, kindLib, slashName+".so")
			dst = filepath.Join(p.DeployLibDir(), slashName+".so")
			kind = kindLib
		default:
			err = fmt.Errorf("module %q: unsupported source extension %q", modName, ext)

			return src, dst, kind, err
		}
	case len(mod.Sources) > 0:
		// Table-form: the build subsystem produced the .so under
		// buildDir/lib/<slashName>.so (matching compileModule output).
		src = filepath.Join(buildDir, kindLib, slashName+".so")
		dst = filepath.Join(p.DeployLibDir(), slashName+".so")
		kind = kindLib
	default:
		err = fmt.Errorf("module %q: neither Path nor Sources set", modName)
	}

	return src, dst, kind, err
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
// "<lua_dir>/?.lua;<lua_dir>/?/init.lua" and "<lib_dir>/?.so".
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

func deployDirForKind(p Paths, kind string) string {
	switch kind {
	case kindLua:
		return p.DeployLuaDir()
	case kindLib:
		return p.DeployLibDir()
	}

	return p.Tree
}

// copyFile writes src → dst with mode, mkdir-p the parent, and returns
// the md5 hex of the bytes written.
func copyFile(src, dst string, mode os.FileMode) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", filepath.Dir(dst), err)
	}

	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open %q: %w", src, err)
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
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

// copyTree recursively copies src → dst, computing an md5 for every file.
// Only files under a "doc" top dir are recorded — into rm.Doc, keyed by the
// rock-root-relative path so the manifest key matches the on-disk layout.
// Files from any other copy_directories entry are written to disk but NOT
// recorded in the rock_manifest: they don't belong to the lua/lib/bin/conf
// buckets, and keeping the manifest closed over those known buckets is
// simpler than inventing a category for arbitrary copied content.
func copyTree(src, dst string, rm *rocks.RockManifest, topName string) error {
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
			return os.MkdirAll(target, dirPerm)
		}

		sum, err := copyFile(p, target, info.Mode().Perm())
		if err != nil {
			return err
		}

		if topName == "doc" {
			key := filepath.ToSlash(rel)
			rm.Doc[key] = sum
		}

		return nil
	})
}
