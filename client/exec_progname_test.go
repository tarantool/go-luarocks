//go:build unix

package client_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// A pooled VM is warmed under the default progname, and luarocks/cmd.lua
// freezes `local program = util.this_program("luarocks")` at require time —
// exactly once per VM, which for a warm VM is the WARM-UP, not the dispatch.
// wrapper.lua therefore drops luarocks.cmd from package.loaded per exec, so
// every dispatch evaluates it under its own progname. This pins that: Exec
// with progname "tt rocks" must print "tt rocks", never the warm-up's
// "go-luarocks" — for the admin interface too, which the wrapper names
// "<progname> admin".
//
//nolint:paralleltest // redirects file descriptor 1, which is process-wide
func TestExec_PrognameNotFrozenByWarmVM(t *testing.T) {
	dir := t.TempDir()

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: dir}, client.WithBackend(client.BackendLua))
	require.NoError(t, err)

	for _, tc := range []struct {
		argv []string
		want string
	}{
		{argv: []string{"help"}, want: "Usage: tt rocks "},
		{argv: []string{"admin", "help"}, want: "Usage: tt rocks admin "},
	} {
		got := captureFd1(t, func() error {
			return r.Exec(context.Background(), "tt rocks", tc.argv)
		})

		assert.Contains(t, got, tc.want, "argv %q", tc.argv)
		assert.NotContains(t, got, "go-luarocks", "argv %q: progname leaked from the warm-up", tc.argv)
	}
}

// captureFd1 runs fn with file descriptor 1 redirected into a pipe and returns
// what was written there. callRaw writes to the process stdout verbatim, and
// gopher-lua binds io.stdout's *os.File at PACKAGE INIT (iolib stdFiles), so
// swapping the os.Stdout variable can never redirect it. fd 1 is restored
// before fn's error is checked, so a failing fn cannot leave the rest of the
// run writing into the pipe.
func captureFd1(t *testing.T, fn func() error) string {
	t.Helper()

	read, write, err := os.Pipe()
	require.NoError(t, err)

	var out bytes.Buffer

	done := make(chan struct{})

	go func() {
		_, _ = io.Copy(&out, read)

		close(done)
	}()

	stdout := int(os.Stdout.Fd())

	saved, err := unix.Dup(stdout)
	require.NoError(t, err)
	require.NoError(t, unix.Dup2(int(write.Fd()), stdout))

	fnErr := fn()

	require.NoError(t, unix.Dup2(saved, stdout))
	require.NoError(t, unix.Close(saved))
	require.NoError(t, write.Close())
	<-done
	require.NoError(t, read.Close())
	require.NoError(t, fnErr)

	return out.String()
}
