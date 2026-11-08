package client_test

import (
	"context"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/tree"
)

// skipUnlocked skips a test of the tree lock where the lock guards nothing.
func skipUnlocked(t *testing.T) {
	t.Helper()

	if !slices.Contains([]string{"linux", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd"}, runtime.GOOS) {
		t.Skipf("no flock(2) on %s: the tree lock guards nothing", runtime.GOOS)
	}
}

// holdTree holds the lock of the tree at root, through tree.Tree.Update,
// until the test ends.
func holdTree(t *testing.T, root string) {
	t.Helper()

	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- tr.Update(context.Background(), func(*tree.Tx) error {
			close(held)
			<-release

			return nil
		})
	}()

	<-held
	t.Cleanup(func() {
		close(release)
		assert.NoError(t, <-done, "the holder's Update")
	})
}

// shortContext returns a context that ends soon.
func shortContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	t.Cleanup(cancel)

	return ctx
}

// TestLuaEngine_WritesTakeTheTreeLock: the typed writes of the lua backend
// wait for the tree's lock as tree.Tree.Update does, so they fail with
// tree.ErrLocked when the context ends while another change holds it, and
// never run upstream then.
func TestLuaEngine_WritesTakeTheTreeLock(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()
	holdTree(t, root)

	e := client.NewLuaEngine(rocks.Config{Tree: root, WorkingDir: t.TempDir()}, nil, nil)
	e.SetCallImpl(func(argv []string) (string, error) {
		t.Errorf("upstream ran without the lock: %v", argv)

		return "", nil
	})

	for name, write := range map[string]func(ctx context.Context) error{
		"Install": func(ctx context.Context) error { return e.Install(ctx, "foo", client.InstallOpts{}) },
		"Build":   func(ctx context.Context) error { return e.Build(ctx, "foo-1.0-1.rockspec", client.BuildOpts{}) },
		"Make":    func(ctx context.Context) error { return e.Make(ctx, client.MakeOpts{}) },
		"Remove":  func(ctx context.Context) error { return e.Remove(ctx, "foo", client.RemoveOpts{}) },
		"Purge":   func(ctx context.Context) error { return e.Purge(ctx, client.PurgeOpts{}) },
	} {
		require.ErrorIs(t, write(shortContext(t)), tree.ErrLocked, "%s of a locked tree", name)
	}
}

// TestLuaEngine_WriteHoldsTheTreeLock: while upstream runs a typed write of
// the lua backend, the tree's lock is held, so a change through
// tree.Tree.Update waits for it.
func TestLuaEngine_WriteHoldsTheTreeLock(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()
	tr, err := tree.Open(rocks.Config{Tree: root})
	require.NoError(t, err)

	ran := false
	e := client.NewLuaEngine(rocks.Config{Tree: root, WorkingDir: t.TempDir()}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) {
		ran = true

		err := tr.Update(shortContext(t), func(*tree.Tx) error { return nil })
		assert.ErrorIs(t, err, tree.ErrLocked, "Update while upstream runs")

		return "", nil
	})

	require.NoError(t, e.Install(context.Background(), "foo", client.InstallOpts{}), "Install")
	assert.True(t, ran, "upstream ran")
	require.NoError(t, tr.Update(shortContext(t), func(*tree.Tx) error { return nil }), "Update once the write is done")
}

// TestNativeRemove_WaitsForTheTreeLock: the native backend's writes go
// through tree.Tree.Update, so they wait for the tree's lock too.
func TestNativeRemove_WaitsForTheTreeLock(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	root := t.TempDir()
	holdTree(t, root)

	r, err := client.New(rocks.Config{Tree: root})
	require.NoError(t, err)
	require.ErrorIs(t, r.Remove(shortContext(t), "foo", client.RemoveOpts{}), tree.ErrLocked, "Remove from a locked tree")
}
