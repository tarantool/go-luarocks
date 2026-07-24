package build_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestMergeEnv_Overrides(t *testing.T) {
	t.Parallel()

	base := []string{"FOO=1", "BAR=2", "PATH=/bin"}
	out := build.MergeEnv(base, map[string]string{"BAR": "99", "NEW": "x"})
	// Verify FOO unchanged, BAR overridden in place, NEW appended, PATH unchanged.
	want := map[string]string{"FOO": "1", "BAR": "99", "PATH": "/bin", "NEW": "x"}
	seen := map[string]string{}

	for _, e := range out {
		k, v, ok := build.SplitEnv(e)
		if !ok {
			t.Errorf("bad env entry: %q", e)

			continue
		}

		seen[k] = v
	}

	for k, v := range want {
		assert.Equal(t, v, seen[k], "env[%q]", k)
	}
}

func TestMergeEnv_EmptyOverlay(t *testing.T) {
	t.Parallel()

	base := []string{"A=1", "B=2"}

	out := build.MergeEnv(base, nil)
	if len(out) != 2 || out[0] != "A=1" || out[1] != "B=2" {
		t.Errorf("nil overlay must return copy of base, got %v", out)
	}

	out[0] = "Z=Z"

	assert.Equal(t, "A=1", base[0], "MergeEnv returned aliased slice; mutation leaked")
}

func TestBuildEnv_AllFieldsPopulated(t *testing.T) {
	t.Parallel()

	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tt",
			Executable: "/opt/tt/bin/tarantool",
			IncludeDir: "/opt/tt/include/tarantool",
		},
	}
	env := build.BuildEnv(cfg)

	checks := map[string]string{
		"TARANTOOL_DIR": "/opt/tt",
		"LUA":           "/opt/tt/bin/tarantool",
		"LUA_INCDIR":    "/opt/tt/include/tarantool",
		"LUA_LIBDIR":    "/opt/tt/lib",
		"LUA_BINDIR":    "/opt/tt/bin",
	}

	for k, want := range checks {
		assert.Equal(t, want, env[k], "BuildEnv[%q]", k)
	}
}

func TestBuildEnv_EmptyConfig(t *testing.T) {
	t.Parallel()

	env := build.BuildEnv(rocks.Config{})
	assert.Empty(t, env, "BuildEnv(empty) should return empty map")
}

func TestRunCmd_NonZeroExit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	err := build.RunCmd(ctx, "sh", []string{"-c", "echo boom 1>&2; exit 3"}, "", nil)
	require.Error(t, err, "expected error for non-zero exit")
	assert.Contains(t, err.Error(), "boom", "error should embed stderr tail")
}

func TestRunCmd_BadProgram(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	err := build.RunCmd(ctx, "/no/such/binary/ever", nil, "", nil)
	require.Error(t, err, "expected error for missing binary")
	// We do not require a specific wrapping here, only that it surfaces.
	assert.NotErrorIs(t, err, context.Canceled, "unexpected ctx-cancel wrapping")
}

func TestRunCmd_ContextCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := build.RunCmd(ctx, "sh", []string{"-c", "sleep 30"}, "", nil)
	require.Error(t, err, "expected error from cancelled context")
}

func TestRunCmd_EnvOverlay(t *testing.T) {
	t.Parallel()

	// Verify the extraEnv map actually reaches the child process.
	ctx := context.Background()
	err := build.RunCmd(ctx, "sh", []string{"-c", `test "$ROCKS_TEST_KEY" = "from-overlay"`},
		"", map[string]string{"ROCKS_TEST_KEY": "from-overlay"})
	assert.NoError(t, err, "child did not see overlay env var")
}
