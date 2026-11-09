package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/internal/safepath"
)

// Files can be deployed under their plain (non-versioned) name or under a
// versioned one, since several installed rocks — two versions of one rock, or
// two rocks — may provide the same item: a module, deployed from the lua or
// lib dir, or a command, deployed from the bin dir. Exactly one provider of an
// item holds the plain path: the lexicographically smallest rock name, and of
// its versions the newest. The others hold versioned paths (MungedPath). This
// file mirrors how upstream keeps that invariant across deploys and removals
// (repos.lua:229-268, 440-551, 585-695).

// ItemKind says which index of the tree manifest a deployed item belongs to.
type ItemKind int

const (
	// ModuleItem is a module: the item a file of the lua or lib deploy dir
	// provides, named by the file's module name (path_to_module).
	ModuleItem ItemKind = iota
	// CommandItem is a command: the item a file of the bin dir provides,
	// named by the file's path under that dir.
	CommandItem
)

// Deploy types: the rock_manifest subtree a deployed file comes from, which
// picks its deploy dir (repos.lua get_deploy_paths).
const (
	deployLua = kindLua
	deployLib = kindLib
	deployBin = "bin"
)

// deployDirOf returns the deploy dir of a deploy type.
func (t *Tree) deployDirOf(dt string) string {
	switch dt {
	case deployLua:
		return t.DeployLuaDir()
	case deployLib:
		return t.DeployLibDir()
	default:
		return t.BinDir()
	}
}

// providedItem mirrors upstream get_provided_item (repos.lua:19-25): the kind
// and name of the item the file rel of deploy type dt provides.
func providedItem(dt, rel string) (ItemKind, string) {
	if dt == deployBin {
		return CommandItem, rel
	}

	return ModuleItem, pathToModule(rel)
}

// providers returns the installed providers of item, as t.Providers reports
// them, or nil when t.Providers is unset.
func (t *Tree) providers(kind ItemKind, item string) []string {
	if t.Providers == nil {
		return nil
	}

	return t.Providers(kind, item)
}

// currentProvider mirrors the lookup of upstream check_spot_if_available
// (repos.lua:249-257): the rock name and version of the first provider of the
// item the file rel of deploy type dt provides. Older LuaRocks registered a
// "foo.init" file as module "foo", so a lua file whose module name ends in
// ".init" falls back to that name.
func (t *Tree) currentProvider(dt, rel string) (name, version string, ok bool) {
	kind, item := providedItem(dt, rel)

	list := t.providers(kind, item)
	if len(list) == 0 && dt == deployLua {
		if base, cut := strings.CutSuffix(item, ".init"); cut {
			list = t.providers(kind, base)
		}
	}

	if len(list) == 0 {
		return "", "", false
	}

	return splitProvider(list[0])
}

// nextProviderIndex is the index of the provider that takes an item's plain
// path over when the provider at index 0 goes away.
const nextProviderIndex = 1

// nextProvider mirrors upstream manif.get_next_provider: the second provider of
// the item the file rel of deploy type dt provides, the one that takes over the
// plain path when the first goes away.
func (t *Tree) nextProvider(dt, rel string) (name, version string, ok bool) {
	kind, item := providedItem(dt, rel)

	list := t.providers(kind, item)
	if len(list) <= nextProviderIndex {
		return "", "", false
	}

	return splitProvider(list[nextProviderIndex])
}

// splitProvider splits a "name/version" provider token.
func splitProvider(token string) (name, version string, ok bool) {
	return strings.Cut(token, "/")
}

// takesPlainPath mirrors the decision of upstream check_spot_if_available
// (repos.lua:259-267): name/version deploys the file rel of deploy type dt under
// its plain path ("nv") when the item has no provider yet, when name sorts
// before the current provider's, or when it is the current provider's name at
// the same or a newer version; otherwise under its versioned name ("v"). It
// also returns the current provider.
func (t *Tree) takesPlainPath(dt, rel, name, version string) (plain bool, curName, curVer string) {
	curName, curVer, ok := t.currentProvider(dt, rel)

	plain = !ok ||
		name < curName ||
		(name == curName && (version == curVer || versionGreater(version, curVer)))

	return plain, curName, curVer
}

// counterpart is one file a deploy or removal renames between its plain and
// its versioned path: a deploy type and a path under its deploy dir.
type counterpart struct {
	dt  string
	rel string
}

// deployCounterparts lists the files of the current provider that a deploy
// demotes when the new rock takes the plain path of the file rel of deploy
// type dt (repos.lua:475-478, 495-500, 514-519): the provider's own file at that
// path, and, for a module, the other form of the module too — the .so beside a
// .lua and the .lua beside a .so — so that one rock's Lua and C halves of a
// module never split between two providers.
func deployCounterparts(dt, rel string) []counterpart {
	switch dt {
	case deployLua:
		return []counterpart{{deployLua, rel}, {deployLib, withSoExt(rel)}}
	case deployLib:
		return []counterpart{{deployLua, withLuaExt(rel)}, {deployLib, rel}}
	default:
		return []counterpart{{deployBin, rel}}
	}
}

// deleteCounterparts lists the files of the next provider that a removal
// promotes to their plain paths when the removed rock held the plain path of
// the file rel of deploy type dt (repos.lua:608-617, 628-636, 647-655). It
// mirrors upstream exactly, including its lua branch, which names the lib
// counterpart with the .lua extension the file already has
// (`file_path:gsub("%.[^.]+$", ".lua")`), so a lib file is never promoted from
// there.
func deleteCounterparts(dt, rel string) []counterpart {
	switch dt {
	case deployLua:
		return []counterpart{{deployLua, rel}, {deployLib, withLuaExt(rel)}}
	case deployLib:
		return []counterpart{{deployLua, withLuaExt(rel)}, {deployLib, rel}}
	default:
		return []counterpart{{deployBin, rel}}
	}
}

// withSoExt mirrors Lua's rel:gsub("%.lua$", ".so") (cfg.lib_extension is "so"
// on every platform the native backend serves): it swaps a trailing .lua for
// .so and leaves any other path alone.
func withSoExt(rel string) string {
	if base, cut := strings.CutSuffix(rel, ".lua"); cut {
		return base + ".so"
	}

	return rel
}

// withLuaExt mirrors Lua's rel:gsub("%.[^.]+$", ".lua"): it swaps the
// extension of the last path element — a final "." followed by anything but
// dots — for .lua, and leaves a path without one alone. As in Lua, the dot may
// sit in a directory name when the file name has none ("a.b/c" becomes
// "a.lua").
func withLuaExt(rel string) string {
	i := strings.LastIndex(rel, ".")
	if i < 0 || i == len(rel)-1 {
		return rel
	}

	return rel[:i] + ".lua"
}

// resolveSpot mirrors what upstream repos.deploy_files does for one file of the
// new rock name/version (repos.lua:470-541): it decides between the plain and
// the versioned path (takesPlainPath), demotes the current provider's files to
// their versioned names when the new rock takes the plain path
// (deployCounterparts), and clears the target: a file there that belongs to no
// provider of the same name and version is moved aside to a backup
// (backupExisting), one that does is removed. It returns the target, the path
// the caller writes the new file to.
//
// Upstream performs every rename of a deploy before it installs any file.
// Deploy writes each file as soon as its spot is resolved instead, so a
// counterpart this Deploy already wrote — the new rock's own .lua, say, when
// its .so is resolved next — is the new rock's and is left alone: upstream
// renamed the old file there before writing the new one.
//
// One deliberate departure: when the current provider is the very name and
// version being deployed, its files are not demoted. Upstream's code would
// rename them to their own versioned names and leave those copies behind, but
// it never gets there — build_rockspec and install_binary_rock delete an
// installed copy of the version before deploying it, as the native client does
// too — so only a caller re-deploying over a tree manifest that still lists the
// version reaches this, and gets the file overwritten in place.
func (t *Tree) resolveSpot(dt, rel, newName, newVer string, written map[string]bool) (string, error) {
	deployDir := t.deployDirOf(dt)
	plain := filepath.Join(deployDir, rel)

	takesPlain, curName, curVer := t.takesPlainPath(dt, rel, newName, newVer)

	if takesPlain && curName != "" && (curName != newName || curVer != newVer) {
		for _, c := range deployCounterparts(dt, rel) {
			dir := t.deployDirOf(c.dt)
			src := filepath.Join(dir, c.rel)

			if written[src] {
				continue
			}

			if err := renameIfExists(src, MungedPath(dir, src, curName, curVer), dir); err != nil {
				return "", fmt.Errorf("demote %q: %w", src, err)
			}
		}
	}

	target := plain
	if !takesPlain {
		target = MungedPath(deployDir, plain, newName, newVer)
	}

	if curName != newName || curVer != newVer {
		if err := backupExisting(deployDir, target); err != nil {
			return "", err
		}
	} else if err := safepath.Remove(deployDir, target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("remove %q: %w", target, err)
	}

	return target, nil
}

// backupExisting mirrors upstream backup_existing (repos.lua:270-288): a file
// already at target, inside root, is renamed to target with "~" appended — as
// many times as it takes to reach a free name, so an earlier backup is never
// overwritten.
func backupExisting(root, target string) error {
	if _, err := safepath.Rel(root, target); err != nil {
		return err
	}

	if _, err := safepath.Lstat(root, target); err != nil {
		return nil //nolint:nilerr // nothing at target: nothing to back up
	}

	backup := target + "~"
	for {
		if _, err := safepath.Lstat(root, backup); err != nil {
			break
		}

		backup += "~"
	}

	if err := safepath.Rename(root, target, backup); err != nil {
		return fmt.Errorf("backup %q: %w", target, err)
	}

	return nil
}

// renameIfExists mirrors upstream op_rename (repos.lua:347-363): when src
// exists it replaces dst, whose directory is created as needed, and the
// directories src leaves empty are pruned up to, not including, stop. Both
// paths must be inside stop.
func renameIfExists(src, dst, stop string) error {
	for _, p := range []string{src, dst} {
		if _, err := safepath.Rel(stop, p); err != nil {
			return err
		}
	}

	if _, err := safepath.Lstat(stop, src); err != nil {
		return nil //nolint:nilerr // a missing source is a no-op, as upstream's fs.exists gate
	}

	if err := safepath.MkdirAll(stop, filepath.Dir(dst), dirPerm); err != nil {
		return err
	}

	if err := safepath.RemoveAll(stop, dst); err != nil {
		return err
	}

	if err := safepath.Rename(stop, src, dst); err != nil {
		return err
	}

	pruneEmptyParents(filepath.Dir(src), stop)

	return nil
}

// pruneEmptyParents removes dir and each parent it leaves empty, stopping at
// stop, which is kept — upstream's remove_dir_tree_if_empty, bounded by the
// deploy dir. Best-effort: a removal that fails ends it.
func pruneEmptyParents(dir, stop string) {
	stop = filepath.Clean(stop)

	for d := filepath.Clean(dir); d != stop && strings.HasPrefix(d, stop+string(filepath.Separator)); d = filepath.Dir(d) {
		if err := safepath.Remove(stop, d); err != nil {
			return
		}
	}
}

// spots records the deploy targets one Deploy call has already resolved: the
// plain (unversioned) target path of each deployed file → the final path
// resolveSpot chose for it, plus the set of final paths written.
//
// Upstream deploys a rock in a single pass: the build backend and
// install_files both write into the per-rock install dir, install_files
// overwriting whatever the backend put at the same path (build.lua:412-423),
// and repos.deploy_files then moves each file of the resulting rock_manifest to
// its deploy spot exactly once (repos.lua:488-541). Deploy instead reads the
// same file from more than one place — the builtin backend stages build.modules
// and build.install.* in the build dir that stage 4 relocates — so a second
// resolveSpot of a target the first one just wrote would find a file there and
// back it up to <target>~. spots makes the second visit reuse the first
// decision instead.
type spots struct {
	final   map[string]string
	written map[string]bool
}

func newSpots() *spots {
	return &spots{final: map[string]string{}, written: map[string]bool{}}
}

// claim returns the final on-disk target for the file rel of deploy type dt
// and whether this is the first time the Deploy call reaches it. The first
// claim resolves the spot (resolveSpot); a repeat returns the recorded final
// path without consulting the tree again.
//
// rel must name an entry of the deploy dir: one that leaves it, or names the
// dir itself, fails with an error wrapping rocks.ErrUnsafePath.
func (s *spots) claim(t *Tree, dt, rel, newName, newVer string) (string, bool, error) {
	plain, err := safepath.JoinEntry(t.deployDirOf(dt), rel)
	if err != nil {
		return "", false, err
	}

	if final, ok := s.final[plain]; ok {
		return final, false, nil
	}

	final, err := t.resolveSpot(dt, rel, newName, newVer, s.written)
	if err != nil {
		return "", false, err
	}

	s.final[plain] = final
	s.written[final] = true

	return final, true, nil
}

// seen reports whether the Deploy call has already claimed the file rel of
// deploy type dt.
func (s *spots) seen(t *Tree, dt, rel string) bool {
	_, ok := s.final[filepath.Join(t.deployDirOf(dt), rel)]

	return ok
}

// DeleteVersion undeploys a single installed rock version, as upstream
// repos.delete_version does (repos.lua:585-695): for each file of its
// rock_manifest it deletes the copy the version deployed — the plain path when
// the version holds it, the versioned one otherwise (takesPlainPath) — and when
// it held the plain path, promotes the item's next provider (t.Providers, index
// 1) by renaming that provider's versioned files to their plain names
// (deleteCounterparts). Then it removes the per-rock install directory and the
// rock's name directory when no other version remains.
//
// t.Providers must still list the version being deleted: removing it from the
// tree manifest is the caller's job, after DeleteVersion. A missing or
// unreadable rock_manifest still lets the install dir be removed, the
// deployed files left where they are; Tx.Remove finds them through the tree
// manifest instead.
//
// Unlike upstream it removes nothing outside the tree: a name or version that
// is not a single directory, or a rock_manifest entry that leaves its deploy
// dir, fails it with an error wrapping rocks.ErrUnsafePath before anything is
// removed.
func (t *Tree) DeleteVersion(name, version string) error {
	return t.deleteVersion(name, version, nil)
}

// deleteVersion is DeleteVersion, undeploying the files of fallback, when not
// nil, if the rock_manifest of name/version cannot be read.
func (t *Tree) deleteVersion(name, version string, fallback *rocks.RockManifest) error {
	if err := checkNameVersion(name, version); err != nil {
		return fmt.Errorf("tree.DeleteVersion: %w", err)
	}

	installDir := t.InstallDir(name, version)

	rm, err := t.Store.ReadRock(filepath.Join(installDir, "rock_manifest"))
	if err != nil || rm == nil {
		rm = fallback
	}

	if rm != nil {
		if err := t.undeployFiles(name, version, rm); err != nil {
			return fmt.Errorf("tree.DeleteVersion: %w", err)
		}
	}

	if err := safepath.RemoveAll(t.RocksDir(), installDir); err != nil {
		return fmt.Errorf("tree.DeleteVersion: remove install dir: %w", err)
	}

	// Prune the rock's name dir when no versions remain.
	nameDir := filepath.Join(t.RocksDir(), name)
	if entries, err := os.ReadDir(nameDir); err == nil && len(entries) == 0 {
		_ = os.Remove(nameDir)
	}

	return nil
}

// checkNameVersion checks that a rock's name and version are one directory
// each, so <rocks-dir>/<name>/<version> stays an install dir of the tree.
func checkNameVersion(name, version string) error {
	if err := safepath.Name(name); err != nil {
		return fmt.Errorf("rock name: %w", err)
	}

	if err := safepath.Name(version); err != nil {
		return fmt.Errorf("rock version: %w", err)
	}

	return nil
}

// undeployFiles deletes the deployed files of name/version listed in rm and
// promotes the next providers, all deletions first, as upstream does. An entry
// of rm that leaves its deploy dir fails it before anything is removed.
func (t *Tree) undeployFiles(name, version string, rm *rocks.RockManifest) error {
	type rename struct{ src, dst, stop string }

	var (
		deletes []counterpart
		renames []rename
	)

	for _, sec := range []struct {
		dt    string
		files map[string]string
	}{
		{deployBin, rm.Bin},
		{deployLua, rm.Lua},
		{deployLib, rm.Lib},
	} {
		for _, rel := range sortedKeys(sec.files) {
			if _, err := safepath.JoinEntry(t.deployDirOf(sec.dt), filepath.FromSlash(rel)); err != nil {
				return fmt.Errorf("rock_manifest %s entry: %w", sec.dt, err)
			}

			if plain, _, _ := t.takesPlainPath(sec.dt, rel, name, version); !plain {
				deletes = append(deletes, counterpart{sec.dt, mungedRel(rel, name, version)})

				continue
			}

			deletes = append(deletes, counterpart{sec.dt, rel})

			nextName, nextVer, ok := t.nextProvider(sec.dt, rel)
			if !ok {
				continue
			}

			for _, c := range deleteCounterparts(sec.dt, rel) {
				cdir := t.deployDirOf(c.dt)
				plainPath := filepath.Join(cdir, c.rel)
				renames = append(renames, rename{MungedPath(cdir, plainPath, nextName, nextVer), plainPath, cdir})
			}
		}
	}

	for _, d := range deletes {
		dir := t.deployDirOf(d.dt)
		p := filepath.Join(dir, filepath.FromSlash(d.rel))

		if err := safepath.Remove(dir, p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", p, err)
		}

		pruneEmptyParents(filepath.Dir(p), dir)
	}

	for _, r := range renames {
		if err := renameIfExists(r.src, r.dst, r.stop); err != nil {
			return fmt.Errorf("promote %q: %w", r.src, err)
		}
	}

	return nil
}

// mungedRel returns the versioned form of the deploy-dir-relative path rel,
// as MungedPath forms it.
func mungedRel(rel, pkg, ver string) string {
	return mungedVersion(pkg, ver) + "-" + rel
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// versionGreater reports whether version a is strictly greater than b under the
// LuaRocks version ordering. Unparseable versions compare as not-greater.
func versionGreater(a, b string) bool {
	av, err := deps.ParseVersion(a)
	if err != nil {
		return false
	}

	bv, err := deps.ParseVersion(b)
	if err != nil {
		return false
	}

	return deps.Compare(av, bv) > 0
}

// pathToModule mirrors upstream path.path_to_module: strip a trailing
// extension, turn slashes into dots, and trim leading/trailing dots.
// "foo/bar.lua" → "foo.bar", "metrics/init.lua" → "metrics.init".
func pathToModule(rel string) string {
	name := rel
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[:i]
	}

	name = strings.ReplaceAll(name, "/", ".")

	return strings.Trim(name, ".")
}

// mungedVersion returns the `<pkg>_<ver>` identifier with every `.` and `-`
// replaced by `_`, matching upstream `(name.."_"..version):gsub("%-","_")
// :gsub("%.","_")` (core/path.lua:33).
func mungedVersion(pkg, ver string) string {
	combined := pkg + "_" + ver
	combined = strings.ReplaceAll(combined, "-", "_")
	combined = strings.ReplaceAll(combined, ".", "_")

	return combined
}

// MungedPath returns the versioned_name form of target under deployDir: the
// munged `<pkg>_<ver>` token dash-joined onto the path remainder after the
// deploy-dir prefix (core/path.lua path.versioned_name). Exported because
// installer logic — selecting the active version among munged siblings — needs
// the same string formation that Deploy used.
func MungedPath(deployDir, target, pkg, ver string) string {
	rest := strings.TrimLeft(strings.TrimPrefix(target, deployDir), "/")

	return filepath.Join(deployDir, mungedVersion(pkg, ver)+"-"+rest)
}

// ModuleIndex inverts a deployed RockManifest into the module index the tree
// manifest carries per installed rock: module name → deployed path, relative to
// the lua/lib deploy dir.
//
// It mirrors upstream repos.package_modules, which derives the same index from
// the rock_manifest's lua/lib sections rather than from the rockspec's declared
// build.modules — and for the same reason. build.modules is populated by the
// builtin backend only, so a cmake, make or command rock declares no modules at
// all and an index keyed on that field silently stays empty, however much the
// rock actually deployed. Like upstream's, the rock_manifest lists every file
// under its plain path, whether the rock holds that path or a versioned one, so
// a rock that lost a module to another provider still lists it.
//
// One deliberate departure from upstream, documented rather than fixed here
// because it would change behaviour beyond the derivation: upstream scans lib
// then lua, so a rock shipping both `foo.so` and `foo.lua` records the .lua
// path. This scans lua then lib, keeping the .so preference the caller's
// rockspec-driven index has always encoded.
//
// Exported for the same reason MungedPath is: the caller that writes the tree
// manifest must invert exactly the path formation Deploy used.
func ModuleIndex(rm *rocks.RockManifest) map[string]string {
	out := map[string]string{}
	if rm == nil {
		return out
	}

	// lua first, then lib: a later assignment wins, so a .so beats a .lua of
	// the same module name.
	for _, files := range []map[string]string{rm.Lua, rm.Lib} {
		for rel := range files {
			if mod := pathToModule(rel); mod != "" {
				out[mod] = rel
			}
		}
	}

	return out
}

// CommandIndex inverts a deployed RockManifest into the command index the tree
// manifest carries per installed rock: command name → its path relative to the
// bin deploy dir, which for a command is the name itself. It mirrors upstream
// repos.package_commands, which reads the rock_manifest's bin section, so a
// command installed by a make, cmake or command backend is indexed as well as
// a build.install.bin entry.
func CommandIndex(rm *rocks.RockManifest) map[string]string {
	out := map[string]string{}
	if rm == nil {
		return out
	}

	for rel := range rm.Bin {
		out[rel] = rel
	}

	return out
}
