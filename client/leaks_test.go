package client_test

// Resource leaks of the client: file descriptors over repeated installs,
// failing fetches and unpacks on the native backend, the staging dirs a
// failing operation leaves in the working directory, and the heap a sequence
// of lua-backend clients keeps. The goroutines every test leaves are checked
// by TestMain.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// leakRuns is how many times a leak test repeats its operation.
const leakRuns = 20

// requireEmptyDir requires dir, the working directory of an operation that
// has returned, to hold nothing: the native backend stages a fetch or a build
// in it and must remove the staging dir whether the operation succeeds or not.
func requireEmptyDir(t *testing.T, dir, what string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	require.Empty(t, names, "%s left these in its working dir %s", what, dir)
}

// TestNativeInstall_NoFDLeak installs a rock and its dependency into a fresh
// tree again and again and requires the process to hold no more file
// descriptors afterwards.
//
//nolint:paralleltest // It counts the file descriptors of the process.
func TestNativeInstall_NoFDLeak(t *testing.T) {
	repo := installTestRepo(t)

	testenv.RequireNoFDLeak(t, leakRuns, func(int) {
		tree, work := t.TempDir(), t.TempDir()

		require.NoError(t, newNativeClient(t, tree, work).Install(context.Background(), "hello",
			client.InstallOpts{Servers: []string{repo}, Deps: client.DepsAll}))
		requireDeployed(t, tree)
		requireEmptyDir(t, work, "Install")
	})
}

// brokenRepo writes a directory server listing one rock, name 1.0-1, whose
// rockspec has source as its source.url and build as its build table, and
// returns the directory and the rockspec's path.
func brokenRepo(t *testing.T, name, source, build string) (string, string) {
	t.Helper()

	repo := t.TempDir()
	spec := filepath.Join(repo, name+"-1.0-1.rockspec")

	require.NoError(t, os.WriteFile(spec, []byte(fmt.Sprintf(
		"package = %q\nversion = \"1.0-1\"\nsource = { url = %q }\nbuild = %s\n", name, source, build)), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(fmt.Sprintf(
		"commands = {}\nmodules = {}\nrepository = {\n   [%q] = { [\"1.0-1\"] = { { arch = \"rockspec\" } } },\n}\n", name)), 0o600))

	return repo, spec
}

// TestNativeInstall_FailingFetchNoFDLeak installs from a server that has
// nothing and a rock whose source is not there, again and again: each fetch
// fails, and neither its file descriptors nor its staging dir may stay.
//
//nolint:paralleltest // It counts the file descriptors of the process.
func TestNativeInstall_FailingFetchNoFDLeak(t *testing.T) {
	dead := deadServer(t)
	gone, _ := brokenRepo(t, "gone", "file://"+filepath.Join(t.TempDir(), "gone.tar.gz"),
		`{ type = "builtin", modules = { gone = "gone.lua" } }`)

	testenv.RequireNoFDLeak(t, leakRuns, func(int) {
		for _, server := range []string{dead, gone} {
			work := t.TempDir()

			err := newNativeClient(t, t.TempDir(), work).Install(context.Background(), "gone",
				client.InstallOpts{Servers: []string{server}, Deps: client.DepsNone})
			require.Error(t, err, "Install from %s", server)
			requireEmptyDir(t, work, "a failed Install")
		}
	})
}

// TestNativeUnpack_NoFDLeak unpacks a packed rock again and again.
//
//nolint:paralleltest // It counts the file descriptors of the process.
func TestNativeUnpack_NoFDLeak(t *testing.T) {
	repo := installTestRepo(t)
	tree := t.TempDir()
	r := newNativeClient(t, tree, t.TempDir())

	require.NoError(t, r.Install(context.Background(), "hello",
		client.InstallOpts{Servers: []string{repo}, Deps: client.DepsNone}))

	rock, err := r.Pack(context.Background(), "hello", client.PackOpts{})
	require.NoError(t, err, "Pack")

	testenv.RequireNoFDLeak(t, leakRuns, func(i int) {
		dest := filepath.Join(t.TempDir(), strconv.Itoa(i))
		require.NoError(t, r.Unpack(context.Background(), rock, dest), "Unpack")
	})
}

// TestNative_FailedBuildLeavesNoStaging runs a Build and an Install of a rock
// whose build command fails and requires each to remove the staging dir it
// made in the working directory.
func TestNative_FailedBuildLeavesNoStaging(t *testing.T) {
	t.Parallel()

	tarball := filepath.Join(t.TempDir(), "broken.tar.gz")
	writeTarGz(t, tarball, "broken", map[string]string{"broken.lua": "return {}\n"})

	repo, spec := brokenRepo(t, "broken", "file://"+tarball, `{ type = "command", build_command = "exit 3" }`)

	work := t.TempDir()
	r := newNativeClient(t, t.TempDir(), work)

	require.Error(t, r.Build(context.Background(), spec, client.BuildOpts{}), "Build")
	requireEmptyDir(t, work, "a failed Build")

	require.Error(t, r.Install(context.Background(), "broken",
		client.InstallOpts{Servers: []string{repo}, Deps: client.DepsNone}), "Install")
	requireEmptyDir(t, work, "a failed Install")
}

// heapClients is how many lua-backend clients TestLuaClients_HeapCeiling
// makes one after the other, and heapCeiling how much more heap the last of
// them may leave in use than the first ones: a client that stays reachable
// once dropped keeps its Lua VMs, several megabytes each.
const (
	heapClients = 50
	heapWarm    = 5
	heapCeiling = 32 << 20
)

// TestLuaClients_HeapCeiling makes heapClients lua-backend clients in turn,
// each booting its VM and running a command, and drops each; the heap in use
// after the last must stay within heapCeiling of the heap after the first
// heapWarm.
//
//nolint:paralleltest // It measures the heap of the process.
func TestLuaClients_HeapCeiling(t *testing.T) {
	inUse := func() uint64 {
		runtime.GC()
		runtime.GC()

		var ms runtime.MemStats

		runtime.ReadMemStats(&ms)

		return ms.HeapInuse
	}

	var base uint64

	for i := range heapClients {
		dir := t.TempDir()

		r, err := client.New(rocks.Config{Tree: dir, WorkingDir: dir}, client.WithBackend(client.BackendLua))
		require.NoError(t, err)

		lua, ok := r.Engine().(*client.LuaEngine)
		require.True(t, ok, "the lua backend's engine")

		got, err := lua.Probe("1 + 1")
		require.NoError(t, err)
		require.Equal(t, "2", got)

		if i+1 == heapWarm {
			base = inUse()
		}
	}

	after := inUse()
	t.Logf("heap in use after %d lua clients: %d MiB, after %d: %d MiB", heapWarm, base>>20, heapClients, after>>20)

	if after > base {
		require.LessOrEqual(t, after-base, uint64(heapCeiling),
			"%d more lua clients grew the heap in use by %d MiB", heapClients-heapWarm, (after-base)>>20)
	}
}
