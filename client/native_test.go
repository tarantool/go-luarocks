package client_test

import (
	"context"
	"testing"

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
