package client_test

// Dispatch tests for the luaEngine write methods. These exercise the
// argv-construction logic in isolation by injecting the callImpl test seam (via
// SetCallImpl) — it records the argv the method builds and returns a canned
// stdout — so we never boot the embedded VM here. The real end-to-end path is
// covered by lua_integration_test.go.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// newRecorderEngine returns a luaEngine wired with a callImpl that records the
// last argv it received and returns the supplied stdout. The cfg.Tree is fixed
// so the global --tree prefix is deterministic across tests.
func newRecorderEngine(stdout string) (*client.LuaEngine, *[]string) {
	var got []string

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree"}, nil, nil)
	e.SetCallImpl(func(argv []string) (string, error) {
		got = argv

		return stdout, nil
	})

	return e, &got
}

func TestLuaEngine_Install_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Install(context.Background(), "foo", client.InstallOpts{
		Version: "1.0-1",
		Servers: []string{"https://a.example", "https://b.example"},
		Deps:    client.DepsNone,
	})
	require.NoError(t, err, "Install")

	want := []string{
		"--tree", "/opt/tree",
		"--server", "https://a.example",
		"--server", "https://b.example",
		"install", "foo", "1.0-1",
		"--deps-mode", "none",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Build_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Build(context.Background(), "spec.rockspec", client.BuildOpts{Keep: true})
	require.NoError(t, err, "Build")

	want := []string{"--tree", "/opt/tree", "build", "spec.rockspec", "--keep"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Make_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Make(context.Background(), client.MakeOpts{RockspecPath: "foo-1.0-1.rockspec"})
	require.NoError(t, err, "Make")

	want := []string{"--tree", "/opt/tree", "make", "foo-1.0-1.rockspec"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Pack_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("Packed: /opt/tree/foo-1.0-1.all.rock\n")
	path, err := e.Pack(context.Background(), "foo", client.PackOpts{})
	require.NoError(t, err, "Pack")

	want := []string{"--tree", "/opt/tree", "pack", "foo"}
	assert.Equal(t, want, *got, "argv")
	assert.Equal(t, "/opt/tree/foo-1.0-1.all.rock", path, "Pack path")
}

func TestLuaEngine_Unpack_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Unpack(context.Background(), "/tmp/foo-1.0-1.all.rock", "/tmp/dest")
	require.NoError(t, err, "Unpack")

	want := []string{"--tree", "/opt/tree", "unpack", "/tmp/foo-1.0-1.all.rock"}
	assert.Equal(t, want, *got, "argv")
}

// --- dispatch tests for the thirteen new write methods ---

func TestLuaEngine_Remove_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Remove(context.Background(), "foo", client.RemoveOpts{
		Version:   "1.0-1",
		Force:     true,
		ForceFast: true,
		Deps:      client.DepsNone,
	})
	require.NoError(t, err, "Remove")

	want := []string{
		"--tree", "/opt/tree", "remove", "foo", "1.0-1",
		"--force", "--force-fast", "--deps-mode", "none",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Purge_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Purge(context.Background(), client.PurgeOpts{OldVersions: true, Force: true})
	require.NoError(t, err, "Purge")

	want := []string{"--tree", "/opt/tree", "purge", "--old-versions", "--force"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Search_Argv_AndParse(t *testing.T) {
	t.Parallel()

	// Two porcelain listing lines: name\tversion\tarch\trepo\tnamespace.
	stdout := "luafun\t0.1.3-1\tsrc\thttps://luarocks.org\t\n" +
		"luafun\t0.1.3-1\trockspec\thttps://luarocks.org\t\n"
	e, got := newRecorderEngine(stdout)
	res, err := e.Search(context.Background(), "luafun", client.SearchOpts{
		Version: "0.1.3",
		Source:  true,
		Servers: []string{"https://luarocks.org"},
	})
	require.NoError(t, err, "Search")

	want := []string{
		"--tree", "/opt/tree", "--server", "https://luarocks.org",
		"search", "luafun", "0.1.3", "--source", "--porcelain",
	}
	assert.Equal(t, want, *got, "argv")
	require.Len(t, res, 2, "Search results (%+v)", res)
	assert.Equal(t, "luafun", res[0].Name, "result[0].Name")
	assert.Equal(t, "0.1.3-1", res[0].Version, "result[0].Version")
	assert.Equal(t, "https://luarocks.org", res[0].Server, "result[0].Server")
}

func TestLuaEngine_Search_EmptyOutput_NoError(t *testing.T) {
	t.Parallel()

	e, _ := newRecorderEngine("")
	res, err := e.Search(context.Background(), "nope", client.SearchOpts{})
	require.NoError(t, err, "Search empty")
	assert.Empty(t, res, "Search empty results")
}

func TestLuaEngine_Download_Argv_ReturnsNewFilePath(t *testing.T) {
	t.Parallel()

	// Upstream prints no saved path; Download diffs the download dir instead.
	// The recorder simulates the file the real dispatch would have written.
	work := t.TempDir()

	var got []string

	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree", WorkingDir: work}, nil, nil)
	e.SetCallImpl(func(argv []string) (string, error) {
		got = argv

		require.NoError(t, os.WriteFile(filepath.Join(work, "foo-1.0-1.src.rock"), []byte("x"), 0o600))

		return "", nil
	})
	path, err := e.Download(context.Background(), "foo", client.DownloadOpts{
		Version: "1.0-1",
		Source:  true,
		Servers: []string{"https://luarocks.org"},
	})
	require.NoError(t, err, "Download: want success")
	assert.Equal(t, filepath.Join(work, "foo-1.0-1.src.rock"), path, "path")

	want := []string{
		"--tree", "/opt/tree", "--server", "https://luarocks.org",
		"download", "foo", "1.0-1", "--source",
	}
	assert.Equal(t, want, got, "argv")
}

func TestLuaEngine_Download_AmbiguousReturnsDir(t *testing.T) {
	t.Parallel()

	// No new file appeared (e.g. an existing download was overwritten): Download
	// returns the download directory rather than erroring on a real success.
	work := t.TempDir()
	e := client.NewLuaEngine(rocks.Config{Tree: "/opt/tree", WorkingDir: work}, nil, nil)
	e.SetCallImpl(func(argv []string) (string, error) { return "", nil })
	path, err := e.Download(context.Background(), "foo", client.DownloadOpts{})
	require.NoError(t, err, "Download: want success")
	assert.Equal(t, work, path, "path, want download dir")
}

func TestLuaEngine_Lint_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Lint(context.Background(), "foo-1.0-1.rockspec", client.LintOpts{})
	require.NoError(t, err, "Lint")

	want := []string{"lint", "foo-1.0-1.rockspec"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_NewVersion_Argv_AndParse(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("Wrote /work/foo-2.0-1.rockspec\n")
	path, err := e.NewVersion(context.Background(), "foo-1.0-1.rockspec", client.NewVersionOpts{
		NewVersion: "2.0",
		Tag:        "v2.0",
		Dir:        "/work",
	})
	require.NoError(t, err, "NewVersion")

	want := []string{
		"new_version", "foo-1.0-1.rockspec", "2.0", "--dir", "/work", "--tag", "v2.0",
	}
	assert.Equal(t, want, *got, "argv")
	assert.Equal(t, "/work/foo-2.0-1.rockspec", path, "NewVersion path")
}

func TestLuaEngine_NewVersion_NoWroteLine_Error(t *testing.T) {
	t.Parallel()

	e, _ := newRecorderEngine("nothing useful here\n")
	_, err := e.NewVersion(context.Background(), "foo.rockspec", client.NewVersionOpts{})
	require.Error(t, err, "NewVersion: want error when no 'Wrote' line")
}

func TestLuaEngine_WriteRockspec_Argv_AndParse(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("Wrote template at /work/foo-1.0-1.rockspec -- you should now edit and finish it.\n")
	path, err := e.WriteRockspec(context.Background(), "https://example.com/foo", client.WriteRockspecOpts{
		Name:    "foo",
		Version: "1.0",
		License: "MIT",
		Summary: "a thing",
	})
	require.NoError(t, err, "WriteRockspec")

	want := []string{
		"write_rockspec", "foo", "1.0", "https://example.com/foo",
		"--license", "MIT", "--summary", "a thing",
	}
	assert.Equal(t, want, *got, "argv")
	assert.Equal(t, "/work/foo-1.0-1.rockspec", path, "WriteRockspec path")
}

func TestLuaEngine_Doc_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Doc(context.Background(), "foo", client.DocOpts{Version: "1.0-1", List: true})
	require.NoError(t, err, "Doc")

	want := []string{"--tree", "/opt/tree", "doc", "foo", "1.0-1", "--list"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Test_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Test(context.Background(), "foo-1.0-1.rockspec", client.TestOpts{
		Prepare:  true,
		TestType: "busted",
		Args:     []string{"--verbose"},
	})
	require.NoError(t, err, "Test")

	want := []string{
		"--tree", "/opt/tree", "test", "foo-1.0-1.rockspec",
		"--prepare", "--test-type", "busted", "--verbose",
	}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Config_Argv_AndParse(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("/usr/local\n")
	val, err := e.Config(context.Background(), client.ConfigOpts{Key: "lua_dir"})
	require.NoError(t, err, "Config")

	want := []string{"config", "lua_dir"}
	assert.Equal(t, want, *got, "argv")
	assert.Equal(t, "/usr/local", val, "Config value")
}

func TestLuaEngine_Upload_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Upload(context.Background(), "foo-1.0-1.rockspec", client.UploadOpts{
		APIKey: "abc",
		Sign:   true,
	})
	require.NoError(t, err, "Upload")

	want := []string{"upload", "foo-1.0-1.rockspec", "--api-key", "abc", "--sign"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_InitProject_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.InitProject(context.Background(), client.InitProjectOpts{
		Name:    "myproj",
		Version: "0.1",
		Reset:   true,
	})
	require.NoError(t, err, "InitProject")

	want := []string{"init", "myproj", "0.1", "--reset"}
	assert.Equal(t, want, *got, "argv")
}

func TestLuaEngine_Admin_Argv(t *testing.T) {
	t.Parallel()

	e, got := newRecorderEngine("")
	err := e.Admin(context.Background(), "add", []string{"/path/to/foo.rock"}, client.AdminOpts{
		Server: "https://my.server",
		Force:  true,
	})
	require.NoError(t, err, "Admin")

	want := []string{
		"admin", "add", "/path/to/foo.rock", "--server", "https://my.server", "--force",
	}
	assert.Equal(t, want, *got, "argv")
}
