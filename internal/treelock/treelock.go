// Package treelock takes the advisory lock that guards a rock tree: a lock
// on the file Name in the tree's rocks dir, shared by the readers of the tree
// and exclusive for a change to it. The tree package takes it around its
// loads and its transactions, and the client around the writes of its lua
// backend.
//
// The lock is flock(2) on Linux, macOS and the BSDs, where it holds between
// processes and between two opens in one process alike, and is released when
// the process exits. Elsewhere — Windows among them — taking it always
// succeeds at once: there it guards nothing.
//
// The lock is advisory: it orders only the writers that take it. An external
// `luarocks`, or upstream code the lua backend runs outside a typed call,
// changes a tree without it.
package treelock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Name is the name of the lock file in a tree's rocks dir.
const Name = ".lock"

// ErrBusy is wrapped by the error of Exclusive and Shared when another holder
// keeps the lock until the context ends.
var ErrBusy = errors.New("the lock stayed held")

// fileMode is the mode of a new lock file: readable by everyone, so a reader
// of the tree who cannot write it still takes a shared lock.
const fileMode fs.FileMode = 0o644

// Waits between two attempts to take a lock that is held: the first, and the
// longest, which the doubling wait reaches.
const (
	firstWait = time.Millisecond
	maxWait   = 50 * time.Millisecond
)

// waitGrowth is the factor each wait for a held lock grows by.
const waitGrowth = 2

// Lock is a lock taken on a tree.
type Lock struct {
	f *os.File
}

// Exclusive takes the exclusive lock of the tree whose rocks dir is dir,
// creating its lock file when it has none, and waits for it until ctx ends.
func Exclusive(ctx context.Context, dir string) (*Lock, error) {
	path := filepath.Join(dir, Name)

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open the tree lock: %w", err)
	}

	return take(ctx, f, true)
}

// Shared takes the shared lock of the tree whose rocks dir is dir and waits
// for it until ctx ends. A tree without a lock file has never been changed
// under the lock: when create is false Shared leaves it so, returning a nil
// Lock, and so it does when the lock file cannot be opened for want of
// permission, as for a tree on a read-only file system.
func Shared(ctx context.Context, dir string, create bool) (*Lock, error) {
	path := filepath.Join(dir, Name)

	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
	}

	switch {
	case err == nil:
		return take(ctx, f, false)
	case errors.Is(err, fs.ErrNotExist), errors.Is(err, fs.ErrPermission), isReadOnly(err):
		return nil, nil //nolint:nilnil // no lock to take: the caller reads unlocked
	default:
		return nil, fmt.Errorf("open the tree lock: %w", err)
	}
}

// take locks f, waiting until ctx ends, and closes it on failure.
func take(ctx context.Context, f *os.File, exclusive bool) (*Lock, error) {
	wait := firstWait

	for {
		ok, err := tryLock(f, exclusive)
		if err != nil {
			_ = f.Close()

			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}

		if ok {
			return &Lock{f: f}, nil
		}

		timer := time.NewTimer(wait)

		select {
		case <-ctx.Done():
			timer.Stop()

			_ = f.Close()

			return nil, fmt.Errorf("%w: %s: %w", ErrBusy, f.Name(), context.Cause(ctx))
		case <-timer.C:
		}

		wait = min(waitGrowth*wait, maxWait)
	}
}

// Unlock releases the lock. Unlocking a nil Lock does nothing.
func (l *Lock) Unlock() {
	if l == nil {
		return
	}

	_ = unlock(l.f)
	_ = l.f.Close()
}
