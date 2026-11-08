package build

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	rocks "github.com/tarantool/go-luarocks"
)

// This file finds the external dependencies of a rockspec the way upstream
// deps.check_external_deps does for a build (deps.lua:380-606) and fills in
// the <NAME>_DIR, <NAME>_BINDIR, <NAME>_INCDIR and <NAME>_LIBDIR variables
// (and the <NAME>_<dir>_FILE of each file it found) that a rockspec
// references, e.g. `incdirs = { "$(TARANTOOL_INCDIR)" }`.
//
// Upstream fails the build when a dependency is not found. The native
// backend keeps its own behaviour there and builds anyway: a dependency that
// is found nowhere gets the directories of the first place looked at, so a
// build referencing them still gets valid paths, and the compiler or linker
// reports what is missing.

// extDepDir is one of the directories of an external dependency's prefix,
// as upstream get_external_deps_dirs describes it.
type extDepDir struct {
	// name is the variable suffix: BINDIR, INCDIR or LIBDIR.
	name string
	// subdirs are the subdirectories of the prefix it may be.
	subdirs []string
	// file returns the ExternalDep file checked in it, "" for none.
	file func(rocks.ExternalDep) string
	// patterns are the file-name patterns, "?" standing for a bare name.
	patterns []string
}

// extDepPlatform is what upstream's cfg holds for the external-dependency
// lookup on one platform.
type extDepPlatform struct {
	// dirs are BINDIR, INCDIR and LIBDIR, in the order upstream checks them.
	dirs []extDepDir
	// systemDirs is cfg.external_deps_dirs before the hardcoded PREFIX.
	systemDirs []string
	// extraDirs is what cfg.external_deps_dirs holds after it.
	extraDirs []string
	// libExtension is cfg.external_lib_extension.
	libExtension string
}

// gccMultiarch is what `gcc -print-multiarch` prints, which upstream adds to
// the library subdirectories on Linux; "" when there is no such gcc.
var gccMultiarch = sync.OnceValue(func() string {
	out, err := exec.Command("gcc", "-print-multiarch").Output()
	if err != nil {
		return ""
	}

	line, _, _ := strings.Cut(string(out), "\n")

	return strings.TrimSpace(line)
})

// extDepPlatformFor returns the lookup settings of goos, as core/cfg.lua
// make_defaults sets them for the Tarantool fork (whose hardcoded.lua names
// the bin, include and lib subdirectories), with multiarch the output of
// `gcc -print-multiarch`.
func extDepPlatformFor(goos, multiarch string) extDepPlatform {
	p := extDepPlatform{
		systemDirs:   []string{"/usr/local", "/usr", "/"},
		libExtension: "so",
	}

	libSubdirs := []string{"lib", "lib64"}
	libPatterns := []string{"lib?.a", "lib?.so", "lib?.so.*"}

	switch goos {
	case "linux":
		if multiarch != "" {
			libSubdirs = []string{"lib", "lib/" + multiarch, "lib64"}
		}
	case "darwin":
		p.libExtension = "dylib"
		p.extraDirs = []string{"/usr/local/opt"}
		libSubdirs = []string{"", "lib"}

		libPatterns = append([]string{"/?/lib/lib?.dylib"}, libPatterns...)
	}

	p.dirs = []extDepDir{
		{name: "BINDIR", subdirs: []string{"bin"}, file: func(rocks.ExternalDep) string { return "" }, patterns: []string{"?"}},
		{name: "INCDIR", subdirs: []string{"include"}, file: func(d rocks.ExternalDep) string { return d.Header }, patterns: []string{"?.h"}},
		{name: "LIBDIR", subdirs: libSubdirs, file: func(d rocks.ExternalDep) string { return d.Library }, patterns: libPatterns},
	}

	return p
}

// externalDepsPrefixes is cfg.external_deps_dirs: the system dirs, then the
// hardcoded PREFIX — Config.Tarantool.Prefix, or /usr as hardcoded.lua
// defaults it — then the platform's extra dirs.
func (p extDepPlatform) externalDepsPrefixes(cfg rocks.Config) []string {
	prefix := cfg.Tarantool.Prefix
	if prefix == "" {
		prefix = "/usr"
	}

	return slices.Concat(p.systemDirs, []string{prefix}, p.extraDirs)
}

// addExternalDepVars adds to vars, for each external dependency of spec, the
// variables upstream check_external_deps sets: <NAME>_DIR, the prefix it was
// found under, <NAME>_BINDIR, <NAME>_INCDIR and <NAME>_LIBDIR, the
// directories under it, and <NAME>_<dir>_FILE for each file found. A
// dependency found under no prefix gets the directories of the first one.
func addExternalDepVars(spec *rocks.Rockspec, cfg rocks.Config, vars map[string]string) {
	if spec == nil || len(spec.ExternalDependencies) == 0 {
		return
	}

	p := extDepPlatformFor(runtime.GOOS, linuxMultiarch())
	p.addVars(spec, p.externalDepsPrefixes(cfg), vars)
}

// addVars is addExternalDepVars looking the dependencies up under prefixes.
func (p extDepPlatform) addVars(spec *rocks.Rockspec, prefixes []string, vars map[string]string) {
	for _, name := range slices.Sorted(maps.Keys(spec.ExternalDependencies)) {
		dep := spec.ExternalDependencies[name]

		found := false

		for _, prefix := range prefixes {
			if dirs, ok := p.lookup(prefix, dep); ok {
				setExternalDepVars(vars, name, prefix, dirs)

				found = true

				break
			}
		}

		if !found {
			setExternalDepVars(vars, name, prefixes[0], p.defaultDirs(prefixes[0]))
		}
	}
}

// linuxMultiarch is gccMultiarch on Linux and "" elsewhere, where upstream
// does not ask.
func linuxMultiarch() string {
	if runtime.GOOS != "linux" {
		return ""
	}

	return gccMultiarch()
}

// foundDir is where one directory of an external dependency was found, and
// the file found in it ("" when none was checked).
type foundDir struct {
	dir, file string
}

// setExternalDepVars sets the variables of the external dependency name,
// found under prefix with dirs, keyed by the dir name.
func setExternalDepVars(vars map[string]string, name, prefix string, dirs map[string]foundDir) {
	for dirName, d := range dirs {
		vars[name+"_"+dirName] = d.dir
		if d.file != "" {
			vars[name+"_"+dirName+"_FILE"] = d.file
		}
	}

	vars[name+"_DIR"] = prefix
}

// defaultDirs returns the first candidate directory under prefix of each
// dir, which is what upstream starts each one out as.
func (p extDepPlatform) defaultDirs(prefix string) map[string]foundDir {
	out := make(map[string]foundDir, len(p.dirs))
	for _, d := range p.dirs {
		out[d.name] = foundDir{dir: filepath.Join(prefix, d.subdirs[0])}
	}

	return out
}

// lookup is check_external_dependency_at for one prefix: it looks every file
// dep names up under its directory's candidate subdirectories of prefix and
// reports where each directory is, or false when a file is missing.
func (p extDepPlatform) lookup(prefix string, dep rocks.ExternalDep) (map[string]foundDir, bool) {
	out := make(map[string]foundDir, len(p.dirs))

	for _, d := range p.dirs {
		paths := make([]string, len(d.subdirs))
		for i, sub := range d.subdirs {
			paths[i] = filepath.Join(prefix, sub)
		}

		found := foundDir{dir: paths[0]}

		if file := d.file(dep); file != "" {
			dir, name, ok := p.findFile(paths, candidateFiles(file, d.patterns))
			if !ok {
				return nil, false
			}

			found = foundDir{dir: dir, file: name}
		}

		out[d.name] = found
	}

	return out, true
}

// candidateFiles is add_patterns_for_file followed by upstream's sort: the
// names file may have on disk under patterns, those without a "*" first,
// each group in the order they were added.
func candidateFiles(file string, patterns []string) []string {
	var files []string

	addAll := func(name string) {
		for _, pat := range patterns {
			files = append(files, strings.ReplaceAll(pat, "?", name))
		}
	}

	if !hasFileExtension(file) {
		addAll(file)
	} else {
		for _, pat := range patterns {
			if m, ok := deconstructPattern(file, pat); ok {
				addAll(m)
			}
		}

		files = append(files, file)
	}

	slices.SortStableFunc(files, func(a, b string) int {
		aw, bw := strings.Contains(a, "*"), strings.Contains(b, "*")

		switch {
		case !aw && bw:
			return -1
		case aw && !bw:
			return 1
		default:
			return 0
		}
	})

	return files
}

// fileExtensionRe is what upstream takes for a file name with an extension:
// one matching "%.[a-z]+$" or "%.[a-z]+%.".
var fileExtensionRe = regexp.MustCompile(`\.[a-z]+$|\.[a-z]+\.`)

// hasFileExtension reports whether file looks like it carries a file-name
// extension, so it is not a bare name the patterns are built around.
func hasFileExtension(file string) bool {
	return fileExtensionRe.MatchString(file)
}

// deconstructPattern is deconstruct_pattern: the part of file a pattern's
// "?" stands for, when file matches it.
func deconstructPattern(file, pattern string) (string, bool) {
	re := "^" + strings.NewReplacer(`\?`, "(.*)", `\*`, ".*").Replace(regexp.QuoteMeta(pattern)) + "$"

	m := regexp.MustCompile(re).FindStringSubmatch(file)
	if m == nil {
		return "", false
	}

	// A pattern without a "?" captures nothing, and Lua's match returns the
	// whole match then.
	return m[len(m)-1], true
}

// findFile looks each candidate file up in each of paths, in order, and
// returns the first path holding one and the file's name there. A name with
// a "*" is matched against the directory's entries; a shared-library name
// takes the platform's library extension, as upstream's "small convenience
// hack" gives it.
func (p extDepPlatform) findFile(paths, files []string) (string, string, bool) {
	listings := map[string][]string{}

	for _, f := range files {
		for _, ext := range []string{".so", ".dylib", ".dll"} {
			if base, ok := strings.CutSuffix(f, ext); ok {
				f = base + "." + p.libExtension

				break
			}
		}

		var match *regexp.Regexp

		if strings.Contains(f, "*") {
			match = regexp.MustCompile("^" + strings.ReplaceAll(regexp.QuoteMeta(f), `\*`, ".*") + "$")
			f = "matching " + f
		}

		for _, d := range paths {
			if match == nil {
				if st, err := os.Stat(filepath.Join(d, f)); err == nil && st.Mode().IsRegular() {
					return d, f, true
				}

				continue
			}

			entries, ok := listings[d]
			if !ok {
				entries = listDir(d)
				listings[d] = entries
			}

			if slices.ContainsFunc(entries, match.MatchString) {
				return d, f, true
			}
		}
	}

	return "", "", false
}

// listDir returns the names in dir, none when it cannot be read.
func listDir(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}

	return names
}
