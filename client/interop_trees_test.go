//go:build integration_luaengine

package client_test

// The scenarios of the backend-interoperability harness (interop_test.go)
// that remove rocks and rewrite the tree manifest.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

// TestBackendInterop_Remove has B remove what A installed: hello and its
// dependency dep from installTestRepo, dep while hello depends on it, with
// and without Force and ForceFast. The native Remove removes dep either way,
// and recomputes no rock's resolved dependencies, where upstream refuses
// without a force and recomputes them with one (divRemoveDependents), so
// those scenarios end where B's backend alone ends, and a reinstall by A
// after them where A's does.
func TestBackendInterop_Remove(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	hello := func(role interopRole) interopStep {
		return installStep(role, 0, repo, "hello")
	}
	rm := removeStep
	both := []string{"hello", "dep"}

	const div = divRemoveDependents

	scenarios := []interopScenario{
		{name: "dependency", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{}),
		}, show: both, which: both, diverges: div, follow: followB},
		{name: "dependency/force", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{Force: true}),
		}, show: both, which: both, diverges: div, follow: followB},
		{name: "dependency/force-fast", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{ForceFast: true}),
		}, show: both, which: both, diverges: div, follow: followB},
		// The native backend's removal rewrites the manifest the lua one
		// wrote, and the lua reinstall then reads it.
		{name: "dependency/force/reinstall", steps: []interopStep{
			hello(roleA), rm(roleB, "dep", client.RemoveOpts{Force: true}), hello(roleA),
		}, diverges: div, follow: followA},
		{name: "dependent-then-dependency", steps: []interopStep{
			hello(roleA), rm(roleB, "hello", client.RemoveOpts{}), rm(roleB, "dep", client.RemoveOpts{}),
		}, show: both, which: both},
		{name: "absent", steps: []interopStep{hello(roleA), rm(roleB, "nosuchrock", client.RemoveOpts{})}},
		{name: "absent-version", steps: []interopStep{hello(roleA), rm(roleB, "dep", client.RemoveOpts{Version: "9.9-1"})}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			testInterop(t, prefix, incDir, sc)
		})
	}
}

// TestBackendInterop_ManifestRoundTrip has B rewrite a tree manifest A wrote
// for a tree of several rocks — two versions of one, a dependency chain, rocks
// sharing a module and a command, a rock with a dependency from
// installTestRepo — by installing an unrelated rock, then by removing it
// again: every entry of the rocks B did not touch must come out as A left it.
// The second version is built with Keep, the one way upstream keeps the
// first. The lua backend recomputes every rock's resolved dependencies,
// transitively, when it writes the manifest and the native one leaves them as
// they are (divResolvedDeps), so each direction ends where the lua backend
// alone ends: a lua B recomputes what a native A wrote, and a native B keeps
// what a lua A wrote.
func TestBackendInterop_ManifestRoundTrip(t *testing.T) {
	prefix, incDir := parityTarantool(t)
	repo := installTestRepo(t)

	var mk, bd parityOp

	for _, op := range parityOps() {
		switch op.name {
		case "make":
			mk = op
		case "build":
			bd = op
		}
	}

	written := []interopStep{
		deployStep(roleA, mk, vrock("1.0"), false),
		deployStep(roleA, bd, vrock("2.0"), true),
		deployStep(roleA, mk, dependent("mid", "vrock"), false),
		deployStep(roleA, mk, dependent("top", "mid"), false),
		deployStep(roleA, mk, sharer("aaa"), false),
		deployStep(roleA, mk, sharer("bbb"), false),
		deployStep(roleA, mk, formRock("ddd", true), false),
		deployStep(roleA, mk, formRock("ccc", false), false),
		installStep(roleA, 0, repo, "hello"),
	}
	unrelated := deployStep(roleB, mk, namedItems("zzz", "zzz.extra", "zzz-cli"), false)

	toLua := map[string]client.Backend{nativeWrites: client.BackendLua, luaWrites: client.BackendLua}

	for _, sc := range []interopScenario{
		{name: "install", steps: append(slices.Clone(written), unrelated), diverges: divResolvedDeps, follow: toLua},
		{name: "install-remove", steps: append(slices.Clone(written), unrelated,
			removeStep(roleB, "zzz", client.RemoveOpts{})), diverges: divResolvedDeps, follow: toLua},
	} {
		t.Run(sc.name, func(t *testing.T) {
			t.Parallel()
			testInterop(t, prefix, incDir, sc)
		})
	}
}

// TestBackendInterop_ConcurrentWritesIntoANewTree installs a rock through
// each backend at once into one tree that does not exist yet, a few times.
// Every write takes the tree's lock, the lua backend's first one into the
// fresh tree too, so the two run one after the other: neither drops the
// manifest entry the other wrote, and the tree keeps the lock file.
func TestBackendInterop_ConcurrentWritesIntoANewTree(t *testing.T) {
	t.Parallel()

	prefix, incDir := parityTarantool(t)

	repo := t.TempDir()
	writeTarballRock(t, repo, "alpha", "", "", nil)
	writeTarballRock(t, repo, "beta", "", "", nil)
	require.NoError(t, os.WriteFile(filepath.Join(repo, "manifest"), []byte(`commands = {}
modules = {}
repository = {
   alpha = { ["1.0-1"] = { { arch = "rockspec" } } },
   beta = { ["1.0-1"] = { { arch = "rockspec" } } },
}
`), 0o600), "write the manifest")

	rockOf := map[client.Backend]string{client.BackendNative: "alpha", client.BackendLua: "beta"}

	for round := range 3 {
		root := filepath.Join(t.TempDir(), "new", "tree")

		clients := map[client.Backend]*client.Rocks{}
		for backend := range rockOf {
			clients[backend] = newClient(t, rocks.Config{
				Tree:       root,
				WorkingDir: t.TempDir(),
				Servers:    []string{repo},
				Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: parityTarantoolVersion},
			}, client.WithBackend(backend))
		}

		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs = map[client.Backend]error{}
		)

		for backend, r := range clients {
			wg.Go(func() {
				err := r.Install(context.Background(), rockOf[backend], client.InstallOpts{Deps: client.DepsNone})

				mu.Lock()
				errs[backend] = err
				mu.Unlock()
			})
		}

		wg.Wait()

		for backend, err := range errs {
			require.NoError(t, err, "round %d: the %v backend's Install of %s", round, backend, rockOf[backend])
		}

		v, err := tree.Inspect(context.Background(), root)
		require.NoError(t, err, "round %d: Inspect", round)

		for backend, name := range rockOf {
			_, ok := v.Installed(name, "1.0-1")
			assert.True(t, ok, "round %d: the tree manifest lists %s, which the %v backend installed", round, name, backend)
		}

		assert.FileExists(t, tree.Paths{Tree: root}.LockFile(), "round %d: the lock file", round)
	}
}
