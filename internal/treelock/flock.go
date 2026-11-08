//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package treelock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes the flock(2) lock of f without waiting and reports whether it
// got it.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}

	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)

		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return false, err
		}
	}
}

// unlock releases the flock(2) lock of f.
func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// isReadOnly reports whether err says the file system is read-only.
func isReadOnly(err error) bool {
	return errors.Is(err, syscall.EROFS)
}
