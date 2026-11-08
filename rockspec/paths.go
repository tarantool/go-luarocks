package rockspec

import (
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
)

// The fields checked here become file system paths: joined under the
// sources (build.install values, build.modules paths and sources,
// copy_directories, source.dir), under the download dir (source.file) or
// under the tree (build.install keys). Upstream joins them as they come, so
// "../../x" reads or writes outside those dirs, even for a "none" rock that
// runs no build code. Validate rejects such a path instead, a deliberate
// strictness beyond upstream; the native backend checks every such join again
// where it is made, for the names that never pass through Validate.

// pathKind says what a path field names: a directory, which may be the one it
// is joined under itself ("."), or an entry beneath it, which may not.
type pathKind int

const (
	dirPath pathKind = iota
	entryPath
)

// isSeparator reports whether r separates path elements: "/" in a rockspec,
// and "\\" too, as the Windows separator rockspec paths never mean literally.
func isSeparator(r rune) bool { return r == '/' || r == '\\' }

// checkPath rejects the value p of field when it is empty, absolute, holds a
// ".." element, or, for an entry, names the directory it is joined under.
func checkPath(field, p string, kind pathKind) error {
	var reason string

	switch {
	case p == "":
		reason = "empty"
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`):
		reason = "an absolute path"
	case slices.Contains(strings.FieldsFunc(p, isSeparator), ".."):
		reason = `it reaches above its directory through ".."`
	case kind == entryPath && path.Clean(strings.ReplaceAll(p, `\`, "/")) == ".":
		reason = "it names its directory, not an entry of it"
	default:
		return nil
	}

	return fmt.Errorf("rockspec: invalid %s %q: %s: %w", field, p, reason, rocks.ErrUnsafePath)
}

// checkSourcePaths checks the path fields of a source table, field being its
// name ("source", or "source.platforms.<name>").
func checkSourcePaths(field string, src rocks.Source) error {
	if src.File != "" {
		if err := checkPath(field+".file", src.File, entryPath); err != nil {
			return err
		}
	}

	if src.Dir != "" {
		if err := checkPath(field+".dir", src.Dir, dirPath); err != nil {
			return err
		}
	}

	for _, plat := range slices.Sorted(maps.Keys(src.Platforms)) {
		if err := checkSourcePaths(field+".platforms."+plat, src.Platforms[plat]); err != nil {
			return err
		}
	}

	return nil
}

// checkBuildPaths checks the path fields of a build table, field being its
// name ("build", or "build.platforms.<name>").
func checkBuildPaths(field string, b rocks.Build) error {
	for _, sec := range []struct {
		name    string
		entries map[string]string
	}{
		{"lua", b.Install.Lua},
		{"lib", b.Install.Lib},
		{"bin", b.Install.Bin},
		{"conf", b.Install.Conf},
	} {
		prefix := field + ".install." + sec.name

		for _, key := range slices.Sorted(maps.Keys(sec.entries)) {
			if err := checkPath(prefix+" key", key, entryPath); err != nil {
				return err
			}

			if err := checkPath(prefix+"["+fmt.Sprintf("%q", key)+"]", sec.entries[key], entryPath); err != nil {
				return err
			}
		}
	}

	for _, name := range slices.Sorted(maps.Keys(b.Modules)) {
		mod := b.Modules[name]
		prefix := field + ".modules[" + fmt.Sprintf("%q", name) + "]"

		if mod.Path != "" {
			if err := checkPath(prefix, mod.Path, entryPath); err != nil {
				return err
			}
		}

		for _, src := range mod.Sources {
			if err := checkPath(prefix+".sources", src, entryPath); err != nil {
				return err
			}
		}
	}

	for _, dir := range b.CopyDirectories {
		if err := checkPath(field+".copy_directories", dir, dirPath); err != nil {
			return err
		}
	}

	for _, plat := range slices.Sorted(maps.Keys(b.Platforms)) {
		if err := checkBuildPaths(field+".platforms."+plat, b.Platforms[plat]); err != nil {
			return err
		}
	}

	return nil
}
