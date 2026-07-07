//go:build integration

package build

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
)

// TestBuiltin_RealCC actually invokes cc against a no-op C source that
// includes a stand-in "tarantool/module.h" header authored in-test. The
// header defines just enough stubs for a hello-world `luaopen_*` function
// to compile-link without an actual Tarantool install.
//
// Tagged `integration` because it requires cc on PATH. Skips gracefully
// if not found.
func TestBuiltin_RealCC(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skipf("cc not on PATH")
	}

	dir := t.TempDir()
	srcDir := filepath.Join(dir, "src")
	destDir := filepath.Join(dir, "dest")
	incDir := filepath.Join(dir, "fake-include")

	// Author a tarantool/module.h stub: empty header, just so -I is honored.
	require.NoError(t, writeFile(filepath.Join(incDir, "tarantool/module.h"), "/* stub */\n"))

	// Author a no-op C source.
	src := `#include <tarantool/module.h>

int luaopen_hello(void *L) {
    (void)L;
    return 0;
}
`
	require.NoError(t, writeFile(filepath.Join(srcDir, "hello.c"), src))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Modules: map[string]rocks.Module{
				"hello": {Path: "hello.c"},
			},
		},
	}
	cfg := rocks.Config{
		Tarantool: rocks.TarantoolConfig{IncludeDir: incDir},
	}

	require.NoError(t, RunBackend(context.Background(), spec, srcDir, destDir, cfg), "real-cc build failed")
	assert.True(t, fileExists(filepath.Join(destDir, "lib/hello.so")), "expected destDir/lib/hello.so")
}
