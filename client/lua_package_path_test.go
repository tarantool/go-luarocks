package client_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/manif"
)

// Module lookup of the lua engine.
//
// LuaRocks requires many modules the engine does not embed: the platform fs
// modules (luarocks.fs.linux, luarocks.fs.macosx and their .tools), optional
// libraries (lfs, socket.http, md5, ...) and every luarocks.cmd.external.* it
// finds on package.path. Whatever answers those requires is code the engine
// runs. These tests pin it: package.path starts empty, so neither the current
// directory, nor a system LuaRocks, nor $LUA_PATH is searched, and the optional
// probes are answered by preload stubs, so a rock tree is not searched for them
// either.

// plantedProbes are modules LuaRocks probes for during an ordinary command.
var plantedProbes = []string{
	"luarocks/fs/linux.lua",
	"luarocks/fs/linux/tools.lua",
	"luarocks/fs/macosx.lua",
	"luarocks/fs/macosx/tools.lua",
	"lfs.lua",
	"posix.lua",
	"md5.lua",
	"digest.lua",
	"bz2.lua",
	"zlib.lua",
	"socket/http.lua",
	"socket/ftp.lua",
	"ltn12.lua",
	"ssl/https.lua",
	"cjson.lua",
	"dkjson.lua",
	"json.lua",
	"mimetypes.lua",
}

// plantedExternal is an external command. Upstream cmd.run_command loads every
// one it finds in the directories of package.path during every command; the
// engine loads one only when argv names it.
const plantedExternal = "luarocks/cmd/external/planted.lua"

// plantModules writes mods under dir. When loaded, each one leaves a file named
// after itself in markers and then fails, so LuaRocks treats it as absent and
// the command still runs: a test sees the load, not its fallout.
func plantModules(t *testing.T, dir, markers string, mods []string) {
	t.Helper()

	for _, mod := range mods {
		marker := filepath.Join(markers, strings.ReplaceAll(mod, "/", "."))
		body := "local f = io.open(" + quoteLuaString(marker) + ", 'w')\n" +
			"f:write('ran')\n" +
			"f:close()\n" +
			"error('planted module " + mod + " ran')\n"

		full := filepath.Join(dir, mod)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(body), 0o644))
	}
}

// requireNoMarkers fails if any planted module left its marker.
func requireNoMarkers(t *testing.T, markers string) {
	t.Helper()

	entries, err := os.ReadDir(markers)
	require.NoError(t, err, "read markers")

	ran := make([]string, 0, len(entries))
	for _, entry := range entries {
		ran = append(ran, entry.Name())
	}

	require.Empty(t, ran, "planted modules ran")
}

// TestLuaEngine_LoadsNoPlantedModules plants modules in each place a require
// could reach them — the current directory and a $LUA_PATH directory, which
// gopher-lua's default package.path names, and the user rock tree, which
// luarocks.loader appends — and runs lua-backed commands that name none of
// them.
// Not parallel: it changes the process directory and the environment.
func TestLuaEngine_LoadsNoPlantedModules(t *testing.T) {
	planted := append([]string{plantedExternal}, plantedProbes...)

	cases := []struct {
		name string
		// dir returns where to plant, given the working directory, and sets up
		// the environment that makes the VM look there.
		dir  func(t *testing.T, work string) string
		mods []string
	}{
		{
			name: "current directory",
			dir: func(t *testing.T, work string) string {
				t.Helper()
				// gopher-lua treats an empty $LUA_PATH as unset and falls
				// back to its default, which starts with ./?.lua.
				t.Setenv("LUA_PATH", "")

				return work
			},
			mods: planted,
		},
		{
			name: "LUA_PATH",
			dir: func(t *testing.T, _ string) string {
				t.Helper()

				dir := t.TempDir()
				t.Setenv("LUA_PATH", filepath.Join(dir, "?.lua"))

				return dir
			},
			mods: planted,
		},
		{
			name: "rock tree",
			dir: func(t *testing.T, work string) string {
				t.Helper()
				t.Setenv("LUA_PATH", "")

				return filepath.Join(work, ".rocks", "share", "tarantool")
			},
			mods: planted,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			work := t.TempDir()
			markers := t.TempDir()

			plantModules(t, c.dir(t, work), markers, c.mods)

			// ./?.lua resolves against the process directory, not WorkingDir.
			t.Chdir(work)

			e := client.NewLuaEngine(rocks.Config{
				Tree:       filepath.Join(work, ".rocks"),
				WorkingDir: work,
			}, manif.FileStore{}, discardLogger())

			for _, argv := range [][]string{{"list"}, {"path"}, {"help"}} {
				require.NoError(t, e.Call(argv), "%v", argv)
			}

			requireNoMarkers(t, markers)

			path, err := e.FreshPackagePath()
			require.NoError(t, err, "fresh VM")
			require.Empty(t, path, "package.path of a fresh VM")
		})
	}
}

// TestLuaEngine_ExternalCommandLoadsOnlyWhenRun installs an external command
// into the user rock tree: the commands that do not name it must not load it,
// and naming it — as the command, after global options, or as the subject of
// help — must still reach it.
func TestLuaEngine_ExternalCommandLoadsOnlyWhenRun(t *testing.T) {
	t.Parallel()

	work := t.TempDir()
	tree := filepath.Join(work, ".rocks")
	markers := t.TempDir()

	mark := func(name string) string {
		return "local f = io.open(" + quoteLuaString(filepath.Join(markers, name)) + ", 'a') f:write('x') f:close()"
	}

	module := filepath.Join(tree, "share", "tarantool", "luarocks", "cmd", "external", "planted.lua")
	require.NoError(t, os.MkdirAll(filepath.Dir(module), 0o755))
	require.NoError(t, os.WriteFile(module, []byte(mark("loaded")+"\n"+
		"return { help = 'a planted command', help_summary = 'Plant a command.', command = function() "+mark("ran")+" return true end }\n"), 0o644))

	e := client.NewLuaEngine(rocks.Config{Tree: tree, WorkingDir: work}, manif.FileStore{}, discardLogger())

	markersAfter := func(argv ...string) []string {
		t.Helper()

		require.NoError(t, os.RemoveAll(markers))
		require.NoError(t, os.MkdirAll(markers, 0o755))

		_, err := e.Output(argv)
		require.NoError(t, err, "%v", argv)

		entries, err := os.ReadDir(markers)
		require.NoError(t, err, "read markers")

		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}

		return names
	}

	for _, argv := range [][]string{{"list"}, {"path"}, {"help"}, {"--tree", tree, "list"}, {"help", "list"}} {
		require.Empty(t, markersAfter(argv...), "%v loaded the external command", argv)
	}

	require.Equal(t, []string{"loaded", "ran"}, markersAfter("planted"), "planted")
	require.Equal(t, []string{"loaded", "ran"}, markersAfter("--tree", tree, "planted"), "--tree <tree> planted")
	require.Equal(t, []string{"loaded"}, markersAfter("help", "planted"), "help planted")

	out, err := e.Output([]string{"help", "planted"})
	require.NoError(t, err, "help planted")
	require.Contains(t, out, "a planted command", "help planted")
}

// TestLuaEngine_OptionalProbesStayInPreload is the gate on luaAbsentModules
// being complete: no require an ordinary command makes reaches package.path.
// A failure names a module LuaRocks started probing for; add it to the list,
// or, if it is a plugin point, say so next to the list instead.
func TestLuaEngine_OptionalProbesStayInPreload(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, discardLogger())

	names, err := e.UnresolvedRequires(
		[]string{"help"}, []string{"list"}, []string{"path"}, []string{"config"},
		[]string{"install", "--help"}, []string{"make", "--help"},
	)
	require.NoError(t, err, "dispatch")
	require.Empty(t, names, "requires that looked on package.path")
}

// TestLuaEngine_PathPrintsStockPackagePath pins the path command's output to
// what the engine printed while it kept gopher-lua's default package.path —
// the want strings were recorded from that engine — although the VM no longer
// searches that path. Not parallel: it changes the process directory and the
// environment.
func TestLuaEngine_PathPrintsStockPackagePath(t *testing.T) {
	const (
		trees = "{work}/.rocks/share/tarantool/?.lua;{work}/.rocks/share/tarantool/?/init.lua;" +
			"{prefix}/share/tarantool/?.lua;{prefix}/share/tarantool/?/init.lua"
		gopherDefault = "./?.lua;/usr/local/share/lua/5.1/?.lua;/usr/local/share/lua/5.1/?/init.lua"
		wantCPath     = "export LUA_CPATH=';{work}/.rocks/lib/tarantool/?.so;{prefix}/lib/tarantool/?.so'"
	)

	cases := []struct {
		name    string
		luaPath string
		want    string
	}{
		{
			name: "LUA_PATH unset",
			want: gopherDefault + ";" + trees,
		},
		{
			name:    "LUA_PATH set",
			luaPath: "{lp}/?.lua",
			want:    "{lp}/?.lua;" + trees,
		},
		{
			// gopher-lua expands ";;" to ";<default>;", which leaves an empty
			// entry in front of the trees.
			name:    "LUA_PATH with the default",
			luaPath: "{lp}/?.lua;;",
			want:    "{lp}/?.lua;" + gopherDefault + ";;" + trees,
		},
		{
			name:    "LUA_PATH with duplicates",
			luaPath: "{lp}/?.lua;{work}/.rocks/share/tarantool/?/init.lua;;{lp}/?.lua;./?.lua",
			want: "{lp}/?.lua;{work}/.rocks/share/tarantool/?/init.lua;" + gopherDefault + ";" +
				"{work}/.rocks/share/tarantool/?.lua;{prefix}/share/tarantool/?.lua;{prefix}/share/tarantool/?/init.lua",
		},
		{
			name:    "LUA_PATH starting with a tree",
			luaPath: "{prefix}/share/tarantool/?.lua;{lp}/?.lua",
			want: "{prefix}/share/tarantool/?.lua;{lp}/?.lua;" +
				"{work}/.rocks/share/tarantool/?.lua;{work}/.rocks/share/tarantool/?/init.lua;" +
				"{prefix}/share/tarantool/?/init.lua",
		},
	}

	for _, c := range cases {
		for _, argv := range [][]string{{"path"}, {"path", "--append"}} {
			t.Run(c.name+" "+strings.Join(argv, " "), func(t *testing.T) {
				work := t.TempDir()
				lp := t.TempDir()
				prefix := t.TempDir()
				markers := t.TempDir()

				expand := strings.NewReplacer("{work}", work, "{lp}", lp, "{prefix}", prefix).Replace

				t.Setenv("LUA_PATH", expand(c.luaPath))
				t.Setenv("LUAROCKS_SYSCONFDIR", t.TempDir())

				plantModules(t, work, markers, append([]string{plantedExternal}, plantedProbes...))
				plantModules(t, lp, markers, append([]string{plantedExternal}, plantedProbes...))
				t.Chdir(work)

				e := client.NewLuaEngine(rocks.Config{
					Tree:       filepath.Join(work, ".rocks"),
					WorkingDir: work,
					Tarantool:  rocks.TarantoolConfig{Prefix: prefix},
				}, manif.FileStore{}, discardLogger())

				out, err := e.Output(argv)
				require.NoError(t, err, "path")

				var got []string

				for line := range strings.SplitSeq(out, "\n") {
					if strings.HasPrefix(line, "export LUA_PATH=") || strings.HasPrefix(line, "export LUA_CPATH=") {
						got = append(got, line)
					}
				}

				require.Equal(t, []string{
					expand("export LUA_PATH='" + c.want + "'"),
					expand(wantCPath),
				}, got, "path output")

				requireNoMarkers(t, markers)
			})
		}
	}
}

// TestLuaEngine_PathShimSwapsPackagePathForTheCommand checks that the path
// shim shows upstream's command the stock package.path and puts the engine's
// back afterwards, also when the command fails.
func TestLuaEngine_PathShimSwapsPackagePathForTheCommand(t *testing.T) {
	t.Parallel()

	e := client.NewLuaEngine(luaTestCfg(t), manif.FileStore{}, discardLogger())

	got := luaProbe(t, e, `(function()
		local upstream = require('extra.upstream.luarocks.cmd.path')
		local shim = require('luarocks.cmd.path')
		local engine = package.path

		local seen
		upstream.command = function() seen = package.path; return true end
		local ok = shim.command({})
		if not ok or package.path ~= engine then return 'not restored after success' end
		if not seen:find(glr_default_package_path(), 1, true) then return 'command saw ' .. seen end

		upstream.command = function() error('boom') end
		local pok, err = pcall(shim.command, {})
		if pok or not tostring(err):find('boom', 1, true) then return 'error lost: ' .. tostring(err) end
		if package.path ~= engine then return 'not restored after an error' end

		return 'ok'
	end)()`)
	require.Equal(t, "ok", got)
}

// TestLuaEngine_PackagePathHoldsOnlyRockTrees checks what package.path holds
// once a dispatch has run: the rock trees luarocks.loader appends
// (cfg.init_package_paths), which is how a build backend installed into a tree
// gets loaded, and nothing else. Not parallel: it sets the environment, to keep
// a host /etc/luarocks config from adding trees.
func TestLuaEngine_PackagePathHoldsOnlyRockTrees(t *testing.T) {
	t.Setenv("LUA_PATH", filepath.Join(t.TempDir(), "?.lua"))
	t.Setenv("LUAROCKS_SYSCONFDIR", t.TempDir())

	work := t.TempDir()
	prefix := t.TempDir()
	userTree := filepath.Join(work, ".rocks")

	e := client.NewLuaEngine(rocks.Config{
		Tree:       userTree,
		WorkingDir: work,
		Tarantool:  rocks.TarantoolConfig{Prefix: prefix},
	}, manif.FileStore{}, discardLogger())

	path := luaProbe(t, e, "package.path")

	entries := strings.Split(path, ";")
	require.Contains(t, entries, filepath.Join(userTree, "share", "tarantool", "?.lua"),
		"the user tree is on package.path")

	for _, entry := range entries {
		if entry == "" {
			continue
		}

		inTree := strings.HasPrefix(entry, userTree+string(filepath.Separator)) ||
			strings.HasPrefix(entry, prefix+string(filepath.Separator))
		require.True(t, inTree, "package.path entry %q is outside the rock trees (package.path %q)", entry, path)
	}
}
