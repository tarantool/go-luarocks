//go:build !unix

package testenv

// SetSuiteUmask does nothing where there is no umask.
func SetSuiteUmask() {}
