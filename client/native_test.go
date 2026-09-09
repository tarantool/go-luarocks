package client_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
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

func TestUpsertProvider_ActiveOrdering(t *testing.T) {
	t.Parallel()

	// Active provider moves to index 0; inactive is appended; dedups.
	assert.Equal(t, []string{"demo/2.0-1", "demo/1.0-1"},
		client.UpsertProvider([]string{"demo/1.0-1"}, "demo/2.0-1", true), "upgrade → new active first")
	assert.Equal(t, []string{"demo/1.0-1", "demo/2.0-1"},
		client.UpsertProvider([]string{"demo/1.0-1"}, "demo/2.0-1", false), "inactive → appended")
	assert.Equal(t, []string{"demo/1.0-1"},
		client.UpsertProvider([]string{"demo/1.0-1"}, "demo/1.0-1", true), "reinstall → no duplicate")
}

func TestManifestProvider_ReadsActiveSlot(t *testing.T) {
	t.Parallel()

	m := &rocks.Manifest{
		Modules:  map[string][]string{"demo": {"demo/2.0-1", "demo/1.0-1"}},
		Commands: map[string][]string{"tool": {"cli/3.0-1"}},
	}
	p := client.ManifestProvider(m)

	name, ver, ok := p("demo")
	require.True(t, ok)
	assert.Equal(t, "demo", name)
	assert.Equal(t, "2.0-1", ver, "active provider is index 0")

	name, ver, ok = p("tool")
	require.True(t, ok)
	assert.Equal(t, "cli", name)
	assert.Equal(t, "3.0-1", ver)

	_, _, ok = p("absent")
	assert.False(t, ok)
}

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
