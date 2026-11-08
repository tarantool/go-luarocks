//go:build !(linux || darwin || dragonfly || freebsd || netbsd || openbsd)

package treelock

import "os"

// tryLock takes no lock where flock(2) is missing: it always succeeds.
func tryLock(*os.File, bool) (bool, error) {
	return true, nil
}

// unlock releases nothing.
func unlock(*os.File) error {
	return nil
}

// isReadOnly reports no error as one of a read-only file system.
func isReadOnly(error) bool {
	return false
}
