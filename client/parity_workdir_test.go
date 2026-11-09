//go:build integration_luaengine

package client_test

// Working-dir half of the backend-parity harness: what an operation leaves
// in its Config.WorkingDir once it has returned. The native backend stages a
// fetch, a build and a pack there and must remove the staging dir whether the
// operation succeeds or not.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// stagingPrefixes are the names of the directories the native backend
// stages a fetch, a build, a source download and a pack in, under
// Config.WorkingDir (client/native.go), each followed by a random suffix.
var stagingPrefixes = []string{"rocks-fetch-", "rocks-build-", "rocks-src-", "rocks-pack-"}

// checkWorkDir requires the working directory dir of an operation that has
// returned, successfully or not, to hold nothing the operation left behind
// but build products: none of the native backend's staging directories, and,
// unless inSource says dir is the source dir the operation builds in, which
// a build legitimately writes into, nothing but the top-level entries the
// test created or expects, named by the glob patterns of created (a native
// Build with BuildOpts.Keep keeps its rocks-build-* staging dir by design).
func checkWorkDir(tb testing.TB, label, dir string, created []string, inSource bool) {
	tb.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(tb, err, "[%s] read the working dir %s", label, dir)

	for _, e := range entries {
		name := e.Name()

		if slices.ContainsFunc(created, func(pattern string) bool {
			ok, _ := filepath.Match(pattern, name)

			return ok
		}) {
			continue
		}

		if slices.ContainsFunc(stagingPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
			tb.Errorf("[%s] the operation left its staging dir %q in the working dir %s", label, name, dir)

			continue
		}

		if !inSource {
			tb.Errorf("[%s] the operation left %q in the working dir %s, which held %v", label, name, dir, created)
		}
	}
}

// emptyWorkDir returns a fresh, empty working directory for one operation
// and checks, when the test ends, that the operation left nothing in it but
// the rock a Pack writes there (checkWorkDir). The check runs before
// t.TempDir removes the directory.
func emptyWorkDir(t *testing.T, label string) string {
	t.Helper()

	dir := t.TempDir()
	t.Cleanup(func() { checkWorkDir(t, label, dir, []string{"*.rock"}, false) })

	return dir
}
