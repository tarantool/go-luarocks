package client

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/fetch"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// nativeEngine is the pure-Go backend (BackendNative). It holds the same
// state the *Rocks methods used before the Engine extraction and retains
// EXACT behavioral compatibility with the pre-task client.Rocks: the
// five implemented method bodies and their private helpers are moved here
// verbatim, with only the receiver renamed from (r *Rocks) to (e *nativeEngine).
//
// The thirteen operations the native backend does not implement return
// rocks.ErrNotImplemented — never a silent no-op.
type nativeEngine struct {
	cfg    rocks.Config
	store  rocks.ManifestStore
	index  rocks.RemoteIndex
	logger *slog.Logger
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
//  3. Resolve transitive deps (unless DepsNone).
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
		idx = serverIndex(opts.Servers, e.cfg.InsecureServers)
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

	// Fetch + eval root rockspec so we can resolve transitive deps.
	rootSpec, err := e.fetchAndEval(ctx, root.URL)
	if err != nil {
		return fmt.Errorf("rocks.Install: fetch/eval root: %w", err)
	}

	root.Spec = rootSpec

	var plan []rocks.InstallStep

	if opts.Deps != DepsNone {
		plan, err = e.resolvePlan(ctx, rootSpec, idx)
		if err != nil {
			return fmt.Errorf("rocks.Install: resolve deps: %w", err)
		}
	}

	for _, step := range plan {
		err := e.installStep(ctx, step)
		if err != nil {
			return fmt.Errorf("rocks.Install: dep %s-%s: %w", step.Name, step.Version.Raw, err)
		}
	}

	// Finally install the requested rock itself. Its source is fetched from
	// the rockspec's source.url, exactly like every dependency step.
	rootStep := rocks.InstallStep{
		Name:     rootSpec.Package,
		Version:  root.Version,
		URL:      root.URL,
		Rockspec: rootSpec,
	}
	if err := e.installFromSource(ctx, rootStep); err != nil {
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
func (e *nativeEngine) resolvePlan(ctx context.Context, rootSpec *rocks.Rockspec, idx rocks.RemoteIndex) ([]rocks.InstallStep, error) {
	fetchSpec := func(ctx context.Context, rock rocks.VersionedRock) (*rocks.Rockspec, error) {
		return e.fetchAndEval(ctx, rock.URL)
	}

	resolveOpts := []deps.Option{deps.WithSpecFetcher(fetchSpec)}

	if e.cfg.Tarantool.Version != "" {
		if ttVer, verr := deps.ParseVersion(e.cfg.Tarantool.Version + "-1"); verr == nil {
			resolveOpts = append(resolveOpts,
				deps.WithProvided(map[string]rocks.Version{"tarantool": ttVer}))
		}
	}

	// Tarantool is single-tree, so the deps_mode tree selection reduces to
	// "consult this tree" for every mode except none (which never reaches here).
	if lookup, lerr := e.installedLookup(); lerr == nil {
		resolveOpts = append(resolveOpts, deps.WithInstalled(lookup))
	}

	return deps.Resolve(ctx, rootSpec, idx, resolveOpts...)
}

// Build evaluates the rockspec at specPath, fetches its declared source,
// runs the build backend, and deploys the result into e.cfg.Tree.
//
// Unlike Install, Build does not perform dependency resolution — it
// assumes prerequisites are already present (matching upstream
// `luarocks build`).
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
// developer-iteration form of Build.
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
// e.cfg.WorkingDir and returns its path.
//
//   - opts.SrcOnly == false: zip the installed tree at
//     <tree>/share/tarantool/rocks/<name>/<version>/ into <name>-<ver>.rock.
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

	var picked string
	for v := range versions {
		if picked == "" || v < picked {
			picked = v
		}
	}

	installDir := t.InstallDir(target, picked)

	suffix := ".rock"
	if opts.SrcOnly {
		suffix = ".src.rock"
	}

	outPath := filepath.Join(e.cfg.WorkingDir, target+"-"+picked+suffix)

	if opts.SrcOnly {
		specPath := filepath.Join(installDir, target+"-"+picked+".rockspec")

		err := zipSingleFile(outPath, specPath, target+"-"+picked+".rockspec")
		if err != nil {
			return "", fmt.Errorf("rocks.Pack: zip rockspec: %w", err)
		}
	} else {
		err := zipDir(outPath, installDir)
		if err != nil {
			return "", fmt.Errorf("rocks.Pack: zip install dir: %w", err)
		}
	}

	return outPath, nil
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

func (e *nativeEngine) installStep(ctx context.Context, step rocks.InstallStep) error {
	// deps.Resolve populates step.Rockspec via the spec fetcher; reuse it rather
	// than fetching and evaluating the same version-pinned artifact a second
	// time. It is nil only if a caller resolved without the fetcher.
	if step.Rockspec == nil {
		spec, err := e.fetchAndEval(ctx, step.URL)
		if err != nil {
			return err
		}

		step.Rockspec = spec
	}

	return e.installFromSource(ctx, step)
}

// fetchAndEval fetches the resolver-produced rock/rockspec URL into a
// throwaway directory, locates the `.rockspec` it contains (a bare
// .rockspec, or one bundled inside a .src.rock), and evaluates it. Only the
// parsed *Rockspec is returned — the fetched directory is the registry
// artifact, NOT the rock's source tree, so it is removed before returning.
// The actual source is fetched separately from spec.Source.URL at build
// time (see installFromSource / Build), mirroring upstream luarocks which
// never builds against the rockspec download directory.
func (e *nativeEngine) fetchAndEval(ctx context.Context, urlStr string) (*rocks.Rockspec, error) {
	tmp, err := os.MkdirTemp(e.cfg.WorkingDir, "rocks-fetch-*")
	if err != nil {
		return nil, fmt.Errorf("mkdir tmp: %w", err)
	}

	defer func() { _ = os.RemoveAll(tmp) }()

	srcDir, err := fetch.FetchWith(ctx, urlStr, tmp, fetch.Options{
		InsecureServers: e.cfg.InsecureServers,
	})
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", urlStr, err)
	}

	specPath, err := findRockspecIn(srcDir)
	if err != nil {
		return nil, err
	}

	return evalAndPrepare(specPath, e.cfg)
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

func (e *nativeEngine) deployFromSource(ctx context.Context, spec *rocks.Rockspec, srcDir string) error {
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
	// active provider of each module/command (check_spot_if_available) — this
	// drives version promotion on reinstall/upgrade (glr-5e9).
	m, err := e.readTreeManifest(t)
	if err != nil {
		return err
	}

	t.Provider = manifestProvider(m)

	// Match upstream's command-wrapper deploy (repos.deploy_files →
	// fs.wrap_script): the interpreter path is LUA_BINDIR/lua_interpreter
	// (prefix/bin/tarantool), and LUAROCKS_SYSCONFDIR mirrors cfg.sysconfdir.
	// repos.should_wrap_bin_scripts wraps unless the rockspec sets
	// deploy.wrap_bin_scripts = false; when it does, leave BinWrap nil so
	// Deploy copies scripts verbatim. (Upstream also honors a cfg-level
	// wrap_bin_scripts override, which the hardcoded Tarantool cfg lacks.)
	if wrapBinScripts(spec) {
		t.BinWrap = &tree.BinWrap{
			Interpreter: filepath.Join(e.cfg.Tarantool.Prefix, "bin", luaInterpreter),
			Sysconfdir:  sysconfDir(),
		}
	}

	destDir := filepath.Join(t.InstallDir(spec.Package, spec.Version), "build")
	if err := os.MkdirAll(destDir, unpackDirMode); err != nil {
		return fmt.Errorf("mkdir destDir: %w", err)
	}

	if err := build.RunBackend(ctx, spec, srcDir, destDir, e.cfg); err != nil {
		return err
	}
	// srcDir holds original .lua / install / copy_dirs files; destDir holds
	// compiled .so artifacts from the build phase. Deploy reads from both.
	rm, err := t.Deploy(spec, srcDir, destDir)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
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
	entry := rocks.RepoEntry{Arch: "installed"}
	pkgVer := spec.Package + "/" + spec.Version

	if mods := tree.ModuleIndex(rm, spec.Package, spec.Version); len(mods) > 0 {
		entry.Modules = mods
		for modName := range mods {
			m.Modules[modName] = upsertProvider(m.Modules[modName], pkgVer, true)
		}
	}

	if len(rm.Bin) > 0 {
		entry.Commands = map[string]string{}
		for binName, srcRel := range spec.Build.Install.Bin {
			entry.Commands[binName] = srcRel
			m.Commands[binName] = upsertProvider(m.Commands[binName], pkgVer, true)
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

// Remove deletes the requested version (or every installed version when
// opts.Version is empty) from the on-disk tree, drops its module/command
// providers from the manifest, and rewrites the tree manifest.
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
	}

	for _, v := range toRemove {
		if err := t.DeleteVersion(name, v); err != nil {
			return fmt.Errorf("rocks.Remove: %s %s: %w", name, v, err)
		}

		pkgVer := name + "/" + v
		delete(m.Repository[name], v)
		removeProvider(m.Modules, pkgVer)
		removeProvider(m.Commands, pkgVer)

		if vers, ok := m.Dependencies[name]; ok {
			delete(vers, v)
		}
	}

	if len(m.Repository[name]) == 0 {
		delete(m.Repository, name)
		delete(m.Dependencies, name)
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

func (e *nativeEngine) Search(ctx context.Context, pattern string, opts SearchOpts) ([]SearchResult, error) {
	return nil, rocks.ErrNotImplemented
}

func (e *nativeEngine) Download(ctx context.Context, name string, opts DownloadOpts) (string, error) {
	return "", rocks.ErrNotImplemented
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
