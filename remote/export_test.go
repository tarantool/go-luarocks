package remote

// This file bridges unexported identifiers to the external `remote_test`
// package so white-box tests can exercise them while the test files
// themselves stay in `_test` packages (testpackage).

// ParseRockFileName exposes parseRockFileName, upstream path.parse_name, so
// the differential tests can tell which rocks a set of file names lists.
func ParseRockFileName(fileName string) (name, version, arch string, ok bool) {
	return parseRockFileName(fileName)
}
