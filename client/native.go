package client

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// nativeEngine is the pure-Go backend (BackendNative). It holds the same
// state the *Rocks methods used before the Engine extraction and retains
// EXACT behavioral compatibility with the pre-task client.Rocks: the
// five implemented method bodies and their private helpers are moved here
// verbatim, with only the receiver renamed from (r *Rocks) to (e *nativeEngine).
//
// Remove, Search and Download were added natively afterwards (native.go,
// native_search.go, native_download.go). The ten operations the native
// backend does not implement return rocks.ErrNotImplemented — never a
// silent no-op.
type nativeEngine struct {
	cfg   rocks.Config
	store rocks.ManifestStore
	index rocks.RemoteIndex
	// manifests is the client's manifest cache, which the index of a per-call
	// server list and the searches of Search and Download share with index.
	manifests *remote.ManifestCache
	logger    *slog.Logger

	// luajitOnce guards the one-shot interpreter probe behind luajitVersion,
	// whose answer (luajit, possibly empty) names the version of the
	// VM-provided rocks a search reports. See native_search.go.
	luajitOnce sync.Once
	luajit     string
}

// unpackDirMode is the permission applied to directories created while
// extracting a .rock archive: owner rwx, group rx, no world access.
const unpackDirMode = 0o750

// luaInterpreter is the interpreter name baked into generated command
// wrappers, matching the tarantool/luarocks fork's hardcoded.LUA_INTERPRETER.
const luaInterpreter = "tarantool"

// defaultSysconfDir is upstream LuaRocks' cfg.sysconfdir fallback on Unix
// (luarocks/core/cfg.lua) when neither LUAROCKS_SYSCONFDIR nor a detected
// install prefix applies.
const defaultSysconfDir = "/etc/luarocks"

// sysconfDir mirrors upstream's cfg.sysconfdir resolution for the value
// exported as LUAROCKS_SYSCONFDIR in command wrappers: honor the
// LUAROCKS_SYSCONFDIR env override, else fall back to the Unix default.
func sysconfDir() string {
	if v := os.Getenv("LUAROCKS_SYSCONFDIR"); v != "" {
		return v
	}

	return defaultSysconfDir
}

// Install installs `name` (with optional version constraint in
// opts.Version) into e.cfg.Tree, including transitive deps per
// opts.Deps. The general algorithm:
//
//  1. Query the remote index for `name` candidates.
//  2. Pick the newest version satisfying opts.Version.
//  3. Resolve transitive deps (unless DepsNone), skipping the ones a rock
//     installed in e.cfg.Tree satisfies.
//  4. For each step in topo order: fetch source, eval rockspec, merge
//     platforms, validate, build, deploy, update tree manifest.
//  5. Install the requested rock itself.
//
// Returns ErrUnsupportedRockspecFeature for unrecognized build types
// (bubbled up from build.RunBackend). May also surface
// ErrMissingTarantoolHeaders when a C-extension rock is built.
func (e *nativeEngine) Install(ctx context.Context, name string, opts InstallOpts) error {
	if name == "" {
		return errors.New("rocks.Install: empty name")
	}

	idx := e.index
	if len(opts.Servers) > 0 {
		// Same dispatch as the facade's own server list (serverIndex): an
		// override entry may be a local directory as readily as an HTTP URL.
		idx = serverIndex(opts.Servers, e.cfg.InsecureServers, e.manifests)
	}

	cs, err := deps.ParseConstraints(opts.Version)
	if err != nil {
		return fmt.Errorf("rocks.Install: parse version %q: %w", opts.Version, err)
	}

	// A user-supplied "owner/rock" target selects the owner's namespace.
	queryName, namespace := name, ""
	if before, after, ok0 := strings.Cut(name, "/"); ok0 {
		namespace, queryName = before, after
	}

	candidates, err := idx.Query(ctx, queryName, namespace)
	if err != nil {
		return fmt.Errorf("rocks.Install: query %q: %w", name, err)
	}

	root, ok := pickNewest(candidates, cs)
	if !ok {
		return fmt.Errorf("rocks.Install: no version of %q matches %q", name, opts.Version)
	}

	e.logger.Info("rocks.Install: selected", "name", name, "version", root.Version.Raw, "url", root.URL)

	fetched, err := e.newArtifacts()
	if err != nil {
		return fmt.Errorf("rocks.Install: %w", err)
	}

	defer fetched.remove()

	// Fetch + eval root rockspec so we can resolve transitive deps.
	rootSpec, err := e.fetchAndEval(ctx, fetched, root.URL)
	if err != nil {
		return fmt.Errorf("rocks.Install: fetch/eval root: %w", err)
	}

	root.Spec = rootSpec

	var plan []rocks.InstallStep

	if opts.Deps != DepsNone {
		plan, err = e.resolvePlan(ctx, fetched, rootSpec, idx)
		if err != nil {
			return fmt.Errorf("rocks.Install: resolve deps: %w", err)
		}
	}

	for _, step := range plan {
		err := e.installStep(ctx, fetched, step)
		if err != nil {
			return fmt.Errorf("rocks.Install: dep %s-%s: %w", step.Name, step.Version.Raw, err)
		}
	}

	// Finally install the requested rock itself, from the artifact it was
	// chosen as, exactly like every dependency step.
	rootStep := rocks.InstallStep{
		Name:     rootSpec.Package,
		Version:  root.Version,
		URL:      root.URL,
		Rockspec: rootSpec,
	}
	if err := e.installArtifact(ctx, fetched, rootStep); err != nil {
		return fmt.Errorf("rocks.Install: install root: %w", err)
	}

	return nil
}

// resolvePlan walks the full transitive dependency closure of rootSpec via
// deps.Resolve. It hands Resolve a fetcher (the remote index returns bare
// name/version/URL rows with no preloaded rockspec, so each chosen rock is
// evaluated on demand), registers `tarantool` as a VM-provided rock at the
// configured version (mirroring util.get_rocks_provided in the
// tarantool/luarocks fork), and supplies the already-installed rocks so
// satisfied deps are skipped (glr-n88).
func (e *nativeEngine) resolvePlan(
	ctx context.Context, fetched *artifacts, rootSpec *rocks.Rockspec, idx rocks.RemoteIndex,
) ([]rocks.InstallStep, error) {
	fetchSpec := func(ctx context.Context, rock rocks.VersionedRock) (*rocks.Rockspec, error) {
		return e.fetchAndEval(ctx, fetched, rock.URL)
	}

	resolveOpts := []deps.Option{deps.WithSpecFetcher(fetchSpec)}

	if e.cfg.Tarantool.Version != "" {
		if ttVer, verr := deps.ParseVersion(e.cfg.Tarantool.Version + "-1"); verr == nil {
			resolveOpts = append(resolveOpts,
				deps.WithProvided(map[string]rocks.Version{"tarantool": ttVer}))
		}
	}

	// Only Config.Tree is consulted: a rock installed in another rock tree
	// does not satisfy a dependency, whatever opts.Deps says.
	if lookup, lerr := e.installedLookup(); lerr == nil {
		resolveOpts = append(resolveOpts, deps.WithInstalled(lookup))
	}

	return deps.Resolve(ctx, rootSpec, idx, resolveOpts...)
}

// Build evaluates the rockspec at specPath, fetches its declared source,
// runs the build backend, and deploys the result into e.cfg.Tree.
//
// Unlike Install, Build does not resolve or install dependencies, its
// build_dependencies included — it assumes the prerequisites are already
// present. Upstream `luarocks build`, which the lua backend runs, installs
// the missing ones first.
func (e *nativeEngine) Build(ctx context.Context, specPath string, opts BuildOpts) error {
	spec, err := evalAndPrepare(specPath, e.cfg)
	if err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-build-*")
	if err != nil {
		return fmt.Errorf("rocks.Build: mkdir tmp: %w", err)
	}

	if !opts.Keep {
		defer func() { _ = os.RemoveAll(tmp) }()
	}

	srcDir, err := e.fetchSource(ctx, spec, tmp)
	if err != nil {
		return fmt.Errorf("rocks.Build: %w", err)
	}

	return e.deployFromSource(ctx, spec, srcDir)
}

// Make is "build the rockspec found in cwd against the source already
// present in cwd" — the upstream `luarocks make` flow. It is the
// developer-iteration form of Build, and like Build it installs no
// dependencies.
func (e *nativeEngine) Make(ctx context.Context, opts MakeOpts) error {
	specPath := opts.RockspecPath
	if specPath == "" {
		entries, err := os.ReadDir(e.cfg.WorkingDir)
		if err != nil {
			return fmt.Errorf("rocks.Make: read %s: %w", e.cfg.WorkingDir, err)
		}

		var found []string

		for _, ent := range entries {
			if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".rockspec") {
				found = append(found, filepath.Join(e.cfg.WorkingDir, ent.Name()))
			}
		}

		if len(found) == 0 {
			return fmt.Errorf("rocks.Make: no .rockspec found in %s", e.cfg.WorkingDir)
		}

		if len(found) > 1 {
			return fmt.Errorf("rocks.Make: multiple .rockspec found in %s (%v); pass MakeOpts.RockspecPath", e.cfg.WorkingDir, found)
		}

		specPath = found[0]
	}

	spec, err := evalAndPrepare(specPath, e.cfg)
	if err != nil {
		return err
	}

	return e.deployFromSource(ctx, spec, e.cfg.WorkingDir)
}

// Pack produces a .rock or .src.rock archive for `target` (rock name) in
// e.cfg.WorkingDir and returns its path. It packs the newest installed version
// of the rock, as upstream `luarocks pack` picks it
// (search.pick_installed_rock).
//
//   - opts.SrcOnly == false: a binary rock, <name>-<ver>.<arch>.rock, as
//     upstream `luarocks pack <name>` makes it: the rock's install dir
//     <tree>/share/tarantool/rocks/<name>/<version>/ with its deployed lua
//     and lib files copied back in, arch being the host's for a rock with a
//     lib file or a binary command and "all" otherwise.
//   - opts.SrcOnly == true:  zip the rockspec only into
//     <name>-<ver>.src.rock. (Source download lives outside the rockspec;
//     upstream pulls source per `source.url` and inlines it. We omit that
//     until a real packaging need surfaces — fail loud over over-engineering.)
func (e *nativeEngine) Pack(ctx context.Context, target string, opts PackOpts) (string, error) {
	_ = ctx

	t, err := tree.Open(e.cfg)
	if err != nil {
		return "", err
	}

	m, err := e.store.ReadTree(t.RocksDir())
	if err != nil {
		return "", err
	}

	versions, ok := m.Repository[target]
	if !ok {
		return "", fmt.Errorf("rocks.Pack: %q not installed in %s", target, t.Tree)
	}

	picked := newestVersion(versions)

	if !opts.SrcOnly {
		return e.packInstalled(t, target, picked)
	}

	outPath := filepath.Join(e.cfg.WorkingDir, target+"-"+picked+".src.rock")
	specPath := filepath.Join(t.InstallDir(target, picked), target+"-"+picked+".rockspec")

	if err := zipSingleFile(outPath, specPath, target+"-"+picked+".rockspec"); err != nil {
		return "", fmt.Errorf("rocks.Pack: zip rockspec: %w", err)
	}

	return outPath, nil
}

// packInstalled packs name/version, installed in t, into a binary rock in
// e.cfg.WorkingDir, as upstream pack.pack_installed_rock does (pack.lua:83-133):
// the contents of the rock's install dir, plus the lua and lib files its
// rock_manifest lists, copied back from the deploy dirs (copy_back_files), into
// <name>-<version>.<arch>.rock, where arch is the host's when the rock has a
// lib file or a command that is an actual binary (repos.has_binaries), and
// "all" otherwise. That is the layout a binary rock is installed from, so the
// rock installs as built on either backend.
func (e *nativeEngine) packInstalled(t *tree.Tree, name, version string) (string, error) {
	installDir := t.InstallDir(name, version)
	if _, err := os.Stat(installDir); err != nil {
		return "", fmt.Errorf("rocks.Pack: %s %s does not seem to be an installed rock: %w", name, version, err)
	}

	rm, err := t.Store.ReadRock(filepath.Join(installDir, "rock_manifest"))
	if err != nil {
		return "", fmt.Errorf("rocks.Pack: %w", err)
	}

	stage, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-pack-*")
	if err != nil {
		return "", fmt.Errorf("rocks.Pack: mkdir tmp: %w", err)
	}

	defer func() { _ = os.RemoveAll(stage) }()

	if err := copyDirContents(installDir, stage); err != nil {
		return "", fmt.Errorf("rocks.Pack: copy %s: %w", installDir, err)
	}

	for _, sec := range []struct {
		files     map[string]string
		deployDir string
		sub       string
		mode      os.FileMode
	}{
		{rm.Lib, t.DeployLibDir(), "lib", packExecMode},
		{rm.Lua, t.DeployLuaDir(), "lua", packReadMode},
	} {
		if err := copyBackFiles(sec.files, sec.deployDir, filepath.Join(stage, sec.sub), name, version, sec.mode); err != nil {
			return "", fmt.Errorf("rocks.Pack: failed copying back files: %w", err)
		}
	}

	arch := "all"
	if len(rm.Lib) > 0 || hasBinaries(t, rm) {
		arch = remote.HostArch()
	}

	outPath := filepath.Join(e.cfg.WorkingDir, name+"-"+version+"."+arch+".rock")
	_ = os.Remove(outPath)

	if err := zipDir(outPath, stage); err != nil {
		return "", fmt.Errorf("rocks.Pack: zip %s: %w", outPath, err)
	}

	return outPath, nil
}

// Modes of the files Pack copies back from the deploy dirs, upstream's "exec"
// and "read" permissions (fs.copy in copy_back_files).
const (
	packExecMode os.FileMode = 0o755
	packReadMode os.FileMode = 0o644
)

// copyBackFiles mirrors upstream copy_back_files (pack.lua:61-79): it copies
// each file of a rock_manifest section, keyed by its path under deployDir,
// into packDir, from the versioned name the rock's file was demoted to when
// there is one (path.versioned_name) and from its plain path otherwise.
//
// One deliberate departure: upstream's recursion into a subdirectory passes
// the subdirectory as the deploy dir, so for "foo/bar.lua" it looks for
// foo/<name>_<version>-bar.lua, which no deploy ever writes — deploy_files
// demotes it to <name>_<version>-foo/bar.lua — and packs the plain foo/bar.lua,
// another rock's file, instead. The versioned name is formed here the way the
// deploy formed it.
func copyBackFiles(files map[string]string, deployDir, packDir, name, version string, mode os.FileMode) error {
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		source := filepath.Join(deployDir, filepath.FromSlash(rel))

		if versioned := tree.MungedPath(deployDir, source, name, version); fileExists(versioned) {
			source = versioned
		}

		if err := copyFileMode(source, filepath.Join(packDir, filepath.FromSlash(rel)), mode); err != nil {
			return err
		}
	}

	return nil
}

// hasBinaries mirrors upstream repos.has_binaries (repos.lua:151-165): whether
// one of the entries of rm's bin section is an actual binary at its deploy
// path — neither a .lua file nor a file starting with "#!" — as
// fs.is_actual_binary (fs/unix.lua:140-155) tells, which takes a file it
// cannot read for one. Like upstream, it looks at the top-level entries of
// the section: a command in a subdirectory, such as tools/nested, makes the
// entry the directory tools, which reads as nothing and so counts as a binary.
func hasBinaries(t *tree.Tree, rm *rocks.RockManifest) bool {
	for rel := range rm.Bin {
		top, _, _ := strings.Cut(rel, "/")
		if isActualBinary(filepath.Join(t.BinDir(), top)) {
			return true
		}
	}

	return false
}

// isActualBinary is upstream fs.is_actual_binary (fs/unix.lua:140-155).
func isActualBinary(p string) bool {
	if strings.HasSuffix(p, ".lua") {
		return false
	}

	f, err := os.Open(p)
	if err != nil {
		return true
	}

	defer func() { _ = f.Close() }()

	first := make([]byte, len(shebang))
	n, _ := io.ReadFull(f, first)

	return n < len(shebang) || string(first) != shebang
}

// shebang starts a script fs.is_actual_binary does not take for a binary.
const shebang = "#!"

func fileExists(p string) bool {
	_, err := os.Stat(p)

	return err == nil
}

// copyFileMode copies the file src to dst, creating dst's directory, with
// mode as dst's permissions.
func copyFileMode(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), unpackDirMode); err != nil {
		return err
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()

		return err
	}

	if err := out.Close(); err != nil {
		return err
	}

	return os.Chmod(dst, mode)
}

// copyDirContents copies every file under src to the same path under dst,
// keeping its permissions, as upstream fs.copy_contents does.
func copyDirContents(src, dst string) error {
	return filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}

		if fi.IsDir() {
			return os.MkdirAll(filepath.Join(dst, rel), unpackDirMode)
		}

		return copyFileMode(p, filepath.Join(dst, rel), fi.Mode().Perm())
	})
}

// Unpack extracts `archive` (a .rock or .src.rock zip) into destDir.
func (e *nativeEngine) Unpack(ctx context.Context, archive, destDir string) error {
	_ = ctx

	if err := os.MkdirAll(destDir, unpackDirMode); err != nil {
		return fmt.Errorf("rocks.Unpack: mkdir: %w", err)
	}

	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("rocks.Unpack: open %s: %w", archive, err)
	}

	defer func() { _ = zr.Close() }()

	cleanDest := filepath.Clean(destDir)

	for _, f := range zr.File {
		target := filepath.Join(destDir, f.Name)
		if !strings.HasPrefix(target, cleanDest+string(os.PathSeparator)) && target != cleanDest {
			return fmt.Errorf("rocks.Unpack: entry %q escapes destDir", f.Name)
		}

		if f.FileInfo().IsDir() {
			err := os.MkdirAll(target, unpackDirMode)
			if err != nil {
				return err
			}

			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), unpackDirMode); err != nil {
			return err
		}

		rc, err := f.Open()
		if err != nil {
			return err
		}

		out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			_ = rc.Close()

			return err
		}

		if _, err := io.Copy(out, rc); err != nil {
			_ = rc.Close()
			_ = out.Close()

			return err
		}

		_ = rc.Close()

		if err := out.Close(); err != nil {
			return err
		}
	}

	return nil
}

// --- internal helpers (used only by the five native write operations) ---

func (e *nativeEngine) installStep(ctx context.Context, fetched *artifacts, step rocks.InstallStep) error {
	// deps.Resolve populates step.Rockspec via the spec fetcher; reuse it rather
	// than fetching and evaluating the same version-pinned artifact a second
	// time. It is nil only if a caller resolved without the fetcher.
	if step.Rockspec == nil {
		spec, err := e.fetchAndEval(ctx, fetched, step.URL)
		if err != nil {
			return err
		}

		step.Rockspec = spec
	}

	return e.installArtifact(ctx, fetched, step)
}

// artifacts keeps the registry artifacts one Install fetched, each unpacked
// under a directory removed when the Install returns. Resolving fetches every
// chosen rock to read its rockspec; keeping what was unpacked lets the install
// step deploy a .rock from it instead of fetching it a second time.
type artifacts struct {
	root string
	dirs map[string]string
}

// newArtifacts creates the directory an Install unpacks its artifacts under.
func (e *nativeEngine) newArtifacts() (*artifacts, error) {
	root, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-fetch-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}

	return &artifacts{root: root, dirs: map[string]string{}}, nil
}

// remove deletes every artifact the Install fetched.
func (a *artifacts) remove() {
	_ = os.RemoveAll(a.root)
}

// fetchArtifact returns the directory the registry artifact at urlStr is
// unpacked in, fetching it on first use. A .rock or .src.rock is a zip, which
// the fetch unpacks; a bare .rockspec is the only file of its directory.
func (e *nativeEngine) fetchArtifact(ctx context.Context, fetched *artifacts, urlStr string) (string, error) {
	if dir, ok := fetched.dirs[urlStr]; ok {
		return dir, nil
	}

	tmp, err := os.MkdirTemp(fetched.root, "artifact-*")
	if err != nil {
		return "", fmt.Errorf("mkdir tmp: %w", err)
	}

	dir, err := fetch.FetchWith(ctx, urlStr, tmp, fetch.Options{
		InsecureServers: e.cfg.InsecureServers,
	})
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", urlStr, err)
	}

	fetched.dirs[urlStr] = dir

	return dir, nil
}

// fetchAndEval fetches the resolver-produced rock/rockspec URL, locates the
// `.rockspec` it contains (a bare .rockspec, or the one every .rock and
// .src.rock bundles), and evaluates it. The unpacked artifact stays in fetched
// for installArtifact.
func (e *nativeEngine) fetchAndEval(ctx context.Context, fetched *artifacts, urlStr string) (*rocks.Rockspec, error) {
	dir, err := e.fetchArtifact(ctx, fetched, urlStr)
	if err != nil {
		return nil, err
	}

	specPath, err := findRockspecIn(dir)
	if err != nil {
		return nil, err
	}

	return evalAndPrepare(specPath, e.cfg)
}

// artifactKind is what a registry artifact holds, which decides how it
// installs.
type artifactKind int

const (
	// rockspecArtifact is a bare .rockspec: the sources are at source.url.
	rockspecArtifact artifactKind = iota
	// sourceRockArtifact is a .src.rock: the rockspec and the sources it was
	// packed with.
	sourceRockArtifact
	// binaryRockArtifact is a .all.rock or a .<arch>.rock: built files laid
	// out as lua/, lib/ and bin/, ready to deploy.
	binaryRockArtifact
)

// kindOf tells the artifact kind from the file name the index built the URL
// with (path.make_url: <name>-<version>.<arch>.rock, or .rockspec).
func kindOf(urlStr string) artifactKind {
	lower := strings.ToLower(urlStr)

	switch {
	case strings.HasSuffix(lower, ".src.rock"):
		return sourceRockArtifact
	case strings.HasSuffix(lower, ".rock"):
		return binaryRockArtifact
	default:
		return rockspecArtifact
	}
}

// installArtifact installs step from the artifact the index chose for it, as
// upstream install does: a binary rock is deployed as built
// (install_binary_rock), a source rock is built from the sources it bundles
// (build.build_rock), and only a bare rockspec sends the build to source.url.
// So a directory server mirroring .rock files installs with no network.
func (e *nativeEngine) installArtifact(ctx context.Context, fetched *artifacts, step rocks.InstallStep) error {
	switch kindOf(step.URL) {
	case binaryRockArtifact:
		dir, err := e.fetchArtifact(ctx, fetched, step.URL)
		if err != nil {
			return err
		}

		return e.deployBinary(step.Rockspec, dir)
	case sourceRockArtifact:
		dir, err := e.fetchArtifact(ctx, fetched, step.URL)
		if err != nil {
			return err
		}

		srcDir, err := bundledSource(ctx, step.Rockspec, dir)
		if err != nil {
			return err
		}

		return e.deployFromSource(ctx, step.Rockspec, srcDir)
	case rockspecArtifact:
	}

	return e.installFromSource(ctx, step)
}

// bundledSource returns the source root of a .src.rock unpacked into dir.
// LuaRocks packs a git checkout as the directory it was cloned into, and an
// archive source as the archive itself, which build.build_rockspec unpacks
// before entering source.dir; findSourceBaseDir then picks the root the same
// way it does for fetched sources.
func bundledSource(ctx context.Context, spec *rocks.Rockspec, dir string) (string, error) {
	file := spec.Source.File
	if file == "" && spec.Source.URL != "" {
		file = filepath.Base(spec.Source.URL)
	}

	if file != "" {
		archive := filepath.Join(dir, file)

		if fi, err := os.Stat(archive); err == nil && fi.Mode().IsRegular() {
			res, err := fetch.Sources(ctx, "file://"+archive, filepath.Join(dir, "sources"), fetch.Options{
				File: file,
				MD5:  spec.Source.MD5,
			})
			if err != nil {
				return "", fmt.Errorf("unpack bundled %s: %w", file, err)
			}

			return findSourceBaseDir(res.Path, spec)
		}
	}

	return findSourceBaseDir(dir, spec)
}

// installFromSource fetches the rockspec's declared source (spec.Source.URL)
// into a fresh build directory and builds+deploys against THAT — never
// against the rockspec download directory, which holds only the registry
// artifact. This mirrors nativeEngine.Build and upstream luarocks'
// fetch_sources → build flow.
func (e *nativeEngine) installFromSource(ctx context.Context, step rocks.InstallStep) error {
	spec := step.Rockspec

	tmp, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-src-*")
	if err != nil {
		return fmt.Errorf("mkdir tmp: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	srcDir, err := e.fetchSource(ctx, spec, tmp)
	if err != nil {
		return err
	}

	return e.deployFromSource(ctx, spec, srcDir)
}

// fetchSource downloads spec.Source.URL into tmp and returns the source
// root to build against.
//
// Whether that root is the fetched path itself is the backend's answer, not a
// guess made from the URL: a git clone and a copied local tree ARE the source
// root, while an unpacked archive usually keeps the real source one level down
// in a single top-level subdirectory (e.g. inspect.lua-3.1.3/) that
// findSourceBaseDir has to find, mirroring upstream fetch.find_base_dir.
// Descending into a path that is already the root picks the wrong directory
// whenever the tree contains a subdirectory named like the archive would be —
// tarantool/checks clones to checks/ and holds a checks/ of its own, so the
// build used to look for CMakeLists.txt in checks/checks and fail.
func (e *nativeEngine) fetchSource(ctx context.Context, spec *rocks.Rockspec, tmp string) (string, error) {
	res, err := fetch.Sources(ctx, spec.Source.URL, tmp, fetch.Options{
		InsecureServers: e.cfg.InsecureServers,
		Tag:             spec.Source.Tag,
		Branch:          spec.Source.Branch,
		File:            spec.Source.File,
		MD5:             spec.Source.MD5,
		Version:         spec.Version,
		IdentifierOut:   &spec.Source.Identifier,
	})
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", spec.Source.URL, err)
	}

	if res.SourceRoot {
		return res.Path, nil
	}

	return findSourceBaseDir(res.Path, spec)
}

// findSourceBaseDir mirrors upstream luarocks fetch.find_base_dir: pick the
// directory the rock's source actually lives in after unpacking.
//
//   - If source.dir is set and names an existing subdirectory, use it
//     (explicit override from the rockspec).
//   - Else, if dir contains exactly one entry and it is a directory, descend
//     into it (the common single-versioned-subdir tarball layout).
//   - Else, use dir as-is (flat layout: a bare module file, or a git/file
//     checkout whose files already sit at the top level).
func findSourceBaseDir(dir string, spec *rocks.Rockspec) (string, error) {
	// (1) explicit source.dir wins when it names an existing subdir.
	if spec.Source.Dir != "" {
		cand := filepath.Join(dir, spec.Source.Dir)
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand, nil
		}
	}

	// (2) the dir inferred from the archive name (deduce_base_dir), when it
	// exists — this is what upstream fetch.find_base_dir prefers, so a tarball
	// unpacking to foo-1.0/ plus stray top-level files still descends into
	// foo-1.0 (glr-qht).
	if src := spec.Source.File; src != "" || spec.Source.URL != "" {
		if src == "" {
			src = spec.Source.URL
		}

		if inferred := deduceBaseDir(src); inferred != "" {
			cand := filepath.Join(dir, inferred)
			if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
				return cand, nil
			}
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read source dir %s: %w", dir, err)
	}

	// (3) the common single-versioned-subdir tarball layout: descend into the
	// lone directory. We deliberately do NOT fall back to the first-sorted
	// subdir for multi-entry roots — the native pipeline also unpacks .src.rock
	// sources flat (files plus .github/, src/, … at the root), where the root
	// itself is the base dir and descending would resolve modules wrongly.
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name()), nil
	}

	// (4) flat layout: files already at the top level.
	return dir, nil
}

// deduceBaseDir infers the directory an archive unpacks into from its name,
// mirroring upstream dir.deduce_base_dir (dir.lua:38-46): strip a trailing
// known archive extension ({zip,git,tgz,tar,gz,bz2}) then any ".tar".
//
// "git" is in that extension set because upstream's own list has it, and it is
// kept deliberately. Turning "checks.git" into "checks" is what made this
// function descend into a git checkout's own checks/ subdirectory, but the
// defect was the call: an archive heuristic was being run over a clone. With
// fetch.Result.SourceRoot answering that question no git URL reaches here any
// more, and upstream needs the entry for the same reason it needs the rest —
// its rockspecs.lua:135 deduces source.dir from any URL, git ones included,
// where it names the clone directory beside the store dir rather than a
// directory inside it. Dropping the entry would make the Go helper disagree
// with the upstream function it mirrors while fixing nothing.
func deduceBaseDir(url string) string {
	base := filepath.Base(url)

	known := map[string]bool{"zip": true, "git": true, "tgz": true, "tar": true, "gz": true, "bz2": true}
	if i := strings.LastIndex(base, "."); i >= 0 && known[base[i+1:]] {
		base = base[:i]
	}

	return strings.ReplaceAll(base, ".tar", "")
}

// checkSupportedPlatforms mirrors upstream deps.check_supported_platforms
// (deps.lua:247-273): a rockspec's supported_platforms list constrains where it
// may install. A "!plat" entry that matches the current platform set rejects
// the install; if there are any positive entries and none matches, it is also
// rejected. An empty list imposes no constraint.
func checkSupportedPlatforms(spec *rocks.Rockspec, plats []string) error {
	if len(spec.SupportedPlatforms) == 0 {
		return nil
	}

	isPlatform := func(name string) bool {
		return slices.Contains(plats, name)
	}

	allNegative := true
	supported := false

	for _, entry := range spec.SupportedPlatforms {
		if name, ok := strings.CutPrefix(entry, "!"); ok {
			if isPlatform(name) {
				return fmt.Errorf("%w: rockspec for %s does not support %s platforms",
					rocks.ErrUnsupportedRockspecFeature, spec.Package, name)
			}

			continue
		}

		allNegative = false

		if isPlatform(entry) {
			supported = true

			break
		}
	}

	if !supported && !allNegative {
		return fmt.Errorf("%w: rockspec for %s does not support the current platform",
			rocks.ErrUnsupportedRockspecFeature, spec.Package)
	}

	return nil
}

// wrapBinScripts mirrors repos.should_wrap_bin_scripts: wrap command scripts
// unless the rockspec explicitly sets deploy.wrap_bin_scripts = false.
func wrapBinScripts(spec *rocks.Rockspec) bool {
	if spec.Deploy.WrapBinScripts != nil {
		return *spec.Deploy.WrapBinScripts
	}

	return true
}

// installedLookup returns a deps.InstalledLookup over the current tree
// manifest's repository — the installed versions of a rock name — so the
// resolver can skip dependencies already satisfied by an installed rock.
func (e *nativeEngine) installedLookup() (deps.InstalledLookup, error) {
	t, err := tree.Open(e.cfg)
	if err != nil {
		return nil, err
	}

	m, err := e.readTreeManifest(t)
	if err != nil {
		return nil, err
	}

	return func(name string) []rocks.Version {
		var out []rocks.Version

		for verStr := range m.Repository[name] {
			if v, verr := deps.ParseVersion(verStr); verr == nil {
				out = append(out, v)
			}
		}

		return out
	}, nil
}

// readTreeManifest loads the tree-level manifest, returning a fresh (empty)
// one with initialized maps when none exists yet.
func (e *nativeEngine) readTreeManifest(t *tree.Tree) (*rocks.Manifest, error) {
	m, err := e.store.ReadTree(t.RocksDir())
	if err != nil {
		if !os.IsNotExist(unwrapPathErr(err)) {
			return nil, fmt.Errorf("read tree manifest: %w", err)
		}

		m = &rocks.Manifest{}
	}

	if m.Repository == nil {
		m.Repository = map[string]map[string]rocks.RepoEntry{}
	}

	if m.Modules == nil {
		m.Modules = map[string][]string{}
	}

	if m.Commands == nil {
		m.Commands = map[string][]string{}
	}

	if m.Dependencies == nil {
		m.Dependencies = map[string]map[string][]rocks.Dep{}
	}

	return m, nil
}

// deployFromSource builds spec from the sources in srcDir and deploys the
// result.
func (e *nativeEngine) deployFromSource(ctx context.Context, spec *rocks.Rockspec, srcDir string) error {
	return e.deployRock(spec, func(t *tree.Tree) (*rocks.RockManifest, error) {
		destDir := filepath.Join(t.InstallDir(spec.Package, spec.Version), "build")
		if err := os.MkdirAll(destDir, unpackDirMode); err != nil {
			return nil, fmt.Errorf("mkdir destDir: %w", err)
		}

		if err := build.RunBackend(ctx, spec, srcDir, destDir, e.cfg); err != nil {
			return nil, err
		}
		// srcDir holds original .lua / install / copy_dirs files; destDir holds
		// compiled .so artifacts from the build phase. Deploy reads from both.
		rm, err := t.Deploy(spec, srcDir, destDir)
		if err != nil {
			return nil, fmt.Errorf("deploy: %w", err)
		}

		return rm, nil
	})
}

// deployBinary deploys the binary rock of spec unpacked into rockDir without
// building it, as upstream install_binary_rock does.
func (e *nativeEngine) deployBinary(spec *rocks.Rockspec, rockDir string) error {
	deploySpec, err := binarySpec(spec, rockDir)
	if err != nil {
		return err
	}

	return e.deployRock(deploySpec, func(t *tree.Tree) (*rocks.RockManifest, error) {
		// The lua/ and lib/ trees of the rock are what tree.Deploy relocates
		// from a build directory, so the rock directory serves as both.
		rm, err := t.Deploy(deploySpec, rockDir, rockDir)
		if err != nil {
			return nil, fmt.Errorf("deploy: %w", err)
		}

		return rm, nil
	})
}

// binarySpec returns the spec to deploy a binary rock unpacked into rockDir
// with: its own rockspec without the build it was made with, since its files
// are built already, plus a build.install.bin entry per bin/ script, so the
// scripts get the wrappers a built rock's get, and every other top-level
// directory (doc/, conf/, …) as a copy_directories entry, since upstream
// unpacks the whole rock into the per-rock install dir.
func binarySpec(spec *rocks.Rockspec, rockDir string) (*rocks.Rockspec, error) {
	entries, err := os.ReadDir(rockDir)
	if err != nil {
		return nil, fmt.Errorf("read rock dir %s: %w", rockDir, err)
	}

	deploySpec := *spec
	deploySpec.Build = rocks.Build{}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		switch entry.Name() {
		case "lua", "lib":
		case "bin":
			bins, err := os.ReadDir(filepath.Join(rockDir, "bin"))
			if err != nil {
				return nil, fmt.Errorf("read rock bin dir: %w", err)
			}

			deploySpec.Build.Install.Bin = map[string]string{}

			for _, bin := range bins {
				if !bin.IsDir() {
					deploySpec.Build.Install.Bin[bin.Name()] = filepath.Join("bin", bin.Name())
				}
			}
		default:
			deploySpec.Build.CopyDirectories = append(deploySpec.Build.CopyDirectories, entry.Name())
		}
	}

	return &deploySpec, nil
}

// deployRock deploys one rock into the tree through deploy and records it in
// the tree manifest.
func (e *nativeEngine) deployRock(
	spec *rocks.Rockspec, deploy func(t *tree.Tree) (*rocks.RockManifest, error),
) error {
	// Abort before build/deploy when the rockspec excludes the current platform
	// (upstream deps.check_supported_platforms, deps.lua:247-273).
	if err := checkSupportedPlatforms(spec, rockspec.RuntimePlatforms()); err != nil {
		return err
	}

	t, err := tree.Open(e.cfg)
	if err != nil {
		return err
	}

	// Read the current tree manifest BEFORE deploying so Deploy can consult the
	// providers of each module/command (check_spot_if_available) — this drives
	// version promotion on reinstall/upgrade (glr-5e9).
	m, err := e.readTreeManifest(t)
	if err != nil {
		return err
	}

	t.Providers = manifestProviders(m)

	// A version already installed is deleted first, as upstream build_rockspec
	// and install_binary_rock do (build.lua:393-395, install.lua:86-88): the
	// rebuilt rock starts from a tree without it, so a file the new build no
	// longer ships does not linger, and the old copy is not mistaken for another
	// provider of the new one's files.
	if _, err := os.Stat(t.InstallDir(spec.Package, spec.Version)); err == nil {
		if err := e.deleteVersion(t, m, spec.Package, spec.Version); err != nil {
			return err
		}

		if err := e.store.WriteTree(t.RocksDir(), m); err != nil {
			return fmt.Errorf("write tree manifest: %w", err)
		}
	}

	// Match upstream's command-wrapper deploy (repos.deploy_files →
	// fs.wrap_script): the interpreter path is LUA_BINDIR/lua_interpreter
	// (prefix/bin/tarantool), and LUAROCKS_SYSCONFDIR mirrors cfg.sysconfdir.
	// repos.should_wrap_bin_scripts wraps unless the rockspec sets
	// deploy.wrap_bin_scripts = false; when it does, leave BinWrap nil so
	// Deploy copies scripts verbatim. (Upstream also honors a cfg-level
	// wrap_bin_scripts override, which the hardcoded Tarantool cfg lacks.) The
	// wrapper's package.path lists Config.Tree alone.
	if wrapBinScripts(spec) {
		t.BinWrap = &tree.BinWrap{
			Interpreter: filepath.Join(e.cfg.Tarantool.Prefix, "bin", luaInterpreter),
			Sysconfdir:  sysconfDir(),
		}
	}

	rm, err := deploy(t)
	if err != nil {
		return err
	}

	rockManifestPath := filepath.Join(t.InstallDir(spec.Package, spec.Version), "rock_manifest")
	if err := e.store.WriteRock(rockManifestPath, rm); err != nil {
		return fmt.Errorf("write rock_manifest: %w", err)
	}

	if m.Repository[spec.Package] == nil {
		m.Repository[spec.Package] = map[string]rocks.RepoEntry{}
	}
	// Build the per-arch entry's modules/commands index from what tree.Deploy
	// actually wrote (rm.Lua, rm.Lib, rm.Bin) — NOT from spec.Build.Modules,
	// which only the builtin backend populates: a cmake, make or command rock
	// declares no modules, so a rockspec-keyed loop indexed nothing for it and
	// left `modules = {}` in the tree manifest. tree.ModuleIndex is the same
	// inversion upstream's repos.package_modules performs over the rock_manifest,
	// and it subsumes the rockspec loop on builtin rocks (Deploy writes each
	// declared module at exactly the slashed path the module name maps to).
	//
	// Every provider is listed, whether it holds an item's plain path or a
	// versioned one, in upstream's order (addProvider), which is the order of
	// the spots Deploy gave them.
	entry := rocks.RepoEntry{Arch: "installed"}
	pkgVer := spec.Package + "/" + spec.Version

	if mods := tree.ModuleIndex(rm); len(mods) > 0 {
		entry.Modules = mods
		for modName := range mods {
			m.Modules[modName] = addProvider(m.Modules[modName], pkgVer)
		}
	}

	// The commands index likewise comes from what Deploy wrote (rm.Bin), as
	// upstream repos.package_commands reads it: keyed and valued by the path
	// under the bin dir, not by the rockspec's build.install.bin source paths.
	if cmds := tree.CommandIndex(rm); len(cmds) > 0 {
		entry.Commands = cmds
		for cmd := range cmds {
			m.Commands[cmd] = addProvider(m.Commands[cmd], pkgVer)
		}
	}

	// Dependencies (glr-6y6): upstream update_dependencies records both the
	// rock's own declared dependency list (top-level manifest.dependencies) and
	// the resolved name→version map of its installed deps (per-entry
	// repo.dependencies). Both are always present — {} for a dep-less rock.
	if m.Dependencies[spec.Package] == nil {
		m.Dependencies[spec.Package] = map[string][]rocks.Dep{}
	}

	m.Dependencies[spec.Package][spec.Version] = spec.Dependencies

	entry.Dependencies = resolveInstalledDeps(spec, m)

	m.Repository[spec.Package][spec.Version] = entry
	if err := e.store.WriteTree(t.RocksDir(), m); err != nil {
		return fmt.Errorf("write tree manifest: %w", err)
	}

	return nil
}

// deleteVersion undeploys name/version from the tree and drops it from m, as
// upstream repos.delete_version does: tree.DeleteVersion consults m, which
// still lists the version, to tell which of its files hold plain paths and
// which provider takes each over, and the version leaves m afterwards. The
// caller writes m.
func (e *nativeEngine) deleteVersion(t *tree.Tree, m *rocks.Manifest, name, version string) error {
	t.Providers = manifestProviders(m)

	if err := t.DeleteVersion(name, version); err != nil {
		return err
	}

	removeFromManifest(m, name, version)

	return nil
}

// Remove deletes the requested version (or every installed version when
// opts.Version is empty) from the on-disk tree, drops its module/command
// providers from the manifest, and rewrites the tree manifest. Each version
// goes through deleteVersion, so the plain paths it held pass to the next
// provider.
//
// It removes what it is asked to without checking whether another installed
// rock depends on it, so opts.Force and opts.ForceFast change nothing here;
// they reach upstream `luarocks remove` on the lua backend, which refuses to
// break a dependent without them. The caller owns the dependency graph, as
// tt, which prunes a tree it resolved itself, does.
func (e *nativeEngine) Remove(_ context.Context, name string, opts RemoveOpts) error {
	t, err := tree.Open(e.cfg)
	if err != nil {
		return err
	}

	m, err := e.readTreeManifest(t)
	if err != nil {
		return err
	}

	versions := m.Repository[name]
	if len(versions) == 0 {
		return fmt.Errorf("rocks.Remove: %s is not installed", name)
	}

	// Determine which versions to remove: a specific one, or all.
	var toRemove []string

	if opts.Version != "" {
		if _, ok := versions[opts.Version]; !ok {
			return fmt.Errorf("rocks.Remove: %s %s is not installed", name, opts.Version)
		}

		toRemove = []string{opts.Version}
	} else {
		for v := range versions {
			toRemove = append(toRemove, v)
		}

		slices.Sort(toRemove)
	}

	for _, v := range toRemove {
		if err := e.deleteVersion(t, m, name, v); err != nil {
			return fmt.Errorf("rocks.Remove: %s %s: %w", name, v, err)
		}
	}

	if err := e.store.WriteTree(t.RocksDir(), m); err != nil {
		return fmt.Errorf("rocks.Remove: write tree manifest: %w", err)
	}

	return nil
}

// --- unimplemented operations: loud ErrNotImplemented ---

func (e *nativeEngine) Purge(ctx context.Context, opts PurgeOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) Lint(ctx context.Context, specPath string, opts LintOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) NewVersion(ctx context.Context, specPath string, opts NewVersionOpts) (string, error) {
	return "", rocks.ErrNotImplemented
}

func (e *nativeEngine) WriteRockspec(ctx context.Context, url string, opts WriteRockspecOpts) (string, error) {
	return "", rocks.ErrNotImplemented
}

func (e *nativeEngine) Doc(ctx context.Context, name string, opts DocOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) Test(ctx context.Context, specPath string, opts TestOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) Config(ctx context.Context, opts ConfigOpts) (string, error) {
	return "", rocks.ErrNotImplemented
}

func (e *nativeEngine) Upload(ctx context.Context, specPath string, opts UploadOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) InitProject(ctx context.Context, opts InitProjectOpts) error {
	return rocks.ErrNotImplemented
}

func (e *nativeEngine) Admin(ctx context.Context, subCmd string, args []string, opts AdminOpts) error {
	return rocks.ErrNotImplemented
}
