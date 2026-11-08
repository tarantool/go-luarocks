package remote

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tarantool/go-luarocks/internal/safepath"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/rockspec"
)

// DefaultLuaVersions are the Lua versions MakeManifest writes a versioned
// manifest for when MakeManifestOpts.LuaVersions is empty: upstream's
// util.lua_versions (util.lua:146-161).
var DefaultLuaVersions = []string{"5.1", "5.2", "5.3", "5.4"}

// MakeManifestOpts tunes MakeManifest.
type MakeManifestOpts struct {
	// LuaVersions are the Lua versions to write a manifest-<version> for, in
	// order. Empty means DefaultLuaVersions, as upstream. Each must be a
	// plain file-name element; a duplicate is an error.
	LuaVersions []string

	// Index also writes index.html, the browsable page of the server, as
	// upstream make_manifest always does in repository mode
	// (admin/index.lua make_index). Its footer links the manifests of
	// LuaVersions.
	Index bool

	// Zip also writes manifest-<version>.zip, holding manifest-<version>, as
	// `luarocks-admin add` does after make_manifest (admin/cmd/add.lua
	// zip_manifests). A manifest-<version>.zip already in the directory is
	// rewritten whether or not Zip is set; see MakeManifest.
	Zip bool

	// Warn, when set, receives each message upstream prints to stderr while
	// indexing: a rockspec the per-Lua-version filter could not load, which
	// leaves its version in every versioned manifest.
	Warn func(msg string)
}

// MakeManifest turns dir into a rock server: it indexes the rock files
// directly in dir into dir/manifest and dir/manifest-<version> for each of
// opts.LuaVersions, as `luarocks-admin make_manifest <dir>` (repository mode,
// not --local-tree) does — admin/cmd/make_manifest.lua and
// manif/writer.lua make_manifest, store_results and filter_by_lua_version.
// Every file is byte-equal to upstream's for the same directory, as
// LuaRocks writes it on Tarantool's LuaJIT; with Index, index.html is too
// (admin/index.lua make_index), with two exceptions where it follows LuaJIT
// rather than the gopher-lua the lua backend runs on: a URL in a detailed
// description that holds '%', '-' or '_' is linked whole, and %s, %w and
// string.lower see ASCII only. Versions that compare equal but are spelled
// apart ("1.0-1", "1.0.0-1") are listed in byte order, where upstream's
// order is unspecified.
//
// The index is built from file names alone, parsed as upstream
// path.parse_name does: <name>-<version>.rockspec is arch "rockspec",
// <name>-<version>.src.rock arch "src", <name>-<version>.<arch>.rock arch
// <arch> (all, linux-x86_64, linux-aarch64, macosx-aarch64, …). Neither a
// rock nor a rockspec is opened to fill the entry, so a rock whose embedded
// rockspec disagrees with its file name is listed under its file name, as
// upstream lists it. Like upstream, a server manifest carries the arches of
// each version only — no dependencies, modules or commands — and the arches
// of a version keep the order of the directory listing, which is byte order
// (upstream lists the directory with `ls`, which sorts it; the order is the
// same in the C and C.UTF-8 locales). Entries whose name starts with a dot are
// skipped, as `ls` skips them, and so are names holding a newline, which
// `ls` would split into two.
//
// manifest lists every rock. manifest-<version> leaves out each version whose
// <name>-<version>.rockspec declares a "lua" dependency that version of Lua
// does not satisfy, with the platform overrides of the host applied first; a
// version with no .rockspec, or whose rockspec cannot be loaded (reported
// through opts.Warn), stays in every one. Namespaces are not indexed:
// make_manifest writes no manifests/<namespace>/, and a file name cannot
// carry one.
//
// Each file is written to a temporary file in dir and renamed over its
// name, so a reader sees the old file or the new one, never a part. Every
// file is computed before the first rename, so an input upstream fails on
// writes nothing. dir/manifest-<version> replaces a stale one, which matters
// because a rock server's client reads manifest-5.1 before manifest; for the
// same reason a manifest-<version>.zip already in dir, which clients read
// before both, is rewritten too, where upstream's make_manifest leaves it
// stale. The files are written with mode 0644, readable by a web server.
//
// Where upstream follows any name it is given, MakeManifest keeps every read
// inside dir: a rock file that is a symlink leading out of dir fails with an
// error wrapping rocks.ErrUnsafePath. It is also stricter about what it
// lists: an indexed name that is not a readable regular file is an error
// (upstream lists a dangling symlink or a directory as a rock). A
// subdirectory holding an entry whose name ends in "-<digits>" looks like an
// installed rock of a rock tree; upstream then reads a rock_manifest from its
// own rock tree and normally fails, and MakeManifest fails. A rockspec that
// upstream's loader raises a Lua error on, rather than rejecting (one whose
// package is not a string, for instance), fails MakeManifest as it fails
// upstream's.
//
// ctx bounds the call, the rockspecs it runs included: one that loops
// forever, which hangs upstream, stops with ctx.
func MakeManifest(ctx context.Context, dir string, opts MakeManifestOpts) error {
	luaVersions, err := makeManifestLuaVersions(opts.LuaVersions)
	if err != nil {
		return err
	}

	st, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("remote.MakeManifest: cannot access repository at %s: %w", dir, err)
	}

	if !st.IsDir() {
		return fmt.Errorf("remote.MakeManifest: cannot access repository at %s: not a directory", dir)
	}

	idx, err := scanRepository(ctx, dir)
	if err != nil {
		return fmt.Errorf("remote.MakeManifest: %w", err)
	}

	files, err := renderRepository(ctx, dir, idx, luaVersions, opts)
	if err != nil {
		return fmt.Errorf("remote.MakeManifest: %w", err)
	}

	if err := writeFilesAtomically(dir, files); err != nil {
		return fmt.Errorf("remote.MakeManifest: %w", err)
	}

	return nil
}

// makeManifestLuaVersions returns the Lua versions to write manifests for.
func makeManifestLuaVersions(given []string) ([]string, error) {
	if len(given) == 0 {
		return DefaultLuaVersions, nil
	}

	for i, lv := range given {
		if err := safepath.Name("manifest-" + lv); err != nil || lv == "" {
			return nil, fmt.Errorf("remote.MakeManifest: Lua version %q is not a plain name element", lv)
		}

		if slices.Contains(given[:i], lv) {
			return nil, fmt.Errorf("remote.MakeManifest: Lua version %q given twice", lv)
		}
	}

	return given, nil
}

// repoIndex is the result of upstream's search.disk_search over a rock
// server: name → version → the arches listed, in directory order.
type repoIndex map[string]map[string][]string

// Upstream path.parse_name (path.lua:175-182). A rock name ends in .rock and
// is matched to the end; the rockspec pattern is not anchored at the end, so
// "foo-1.0-1.rockspec" anywhere in the base name parses. disk_search then
// keeps only names ending in "rockspec" or "rock".
var (
	rockNameRe     = regexp.MustCompile(`(?s)^(.*)-([^-]+-[0-9]+)\.([^.]+)\.rock$`)
	rockspecNameRe = regexp.MustCompile(`(?s)^(.*)-([^-]+-[0-9]+)\.(rockspec)`)
	installedVerRe = regexp.MustCompile(`-[0-9]+$`)
)

// parseRockFileName mirrors path.parse_name on a directory entry name.
func parseRockFileName(fileName string) (name, version, arch string, ok bool) {
	base := luaBaseName(fileName)

	re := rockspecNameRe
	if strings.HasSuffix(fileName, ".rock") {
		re = rockNameRe
	}

	m := re.FindStringSubmatch(base)
	if m == nil {
		return "", "", "", false
	}

	return m[1], m[2], m[3], true
}

// luaBaseName mirrors dir.base_name (dir.lua:15-20): the part after the
// last slash or backslash, once trailing ones are dropped.
func luaBaseName(p string) string {
	trimmed := strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(trimmed, `/\`); i >= 0 {
		return trimmed[i+1:]
	}

	return p
}

// listLikeLs returns the names of the entries of the directory f, as the
// `ls` of upstream's shell-tool fs.dir prints them (fs/tools.lua
// dir_iterator): sorted, without the names starting with a dot. A name
// holding a newline, which `ls` would print as two lines, is left out.
func listLikeLs(f *os.File) ([]string, error) {
	entries, err := f.ReadDir(-1)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.Contains(n, "\n") {
			continue
		}

		names = append(names, n)
	}

	slices.Sort(names)

	return names, nil
}

// scanRepository mirrors search.disk_search(repo, queries.all("any"))
// (search.lua:60-89) over dir.
func scanRepository(ctx context.Context, dir string) (repoIndex, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("cannot access repository at %s: %w", dir, err)
	}

	defer func() { _ = root.Close() }()

	top, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	names, err := listLikeLs(top)
	_ = top.Close()

	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}

	idx := repoIndex{}

	for _, n := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		name, version, arch, ok := parseRockFileName(n)
		if ok && (strings.HasSuffix(n, "rockspec") || strings.HasSuffix(n, "rock")) {
			if err := checkRockFile(dir, n); err != nil {
				return nil, err
			}

			if idx[name] == nil {
				idx[name] = map[string][]string{}
			}

			idx[name][version] = append(idx[name][version], arch)

			continue
		}

		if err := checkNotInstalledRock(root, dir, n); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

// checkRockFile checks that the entry n of dir, which the manifest is about
// to list, is a regular file that can be read without leaving dir.
func checkRockFile(dir, n string) error {
	p := filepath.Join(dir, n)

	f, err := safepath.Open(dir, p)
	if err != nil {
		return fmt.Errorf("rock file %s: %w", p, err)
	}

	defer func() { _ = f.Close() }()

	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("rock file %s: %w", p, err)
	}

	if !st.Mode().IsRegular() {
		return fmt.Errorf("rock file %s is not a regular file", p)
	}

	return nil
}

// checkNotInstalledRock is the other branch of disk_search: an entry that is
// a directory and holds a "<version>-<revision>" entry is an installed rock
// of a rock tree, which upstream lists with arch "installed" and then fails
// to find the rock_manifest of in its own tree (store_results,
// manif.load_rock_manifest). An entry that cannot be reached inside dir —
// a symlink leading out, a dangling one — is not a directory here.
func checkNotInstalledRock(root *os.Root, dir, n string) error {
	st, err := root.Stat(n)
	if err != nil || !st.IsDir() {
		return nil //nolint:nilerr // Not a directory of dir: disk_search skips it.
	}

	sub, err := root.Open(n)
	if err != nil {
		return nil //nolint:nilerr // Unreadable: upstream's ls lists nothing either.
	}

	names, err := listLikeLs(sub)
	_ = sub.Close()

	if err != nil {
		return nil //nolint:nilerr // Unreadable: upstream's ls lists nothing either.
	}

	for _, v := range names {
		if installedVerRe.MatchString(v) {
			return fmt.Errorf("%s looks like the installed rock %s %s of a rock tree "+
				"(rock_manifest file not found for %s %s - not a LuaRocks tree?); "+
				"a rock server holds rock files only",
				filepath.Join(dir, n, v), n, v, n, v)
		}
	}

	return nil
}

// outFile is one file MakeManifest writes.
type outFile struct {
	name string
	data []byte
}

// renderRepository computes every file MakeManifest writes, in upstream's
// write order: each manifest-<version> (and its zip), manifest, index.html.
func renderRepository(
	ctx context.Context, dir string, idx repoIndex, luaVersions []string, opts MakeManifestOpts,
) ([]outFile, error) {
	filter := newLuaVersionFilter(ctx, dir, rockspec.RuntimePlatforms(), opts.Warn)
	defer filter.close()

	var files []outFile

	for _, lv := range luaVersions {
		kept, err := filter.apply(idx, lv)
		if err != nil {
			return nil, err
		}

		data, err := serverManifest(kept)
		if err != nil {
			return nil, err
		}

		name := "manifest-" + lv
		files = append(files, outFile{name: name, data: data})

		zipName := name + ".zip"

		if opts.Zip || fileExists(dir, zipName) {
			z, err := zipManifest(name, data)
			if err != nil {
				return nil, err
			}

			files = append(files, outFile{name: zipName, data: z})
		}
	}

	data, err := serverManifest(idx)
	if err != nil {
		return nil, err
	}

	files = append(files, outFile{name: "manifest", data: data})

	if opts.Index {
		page, err := indexHTML(ctx, dir, idx, luaVersions)
		if err != nil {
			return nil, err
		}

		files = append(files, outFile{name: "index.html", data: page})
	}

	return files, nil
}

// fileExists reports whether dir has an entry called name, of any kind.
func fileExists(dir, name string) bool {
	_, err := os.Lstat(filepath.Join(dir, name))

	return err == nil
}

// serverManifest serializes idx as the manifest store_results builds for a
// rock server — commands and modules empty, one {arch = …} per listed file —
// with upstream's persist writer.
func serverManifest(idx repoIndex) ([]byte, error) {
	repository := make(map[string]any, len(idx))

	for name, versions := range idx {
		vt := make(map[string]any, len(versions))

		for version, arches := range versions {
			entries := make([]any, 0, len(arches))
			for _, a := range arches {
				entries = append(entries, map[string]any{"arch": a})
			}

			vt[version] = entries
		}

		repository[name] = vt
	}

	var buf bytes.Buffer

	err := manif.Write(&buf, map[string]any{
		"commands":   map[string]any{},
		"modules":    map[string]any{},
		"repository": repository,
	})
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// zipManifest packs the manifest called name into a zip archive holding it
// alone, as `zip -r name.zip name` does for zip_manifests.
func zipManifest(name string, data []byte) ([]byte, error) {
	var buf bytes.Buffer

	zw := zip.NewWriter(&buf)

	var hdr zip.FileHeader

	hdr.Name = name
	hdr.Method = zip.Deflate
	hdr.Modified = time.Now()
	hdr.SetMode(manifestPerm)

	w, err := zw.CreateHeader(&hdr)
	if err != nil {
		return nil, fmt.Errorf("zip %s: %w", name, err)
	}

	if _, err := w.Write(data); err != nil {
		return nil, fmt.Errorf("zip %s: %w", name, err)
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("zip %s: %w", name, err)
	}

	return buf.Bytes(), nil
}

// manifestPerm is the mode of the files MakeManifest writes: a rock server
// is read by whoever serves it.
const manifestPerm fs.FileMode = 0o644

// writeFilesAtomically writes every file to a temporary file in dir first,
// then renames them over their names in order; on a failure before the
// renames nothing changes.
func writeFilesAtomically(dir string, files []outFile) error {
	temps := make([]string, 0, len(files))

	defer func() {
		for _, t := range temps {
			if t != "" {
				_ = os.Remove(t)
			}
		}
	}()

	for _, f := range files {
		t, err := writeTemp(dir, f)
		if err != nil {
			return err
		}

		temps = append(temps, t)
	}

	for i, f := range files {
		if err := os.Rename(temps[i], filepath.Join(dir, f.name)); err != nil {
			return fmt.Errorf("write %s: %w", filepath.Join(dir, f.name), err)
		}

		temps[i] = ""
	}

	return nil
}

// writeTemp writes f to a new temporary file in dir and returns its path.
// The name starts with a dot, so neither a concurrent MakeManifest nor
// upstream's `ls` lists it.
func writeTemp(dir string, f outFile) (string, error) {
	tmp, err := os.CreateTemp(dir, "."+f.name+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("write %s: %w", filepath.Join(dir, f.name), err)
	}

	_, err = tmp.Write(f.data)
	if err == nil {
		err = tmp.Chmod(manifestPerm)
	}

	err = errors.Join(err, tmp.Close())
	if err != nil {
		_ = os.Remove(tmp.Name())

		return "", fmt.Errorf("write %s: %w", filepath.Join(dir, f.name), err)
	}

	return tmp.Name(), nil
}
