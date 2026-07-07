package build_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestRunBackend_None(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "none"}}
	require.NoError(t, build.RunBackend(context.Background(), spec, t.TempDir(), t.TempDir(), rocks.Config{}), "none backend must be a no-op")
}

func TestRunBackend_Unsupported(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "wibble"}}
	err := build.RunBackend(context.Background(), spec, t.TempDir(), t.TempDir(), rocks.Config{})
	require.Error(t, err, "expected error for unknown build.type")
	assert.ErrorIs(t, err, rocks.ErrUnsupportedRockspecFeature)
}

func TestRunBackend_DefaultEmptyIsBuiltin(t *testing.T) {
	t.Parallel()

	// Empty Modules + no install + no copy_directories → builtin should succeed.
	spec := &rocks.Rockspec{}
	require.NoError(t, build.RunBackend(context.Background(), spec, t.TempDir(), t.TempDir(), rocks.Config{}), "empty builtin must succeed")
}

func TestRunBackend_BuiltinMissingHeaders(t *testing.T) {
	t.Parallel()

	// C module requires Tarantool.IncludeDir; absent → ErrMissingTarantoolHeaders.
	srcDir := t.TempDir()
	destDir := t.TempDir()
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo": {Path: "foo.c"},
			},
		},
	}
	err := build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{})
	require.Error(t, err, "expected ErrMissingTarantoolHeaders")
	assert.ErrorIs(t, err, rocks.ErrMissingTarantoolHeaders)
}

func TestRunBackend_BuiltinLuaCopy(t *testing.T) {
	t.Parallel()

	// Pure-Lua module path needs no compiler — verifies the .lua dispatch
	// fully without touching cc.
	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(srcDir+"/src/foo/bar.lua", "return {}\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"foo.bar": {Path: "src/foo/bar.lua"},
			},
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "builtin lua-only must succeed")
	assert.True(t, build.FileExists(destDir+"/lua/foo/bar.lua"), "expected destDir/lua/foo/bar.lua to exist")
}

func TestRunBackend_BuiltinInstallAndCopyDir(t *testing.T) {
	t.Parallel()

	srcDir := t.TempDir()
	destDir := t.TempDir()
	require.NoError(t, build.WriteFile(srcDir+"/extra.lua", "-- extra\n"))
	require.NoError(t, build.WriteFile(srcDir+"/bin/tool", "#!/bin/sh\necho hi\n"))
	require.NoError(t, build.WriteFile(srcDir+"/doc/README.md", "# doc\n"))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Install: rocks.BuildInstall{
				Lua: map[string]string{"extras": "extra.lua"},
				Bin: map[string]string{"tool": "bin/tool"},
			},
			CopyDirectories: []string{"doc"},
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, srcDir, destDir, rocks.Config{}), "builtin install must succeed")

	for _, p := range []string{
		"/lua/extras.lua",
		"/bin/tool",
		"/doc/README.md",
	} {
		assert.True(t, build.FileExists(destDir+p), "expected %s under destDir", p)
	}
}
