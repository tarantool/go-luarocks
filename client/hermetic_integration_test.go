//go:build integration_luaengine

package client_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// realLuaRocksPaths returns where LuaRocks would cache and read user config
// in the environment the test binary started with — the developer's home.
func realLuaRocksPaths(t *testing.T) []string {
	t.Helper()

	home, ok := testenv.Original("HOME")
	if !ok || home == "" {
		testenv.Missingf(t, "HOME is not set; there is no real home to protect")
	}

	cache := filepath.Join(home, ".cache")
	if v, ok := testenv.Original("XDG_CACHE_HOME"); ok && v != "" {
		cache = v
	}

	config := filepath.Join(home, ".config")
	if v, ok := testenv.Original("XDG_CONFIG_HOME"); ok && v != "" {
		config = v
	}

	paths := []string{
		filepath.Join(cache, "luarocks"),
		filepath.Join(home, ".cache", "luarocks"),
		filepath.Join(home, ".luarocks"),
		filepath.Join(config, "luarocks"),
	}
	slices.Sort(paths)

	return slices.Compact(paths)
}

// pathState is what a snapshot records of one path: whether it exists and,
// if so, its modification time, which changes when an entry is created in
// or removed from a directory.
type pathState struct {
	exists  bool
	modTime int64
}

func snapshotPaths(t *testing.T, paths []string) map[string]pathState {
	t.Helper()

	states := map[string]pathState{}

	for _, p := range paths {
		fi, err := os.Lstat(p)
		if os.IsNotExist(err) {
			states[p] = pathState{}

			continue
		}

		require.NoError(t, err, "stat %s", p)

		states[p] = pathState{exists: true, modTime: fi.ModTime().UnixNano()}
	}

	return states
}

// TestLuaEngine_Install_LeavesRealHomeAlone proves the hermetic environment
// holds for the lua engine: an Install, which makes LuaRocks open its
// download cache for every HTTP server it consults (the hardcoded
// rocks.tarantool.org among them), creates nothing under the real home's
// .cache/luarocks or .luarocks, and the cache lands in the hermetic home
// instead. The second half is what makes the first mean something: a run
// that wrote no cache at all would leave the real home alone too.
func TestLuaEngine_Install_LeavesRealHomeAlone(t *testing.T) {
	ttBin, err := exec.LookPath("tarantool")
	if err != nil {
		testenv.Missingf(t, "tarantool not found on PATH; the lua backend needs a real interpreter: %v", err)
	}

	prefix := filepath.Dir(filepath.Dir(ttBin))

	incDir := filepath.Join(prefix, "include", "tarantool")
	if _, err := os.Stat(filepath.Join(incDir, "lua.h")); err != nil {
		testenv.Missingf(t, "tarantool lua.h not found at %s; the lua backend builds need Lua headers: %v", incDir, err)
	}

	// Upstream unpacks a .rock with the unzip program.
	if _, err := exec.LookPath("unzip"); err != nil {
		testenv.Missingf(t, "unzip not found on PATH; the lua backend unpacks a .rock with it: %v", err)
	}

	watched := realLuaRocksPaths(t)
	before := snapshotPaths(t, watched)

	dirs := testenv.Hermetic(t)

	// Offline, as in TestBackendParity_Download: the hardcoded server is
	// still consulted, but through a proxy that refuses the connection.
	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY"} {
		t.Setenv(v, "http://127.0.0.1:1")
	}

	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	repo := rockRepo(t, `commands = {}
modules = {}
repository = {
   glrhermetic = { ["1.0-1"] = { { arch = "src" } } },
}
`, map[string]map[string]string{
		// LuaRocks packs a git source as the directory it was cloned into.
		"glrhermetic-1.0-1.src.rock": {
			"glrhermetic-1.0-1.rockspec": `package = "glrhermetic"
version = "1.0-1"
source = { url = "` + unreachableSource + `glrhermetic.git", tag = "1.0" }
build = { type = "builtin", modules = { glrhermetic = "glrhermetic.lua" } }
`,
			"glrhermetic/glrhermetic.lua": "return 'hermetic'\n",
		},
	})

	tree := t.TempDir()
	r, err := client.New(rocks.Config{
		Tree:       tree,
		WorkingDir: t.TempDir(),
		Tarantool: rocks.TarantoolConfig{
			Prefix:     prefix,
			IncludeDir: incDir,
			Version:    parityTarantoolVersion,
		},
	}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	require.NoError(t,
		r.Install(context.Background(), "glrhermetic", client.InstallOpts{Servers: []string{repo}, Deps: client.DepsNone}),
		"Install")
	requireFile(t, filepath.Join(tree, "share", "tarantool", "glrhermetic.lua"), "return 'hermetic'\n")

	// LuaRocks run as root caches under the system cache directory instead
	// (cmd.lua), so only an unprivileged run says where the user cache went.
	if os.Geteuid() != 0 {
		assert.DirExists(t, filepath.Join(dirs.CacheHome, "luarocks"),
			"LuaRocks wrote no cache into the hermetic home, so the check below proves nothing")
	}

	assert.Equal(t, before, snapshotPaths(t, watched),
		"the lua engine touched LuaRocks paths under the real home %v", watched)
}
