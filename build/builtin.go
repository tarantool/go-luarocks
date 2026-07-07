package build

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// autodetectSkiplist is the set of top-level path segments upstream's
// autodetect_modules ignores (builtin.lua:71-77).
var autodetectSkiplist = map[string]bool{
	"spec":        true,
	".luarocks":   true,
	"lua_modules": true,
	"test.lua":    true,
	"tests.lua":   true,
}

// luaopenRe extracts the module name from a `luaopen_<name>` C symbol, matching
// upstream get_cmod_name (builtin.lua:68).
var luaopenRe = regexp.MustCompile(`int\s+luaopen_([a-zA-Z0-9_]+)`)

// autodetectModules fills spec.Build.Modules / Install.Bin / CopyDirectories by
// scanning srcDir, mirroring upstream builtin.autodetect_modules
// (builtin.lua:79-137): it descends into the first of src/lua/lib that exists,
// registers every *.lua and *.c file as a module (deriving the name via
// path_to_module or the luaopen_ symbol), picks up a src/bin|bin dir as
// install.bin, and doc/docs/samples/tests as copy_directories.
func autodetectModules(spec *rocks.Rockspec, srcDir string) {
	prefix := ""
	scanRoot := srcDir

	for _, parent := range []string{"src", "lua", "lib"} {
		if isDir(filepath.Join(srcDir, parent)) {
			prefix = parent + "/"
			scanRoot = filepath.Join(srcDir, parent)

			break
		}
	}

	modules := map[string]rocks.Module{}

	_ = filepath.Walk(scanRoot, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries, keep scanning
		}

		rel, rerr := filepath.Rel(scanRoot, p)
		if rerr != nil {
			return nil //nolint:nilerr // unrelatable path: skip this entry, keep scanning
		}

		rel = filepath.ToSlash(rel)
		if base, _, _ := strings.Cut(rel, "/"); autodetectSkiplist[base] {
			return nil
		}

		switch {
		case strings.HasSuffix(rel, ".lua"):
			modules[moduleNameFromPath(rel)] = rocks.Module{Path: prefix + rel}
		case strings.HasSuffix(rel, ".c"):
			name := cmodName(p)
			if name == "" {
				name = moduleNameFromPath(strings.TrimSuffix(rel, ".c") + ".lua")
			}

			modules[name] = rocks.Module{Sources: []string{prefix + rel}}
		}

		return nil
	})

	if len(modules) > 0 {
		spec.Build.Modules = modules
	}

	// src/bin | bin → install.bin (keyed by basename).
	for _, cand := range []string{"src/bin", "bin"} {
		binDir := filepath.Join(srcDir, cand)
		if !isDir(binDir) {
			continue
		}

		entries, derr := os.ReadDir(binDir)
		if derr != nil {
			break
		}

		bin := map[string]string{}

		for _, e := range entries {
			if !e.IsDir() {
				bin[e.Name()] = cand + "/" + e.Name()
			}
		}

		if len(bin) > 0 {
			spec.Build.Install.Bin = bin
		}

		break
	}

	// doc/docs/samples/tests → copy_directories.
	for _, d := range []string{"doc", "docs", "samples", "tests"} {
		if isDir(filepath.Join(srcDir, d)) {
			spec.Build.CopyDirectories = append(spec.Build.CopyDirectories, d)
		}
	}
}

// moduleNameFromPath mirrors upstream path.path_to_module: strip the extension,
// turn slashes into dots, trim leading/trailing dots. "a/b.lua" → "a.b".
func moduleNameFromPath(rel string) string {
	name := rel
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[:i]
	}

	name = strings.ReplaceAll(name, "/", ".")

	return strings.Trim(name, ".")
}

// cmodName extracts the module name from a C source's luaopen_ symbol, or ""
// when none is present (upstream get_cmod_name).
func cmodName(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // scanning the rock's own source tree
	if err != nil {
		return ""
	}

	if m := luaopenRe.FindSubmatch(data); m != nil {
		return string(m[1])
	}

	return ""
}

// isDir reports whether path exists and is a directory.
func isDir(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

const (
	// dirPerm is the mode used when creating destination directories.
	dirPerm os.FileMode = 0o750
	// exePerm is the mode applied to installed executable artifacts.
	exePerm os.FileMode = 0o750
	// dirOwnerRWX is OR-ed into a copied directory's mode so the owner can
	// always traverse/write the recreated tree.
	dirOwnerRWX os.FileMode = 0o700
	// initialArgsCap pre-sizes the cc argument slice to avoid early regrows.
	initialArgsCap = 16
)

// runBuiltin implements the `builtin` build backend.
//
// It iterates spec.Build.Modules and dispatches per module shape:
//
//   - Path != "" and ends in ".lua"  → copy file (no compile)
//   - Path != "" and ends in ".c"    → single-source C compile
//   - Sources non-empty (table form) → multi-source C compile with the
//     per-module incdirs/libdirs/libraries/defines applied
//
// After modules, spec.Build.Install.{Lua,Lib,Bin,Conf} entries are copied
// into the matching subdirectory under destDir (lua/lib/bin/conf). The
// downstream tree.Deploy step is responsible for moving these to their
// final deploy paths; this backend only writes them under destDir.
//
// spec.Build.CopyDirectories entries are copied recursively from srcDir
// to destDir/<dir> verbatim.
//
// If ANY module needs a C compile and cfg.Tarantool.IncludeDir is empty,
// the function returns ErrMissingTarantoolHeaders before invoking cc.
//
// destDir is the rock's staging tree root (the "build" subdir the facade
// hands us). Files live at:
//
//	destDir/lua/<a>/<b>/<c>.lua            for ["a.b.c"] = ".../c.lua"
//	destDir/lib/<a>/<b>/<c>.so             for compiled C modules
//	destDir/lua|lib|bin|conf/<entries...>  for build.install.*
//	destDir/<dir>/...                      for build.copy_directories
func runBuiltin(ctx context.Context, spec *rocks.Rockspec, srcDir, destDir string, cfg rocks.Config) error {
	flags := DeriveFlags(cfg)

	// Upstream autodetects build.modules by scanning the source tree when the
	// rockspec omits it and rockspec_format >= 3.0 (builtin.lua:282-291). We
	// fill spec.Build in place so the deploy/manifest steps see the same set.
	if len(spec.Build.Modules) == 0 && rockspecFormatAtLeast3(spec.RockspecFormat) {
		autodetectModules(spec, srcDir)
	}

	// Stable iteration so error reports are deterministic.
	names := make([]string, 0, len(spec.Build.Modules))
	for n := range spec.Build.Modules {
		names = append(names, n)
	}

	sort.Strings(names)

	// rockspec.variables for $(NAME) expansion in the per-module compile flags
	// (defines/incdirs/libdirs/libraries), mirroring builtin.lua's add_flags →
	// util.variable_substitutions.
	vars := buildVars(spec, cfg)

	// Pre-flight: any C compile needs headers.
	for _, n := range names {
		m := spec.Build.Modules[n]
		if needsCC(m) && cfg.Tarantool.IncludeDir == "" {
			return fmt.Errorf("build: module %q: %w", n, rocks.ErrMissingTarantoolHeaders)
		}
	}

	for _, n := range names {
		m := spec.Build.Modules[n]

		switch {
		case m.Path != "" && strings.HasSuffix(m.Path, ".lua"):
			err := installLuaModule(srcDir, destDir, n, m.Path)
			if err != nil {
				return fmt.Errorf("build: module %q: %w", n, err)
			}
		case m.Path != "" && !strings.HasSuffix(m.Path, ".lua"):
			// Upstream (builtin.lua:296-309) treats any string module whose
			// extension is not "lua" as a single-source C/C++ compile
			// (info = {info}), so .c/.cc/.cpp/.cxx/.m all compile.
			err := compileModule(ctx, srcDir, destDir, n,
				rocks.Module{Sources: []string{m.Path}}, flags, cfg, vars)
			if err != nil {
				return fmt.Errorf("build: module %q: %w", n, err)
			}
		case len(m.Sources) > 0:
			err := compileModule(ctx, srcDir, destDir, n, m, flags, cfg, vars)
			if err != nil {
				return fmt.Errorf("build: module %q: %w", n, err)
			}
		default:
			return fmt.Errorf("build: module %q: empty entry (no path, no sources)", n)
		}
	}

	err := installBuildInstall(srcDir, destDir, spec.Build.Install)
	if err != nil {
		return fmt.Errorf("build: install: %w", err)
	}

	for _, d := range spec.Build.CopyDirectories {
		err := copyDir(filepath.Join(srcDir, d), filepath.Join(destDir, d))
		if err != nil {
			return fmt.Errorf("build: copy_directories %q: %w", d, err)
		}
	}

	return nil
}

// needsCC reports whether the module entry compiles a C source. A string
// module compiles unless it is a .lua source (builtin.lua:296-309).
func needsCC(m rocks.Module) bool {
	if m.Path != "" && !strings.HasSuffix(m.Path, ".lua") {
		return true
	}

	return len(m.Sources) > 0
}

// moduleSlashPath converts a dotted module name to the slashed
// destination subpath. "foo.bar.baz" → "foo/bar/baz".
func moduleSlashPath(name string) string {
	return strings.ReplaceAll(name, ".", "/")
}

// moduleDirPath mirrors Lua path.module_to_path: it strips the final dotted
// component and turns the remaining dots into slashes. "a.b" → "a",
// "foo" → "". This is the destination DIRECTORY for an is_module_path install.
func moduleDirPath(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return strings.ReplaceAll(name[:i], ".", "/")
	}

	return ""
}

// moduleLastSegment returns the part of a dotted module name after the last
// dot ("a.b" → "b", "foo" → "foo").
func moduleLastSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}

	return name
}

// installLuaModule copies a single .lua module file from srcDir/path into
// destDir/lua/<dotted-to-slashed-name>.lua.
//
// Upstream builtin.lua (299-306) special-cases a source whose basename is
// exactly "init.lua": unless the module name already ends in ".init", the file
// keeps its "init.lua" name and lands under module_to_path(name..".init"),
// i.e. <slashed-name>/init.lua — preserving the package layout rather than
// collapsing it into a single <name>.lua file.
func installLuaModule(srcDir, destDir, name, path string) error {
	var dst string
	if filepath.Base(path) == "init.lua" && !strings.HasSuffix(name, ".init") {
		dst = filepath.Join(destDir, "lua", moduleSlashPath(name), "init.lua")
	} else {
		dst = filepath.Join(destDir, "lua", moduleSlashPath(name)+".lua")
	}

	return copyFile(filepath.Join(srcDir, path), dst)
}

// compileModule builds a single .so via one cc invocation:
//
//	$CC $CFLAGS [-Ddefine...] [-Iincdir...] $LIBFLAG \
//	    -o destDir/lib/<slashed>.so <sources...> \
//	    [-Llibdir...] [-llibname...] $LDFLAGS
//
// All sources are passed in a single cc call (do not reimplement
// build infrastructure; rely on cc to handle multiple .c inputs).
func compileModule(ctx context.Context, srcDir, destDir, name string, m rocks.Module, flags Flags, cfg rocks.Config, vars map[string]string) error {
	out := filepath.Join(destDir, "lib", moduleSlashPath(name)+flags.Ext)

	err := os.MkdirAll(filepath.Dir(out), dirPerm)
	if err != nil {
		return err
	}

	// Expand $(NAME) placeholders in the compile/link flags (glr-0uv). Source
	// paths are left verbatim — upstream substitutes only defines/incdirs/
	// libdirs/libraries.
	defines := substituteVarsSlice(m.Defines, vars)
	incdirs := substituteVarsSlice(m.Incdirs, vars)
	libdirs := substituteVarsSlice(m.Libdirs, vars)
	libraries := substituteVarsSlice(m.Libraries, vars)

	args := make([]string, 0, initialArgsCap)

	args = append(args, flags.CFLAGS...)

	for _, d := range defines {
		args = append(args, "-D"+d)
	}

	for _, inc := range incdirs {
		args = append(args, "-I"+inc)
	}

	args = append(args, flags.LIBFLAG...)

	args = append(args, "-o", out)

	for _, s := range m.Sources {
		args = append(args, filepath.Join(srcDir, s))
	}

	for _, l := range libdirs {
		args = append(args, "-L"+l)
		// Mirror builtin.lua:256-259: on platforms with gcc_rpath on (unix/
		// linux) emit a matching runtime search path so the .so can dlopen an
		// external lib from a non-standard libdir.
		if flags.GccRpath {
			args = append(args, "-Wl,-rpath,"+l)
		}
	}

	for _, lib := range libraries {
		args = append(args, "-l"+lib)
	}

	args = append(args, flags.LDFLAGS...)

	return runCmd(ctx, flags.CC, args, srcDir, buildEnv(cfg))
}

// installBuildInstall processes the four sub-maps of build.install. Each
// map is destination-name → source-path-relative-to-srcDir. The
// destination layout under destDir is:
//
//	install.lua  → destDir/lua/<module_to_path(key)>/<last>.lua
//	install.lib  → destDir/lib/<module_to_path(key)>/<source-basename>  (mode 0o750)
//	install.bin  → destDir/bin/<key-as-given>                 (mode 0o750)
//	install.conf → destDir/conf/<key-as-given>
//
// The is_module_path groups (lua, lib) mirror upstream build.lua install_to:
// the destination directory is module_to_path(key) and the source basename is
// preserved UNLESS it is a .lua file, in which case it is renamed to
// <last-dotted-segment>.lua. A shared library keeps its own basename (no
// forced .so, no rename).
//
// tree.Deploy may later re-copy these into the deploy tree; that
// duplication is acceptable for now and the install dirs win since deploy
// runs after build.
func installBuildInstall(srcDir, destDir string, bi rocks.BuildInstall) error {
	type sub struct {
		name     string
		entries  map[string]string
		subdir   string
		isModule bool
		isExe    bool
	}

	subs := []sub{
		{name: "install.lua", entries: bi.Lua, subdir: "lua", isModule: true},
		{name: "install.lib", entries: bi.Lib, subdir: "lib", isModule: true, isExe: true},
		{name: "install.bin", entries: bi.Bin, subdir: "bin", isExe: true},
		{name: "install.conf", entries: bi.Conf, subdir: "conf"},
	}
	for _, s := range subs {
		keys := make([]string, 0, len(s.entries))
		for k := range s.entries {
			keys = append(keys, k)
		}

		sort.Strings(keys)

		for _, k := range keys {
			src := filepath.Join(srcDir, s.entries[k])

			var rel string

			if s.isModule {
				filename := filepath.Base(s.entries[k])
				if strings.HasSuffix(filename, ".lua") {
					filename = moduleLastSegment(k) + ".lua"
				}

				rel = filepath.Join(moduleDirPath(k), filename)
			} else {
				rel = k
			}

			dst := filepath.Join(destDir, s.subdir, rel)

			err := copyFile(src, dst)
			if err != nil {
				return fmt.Errorf("%s[%q]: %w", s.name, k, err)
			}

			if s.isExe {
				err := os.Chmod(dst, exePerm) //nolint:gosec // installed binaries/libraries must be executable
				if err != nil {
					return fmt.Errorf("%s[%q]: chmod: %w", s.name, k, err)
				}
			}
		}
	}

	return nil
}

// copyFile copies a file preserving the source file's regular-file mode
// bits (lower 9 bits). Parent directories of dst are created with dirPerm
// (0o750).
func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return err
	}

	in, err := os.Open(src) //nolint:gosec // src is a rockspec-derived build path, internally controlled
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	st, err := in.Stat()
	if err != nil {
		return err
	}

	mode := st.Mode().Perm()
	if mode == 0 {
		mode = 0o644
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode) //nolint:gosec // dst is a rockspec-derived build path, internally controlled
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	return out.Close()
}

// copyDir recursively copies src into dst. Symlinks are not specially
// handled — they are dereferenced (matches upstream luarocks behavior for
// copy_directories on unix).
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(src, p)
		if relErr != nil {
			return relErr
		}

		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm()|dirOwnerRWX)
		}

		if info.Mode()&os.ModeSymlink != 0 {
			// Resolve to real file and copy its contents.
			return copyFile(p, target)
		}

		return copyFile(p, target)
	})
}
