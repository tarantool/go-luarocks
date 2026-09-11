package client_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
)

// A pooled VM is warmed under the default progname, and luarocks/cmd.lua
// freezes `local program = util.this_program("luarocks")` at require time —
// exactly once per VM, which for a warm VM is the WARM-UP, not the dispatch.
// wrapper.lua therefore drops luarocks.cmd from package.loaded per exec, so
// every dispatch evaluates it under its own progname. This pins that: Exec
// with progname "tt rocks" must print "tt rocks", never the warm-up's
// "go-luarocks".
//
// callRaw writes to the process stdout verbatim, and gopher-lua binds
// io.stdout's *os.File at PACKAGE INIT (iolib stdFiles), so swapping the
// os.Stdout variable can never redirect it. The test dups fd 1 onto the pipe
// write end instead; a reader goroutine keeps the pipe from filling.
func TestExec_PrognameNotFrozenByWarmVM(t *testing.T) {
	dir := t.TempDir()

	read, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(&out, read)
		close(done)
	}()

	dupSaved, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup2(int(w.Fd()), int(os.Stdout.Fd())); err != nil {
		t.Fatal(err)
	}

	r, err := client.New(rocks.Config{Tree: dir, WorkingDir: dir}, client.WithBackend(client.BackendLua))
	if err != nil {
		t.Fatal(err)
	}

	if err := r.Exec(context.Background(), "tt rocks", []string{"help"}); err != nil {
		t.Fatalf("Exec help: %v", err)
	}

	// Restore fd 1 from the saved copy, then drain.
	_ = syscall.Dup2(dupSaved, int(os.Stdout.Fd()))
	_ = syscall.Close(dupSaved)
	w.Close()
	<-done

	got := out.String()
	if want := "Usage: tt rocks"; !strings.Contains(got, want) {
		t.Errorf("expected %q in output", want)
	}
	if bad := "go-luarocks"; strings.Contains(got, bad) {
		t.Errorf("progname leaked from warm VM: %q found in output", bad)
	}
}
