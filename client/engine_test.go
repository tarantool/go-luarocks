package client_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// fakeEngine records the most recent call to each delegated method so the
// dispatcher tests can assert *Rocks forwards verbatim to r.engine. Every
// method returns a sentinel value the test can recognize.
type fakeEngine struct {
	installName string
	installOpts client.InstallOpts

	buildSpec string
	buildOpts client.BuildOpts

	makeOpts client.MakeOpts

	packTarget string
	packOpts   client.PackOpts

	unpackArchive string
	unpackDest    string

	err error
}

func (f *fakeEngine) Install(_ context.Context, name string, opts client.InstallOpts) error {
	f.installName = name
	f.installOpts = opts

	return f.err
}

func (f *fakeEngine) Build(_ context.Context, specPath string, opts client.BuildOpts) error {
	f.buildSpec = specPath
	f.buildOpts = opts

	return f.err
}

func (f *fakeEngine) Make(_ context.Context, opts client.MakeOpts) error {
	f.makeOpts = opts

	return f.err
}

func (f *fakeEngine) Pack(_ context.Context, target string, opts client.PackOpts) (string, error) {
	f.packTarget = target
	f.packOpts = opts

	return "packed-" + target, f.err
}

func (f *fakeEngine) Unpack(_ context.Context, archive, destDir string) error {
	f.unpackArchive = archive
	f.unpackDest = destDir

	return f.err
}

// The thirteen stubs are unused by the dispatcher tests but required to
// satisfy the Engine interface.
func (f *fakeEngine) Remove(context.Context, string, client.RemoveOpts) error { return nil }
func (f *fakeEngine) Purge(context.Context, client.PurgeOpts) error           { return nil }
func (f *fakeEngine) Search(context.Context, string, client.SearchOpts) ([]client.SearchResult, error) {
	return nil, nil
}
func (f *fakeEngine) Download(context.Context, string, client.DownloadOpts) (string, error) {
	return "", nil
}
func (f *fakeEngine) Lint(context.Context, string, client.LintOpts) error { return nil }
func (f *fakeEngine) NewVersion(context.Context, string, client.NewVersionOpts) (string, error) {
	return "", nil
}
func (f *fakeEngine) WriteRockspec(context.Context, string, client.WriteRockspecOpts) (string, error) {
	return "", nil
}
func (f *fakeEngine) Doc(context.Context, string, client.DocOpts) error   { return nil }
func (f *fakeEngine) Test(context.Context, string, client.TestOpts) error { return nil }
func (f *fakeEngine) Config(context.Context, client.ConfigOpts) (string, error) {
	return "", nil
}
func (f *fakeEngine) Upload(context.Context, string, client.UploadOpts) error   { return nil }
func (f *fakeEngine) InitProject(context.Context, client.InitProjectOpts) error { return nil }
func (f *fakeEngine) Admin(context.Context, string, []string, client.AdminOpts) error {
	return nil
}

func TestRocksInstall_DelegatesToEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	r := client.NewRocksWithEngine(fake)
	opts := client.InstallOpts{Version: "1.2.3", Servers: []string{"https://example/"}, Deps: client.DepsNone}
	require.NoError(t, r.Install(context.Background(), "foo", opts), "Install")
	assert.Equal(t, "foo", fake.installName, "engine.Install name")
	// InstallOpts contains a []string field; require.Equal deep-compares slices.
	assert.Equal(t, opts, fake.installOpts, "engine.Install opts")
}

func TestRocksBuild_DelegatesToEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	r := client.NewRocksWithEngine(fake)
	opts := client.BuildOpts{Keep: true}
	require.NoError(t, r.Build(context.Background(), "foo.rockspec", opts), "Build")
	assert.Equal(t, "foo.rockspec", fake.buildSpec, "engine.Build specPath")
	assert.Equal(t, opts, fake.buildOpts, "engine.Build opts")
}

func TestRocksMake_DelegatesToEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	r := client.NewRocksWithEngine(fake)
	opts := client.MakeOpts{RockspecPath: "x.rockspec"}
	require.NoError(t, r.Make(context.Background(), opts), "Make")
	assert.Equal(t, opts, fake.makeOpts, "engine.Make opts")
}

func TestRocksPack_DelegatesToEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	r := client.NewRocksWithEngine(fake)
	opts := client.PackOpts{SrcOnly: true}
	got, err := r.Pack(context.Background(), "foo", opts)
	require.NoError(t, err, "Pack")
	assert.Equal(t, "foo", fake.packTarget, "engine.Pack target")
	assert.Equal(t, opts, fake.packOpts, "engine.Pack opts")
	assert.Equal(t, "packed-foo", got, "Pack returned value (engine result must pass through)")
}

func TestRocksUnpack_DelegatesToEngine(t *testing.T) {
	t.Parallel()

	fake := &fakeEngine{}
	r := client.NewRocksWithEngine(fake)
	require.NoError(t, r.Unpack(context.Background(), "a.rock", "/dest"), "Unpack")
	assert.Equal(t, "a.rock", fake.unpackArchive, "engine.Unpack archive")
	assert.Equal(t, "/dest", fake.unpackDest, "engine.Unpack destDir")
}

func TestNew_DefaultBackendIsNative(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()})
	require.NoError(t, err, "New")

	_, ok := r.Engine().(*client.NativeEngine)
	assert.True(t, ok, "default engine = %T, want *nativeEngine", r.Engine())
}

func TestNew_WithBackendLua(t *testing.T) {
	t.Parallel()

	r, err := client.New(rocks.Config{Tree: t.TempDir()}, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	// The lua backend swaps the engine to luaEngine; assert the concrete type.
	_, ok := r.Engine().(*client.LuaEngine)
	assert.True(t, ok, "lua engine = %T, want *luaEngine", r.Engine())
}

// guard: fakeEngine must satisfy Engine.
var _ client.Engine = (*fakeEngine)(nil)

// guard: errors.Is wiring for ErrNotImplemented compiles against rocks.
var _ = errors.Is(rocks.ErrNotImplemented, rocks.ErrNotImplemented)
