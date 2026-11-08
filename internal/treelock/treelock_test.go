package treelock_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/treelock"
)

// skipUnlocked skips a test of the lock's exclusion where the lock guards
// nothing.
func skipUnlocked(t *testing.T) {
	t.Helper()

	if !slices.Contains([]string{"linux", "darwin", "dragonfly", "freebsd", "netbsd", "openbsd"}, runtime.GOOS) {
		t.Skipf("no flock(2) on %s: the lock guards nothing", runtime.GOOS)
	}
}

// expired returns a context that has already ended.
func expired(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	t.Cleanup(cancel)

	return ctx
}

func TestExclusive_ExcludesEveryOther(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	dir := t.TempDir()

	l, err := treelock.Exclusive(context.Background(), dir)
	require.NoError(t, err, "Exclusive")

	_, err = treelock.Exclusive(expired(t), dir)
	require.ErrorIs(t, err, treelock.ErrBusy, "a second Exclusive waits until its context ends")
	require.ErrorIs(t, err, context.DeadlineExceeded, "and says why it stopped waiting")

	_, err = treelock.Shared(expired(t), dir, false)
	require.ErrorIs(t, err, treelock.ErrBusy, "Shared waits for Exclusive")

	l.Unlock()

	l, err = treelock.Exclusive(expired(t), dir)
	require.NoError(t, err, "Exclusive once the lock is released")
	l.Unlock()
}

func TestShared_SharesWithShared(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	dir := t.TempDir()

	a, err := treelock.Shared(context.Background(), dir, true)
	require.NoError(t, err, "Shared")
	require.NotNil(t, a, "Shared creates the lock file when asked to")

	b, err := treelock.Shared(expired(t), dir, false)
	require.NoError(t, err, "a second Shared does not wait")
	require.NotNil(t, b)

	_, err = treelock.Exclusive(expired(t), dir)
	require.ErrorIs(t, err, treelock.ErrBusy, "Exclusive waits for Shared")

	a.Unlock()
	b.Unlock()
}

// TestExclusive_WaitsForRelease: a waiting Exclusive gets the lock once its
// holder lets go.
func TestExclusive_WaitsForRelease(t *testing.T) {
	t.Parallel()
	skipUnlocked(t)

	dir := t.TempDir()

	l, err := treelock.Exclusive(context.Background(), dir)
	require.NoError(t, err)

	got := make(chan error, 1)

	go func() {
		w, err := treelock.Exclusive(context.Background(), dir)
		w.Unlock()

		got <- err
	}()

	select {
	case err := <-got:
		t.Fatalf("a second Exclusive got the lock while it was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	l.Unlock()
	require.NoError(t, <-got, "the waiting Exclusive gets the lock")
}

// TestShared_NoLockFile: a reader that may not create the lock file reads
// unlocked, and leaves the tree as it is.
func TestShared_NoLockFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	l, err := treelock.Shared(context.Background(), dir, false)
	require.NoError(t, err)
	assert.Nil(t, l, "no lock file, no lock")
	l.Unlock()

	_, err = os.Stat(filepath.Join(dir, treelock.Name))
	require.ErrorIs(t, err, os.ErrNotExist, "Shared created no lock file")

	l, err = treelock.Shared(context.Background(), filepath.Join(dir, "missing"), true)
	require.NoError(t, err, "a missing dir reads unlocked")
	assert.Nil(t, l)
}
