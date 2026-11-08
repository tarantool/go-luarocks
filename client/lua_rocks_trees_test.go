//go:build unix

package client_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// quotedPathRE matches a quoted absolute path in what `config rocks_trees`
// prints: a tree is written either as a string or as a table whose root is
// one, next to its quoted name.
var quotedPathRE = regexp.MustCompile(`"(/[^"]*)"`)

// printedTrees returns the tree roots `luarocks config rocks_trees` printed,
// in order.
func printedTrees(out string) []string {
	matches := quotedPathRE.FindAllStringSubmatch(out, -1)

	roots := make([]string, 0, len(matches))
	for _, m := range matches {
		roots = append(roots, m[1])
	}

	return roots
}

// execOutput runs argv through Exec and returns what it printed.
func execOutput(t *testing.T, r *client.Rocks, argv ...string) string {
	t.Helper()

	return captureFd1(t, func() error {
		return r.Exec(context.Background(), "tt rocks", argv)
	})
}

// TestLuaEngine_RocksTrees checks which rock trees the embedded LuaRocks runs
// over: a typed method over the Config's Tree and the prefix, Exec over the
// ones LuaRocks configured for itself.
//
//nolint:paralleltest // Exec writes to file descriptor 1, which is process-wide
func TestLuaEngine_RocksTrees(t *testing.T) {
	work := t.TempDir()
	prefix := t.TempDir()
	target := t.TempDir()

	cfg := rocks.Config{Tree: target, WorkingDir: work, Tarantool: rocks.TarantoolConfig{Prefix: prefix}}

	r := newClient(t, cfg, client.WithBackend(client.BackendLua))

	got, err := r.Config(context.Background(), client.ConfigOpts{Key: "rocks_trees"})
	require.NoError(t, err, "Config rocks_trees")
	assert.Equal(t, []string{target, prefix}, printedTrees(got), "a typed method runs over Tree and the prefix")

	assert.Equal(t, []string{filepath.Join(work, ".rocks"), prefix},
		printedTrees(execOutput(t, r, "config", "rocks_trees")),
		"Exec keeps the trees LuaRocks configured for itself")
}

// TestLuaEngine_TypedMethodsIgnoreConfigFile writes a system config file
// that sets rocks_trees and deps_mode: a typed method runs over the Config's
// trees all the same, but takes the deps mode from the file, as upstream
// does where no --deps-mode is passed (Build and Make), and Exec, which runs
// the command as the host's LuaRocks would, sees both settings.
//
//nolint:paralleltest // sets the environment, and Exec writes to file descriptor 1
func TestLuaEngine_TypedMethodsIgnoreConfigFile(t *testing.T) {
	dirs := testenv.Hermetic(t)
	writeHostileConfig(t, dirs.SysconfDir)

	work := t.TempDir()
	prefix := t.TempDir()
	target := t.TempDir()

	r := newClient(t, rocks.Config{Tree: target, WorkingDir: work, Tarantool: rocks.TarantoolConfig{Prefix: prefix}},
		client.WithBackend(client.BackendLua))

	got, err := r.Config(context.Background(), client.ConfigOpts{Key: "rocks_trees"})
	require.NoError(t, err, "Config rocks_trees")
	assert.Equal(t, []string{target, prefix}, printedTrees(got), "a typed method's trees")

	got, err = r.Config(context.Background(), client.ConfigOpts{Key: "deps_mode"})
	require.NoError(t, err, "Config deps_mode")
	assert.Equal(t, "all", got, "a typed method's deps_mode")

	assert.Equal(t, []string{"/nonexistent"}, printedTrees(execOutput(t, r, "config", "rocks_trees")),
		"Exec's trees")
	assert.Equal(t, "all", strings.TrimSpace(execOutput(t, r, "config", "deps_mode")), "Exec's deps_mode")
}

// writeHostileConfig writes into sysconfDir a config-5.1.lua that sets the
// rock trees, which a typed method must not take from a config file, and the
// deps mode, which it takes where it passes no --deps-mode.
func writeHostileConfig(t *testing.T, sysconfDir string) {
	t.Helper()

	require.NoError(t, os.WriteFile(filepath.Join(sysconfDir, "config-5.1.lua"),
		[]byte("deps_mode = \"all\"\nrocks_trees = { \"/nonexistent\" }\n"), 0o600), "write the system config")
}
