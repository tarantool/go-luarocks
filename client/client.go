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

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/deps"
	"github.com/tarantool/go-luarocks/manif"
	"github.com/tarantool/go-luarocks/remote"
	"github.com/tarantool/go-luarocks/rockspec"
	"github.com/tarantool/go-luarocks/tree"
)

// Rocks is the public facade. Construct via New(Config). All operations
// take a context and read configuration from r.cfg — no hidden global
// state, no os.Getwd / os.Chdir, no os.Setenv.
//
// Write operations delegate to r.engine, which is selected once at New()
// time per r.backend and is final for the lifetime of the facade.
// Read operations (List, Show, Which, ReadTreeManifest) are served directly
// from r.store regardless of backend.
type Rocks struct {
	cfg     rocks.Config
	store   rocks.ManifestStore
	index   rocks.RemoteIndex
	logger  *slog.Logger
	backend Backend
	engine  Engine
}

// InstallOpts tunes Install.
type InstallOpts struct {
	// Version, if set, narrows the candidate set to those matching this
	// constraint (parsed via deps.ParseConstraints). Empty means "any
	// version satisfying the rockspec's transitive constraints" — i.e.
	// the resolver picks the newest.
	Version string

	// Servers overrides r.cfg.Servers for this Install. Empty means use
	// the facade's configured servers.
	Servers []string

	// Deps controls whether transitive dependencies are also installed.
	Deps DepsPolicy
}

// DepsPolicy says whether Install resolves and installs the transitive
// dependencies of a rock. The native backend reads it as its name says; the
// lua backend passes it to upstream as --deps-mode (DepsAll as "all",
// DepsNone as "none", DepsOnlyNew as "order"), which also selects the rock
// trees a dependency may be satisfied from and a command wrapper lists.
type DepsPolicy int

const (
	// DepsAll resolves and installs every transitive dependency that the
	// tree does not already satisfy. It is the zero value.
	DepsAll DepsPolicy = 0
	// DepsNone installs only the named rock; its dependencies are neither
	// resolved nor checked.
	DepsNone DepsPolicy = 1
	// DepsOnlyNew installs deps that aren't already in the tree, as DepsAll
	// does on the native backend.
	DepsOnlyNew DepsPolicy = 2
)

// BuildOpts tunes Build.
type BuildOpts struct {
	// Keep, if true, leaves the staging build directory in place after a
	// successful build for debugging. Default removes it. The lua backend
	// passes it to upstream as `luarocks build --keep`, which keeps the
	// other installed versions of the rock instead; the native Build keeps
	// them either way.
	Keep bool
}

// MakeOpts tunes Make.
type MakeOpts struct {
	// RockspecPath, if non-empty, names the rockspec to build. Empty
	// means search r.cfg.WorkingDir for exactly one `*.rockspec`.
	RockspecPath string
}

// PackOpts tunes Pack.
type PackOpts struct {
	// SrcOnly, if true, produces a `.src.rock` containing the rockspec
	// and original source archive rather than a deployable `.rock`.
	SrcOnly bool
}

// InstalledRock is re-exported from the root package for caller
// convenience (so `client.InstalledRock` and `rocks.InstalledRock` both
// resolve to the same type).
type InstalledRock = rocks.InstalledRock

// ShowInfo is re-exported from the root package — see InstalledRock.
type ShowInfo = rocks.ShowInfo

// New constructs a Rocks facade from cfg. Validates the minimum required
// fields and wires up default implementations.
//
// The only error New itself returns is a descriptive error when cfg.Tree is
// empty. Tarantool-header validation is NOT done here: New does not stat
// cfg.Tarantool.IncludeDir; ErrMissingTarantoolHeaders surfaces later from the
// operations that actually need the headers (e.g. building a C-extension rock).
//
// opts apply at construction time. The default backend is BackendNative
// (the zero value); WithBackend overrides it. Backend selection is final
// for the returned *Rocks. The existing New(cfg) call shape is
// preserved by the variadic.
func New(cfg rocks.Config, opts ...Option) (*Rocks, error) {
	if cfg.Tree == "" {
		return nil, errors.New("rocks: Config.Tree is required")
	}

	// One manifest cache for the client's life, shared by the configured
	// index and by every index a call builds for its own server list.
	manifests := &remote.ManifestCache{}

	r := &Rocks{
		cfg:    cfg,
		store:  manif.FileStore{},
		index:  serverIndex(cfg.Servers, cfg.InsecureServers, manifests),
		logger: cfg.Logger,
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}

	for _, opt := range opts {
		opt(r)
	}
	// Select the engine per r.backend. The native engine shares the same
	// cfg/store/index/logger so reads (r.store) and writes (r.engine) see
	// one consistent tree.
	native := &nativeEngine{
		cfg:       r.cfg,
		store:     r.store,
		index:     r.index,
		manifests: manifests,
		logger:    r.logger,
	}

	switch r.backend {
	case BackendLua:
		// The gopher-lua backend. newLuaEngine does not create a VM here;
		// the first write call warms one, and every call gets its own. The
		// native engine is still constructed above (reads use r.store
		// regardless of backend) — harmless if its write methods go
		// unused for the lua backend.
		r.engine = newLuaEngine(r.cfg, r.store, r.logger)
	case BackendNative:
		r.engine = native
	default:
		r.engine = native
	}

	return r, nil
}

// serverIndex builds the RemoteIndex for a configured server list. Each entry
// is dispatched by its own form — a local directory becomes a file index, an
// HTTP(S) URL an HTTP one (see remote.NewIndex) — so a rock server may be a
// directory anywhere one may be a URL, which is what `luarocks
// --only-server=/path/to/repo` allows and what an offline install needs.
//
// The list is then consulted in configuration order, first-found-wins. That is
// a change from the previous unconditional multi-server HTTPRemoteIndex, which
// merged every server's offering of a version before picking an arch (upstream
// search_repos, search.lua:26-30): a merge cannot span transports, and
// first-found-wins is already the rule tt's resolver applies over this same
// list, so the facade now agrees with its main consumer instead of differing
// from it. For a single configured server — the common case, and every case in
// this repository's fixtures — the two rules coincide exactly.
//
//nolint:ireturn // returns the RemoteIndex interface the facade stores
func serverIndex(servers, insecure []string, manifests *remote.ManifestCache) rocks.RemoteIndex {
	return remote.NewOrderedIndex(remote.NewIndexes(servers, remote.IndexOptions{
		InsecureServers: insecure,
		Cache:           manifests,
	})...)
}

// Exec runs an arbitrary LuaRocks command line — argv is everything after
// `luarocks` — through the embedded upstream dispatcher, letting LuaRocks print
// its own output to the process stdout/stderr verbatim. It is the escape hatch
// tt's `tt rocks` is built on; programmatic callers should prefer the typed
// methods (Install, Search, …), which capture and shape their output.
//
// progname is the program name LuaRocks prints in its usage/help text (e.g.
// "tt rocks"); use the library's default progname otherwise.
//
// Exec requires BackendLua: the native engine cannot run the full CLI, so on
// any other backend Exec returns rocks.ErrNotImplemented.
func (r *Rocks) Exec(ctx context.Context, progname string, argv []string) error {
	le, ok := r.engine.(*luaEngine)
	if !ok {
		return rocks.ErrNotImplemented
	}

	return le.callRaw(progname, argv)
}

// ReadTreeManifest is the method-style convenience for
// manif.FileStore.ReadTree against r.cfg.Tree. It returns the top-level
// manifest the tree currently advertises (composes the store).
func (r *Rocks) ReadTreeManifest() (*rocks.Manifest, error) {
	t, err := tree.Open(r.cfg)
	if err != nil {
		return nil, err
	}

	return r.store.ReadTree(t.RocksDir())
}

// Install installs `name` (with optional version constraint in
// opts.Version) into r.cfg.Tree, including transitive deps per
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
func (r *Rocks) Install(ctx context.Context, name string, opts InstallOpts) error {
	return r.engine.Install(ctx, name, opts)
}

// Build evaluates the rockspec at specPath, fetches its declared source,
// runs the build backend, and deploys the result into r.cfg.Tree.
//
// Unlike Install, Build on the native backend does not resolve or install
// dependencies, its build_dependencies included — it assumes the
// prerequisites are already present. On the lua backend it runs upstream
// `luarocks build`, which installs the missing ones first.
func (r *Rocks) Build(ctx context.Context, specPath string, opts BuildOpts) error {
	return r.engine.Build(ctx, specPath, opts)
}

// Make is "build the rockspec found in cwd against the source already
// present in cwd" — the upstream `luarocks make` flow. It is the
// developer-iteration form of Build, and installs dependencies on the lua
// backend only, as Build does.
func (r *Rocks) Make(ctx context.Context, opts MakeOpts) error {
	return r.engine.Make(ctx, opts)
}

// List returns every rock currently installed in r.cfg.Tree.
func (r *Rocks) List(ctx context.Context) ([]InstalledRock, error) {
	_ = ctx // local read, no I/O cancellation needed

	t, err := tree.Open(r.cfg)
	if err != nil {
		return nil, err
	}

	m, err := r.store.ReadTree(t.RocksDir())
	if err != nil {
		if os.IsNotExist(unwrapPathErr(err)) {
			return []InstalledRock{}, nil
		}

		return nil, err
	}

	out := []InstalledRock{}

	for name, versions := range m.Repository {
		for ver := range versions {
			out = append(out, InstalledRock{Name: name, Version: ver})
		}
	}

	return out, nil
}

// Show returns the summary info for a single rock installed in the tree: of
// its newest installed version, as upstream `luarocks show` picks it
// (search.pick_installed_rock). Returns an error wrapping os.ErrNotExist when
// the rock is not present.
func (r *Rocks) Show(ctx context.Context, name string) (*ShowInfo, error) {
	_ = ctx

	t, err := tree.Open(r.cfg)
	if err != nil {
		return nil, err
	}

	m, err := r.store.ReadTree(t.RocksDir())
	if err != nil {
		return nil, err
	}

	versions, ok := m.Repository[name]
	if !ok {
		return nil, fmt.Errorf("rocks.Show: %q not installed: %w", name, os.ErrNotExist)
	}

	picked := newestVersion(versions)

	out := &ShowInfo{Package: name, Version: picked}

	// Its modules by the name require takes, as upstream show lists the
	// rock_manifest's lua and lib entries through path.path_to_module
	// (cmd/show.lua modules_to_list), not by the files that provide them.
	rockManifestPath := filepath.Join(t.InstallDir(name, picked), "rock_manifest")

	rm, err := r.store.ReadRock(rockManifestPath)
	if err == nil && rm != nil {
		out.Modules = slices.Sorted(maps.Keys(tree.ModuleIndex(rm)))
	}

	// Try to read the rockspec for richer info — non-fatal if missing.
	specPath := filepath.Join(t.InstallDir(name, picked), name+"-"+picked+".rockspec")
	if spec, err := rockspec.Eval(specPath, r.cfg.Rockspec); err == nil {
		out.Summary = spec.Description.Summary
		out.License = spec.Description.License
		out.Homepage = spec.Description.Homepage
		out.Dependencies = spec.Dependencies
	}

	return out, nil
}

// Which resolves a dotted Lua module name to its on-disk file path in
// r.cfg.Tree, by the module's plain paths there (tree.Tree.Which). Returns
// (path, true, nil) on hit; ("", false, nil) on miss.
//
// It looks in r.cfg.Tree alone, whichever backend the client uses, and so
// differs from upstream `luarocks which`, which the lua engine's Exec runs:
// that asks the LuaRocks loader, over the manifests of every rock tree.
func (r *Rocks) Which(ctx context.Context, module string) (string, bool, error) {
	_ = ctx

	t, err := tree.Open(r.cfg)
	if err != nil {
		return "", false, err
	}

	p, ok := t.Which(module)

	return p, ok, nil
}

// Pack produces a .rock or .src.rock archive for `target` (rock name) in
// r.cfg.WorkingDir and returns its path. It packs the newest installed version
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
func (r *Rocks) Pack(ctx context.Context, target string, opts PackOpts) (string, error) {
	return r.engine.Pack(ctx, target, opts)
}

// Unpack extracts `archive` (a .rock or .src.rock zip) into destDir.
func (r *Rocks) Unpack(ctx context.Context, archive, destDir string) error {
	return r.engine.Unpack(ctx, archive, destDir)
}

// Remove uninstalls a rock from r.cfg.Tree (upstream `luarocks remove`). Both
// backends implement it: BackendNative deletes the requested version (or all
// versions when opts.Version is empty) from the on-disk tree and rewrites the
// tree manifest; BackendLua runs upstream `luarocks remove`.
func (r *Rocks) Remove(ctx context.Context, name string, opts RemoveOpts) error {
	return r.engine.Remove(ctx, name, opts)
}

// Search queries the configured servers for rocks matching pattern (upstream
// `luarocks search`). Both backends implement it: BackendNative walks the
// manifests itself through remote.Search, BackendLua runs upstream and parses
// the --porcelain listing. The two agree on the result set and its order; see
// nativeEngine.Search for the two documented differences at the edges (an
// empty pattern without SearchOpts.All, and how the VM-provided rocks are
// versioned).
func (r *Rocks) Search(ctx context.Context, pattern string, opts SearchOpts) ([]SearchResult, error) {
	return r.engine.Search(ctx, pattern, opts)
}

// Download fetches a rock file into r.cfg.WorkingDir and returns its path
// (upstream `luarocks download`). Both backends implement it: BackendNative
// resolves the artifact through remote.Search and retrieves it with
// fetch.File, BackendLua runs upstream and reports the file that appeared.
// The two agree on which file is downloaded and on the returned path; see
// nativeEngine.Download for the one documented difference (what a
// single-file download returns when it overwrote an existing file).
func (r *Rocks) Download(ctx context.Context, name string, opts DownloadOpts) (string, error) {
	return r.engine.Download(ctx, name, opts)
}

// --- engine-delegated operations not served by the native backend ---
//
// Each method below is a pure one-line delegation to r.engine — no
// backend-aware branching here. BackendNative returns rocks.ErrNotImplemented
// for every one of them; BackendLua runs the corresponding upstream
// LuaRocks command in the embedded VM.

// Purge removes all rocks from r.cfg.Tree (upstream `luarocks purge`).
// BackendNative returns rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) Purge(ctx context.Context, opts PurgeOpts) error {
	return r.engine.Purge(ctx, opts)
}

// Lint checks the syntax of a rockspec (upstream `luarocks lint`).
// BackendNative returns rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) Lint(ctx context.Context, specPath string, opts LintOpts) error {
	return r.engine.Lint(ctx, specPath, opts)
}

// NewVersion writes an updated rockspec for a new version and returns its path
// (upstream `luarocks new_version`). BackendNative returns
// rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) NewVersion(ctx context.Context, specPath string, opts NewVersionOpts) (string, error) {
	return r.engine.NewVersion(ctx, specPath, opts)
}

// WriteRockspec writes a starter rockspec for sources at url and returns its
// path (upstream `luarocks write_rockspec`). BackendNative returns
// rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) WriteRockspec(ctx context.Context, url string, opts WriteRockspecOpts) (string, error) {
	return r.engine.WriteRockspec(ctx, url, opts)
}

// Doc shows or lists documentation for an installed rock (upstream
// `luarocks doc`). BackendNative returns rocks.ErrNotImplemented; BackendLua
// runs upstream.
func (r *Rocks) Doc(ctx context.Context, name string, opts DocOpts) error {
	return r.engine.Doc(ctx, name, opts)
}

// Test runs a rock's test suite (upstream `luarocks test`). BackendNative
// returns rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) Test(ctx context.Context, specPath string, opts TestOpts) error {
	return r.engine.Test(ctx, specPath, opts)
}

// Config reads or writes LuaRocks configuration and returns the printed value
// (upstream `luarocks config`). BackendNative returns rocks.ErrNotImplemented;
// BackendLua runs upstream.
func (r *Rocks) Config(ctx context.Context, opts ConfigOpts) (string, error) {
	return r.engine.Config(ctx, opts)
}

// Upload publishes a rockspec to a rocks server (upstream `luarocks upload`).
// BackendNative returns rocks.ErrNotImplemented; BackendLua runs upstream.
func (r *Rocks) Upload(ctx context.Context, specPath string, opts UploadOpts) error {
	return r.engine.Upload(ctx, specPath, opts)
}

// InitProject scaffolds a LuaRocks project in r.cfg.WorkingDir (upstream
// `luarocks init`). BackendNative returns rocks.ErrNotImplemented; BackendLua
// runs upstream.
func (r *Rocks) InitProject(ctx context.Context, opts InitProjectOpts) error {
	return r.engine.InitProject(ctx, opts)
}

// Admin runs a `luarocks admin <subCmd>` repository-administration command
// (upstream `luarocks admin`). BackendNative returns rocks.ErrNotImplemented;
// BackendLua runs upstream.
func (r *Rocks) Admin(ctx context.Context, subCmd string, args []string, opts AdminOpts) error {
	return r.engine.Admin(ctx, subCmd, args, opts)
}

// --- internal helpers ---

// addProvider records provider (a "name/version" token) in a module or command
// provider list and keeps the list in upstream's order (manif/writer.lua
// sort_package_matching_table): by rock name, then newest version first, with
// no duplicates. Index 0 is then the provider holding the item's plain path,
// the one tree.Deploy gives it (repos.lua check_spot_if_available), and index 1
// the one that takes it over when index 0 is removed.
func addProvider(list []string, provider string) []string {
	out := make([]string, 0, len(list)+1)
	for _, x := range list {
		if x != provider {
			out = append(out, x)
		}
	}

	out = append(out, provider)
	slices.SortStableFunc(out, compareProviders)

	return out
}

// compareProviders orders provider tokens as upstream sort_pkgs does
// (manif/writer.lua:116-127): rocks by name, and versions of one rock newest
// first. Versions that do not parse compare as strings.
func compareProviders(a, b string) int {
	na, va, _ := strings.Cut(a, "/")
	nb, vb, _ := strings.Cut(b, "/")

	if na != nb {
		return strings.Compare(na, nb)
	}

	pa, errA := deps.ParseVersion(va)
	pb, errB := deps.ParseVersion(vb)

	if errA != nil || errB != nil {
		return strings.Compare(vb, va)
	}

	return deps.Compare(pb, pa)
}

// resolveInstalledDeps builds the per-entry repo.dependencies map (name →
// installed version) for a rock, mirroring upstream scan_deps' observable
// output: each of the rock's declared dependencies is resolved to a version
// already present in the tree manifest's repository. Dependencies not (yet)
// installed are omitted; the self-entry never appears. Always returns a
// non-nil map so the manifest carries `dependencies = {}` for a dep-less rock.
//
// It is computed once, when the rock is deployed, and the entries of the
// other rocks are left as they are, where upstream recomputes every entry,
// transitively, on each write of the manifest.
func resolveInstalledDeps(spec *rocks.Rockspec, m *rocks.Manifest) map[string]string {
	out := map[string]string{}

	for _, d := range spec.Dependencies {
		if d.Name == spec.Package {
			continue
		}

		versions, ok := m.Repository[d.Name]
		if !ok || len(versions) == 0 {
			continue
		}

		// Pick a deterministic installed version (highest by string order is a
		// reasonable stand-in; the resolver has already ensured one satisfies).
		best := ""
		for ver := range versions {
			if ver > best {
				best = ver
			}
		}

		out[d.Name] = best
	}

	return out
}

// removeProvider drops the "name/version" provider from every list in a
// module/command index, deleting a list that becomes empty. The first surviving
// provider (if any) remains the active one at index 0.
func removeProvider(index map[string][]string, provider string) {
	for item, list := range index {
		out := list[:0]

		for _, x := range list {
			if x != provider {
				out = append(out, x)
			}
		}

		if len(out) == 0 {
			delete(index, item)
		} else {
			index[item] = out
		}
	}
}

// manifestProviders builds the tree.Tree provider lookup over the manifest's
// module and command indexes, as upstream manif.get_providers reads them: a
// module is looked up in m.Modules and a command in m.Commands, never one in
// the other's index, since a rock may well ship a module and a command of the
// same name. The lookup reads m as it is when called, so it follows the
// manifest's updates.
func manifestProviders(m *rocks.Manifest) func(kind tree.ItemKind, item string) []string {
	return func(kind tree.ItemKind, item string) []string {
		if kind == tree.CommandItem {
			return m.Commands[item]
		}

		return m.Modules[item]
	}
}

// removeFromManifest drops name/version from the tree manifest: its
// repository entry, its place in the module and command provider lists, and
// its dependency list (upstream writer.remove_from_manifest).
func removeFromManifest(m *rocks.Manifest, name, version string) {
	pkgVer := name + "/" + version

	delete(m.Repository[name], version)
	removeProvider(m.Modules, pkgVer)
	removeProvider(m.Commands, pkgVer)

	if vers, ok := m.Dependencies[name]; ok {
		delete(vers, version)
	}

	if len(m.Repository[name]) == 0 {
		delete(m.Repository, name)
		delete(m.Dependencies, name)
	}
}

func evalAndPrepare(specPath string, cfg rocks.Config) (*rocks.Rockspec, error) {
	spec, err := rockspec.Eval(specPath, cfg.Rockspec)
	if err != nil {
		return nil, fmt.Errorf("rockspec.Eval %s: %w", specPath, err)
	}

	rockspec.MergePlatforms(spec, rockspec.RuntimePlatforms())

	if err := rockspec.Validate(spec); err != nil {
		return nil, fmt.Errorf("rockspec.Validate %s: %w", specPath, err)
	}

	return spec, nil
}

// findRockspecIn locates the rockspec of a fetched registry artifact. It
// scans ONLY the top level of dir: a bare `.rockspec` download is a single
// top-level file, and a `.src.rock` archive carries its rockspec at the
// archive root. We deliberately do NOT recurse — a .src.rock also bundles
// the rock's source tree, which may itself ship a `rockspecs/` directory of
// unrelated rockspec files (e.g. `say`), and recursing would ambiguously
// match those.
func findRockspecIn(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", dir, err)
	}

	var found string

	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".rockspec") {
			continue
		}

		p := filepath.Join(dir, ent.Name())
		if found != "" {
			return "", fmt.Errorf("multiple .rockspec under %s: %s and %s", dir, found, p)
		}

		found = p
	}

	if found == "" {
		return "", fmt.Errorf("no .rockspec under %s", dir)
	}

	return found, nil
}

// pickNewest filters candidates by cs and returns the highest version.
// Mirrors deps.pickNewest (which is unexported); duplicating here keeps
// the client free of a deps-internal symbol dependency.
func pickNewest(candidates []rocks.VersionedRock, cs []rocks.VersionConstraint) (rocks.VersionedRock, bool) {
	var best rocks.VersionedRock

	have := false

	for _, c := range candidates {
		if !deps.Match(c.Version, cs) {
			continue
		}

		if !have || deps.Compare(c.Version, best.Version) > 0 {
			best = c
			have = true
		}
	}

	return best, have
}

// newestVersion returns the newest of the installed versions of a rock, the
// one upstream search.pick_installed_rock picks for show and pack
// (search.lua:395: the first of the versions sorted by vers.compare_versions,
// newest first). A version that does not parse sorts below every one that
// does, and two that do not parse compare as strings, so the choice does not
// depend on map order.
func newestVersion(versions map[string]rocks.RepoEntry) string {
	var (
		picked string
		best   rocks.Version
		bestOK bool
	)

	for raw := range versions {
		v, err := deps.ParseVersion(raw)
		ok := err == nil

		switch {
		case picked == "":
		case ok && bestOK:
			if deps.Compare(v, best) <= 0 {
				continue
			}
		case ok != bestOK:
			if !ok {
				continue
			}
		case raw < picked:
			continue
		}

		picked, best, bestOK = raw, v, ok
	}

	return picked
}

func unwrapPathErr(err error) error {
	type unwrapper interface{ Unwrap() error }

	for err != nil {
		pe := &os.PathError{}
		if errors.As(err, &pe) {
			return pe.Err
		}

		u, ok := err.(unwrapper)
		if !ok {
			return err
		}

		err = u.Unwrap()
	}

	return nil
}

// zipDir recursively zips srcDir contents into outPath. Entries are
// stored with paths relative to srcDir.
func zipDir(outPath, srcDir string) error {
	f, err := os.Create(outPath)
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	zw := zip.NewWriter(f)

	walkErr := filepath.Walk(srcDir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(srcDir, p)
		if err != nil {
			return err
		}

		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}

		in, err := os.Open(p)
		if err != nil {
			return err
		}

		defer func() { _ = in.Close() }()

		_, err = io.Copy(w, in)

		return err
	})
	if walkErr != nil {
		_ = zw.Close()

		return walkErr
	}

	return zw.Close()
}

func zipSingleFile(outPath, srcPath, entryName string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}

	defer func() { _ = in.Close() }()

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}

	defer func() { _ = out.Close() }()

	zw := zip.NewWriter(out)

	w, err := zw.Create(entryName)
	if err != nil {
		_ = zw.Close()

		return err
	}

	if _, err := io.Copy(w, in); err != nil {
		_ = zw.Close()

		return err
	}

	return zw.Close()
}
