package client_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
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

	for name, write := range luaTypedWrites(e) {
		require.ErrorIs(t, write(shortContext(t)), tree.ErrLocked, "%s of a locked tree", name)
	}
}

// luaTypedWrites returns the typed writes of the lua engine e, by name.
func luaTypedWrites(e *client.LuaEngine) map[string]func(ctx context.Context) error {
	return map[string]func(ctx context.Context) error{
		"Install": func(ctx context.Context) error { return e.Install(ctx, "foo", client.InstallOpts{}) },
		"Build":   func(ctx context.Context) error { return e.Build(ctx, "foo-1.0-1.rockspec", client.BuildOpts{}) },
		"Make":    func(ctx context.Context) error { return e.Make(ctx, client.MakeOpts{}) },
		"Remove":  func(ctx context.Context) error { return e.Remove(ctx, "foo", client.RemoveOpts{}) },
		"Purge":   func(ctx context.Context) error { return e.Purge(ctx, client.PurgeOpts{}) },
	}
}

// TestLuaEngine_WriteIntoANewTreeTakesTheTreeLock: a typed write of the lua
// backend into a tree that does not exist yet creates the tree's rocks dir
// and lock file, with the modes the native backend gives them, and holds the
// lock while upstream runs, so a native write into the same fresh tree at
// that time waits for it, as it does for an existing tree.
func TestLuaEngine_WriteIntoANewTreeTakesTheTreeLock(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	for name := range luaTypedWrites(nil) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			base := t.TempDir()
			root := filepath.Join(base, "new", "tree")
			paths := tree.Paths{Tree: root}

			native, err := client.New(rocks.Config{Tree: root})
			require.NoError(t, err)

			ran := false
			e := client.NewLuaEngine(rocks.Config{Tree: root, WorkingDir: t.TempDir()}, nil, nil)
			e.SetCallImpl(func([]string) (string, error) {
				ran = true

				lock, err := os.Lstat(paths.LockFile())
				if assert.NoError(t, err, "the lock file while upstream runs") {
					assert.True(t, lock.Mode().IsRegular(), "the lock file is a regular file")
					assert.Equal(t, fs.FileMode(0o644), lock.Mode().Perm(), "the lock file's mode")
				}

				for dir := paths.RocksDir(); dir != base; dir = filepath.Dir(dir) {
					st, err := os.Stat(dir)
					if assert.NoError(t, err, "%s while upstream runs", dir) {
						assert.Equal(t, fs.ModeDir|0o755, st.Mode(), "the mode of %s", dir)
					}
				}

				err = native.Remove(shortContext(t), "foo", client.RemoveOpts{})
				assert.ErrorIs(t, err, tree.ErrLocked, "a native write while upstream runs")

				return "", nil
			})

			require.NoError(t, luaTypedWrites(e)[name](context.Background()), name)
			assert.True(t, ran, "upstream ran")

			err = native.Remove(shortContext(t), "foo", client.RemoveOpts{})
			require.NotErrorIs(t, err, tree.ErrLocked, "a native write once the lua one is done")
		})
	}
}

// TestLuaEngine_WriteLocksARelativeTreeInTheWorkingDir: upstream takes a
// relative --tree from the directory it runs in, Config.WorkingDir, so that
// is where a typed write of the lua backend creates and locks the tree, and
// it creates nothing in the process's current directory.
func TestLuaEngine_WriteLocksARelativeTreeInTheWorkingDir(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	work := t.TempDir()
	rel := filepath.Join("glr-relative-tree-test", "tree")

	e := client.NewLuaEngine(rocks.Config{Tree: rel, WorkingDir: work}, nil, nil)
	e.SetCallImpl(func([]string) (string, error) {
		assert.FileExists(t, tree.Paths{Tree: filepath.Join(work, rel)}.LockFile(), "the lock file in the working dir")

		return "", nil
	})

	require.NoError(t, e.Install(context.Background(), "foo", client.InstallOpts{}), "Install")
	assert.NoDirExists(t, filepath.Dir(rel), "the tree in the process's current directory")
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
