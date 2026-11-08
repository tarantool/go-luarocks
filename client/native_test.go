package client_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

// The native backend implements the install/build/make/pack/which/list/show
// write and query operations plus Remove, Search and Download; the remaining
// operations return
// rocks.ErrNotImplemented — loud, not a silent no-op. The table below exercises
// EACH of them through a *Rocks built with the default (native) backend, so the
// delegation path (r.engine == nativeEngine) is covered too.
func TestNativeEngine_Unimplemented_ReturnsErrNotImplemented(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	r, err := client.New(rocks.Config{Tree: "/opt/tree"})
	require.NoError(t, err, "New")

	cases := []struct {
		name string
		call func() error
	}{
		{"Purge", func() error { return r.Purge(ctx, client.PurgeOpts{}) }},
		{"Lint", func() error { return r.Lint(ctx, "foo.rockspec", client.LintOpts{}) }},
		{"NewVersion", func() error {
			_, e := r.NewVersion(ctx, "foo.rockspec", client.NewVersionOpts{})

			return e
		}},
		{"WriteRockspec", func() error {
			_, e := r.WriteRockspec(ctx, "https://x/foo", client.WriteRockspecOpts{})

			return e
		}},
		{"Doc", func() error { return r.Doc(ctx, "foo", client.DocOpts{}) }},
		{"Test", func() error { return r.Test(ctx, "foo.rockspec", client.TestOpts{}) }},
		{"Config", func() error {
			_, e := r.Config(ctx, client.ConfigOpts{})

			return e
		}},
		{"Upload", func() error { return r.Upload(ctx, "foo.rockspec", client.UploadOpts{}) }},
		{"InitProject", func() error { return r.InitProject(ctx, client.InitProjectOpts{}) }},
		{"Admin", func() error { return r.Admin(ctx, "add", nil, client.AdminOpts{}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.call(), rocks.ErrNotImplemented, "%s err, want ErrNotImplemented", tc.name)
		})
	}
}

// guard: nativeEngine must satisfy Engine.
var _ client.Engine = (*client.NativeEngine)(nil)

func TestWrapBinScripts_ShouldWrapDecision(t *testing.T) {
	t.Parallel()

	f := false
	tr := true

	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"absent defaults to wrap", nil, true},
		{"explicit false opts out", &f, false},
		{"explicit true wraps", &tr, true},
	}
	for _, c := range cases {
		spec := &rocks.Rockspec{Deploy: rocks.Deploy{WrapBinScripts: c.val}}
		require.Equal(t, c.want, client.WrapBinScripts(spec), c.name)
	}
}

func TestCheckSupportedPlatforms(t *testing.T) {
	t.Parallel()

	linux := []string{"unix", "linux"}
	macos := []string{"unix", "macosx"}

	cases := []struct {
		name    string
		plats   []string
		support []string
		wantErr bool
	}{
		{"empty imposes no constraint", linux, nil, false},
		{"positive match", linux, []string{"unix"}, false},
		{"positive mismatch rejected", macos, []string{"linux"}, true},
		{"negated non-match allowed", linux, []string{"!windows"}, false},
		{"negated match rejected", linux, []string{"!linux"}, true},
		{"mixed with a positive match", linux, []string{"!windows", "linux"}, false},
		{"all-negative allows anything not excluded", macos, []string{"!windows"}, false},
	}
	for _, c := range cases {
		spec := &rocks.Rockspec{Package: "foo", SupportedPlatforms: c.support}

		err := client.CheckSupportedPlatforms(spec, c.plats)
		if c.wantErr {
			require.Error(t, err, c.name)
		} else {
			require.NoError(t, err, c.name)
		}
	}
}

// TestAddProvider_UpstreamOrder: provider lists keep upstream's sort_pkgs
// order — rock name first, then newest version first — whatever the order the
// rocks were installed in, so index 0 is always the provider Deploy gave the
// plain path. Recording a newly installed rock at the front made a downgrade
// (or a second rock sorting after the first) the "active" provider of files it
// had deployed under versioned names.
func TestAddProvider_UpstreamOrder(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		list     []string
		provider string
		want     []string
	}{
		{"upgrade", []string{"demo/1.0-1"}, "demo/2.0-1", []string{"demo/2.0-1", "demo/1.0-1"}},
		{"downgrade", []string{"demo/2.0-1"}, "demo/1.0-1", []string{"demo/2.0-1", "demo/1.0-1"}},
		{"reinstall", []string{"demo/1.0-1"}, "demo/1.0-1", []string{"demo/1.0-1"}},
		{"middle version", []string{"demo/3.0-1", "demo/1.0-1"}, "demo/2.0-1", []string{"demo/3.0-1", "demo/2.0-1", "demo/1.0-1"}},
		{"versions compare as versions", []string{"demo/9.0-1"}, "demo/10.0-1", []string{"demo/10.0-1", "demo/9.0-1"}},
		{"smaller name first", []string{"bbb/1.0-1"}, "aaa/1.0-1", []string{"aaa/1.0-1", "bbb/1.0-1"}},
		{"larger name after", []string{"aaa/1.0-1"}, "bbb/2.0-1", []string{"aaa/1.0-1", "bbb/2.0-1"}},
		{"first provider", nil, "demo/1.0-1", []string{"demo/1.0-1"}},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, client.AddProvider(c.list, c.provider), c.name)
	}
}

// TestManifestProviders_KeepsModulesAndCommandsApart: a module and a command
// of the same name are looked up in their own index, as upstream
// manif.get_providers does.
func TestManifestProviders_KeepsModulesAndCommandsApart(t *testing.T) {
	t.Parallel()

	m := &rocks.Manifest{
		Modules:  map[string][]string{"demo": {"demo/2.0-1", "demo/1.0-1"}},
		Commands: map[string][]string{"demo": {"cli/3.0-1"}, "tool": {"cli/3.0-1"}},
	}
	p := client.ManifestProviders(m)

	assert.Equal(t, []string{"demo/2.0-1", "demo/1.0-1"}, p(tree.ModuleItem, "demo"))
	assert.Equal(t, []string{"cli/3.0-1"}, p(tree.CommandItem, "demo"))
	assert.Equal(t, []string{"cli/3.0-1"}, p(tree.CommandItem, "tool"))
	assert.Empty(t, p(tree.ModuleItem, "tool"))
	assert.Empty(t, p(tree.ModuleItem, "absent"))
}

// TestResolveInstalledDeps pins go-luarocks' own per-entry repo.dependencies:
// the rock's direct dependencies, each at the installed version with the
// greatest string (not the newest version its constraint accepts, as upstream
// scan_deps picks it), without the rock itself or what is not installed.
func TestResolveInstalledDeps(t *testing.T) {
	t.Parallel()

	m := &rocks.Manifest{
		Repository: map[string]map[string]rocks.RepoEntry{
			"lib":  {"1.5-1": {Arch: "installed"}, "1.6-1": {Arch: "installed"}},
			"self": {"2.0-1": {Arch: "installed"}},
		},
	}
	spec := &rocks.Rockspec{
		Package: "self",
		Dependencies: []rocks.Dep{
			{Name: "lib"},          // installed → resolves to highest (1.6-1)
			{Name: "self"},         // self → excluded
			{Name: "notinstalled"}, // absent → omitted
		},
	}
	got := client.ResolveInstalledDeps(spec, m)
	assert.Equal(t, map[string]string{"lib": "1.6-1"}, got)
}

func TestRemoveProvider(t *testing.T) {
	t.Parallel()

	idx := map[string][]string{
		"mod":  {"demo/2.0-1", "demo/1.0-1"},
		"only": {"demo/1.0-1"},
	}
	client.RemoveProvider(idx, "demo/1.0-1")
	assert.Equal(t, []string{"demo/2.0-1"}, idx["mod"], "provider removed, active survivor kept")
	_, ok := idx["only"]
	assert.False(t, ok, "list emptied of all providers must be deleted")
}
