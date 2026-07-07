package build_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
)

func TestRunCommand_BothPhases(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:           "command",
			BuildCommand:   "echo build >> " + logFile,
			InstallCommand: "echo install >> " + logFile,
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, dir, t.TempDir(), rocks.Config{}), "command backend failed")

	data, err := build.ReadFile(logFile)
	require.NoError(t, err, "missing log")
	assert.Equal(t, "build\ninstall\n", string(data))
}

func TestRunCommand_EmptyIsNoop(t *testing.T) {
	t.Parallel()

	spec := &rocks.Rockspec{Build: rocks.Build{Type: "command"}}
	require.NoError(t, build.RunBackend(context.Background(), spec, t.TempDir(), t.TempDir(), rocks.Config{}), "empty command phases should be a no-op")
}

func TestRunCommand_ExportsCC(t *testing.T) {
	// glr-8rc: the command backend exports CC (upstream command.run env), so a
	// $CC-based build command uses the configured compiler even when CC is not
	// already in the inherited environment.
	t.Setenv("CC", "") // → DeriveFlags default "cc"

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:         "command",
			BuildCommand: `echo "cc=$CC" > ` + logFile,
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, dir, t.TempDir(), rocks.Config{}),
		"command backend failed")

	data, _ := build.ReadFile(logFile)
	assert.Equal(t, "cc=cc\n", string(data))
}

func TestRunCommand_SubstitutesVars(t *testing.T) {
	t.Parallel()

	// glr-1tu: luarocks $(NAME) placeholders are expanded in Go before the
	// command reaches sh (where $(...) would otherwise be command substitution).
	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	spec := &rocks.Rockspec{
		Package: "foo", Version: "1.0-1",
		Build: rocks.Build{
			Type:         "command",
			BuildCommand: `echo "prefix=$(PREFIX)" > ` + logFile,
		},
	}
	cfg := rocks.Config{Tree: "/tree"}
	require.NoError(t, build.RunBackend(context.Background(), spec, dir, t.TempDir(), cfg),
		"command backend failed")

	data, _ := build.ReadFile(logFile)
	assert.Equal(t, "prefix=/tree/share/tarantool/rocks/foo/1.0-1\n", string(data))
}

func TestRunCommand_EnvVisible(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	logFile := filepath.Join(dir, "log")
	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type:         "command",
			BuildCommand: `echo "td=$TARANTOOL_DIR inc=$LUA_INCDIR" > ` + logFile,
		},
	}
	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{
			Prefix:     "/opt/tt",
			IncludeDir: "/opt/tt/include/tarantool",
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, dir, t.TempDir(), cfg), "command backend failed")

	data, _ := build.ReadFile(logFile)
	want := "td=/opt/tt inc=/opt/tt/include/tarantool\n"
	assert.Equal(t, want, string(data))
}
