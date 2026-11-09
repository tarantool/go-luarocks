//go:build unix

package build_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/build"
	"github.com/tarantool/go-luarocks/internal/testenv"
)

// TestBuiltin_ExecModesUnderUmask: the builtin backend stages the commands
// and the C modules of build.install with upstream's "exec" permissions, 0755
// less the process umask, as the tree deploys them, a file a staging dir
// already had at the name included; it used to set 0750 whatever the umask.
func TestBuiltin_ExecModesUnderUmask(t *testing.T) {
	t.Parallel()

	for _, mask := range []int{0o022, 0o077, 0o002} {
		root := t.TempDir()
		testenv.RunWithUmask(t, "TestBuiltin_ExecModesChild", mask, root)

		want := (0o755 &^ os.FileMode(mask)).String()

		for _, rel := range []string{"bin/tool", "bin/again", "lib/a/libc.so"} {
			st, err := os.Stat(filepath.Join(root, "dest", filepath.FromSlash(rel)))
			require.NoError(t, err)
			assert.Equal(t, want, st.Mode().Perm().String(), "%s under umask %#o", rel, mask)
		}
	}
}

// TestBuiltin_ExecModesChild is the body of the child processes of
// TestBuiltin_ExecModesUnderUmask: it runs the builtin backend of a rock
// installing two commands and a C module into root/dest, where one of the
// commands is already, with mode 0600.
func TestBuiltin_ExecModesChild(t *testing.T) {
	t.Parallel()

	root := testenv.UmaskChild(t)
	src := filepath.Join(root, "src")
	dest := filepath.Join(root, "dest")

	for rel, body := range map[string]string{
		"bin/tool":         "print(1)\n",
		"prebuilt/libc.so": "\x7fELF stub\n",
	} {
		require.NoError(t, build.WriteFile(filepath.Join(src, filepath.FromSlash(rel)), body))
	}

	require.NoError(t, os.MkdirAll(filepath.Join(dest, "bin"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dest, "bin", "again"), []byte("old\n"), 0o600))

	spec := &rocks.Rockspec{
		Build: rocks.Build{
			Type: "builtin",
			Install: rocks.BuildInstall{
				Bin: map[string]string{"tool": "bin/tool", "again": "bin/tool"},
				Lib: map[string]string{"a.b": "prebuilt/libc.so"},
			},
		},
	}
	require.NoError(t, build.RunBackend(context.Background(), spec, src, dest, rocks.Config{}))
}
