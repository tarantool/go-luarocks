package sandbox_test

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tarantool/go-luarocks/internal/testenv"
)

// TestSandbox checks the guarantees of scripts/test-in-docker.sh from the
// inside: if one of them stops holding, the sandbox no longer catches stray
// writes and this test fails. It runs only inside the sandbox.
func TestSandbox(t *testing.T) {
	t.Parallel()

	if os.Getenv("GO_LUAROCKS_SANDBOX") != "1" {
		t.Skip("not inside the test sandbox; run scripts/test-in-docker.sh")
	}

	t.Run("unprivileged", func(t *testing.T) {
		t.Parallel()
		assert.NotZero(t, os.Geteuid(), "tests must not run as root: it bypasses permission checks")
	})

	t.Run("tempdir is writable", func(t *testing.T) {
		t.Parallel()
		require.NoError(t, os.WriteFile(filepath.Join(t.TempDir(), "f"), nil, 0o600))
	})

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	// The test runs in its package directory, two levels below the module
	// root; both are part of the source tree.
	wd, err := os.Getwd()
	require.NoError(t, err)

	root := filepath.Join(wd, "..", "..")
	require.FileExists(t, filepath.Join(root, "go.mod"), "module root not found")

	for _, dir := range []string{"/", "/usr/local", "/etc", home, root, wd} {
		t.Run("read-only "+dir, func(t *testing.T) {
			t.Parallel()

			f, err := os.CreateTemp(dir, "sandbox-canary-")
			if err == nil {
				_ = f.Close()
				_ = os.Remove(f.Name())

				t.Fatalf("created %s: the sandbox lets writes through", f.Name())
			}

			assert.True(t, errors.Is(err, syscall.EROFS) || errors.Is(err, fs.ErrPermission),
				"want EROFS or a permission error, got %v", err)
		})
	}

	t.Run("offline", func(t *testing.T) {
		t.Parallel()

		if !testenv.Offline() {
			t.Skip("the sandbox was started with network access")
		}

		d := net.Dialer{Timeout: 5 * time.Second}

		conn, err := d.DialContext(t.Context(), "tcp", "1.1.1.1:443")
		if err == nil {
			_ = conn.Close()
		}

		require.Error(t, err, "dialed out of an offline sandbox")

		_, err = net.DefaultResolver.LookupHost(t.Context(), "luarocks.org")
		require.Error(t, err, "resolved a name in an offline sandbox")
	})
}
