package build_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

// patchedSpec is a builtin rock whose module comes out of its build.patches
// and whose build.extra_files adds a second one.
func patchedSpec(format string) *rocks.Rockspec {
	return &rocks.Rockspec{
		Package:        "patched",
		Version:        "1.0-1",
		RockspecFormat: format,
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"patched":       {Path: "patched.lua"},
				"patched.extra": {Path: "extra.lua"},
			},
			ExtraFiles: map[string]string{"extra.lua": "return 'extra'\n"},
			Patches: map[string]string{
				"fix.diff": "--- a/patched.lua\n+++ b/patched.lua\n@@ -1,2 +1,2 @@\n local M = {}\n-M.broken = true\n+M.broken = false\n",
			},
		},
	}
}

// TestRunBackend_AppliesPatches — the native backend ignored build.patches
// and build.extra_files, so a rock that needs its patches was built from
// unpatched sources, without the files the rockspec adds. The extra files and
// the patch files must be written into the sources, the patches applied
// before the backend runs, and, with the sources marked, not applied again by
// a second build of the same sources.
func TestRunBackend_AppliesPatches(t *testing.T) {
	t.Parallel()

	src, dest := t.TempDir(), t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "patched.lua"), []byte("local M = {}\nM.broken = true\nreturn M\n"), 0o600))

	spec := patchedSpec("")
	require.NoError(t, build.RunBackend(context.Background(), spec, src, dest, rocks.Config{}))

	got, err := os.ReadFile(filepath.Join(dest, "lua", "patched.lua")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "local M = {}\nM.broken = false\nreturn M\n", string(got), "the module is built from the patched source")

	got, err = os.ReadFile(filepath.Join(dest, "lua", "patched", "extra.lua")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "return 'extra'\n", string(got), "the extra file is built")

	assert.FileExists(t, filepath.Join(src, "fix.diff"), "the patch is written into the sources, as upstream does")
	assert.FileExists(t, filepath.Join(src, ".luarocks.patches.applied"))

	// The patch no longer applies to the patched source, so applying it again
	// would fail; the marker keeps a second build from trying.
	require.NoError(t, build.RunBackend(context.Background(), spec, src, t.TempDir(), rocks.Config{}), "rebuild")
}

// TestRunBackend_PatchThatDoesNotApplyFails — a patch whose context is not
// in the sources fails the build, and the sources are not marked.
func TestRunBackend_PatchThatDoesNotApplyFails(t *testing.T) {
	t.Parallel()

	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "patched.lua"), []byte("something else\n"), 0o600))

	err := build.RunBackend(context.Background(), patchedSpec(""), src, t.TempDir(), rocks.Config{})
	require.ErrorContains(t, err, "failed applying patch fix.diff")
	assert.NoFileExists(t, filepath.Join(src, ".luarocks.patches.applied"))
}

// TestRunBackend_PatchCreatesFileForFormat3 — rockspec_format 3.0 lets a
// patch create a file from /dev/null; before it, the patch fails.
func TestRunBackend_PatchCreatesFileForFormat3(t *testing.T) {
	t.Parallel()

	spec := func(format string) *rocks.Rockspec {
		return &rocks.Rockspec{
			Package: "created", Version: "1.0-1", RockspecFormat: format,
			Build: rocks.Build{Type: "none", Patches: map[string]string{
				"new.diff": "--- /dev/null\n+++ b/new.lua\n@@ -0,0 +1 @@\n+return 1\n",
			}},
		}
	}

	src := t.TempDir()
	require.NoError(t, build.RunBackend(context.Background(), spec("3.0"), src, t.TempDir(), rocks.Config{}))

	got, err := os.ReadFile(filepath.Join(src, "new.lua")) //nolint:gosec // test temp path
	require.NoError(t, err)
	assert.Equal(t, "return 1\n", string(got))

	require.Error(t, build.RunBackend(context.Background(), spec("1.0"), t.TempDir(), t.TempDir(), rocks.Config{}))
}

// TestRunBackend_PatchNamesStayInSources — an extra file, a patch file or a
// file a patch names that leads out of the sources fails with
// rocks.ErrUnsafePath instead of being written there.
func TestRunBackend_PatchNamesStayInSources(t *testing.T) {
	t.Parallel()

	for name, b := range map[string]rocks.Build{
		"extra file": {Type: "none", ExtraFiles: map[string]string{"../escaped": "x"}},
		"patch file": {Type: "none", Patches: map[string]string{"../escaped": "x"}},
		"patched file": {Type: "none", Patches: map[string]string{
			"p.diff": "--- a/../escaped\n+++ b/../escaped\n@@ -1 +1 @@\n-keep\n+changed\n",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			parent := t.TempDir()
			src := filepath.Join(parent, "src")
			require.NoError(t, os.MkdirAll(src, 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(parent, "escaped"), []byte("keep\n"), 0o600))

			spec := &rocks.Rockspec{Package: "p", Version: "1.0-1", Build: b}
			require.ErrorIs(t, build.RunBackend(context.Background(), spec, src, t.TempDir(), rocks.Config{}), rocks.ErrUnsafePath)

			got, err := os.ReadFile(filepath.Join(parent, "escaped")) //nolint:gosec // test temp path
			require.NoError(t, err)
			assert.Equal(t, "keep\n", string(got))
		})
	}
}
