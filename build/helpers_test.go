//nolint:testpackage // white-box: shared helpers used by in-package integration tests (build tag) and re-exported via export_test.go
package build

import (
	"os"
	"path/filepath"
	"strings"
)

// writeFile is a test helper: create parents, then write content with 0644.
func writeFile(path, content string) error {
	err := os.MkdirAll(filepath.Dir(path), 0o750)
	if err != nil {
		return err
	}

	return os.WriteFile(path, []byte(content), 0o600)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)

	return err == nil
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // test helper reads a test-controlled path
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}

func chmodX(path string) error {
	return os.Chmod(path, 0o750) //nolint:gosec // test helper marks a stub executable
}
