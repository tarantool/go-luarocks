// Package rocks_test exercises the root package's exported surface.
//
// The root package (config.go, errors.go, types.go, rocks.go,
// interfaces.go) is deliberately behavior-free: it holds shared struct/
// interface/const/sentinel-error declarations for the sub-packages to
// exchange, with zero executable statements of its own (every method lives
// in the owning subsystem package — see doc.go). `go test -cover` therefore
// reports "no statements" / 0.0% for this package regardless of test
// quality — there is no control flow to exercise. These tests still lock
// down the exported contract other packages depend on: field shapes,
// sentinel error identity/wrapping, and interface signatures.
package rocks_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
)

// --- Config -----------------------------------------------------------

// TestConfig_ZeroValue confirms a zero-value Config is usable: nil Logger
// disables logging per the doc comment, and every field is independently
// addressable.
func TestConfig_ZeroValue(t *testing.T) {
	t.Parallel()

	var cfg rocks.Config

	assert.Empty(t, cfg.Tree)
	assert.Empty(t, cfg.WorkingDir)
	assert.Empty(t, cfg.Servers)
	assert.Empty(t, cfg.InsecureServers)
	assert.Nil(t, cfg.Logger, "nil Logger must be a valid zero value (disables logging)")
	assert.Equal(t, rocks.TarantoolConfig{}, cfg.Tarantool)
	assert.Equal(t, rocks.RockspecConfig{}, cfg.Rockspec)
}

// TestConfig_FullyPopulated round-trips every field through a struct
// literal, guarding against a field being silently dropped or renamed.
func TestConfig_FullyPopulated(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	cfg := rocks.Config{
		Tree:       "/opt/tarantool/rocks",
		WorkingDir: "/tmp/work",
		Tarantool: rocks.TarantoolConfig{
			Executable: "/usr/bin/tarantool",
			Prefix:     "/usr",
			IncludeDir: "/usr/include/tarantool",
			Version:    "3.0.0",
		},
		Servers:         []string{"https://rocks.tarantool.org"},
		Rockspec:        rocks.RockspecConfig{},
		Logger:          logger,
		InsecureServers: []string{"internal.example.com"},
	}

	assert.Equal(t, "/opt/tarantool/rocks", cfg.Tree)
	assert.Equal(t, "/tmp/work", cfg.WorkingDir)
	assert.Equal(t, "/usr/bin/tarantool", cfg.Tarantool.Executable)
	assert.Equal(t, "/usr", cfg.Tarantool.Prefix)
	assert.Equal(t, "/usr/include/tarantool", cfg.Tarantool.IncludeDir)
	assert.Equal(t, "3.0.0", cfg.Tarantool.Version)
	assert.Equal(t, []string{"https://rocks.tarantool.org"}, cfg.Servers)
	assert.Equal(t, []string{"internal.example.com"}, cfg.InsecureServers)
	assert.Same(t, logger, cfg.Logger)
}

// --- Sentinel errors ----------------------------------------------------

// TestSentinelErrors_Messages locks down each sentinel's Error() text —
// callers besides this repo may match on the message via logs/tests.
func TestSentinelErrors_Messages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		err  error
		want string
	}{
		{rocks.ErrMissingTarantoolHeaders, "rocks: tarantool headers not found"},
		{rocks.ErrMissingTarantoolBinary, "rocks: tarantool binary not found"},
		{rocks.ErrUnsupportedCommand, "rocks: unsupported command"},
		{rocks.ErrUnsupportedRockspecFeature, "rocks: unsupported rockspec feature"},
		{rocks.ErrNotImplemented, "rocks: method not implemented by this backend"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()

			require.Error(t, c.err)
			assert.Equal(t, c.want, c.err.Error())
		})
	}
}

// TestSentinelErrors_Distinct confirms the five sentinels are pairwise
// distinct under errors.Is — a caller branching on one must never
// accidentally match another.
func TestSentinelErrors_Distinct(t *testing.T) {
	t.Parallel()

	all := []error{
		rocks.ErrMissingTarantoolHeaders,
		rocks.ErrMissingTarantoolBinary,
		rocks.ErrUnsupportedCommand,
		rocks.ErrUnsupportedRockspecFeature,
		rocks.ErrNotImplemented,
	}
	for i, a := range all {
		for j, b := range all {
			if i == j {
				continue
			}

			assert.NotErrorIsf(t, a, b, "sentinel %d must not match sentinel %d", i, j)
		}
	}
}

// TestSentinelErrors_WrapAndUnwrap confirms every sentinel survives
// fmt.Errorf %w wrapping (including multiple layers) for errors.Is
// discrimination — the pattern every facade method is documented to use.
func TestSentinelErrors_WrapAndUnwrap(t *testing.T) {
	t.Parallel()

	base := rocks.ErrNotImplemented
	wrapped := fmt.Errorf("client.Install: %w", base)
	doubleWrapped := fmt.Errorf("facade: %w", wrapped)

	require.ErrorIs(t, wrapped, base)
	require.ErrorIs(t, doubleWrapped, base)
	assert.Equal(t, base, errors.Unwrap(wrapped))
	assert.Contains(t, doubleWrapped.Error(), base.Error())
}

// --- types.go -------------------------------------------------------

// TestRockspec_ZeroValueAndPopulated exercises the Rockspec struct's field
// shape, including nested Description/Source/Build/Deploy and the
// per-list platform-override maps.
func TestRockspec_ZeroValueAndPopulated(t *testing.T) {
	t.Parallel()

	var zero rocks.Rockspec

	assert.False(t, zero.HasBuild, "zero-value Rockspec must not claim a build table")
	assert.Empty(t, zero.RawSource)

	spec := rocks.Rockspec{
		Package:        "metrics",
		Version:        "1.0.0-1",
		RockspecFormat: "3.0",
		Description: rocks.Description{
			Summary:    "A metrics library",
			Detailed:   "Longer description.",
			License:    "BSD",
			Homepage:   "https://example.com",
			IssuesURL:  "https://example.com/issues",
			Maintainer: "Someone <someone@example.com>",
			Labels:     []string{"observability"},
		},
		SupportedPlatforms: []string{"linux", "macosx"},
		Dependencies:       []rocks.Dep{{Name: "checks", Constraints: []rocks.VersionConstraint{{Op: ">=", Version: rocks.Version{Raw: "3.1.0"}}}}},
		BuildDependencies:  []rocks.Dep{{Name: "cmake"}},
		TestDependencies:   []rocks.Dep{{Name: "luaunit"}},
		ExternalDependencies: map[string]rocks.ExternalDep{
			"TARANTOOL": {Header: "tarantool/module.h", Library: "tarantool"},
		},
		Source: rocks.Source{
			URL:        "https://example.com/metrics-1.0.0-1.tar.gz",
			Tag:        "v1.0.0",
			Branch:     "main",
			MD5:        "deadbeef",
			File:       "metrics.tar.gz",
			Dir:        "metrics-1.0.0",
			Module:     "metrics",
			Identifier: "20240101.000000.abcdef0",
		},
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"metrics": {Path: "src/metrics.lua"},
				"metrics.native": {
					Sources:   []string{"src/native.c"},
					Incdirs:   []string{"include"},
					Libdirs:   []string{"lib"},
					Libraries: []string{"m"},
					Defines:   []string{"NDEBUG"},
				},
			},
			Install: rocks.BuildInstall{
				Lua:  map[string]string{"metrics.lua": "src/metrics.lua"},
				Lib:  map[string]string{},
				Bin:  map[string]string{},
				Conf: map[string]string{},
			},
			CopyDirectories: []string{"docs"},
			Variables:       map[string]string{"CMAKE_BUILD_TYPE": "Release"},
			Makefile:        "Makefile",
			BuildTarget:     "all",
			InstallTarget:   "install",
			BuildCommand:    "make",
			InstallCommand:  "make install",
		},
		Deploy:    rocks.Deploy{},
		HasBuild:  true,
		RawSource: []byte("package = 'metrics'\n"),
		DependenciesPlatforms: map[string][]rocks.Dep{
			"linux": {{Name: "linux-only"}},
		},
	}

	assert.Equal(t, "metrics", spec.Package)
	assert.Equal(t, "3.0", spec.RockspecFormat)
	assert.Equal(t, "A metrics library", spec.Description.Summary)
	assert.Equal(t, []string{"linux", "macosx"}, spec.SupportedPlatforms)
	require.Len(t, spec.Dependencies, 1)
	assert.Equal(t, "checks", spec.Dependencies[0].Name)
	assert.Equal(t, "builtin", spec.Build.Type)
	assert.Equal(t, "src/metrics.lua", spec.Build.Modules["metrics"].Path)
	require.Contains(t, spec.Build.Modules, "metrics.native")
	assert.Equal(t, []string{"src/native.c"}, spec.Build.Modules["metrics.native"].Sources)
	assert.Equal(t, "https://example.com/metrics-1.0.0-1.tar.gz", spec.Source.URL)
	assert.True(t, spec.HasBuild)
	assert.Equal(t, []rocks.Dep{{Name: "linux-only"}}, spec.DependenciesPlatforms["linux"])
}

// TestDeploy_WrapBinScriptsTriState locks down the nil-means-default,
// explicit-false-means-skip semantics documented on Deploy.WrapBinScripts.
func TestDeploy_WrapBinScriptsTriState(t *testing.T) {
	t.Parallel()

	var absent rocks.Deploy

	assert.Nil(t, absent.WrapBinScripts, "absent field must be nil (default: wrap)")

	no := false
	explicit := rocks.Deploy{WrapBinScripts: &no}
	require.NotNil(t, explicit.WrapBinScripts)
	assert.False(t, *explicit.WrapBinScripts)
}

// TestBuild_PassTriState mirrors the same nil-means-default pattern for
// Build.BuildPass / InstallPass.
func TestBuild_PassTriState(t *testing.T) {
	t.Parallel()

	var b rocks.Build

	assert.Nil(t, b.BuildPass)
	assert.Nil(t, b.InstallPass)

	yes := true
	b.BuildPass = &yes
	require.NotNil(t, b.BuildPass)
	assert.True(t, *b.BuildPass)
}

// TestVersion_Fields covers Version's HasRevision discriminator and the
// SCM/dev flags, which distinguish "no revision" (0, false) from an
// explicit "-0" revision (0, true).
func TestVersion_Fields(t *testing.T) {
	t.Parallel()

	v := rocks.Version{
		Raw:         "1.2.3-1",
		Components:  []float64{1, 2, 3},
		Revision:    1,
		HasRevision: true,
	}
	assert.Equal(t, "1.2.3-1", v.Raw)
	assert.Equal(t, []float64{1, 2, 3}, v.Components)
	assert.True(t, v.HasRevision)
	assert.False(t, v.IsSCM)
	assert.False(t, v.IsDev)

	scm := rocks.Version{Raw: "scm-1", IsSCM: true}
	assert.True(t, scm.IsSCM)
	assert.False(t, scm.HasRevision, "IsSCM alone does not imply an explicit revision")

	var zero rocks.Version

	assert.False(t, zero.HasRevision, "the zero value must read as 'no explicit revision', not revision 0")
}

// TestVersionConstraint_Fields covers the (Op, Version) pair shape.
func TestVersionConstraint_Fields(t *testing.T) {
	t.Parallel()

	vc := rocks.VersionConstraint{Op: "~>", Version: rocks.Version{Raw: "2.0"}}
	assert.Equal(t, "~>", vc.Op)
	assert.Equal(t, "2.0", vc.Version.Raw)
}

// TestManifest_Fields covers the tree-manifest projection's four indexes.
func TestManifest_Fields(t *testing.T) {
	t.Parallel()

	m := rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"metrics": {"1.0.0-1": {Arch: "installed"}},
		},
		Modules:  map[string][]string{"metrics": {"metrics/1.0.0-1"}},
		Commands: map[string][]string{"metrics-cli": {"metrics/1.0.0-1"}},
		Dependencies: map[string]map[string][]rocks.Dep{
			"metrics": {"1.0.0-1": {{Name: "checks"}}},
		},
	}

	assert.Equal(t, "installed", m.Repository["metrics"]["1.0.0-1"].Arch)
	assert.Equal(t, []string{"metrics/1.0.0-1"}, m.Modules["metrics"])
	assert.Equal(t, []string{"metrics/1.0.0-1"}, m.Commands["metrics-cli"])
	require.Len(t, m.Dependencies["metrics"]["1.0.0-1"], 1)
	assert.Equal(t, "checks", m.Dependencies["metrics"]["1.0.0-1"][0].Name)
}

// TestRepoEntry_Fields covers the per-arch installed-rock projection.
func TestRepoEntry_Fields(t *testing.T) {
	t.Parallel()

	re := rocks.RepoEntry{
		Arch:         "installed",
		Modules:      map[string]string{"metrics": "lua/metrics/init.lua"},
		Commands:     map[string]string{"metrics-cli": "bin/metrics-cli"},
		Dependencies: map[string]string{"checks": "3.1.0-1"},
	}
	assert.Equal(t, "installed", re.Arch)
	assert.Equal(t, "lua/metrics/init.lua", re.Modules["metrics"])
	assert.Equal(t, "bin/metrics-cli", re.Commands["metrics-cli"])
	assert.Equal(t, "3.1.0-1", re.Dependencies["checks"])
}

// TestVersionedRock_OptionalSpec covers VersionedRock's optional preloaded
// Spec: nil (needs a re-fetch) vs. populated (resolver reuses it).
func TestVersionedRock_OptionalSpec(t *testing.T) {
	t.Parallel()

	noSpec := rocks.VersionedRock{Name: "metrics", Version: rocks.Version{Raw: "1.0.0-1"}, URL: "https://x/metrics-1.0.0-1.rockspec"}
	assert.Nil(t, noSpec.Spec)

	spec := &rocks.Rockspec{Package: "metrics", Version: "1.0.0-1"}
	withSpec := rocks.VersionedRock{Name: "metrics", Version: rocks.Version{Raw: "1.0.0-1"}, Spec: spec}
	require.NotNil(t, withSpec.Spec)
	assert.Same(t, spec, withSpec.Spec)
}

// TestInstallStep_Fields covers the topo-ordered install-step projection.
func TestInstallStep_Fields(t *testing.T) {
	t.Parallel()

	step := rocks.InstallStep{
		Name:     "metrics",
		Version:  rocks.Version{Raw: "1.0.0-1"},
		URL:      "https://x/metrics-1.0.0-1.src.rock",
		Rockspec: &rocks.Rockspec{Package: "metrics"},
	}
	assert.Equal(t, "metrics", step.Name)
	assert.Equal(t, "1.0.0-1", step.Version.Raw)
	require.NotNil(t, step.Rockspec)
	assert.Equal(t, "metrics", step.Rockspec.Package)
}

// TestInstalledRock_And_ShowInfo covers the two List/Show projections.
func TestInstalledRock_And_ShowInfo(t *testing.T) {
	t.Parallel()

	ir := rocks.InstalledRock{Name: "metrics", Version: "1.0.0-1"}
	assert.Equal(t, "metrics", ir.Name)
	assert.Equal(t, "1.0.0-1", ir.Version)

	si := rocks.ShowInfo{
		Package:      "metrics",
		Version:      "1.0.0-1",
		Summary:      "A metrics library",
		License:      "BSD",
		Homepage:     "https://example.com",
		Modules:      []string{"metrics"},
		Dependencies: []rocks.Dep{{Name: "checks"}},
	}
	assert.Equal(t, "metrics", si.Package)
	assert.Equal(t, []string{"metrics"}, si.Modules)
	require.Len(t, si.Dependencies, 1)
}

// TestRockManifest_Fields covers the per-rock on-disk manifest projection,
// including the RockspecFile / Rockspec keying documented on the type.
func TestRockManifest_Fields(t *testing.T) {
	t.Parallel()

	rm := rocks.RockManifest{
		RockspecFile: "metrics-1.0.0-1.rockspec",
		Rockspec:     "deadbeef",
		Lua:          map[string]string{"metrics/init.lua": "1111"},
		Lib:          map[string]string{"native.so": "2222"},
		Bin:          map[string]string{"metrics-cli": "3333"},
		Conf:         map[string]string{},
		Doc:          map[string]string{},
	}
	assert.Equal(t, "metrics-1.0.0-1.rockspec", rm.RockspecFile)
	assert.Equal(t, "deadbeef", rm.Rockspec)
	assert.Equal(t, "1111", rm.Lua["metrics/init.lua"])

	var zero rocks.RockManifest

	assert.Empty(t, zero.RockspecFile, "an empty RockspecFile signals the legacy fallback key")
}

// --- rocks.go ---------------------------------------------------------

// TestLuaRocksVersion pins the upstream LuaRocks release the native backend
// targets byte-for-byte compatibility with.
func TestLuaRocksVersion(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "3.9.2", rocks.LuaRocksVersion)
}

// --- interfaces.go ------------------------------------------------------

// fakeFetcher, fakeBuilder, fakeRemoteIndex, and fakeManifestStore are
// minimal stand-ins used only to pin the four interface signatures at
// compile time and confirm a value assigned through the interface actually
// dispatches to the concrete implementation.

type fakeFetcher struct{ path string }

func (f fakeFetcher) Fetch(_ context.Context, _, _ string) (string, error) { return f.path, nil }

type fakeBuilder struct{ called *bool }

func (b fakeBuilder) Build(_ context.Context, _ *rocks.Rockspec, _, _ string) error {
	*b.called = true

	return nil
}

type fakeRemoteIndex struct{ rocksList []rocks.VersionedRock }

func (r fakeRemoteIndex) Query(_ context.Context, _, _ string) ([]rocks.VersionedRock, error) {
	return r.rocksList, nil
}

type fakeManifestStore struct{}

func (fakeManifestStore) ReadTree(string) (*rocks.Manifest, error) { return &rocks.Manifest{}, nil }
func (fakeManifestStore) WriteTree(string, *rocks.Manifest) error  { return nil }
func (fakeManifestStore) ReadRock(string) (*rocks.RockManifest, error) {
	return &rocks.RockManifest{}, nil
}
func (fakeManifestStore) WriteRock(string, *rocks.RockManifest) error { return nil }

// TestFetcherInterface pins Fetcher's signature and confirms interface
// dispatch reaches the concrete implementation.
func TestFetcherInterface(t *testing.T) {
	t.Parallel()

	var f rocks.Fetcher = fakeFetcher{path: "/tmp/src"}

	got, err := f.Fetch(context.Background(), "https://example.com/x.tar.gz", "/tmp")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/src", got)
}

// TestBuilderInterface pins Builder's signature.
func TestBuilderInterface(t *testing.T) {
	t.Parallel()

	called := false

	var b rocks.Builder = fakeBuilder{called: &called}

	err := b.Build(context.Background(), &rocks.Rockspec{Package: "metrics"}, "/src", "/dst")
	require.NoError(t, err)
	assert.True(t, called)
}

// TestRemoteIndexInterface pins RemoteIndex's signature.
func TestRemoteIndexInterface(t *testing.T) {
	t.Parallel()

	want := []rocks.VersionedRock{{Name: "metrics", Version: rocks.Version{Raw: "1.0.0-1"}}}

	var ri rocks.RemoteIndex = fakeRemoteIndex{rocksList: want}

	got, err := ri.Query(context.Background(), "metrics", "")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// TestManifestStoreInterface pins ManifestStore's four-method signature.
func TestManifestStoreInterface(t *testing.T) {
	t.Parallel()

	var ms rocks.ManifestStore = fakeManifestStore{}

	tree, err := ms.ReadTree("/tree")
	require.NoError(t, err)
	assert.NotNil(t, tree)

	require.NoError(t, ms.WriteTree("/tree", &rocks.Manifest{}))

	rm, err := ms.ReadRock("/tree/rock_manifest")
	require.NoError(t, err)
	assert.NotNil(t, rm)

	require.NoError(t, ms.WriteRock("/tree/rock_manifest", &rocks.RockManifest{}))
}
