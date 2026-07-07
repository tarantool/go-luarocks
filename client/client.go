// Package client implements the Rocks facade — the public API that drives
// LuaRocks operations. At this stage the facade is backed solely by the
// gopher-lua engine (lua.go): every write operation is dispatched into the
// embedded upstream LuaRocks 3.9.2 VM. The pure-Go native backend, and the
// native tree reads (List/Show/Which), are reimplemented in later commits;
// until then the lua backend is the single source of behavior.
//
// The root rocks package holds the shared data types and interfaces; the
// operational Rocks struct + methods live here. Callers spell it
// `client.New(cfg)`.
package client

import (
	"context"
	"errors"
	"log/slog"

	rocks "github.com/tarantool/go-luarocks"
)

// Rocks is the public facade. Construct via New(Config). All operations take
// a context and read configuration from r.cfg — no hidden global state.
//
// Write operations delegate to r.engine, which is selected once at New() time
// and is final for the lifetime of the facade.
type Rocks struct {
	cfg     rocks.Config
	logger  *slog.Logger
	backend Backend
	engine  Engine
}

// InstallOpts tunes Install.
type InstallOpts struct {
	// Version, if set, narrows the candidate set to those matching this
	// constraint. Empty means "any version satisfying the rockspec's
	// transitive constraints".
	Version string

	// Servers overrides r.cfg.Servers for this Install. Empty means use the
	// facade's configured servers.
	Servers []string

	// Deps controls whether transitive dependencies are also installed.
	Deps DepsPolicy
}

// DepsPolicy mirrors upstream's `--deps-mode` flag.
type DepsPolicy int

const (
	// DepsAll resolves and installs every transitive dependency.
	DepsAll DepsPolicy = 0
	// DepsNone installs only the named rock; missing deps cause Install to
	// fail.
	DepsNone DepsPolicy = 1
	// DepsOnlyNew installs deps that aren't already in the tree.
	DepsOnlyNew DepsPolicy = 2
)

// BuildOpts tunes Build.
type BuildOpts struct {
	// Keep, if true, leaves the staging build directory in place after a
	// successful build for debugging. Default removes it.
	Keep bool
}

// MakeOpts tunes Make.
type MakeOpts struct {
	// RockspecPath, if non-empty, names the rockspec to build. Empty means
	// search r.cfg.WorkingDir for exactly one `*.rockspec`.
	RockspecPath string
}

// PackOpts tunes Pack.
type PackOpts struct {
	// SrcOnly, if true, produces a `.src.rock` containing the rockspec and
	// original source archive rather than a deployable `.rock`.
	SrcOnly bool
}

// InstalledRock is re-exported from the root package for caller convenience.
type InstalledRock = rocks.InstalledRock

// ShowInfo is re-exported from the root package — see InstalledRock.
type ShowInfo = rocks.ShowInfo

// New constructs a Rocks facade from cfg. Validates the minimum required
// fields and wires up the lua backend.
//
// The only error New itself returns is a descriptive error when cfg.Tree is
// empty. opts apply at construction time; backend selection is final for the
// returned *Rocks. At this stage only the lua backend exists — the native
// backend is introduced in a later commit — so the engine is always the
// gopher-lua engine. Boot is lazy: newLuaEngine does not touch the VM here;
// the LState is created on first write call.
func New(cfg rocks.Config, opts ...Option) (*Rocks, error) {
	if cfg.Tree == "" {
		return nil, errors.New("rocks: Config.Tree is required")
	}

	r := &Rocks{
		cfg:    cfg,
		logger: cfg.Logger,
	}
	if r.logger == nil {
		r.logger = slog.New(slog.DiscardHandler)
	}

	for _, opt := range opts {
		opt(r)
	}

	r.engine = newLuaEngine(r.cfg, nil, r.logger)

	return r, nil
}

// Exec runs an arbitrary LuaRocks command line — argv is everything after
// `luarocks` — through the embedded upstream dispatcher, letting LuaRocks
// print its own output to the process stdout/stderr verbatim. It is the
// escape hatch tt's `tt rocks` is built on; programmatic callers should prefer
// the typed methods (Install, Search, …), which capture and shape output.
//
// progname is the program name LuaRocks prints in its usage/help text.
func (r *Rocks) Exec(ctx context.Context, progname string, argv []string) error {
	_ = ctx

	le, ok := r.engine.(*luaEngine)
	if !ok {
		return rocks.ErrNotImplemented
	}

	return le.callRaw(progname, argv)
}

// Install installs `name` (with optional version constraint in opts.Version)
// into r.cfg.Tree, including transitive deps per opts.Deps.
func (r *Rocks) Install(ctx context.Context, name string, opts InstallOpts) error {
	return r.engine.Install(ctx, name, opts)
}

// Build evaluates the rockspec at specPath, fetches its declared source, runs
// the build backend, and deploys the result into r.cfg.Tree.
func (r *Rocks) Build(ctx context.Context, specPath string, opts BuildOpts) error {
	return r.engine.Build(ctx, specPath, opts)
}

// Make builds the rockspec found in cwd against the source already present in
// cwd — the upstream `luarocks make` flow.
func (r *Rocks) Make(ctx context.Context, opts MakeOpts) error {
	return r.engine.Make(ctx, opts)
}

// Pack produces a .rock or .src.rock archive for `target` (rock name) in
// r.cfg.WorkingDir and returns its path.
func (r *Rocks) Pack(ctx context.Context, target string, opts PackOpts) (string, error) {
	return r.engine.Pack(ctx, target, opts)
}

// Unpack extracts `archive` (a .rock or .src.rock zip) into destDir.
func (r *Rocks) Unpack(ctx context.Context, archive, destDir string) error {
	return r.engine.Unpack(ctx, archive, destDir)
}

// --- engine-delegated operations ---
//
// Each method below is a pure one-line delegation to r.engine. The lua
// backend runs the corresponding upstream LuaRocks command in the embedded VM.

// Remove uninstalls a rock from r.cfg.Tree (upstream `luarocks remove`).
func (r *Rocks) Remove(ctx context.Context, name string, opts RemoveOpts) error {
	return r.engine.Remove(ctx, name, opts)
}

// Purge removes all rocks from r.cfg.Tree (upstream `luarocks purge`).
func (r *Rocks) Purge(ctx context.Context, opts PurgeOpts) error {
	return r.engine.Purge(ctx, opts)
}

// Search queries the configured servers for rocks matching pattern (upstream
// `luarocks search`).
func (r *Rocks) Search(ctx context.Context, pattern string, opts SearchOpts) ([]SearchResult, error) {
	return r.engine.Search(ctx, pattern, opts)
}

// Download fetches a rock file into r.cfg.WorkingDir and returns its path
// (upstream `luarocks download`).
func (r *Rocks) Download(ctx context.Context, name string, opts DownloadOpts) (string, error) {
	return r.engine.Download(ctx, name, opts)
}

// Lint checks the syntax of a rockspec (upstream `luarocks lint`).
func (r *Rocks) Lint(ctx context.Context, specPath string, opts LintOpts) error {
	return r.engine.Lint(ctx, specPath, opts)
}

// NewVersion writes an updated rockspec for a new version and returns its path
// (upstream `luarocks new_version`).
func (r *Rocks) NewVersion(ctx context.Context, specPath string, opts NewVersionOpts) (string, error) {
	return r.engine.NewVersion(ctx, specPath, opts)
}

// WriteRockspec writes a starter rockspec for sources at url and returns its
// path (upstream `luarocks write_rockspec`).
func (r *Rocks) WriteRockspec(ctx context.Context, url string, opts WriteRockspecOpts) (string, error) {
	return r.engine.WriteRockspec(ctx, url, opts)
}

// Doc shows or lists documentation for an installed rock (upstream
// `luarocks doc`).
func (r *Rocks) Doc(ctx context.Context, name string, opts DocOpts) error {
	return r.engine.Doc(ctx, name, opts)
}

// Test runs a rock's test suite (upstream `luarocks test`).
func (r *Rocks) Test(ctx context.Context, specPath string, opts TestOpts) error {
	return r.engine.Test(ctx, specPath, opts)
}

// Config reads or writes LuaRocks configuration and returns the printed value
// (upstream `luarocks config`).
func (r *Rocks) Config(ctx context.Context, opts ConfigOpts) (string, error) {
	return r.engine.Config(ctx, opts)
}

// Upload publishes a rockspec to a rocks server (upstream `luarocks upload`).
func (r *Rocks) Upload(ctx context.Context, specPath string, opts UploadOpts) error {
	return r.engine.Upload(ctx, specPath, opts)
}

// InitProject scaffolds a LuaRocks project in r.cfg.WorkingDir (upstream
// `luarocks init`).
func (r *Rocks) InitProject(ctx context.Context, opts InitProjectOpts) error {
	return r.engine.InitProject(ctx, opts)
}

// Admin runs a `luarocks admin <subCmd>` repository-administration command
// (upstream `luarocks admin`).
func (r *Rocks) Admin(ctx context.Context, subCmd string, args []string, opts AdminOpts) error {
	return r.engine.Admin(ctx, subCmd, args, opts)
}
