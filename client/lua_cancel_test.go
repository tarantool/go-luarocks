//go:build unix

package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rocks "github.com/tarantool/go-luarocks"
	"github.com/tarantool/go-luarocks/client"
	"github.com/tarantool/go-luarocks/internal/testenv"
	"github.com/tarantool/go-luarocks/manif"
)

// Cancellation of the lua backend. Its typed methods and Exec ignored their
// context: the VM ran on, and so did the processes LuaRocks started through
// os.execute and io.popen, so a cancelled call ran to completion, or forever.

const (
	// luaCancelAfter is how long a test lets a call run before cancelling it.
	luaCancelAfter = 200 * time.Millisecond
	// luaCancelBound is how soon after the cancellation the call must return.
	luaCancelBound = 2 * time.Second
	// luaStartBound is how long a test waits for the call to start the
	// command it cancels. It measures the machine, not the engine: the call
	// fetches and unpacks the sources first, which takes seconds on a loaded
	// runner.
	luaStartBound = time.Minute
)

// newLuaStubCfg returns a Config of the lua backend for a tree and working
// dir in dir, with a stand-in Tarantool: a lua.h of Lua 5.1, which a build
// checks for, and a tarantool that fails, so no real one is needed.
func newLuaStubCfg(t *testing.T, dir string) rocks.Config {
	t.Helper()

	prefix := filepath.Join(dir, "tarantool")
	incDir := filepath.Join(prefix, "include", "tarantool")
	bin := filepath.Join(prefix, "bin")

	require.NoError(t, os.MkdirAll(incDir, 0o750))
	require.NoError(t, os.MkdirAll(bin, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(incDir, "lua.h"), []byte("#define LUA_VERSION_NUM 501\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "tarantool"), []byte("#!/bin/sh\nexit 1\n"), 0o700)) //nolint:gosec // an executable stub

	tree := filepath.Join(dir, "tree")
	work := filepath.Join(dir, "work")

	require.NoError(t, os.MkdirAll(tree, 0o750))
	require.NoError(t, os.MkdirAll(work, 0o750))

	return rocks.Config{
		Tree:       tree,
		WorkingDir: work,
		Tarantool:  rocks.TarantoolConfig{Prefix: prefix, IncludeDir: incDir, Version: "3.9.0"},
	}
}

// writeCommandRockspec writes, into the working dir of cfg, the rockspec of
// a rock whose command build runs buildCommand, and returns its path.
func writeCommandRockspec(t *testing.T, cfg rocks.Config, name, buildCommand string) string {
	t.Helper()

	tarball := filepath.Join(t.TempDir(), name+".tar.gz")
	writeTarGz(t, tarball, name, map[string]string{"README": name + "\n"})

	spec := `package = "` + name + `"
version = "1.0-1"
source = { url = "file://` + tarball + `" }
description = { summary = "A rock of a cancellation test.", license = "MIT" }
build = {
   type = "command",
   build_command = [[` + buildCommand + `]],
   install_command = "true",
}
`
	path := filepath.Join(cfg.WorkingDir, name+"-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(path, []byte(spec), 0o600))

	return path
}

// callAndCancel starts call with a context it cancels luaCancelAfter after
// running, when it is not nil, started, and requires call to return within
// luaCancelBound of that with an error wrapping context.Canceled.
func callAndCancel(t *testing.T, call func(ctx context.Context) error, started func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)

	go func() { done <- call(ctx) }()

	if started != nil {
		started()
	}

	time.Sleep(luaCancelAfter)

	start := time.Now()

	cancel()

	select {
	case err := <-done:
		assert.Less(t, time.Since(start), luaCancelBound, "the call returns at once")
		require.ErrorIs(t, err, context.Canceled, "the error of a cancelled call")
	case <-time.After(30 * time.Second):
		t.Fatal("the call did not return after its context was cancelled")
	}
}

// waitPids returns the pids the shell of a test wrote into path, once it has.
func waitPids(t *testing.T, path string, n int) []int {
	t.Helper()

	var pids []int

	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path) //nolint:gosec // test temp path
		if err != nil {
			return false
		}

		f := strings.Fields(string(b))
		if len(f) != n {
			return false
		}

		pids = pids[:0]

		for _, s := range f {
			pid, err := strconv.Atoi(s)
			if err != nil {
				return false
			}

			pids = append(pids, pid)
		}

		return true
	}, luaStartBound, 10*time.Millisecond, "the shell starts")

	return pids
}

// groupName names the subtest of a process-group mode. It has no "=", which
// would make LuaRocks take a path of the test's temp dirs in argv for a
// variable assignment.
func groupName(isolate bool) string {
	if isolate {
		return "IsolateBuilds"
	}

	return "CallersGroup"
}

// gone reports whether no process with pid, or with -pid a process group,
// exists. The engine waits for its children, so a killed one is no zombie.
func gone(pid int) bool { return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// killOnCleanup kills pid at the end of the test, for a process the test
// expects to outlive the call.
func killOnCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
}

// TestLuaEngine_CancelStopsLua — the VM ran no context, so Lua code that
// does not end, here a loop in a rockspec, which LuaRocks runs to load it,
// ran forever whatever the context of the call. Cancelling must stop it, the
// loops of a pcall and of a coroutine included.
func TestLuaEngine_CancelStopsLua(t *testing.T) {
	t.Parallel()

	cfg := newLuaStubCfg(t, t.TempDir())
	r, err := client.New(cfg, client.WithBackend(client.BackendLua))
	require.NoError(t, err, "New")

	spec := filepath.Join(cfg.WorkingDir, "loop-1.0-1.rockspec")
	require.NoError(t, os.WriteFile(spec, []byte("while true do end\n"), 0o600))

	callAndCancel(t, func(ctx context.Context) error {
		return r.Lint(ctx, spec, client.LintOpts{})
	}, nil)
	callAndCancel(t, func(ctx context.Context) error {
		return r.Make(ctx, client.MakeOpts{RockspecPath: spec})
	}, nil)

	e, ok := r.Engine().(*client.LuaEngine)
	require.True(t, ok, "the lua engine")

	for _, chunk := range []string{
		"while true do end",
		"while true do pcall(function() while true do end end) end",
		"coroutine.wrap(function() while true do end end)()",
		"while true do pcall(error, 'x') end",
	} {
		callAndCancel(t, func(ctx context.Context) error { return e.Run(ctx, chunk) }, nil)
	}

	// The client still works.
	require.NoError(t, r.Lint(context.Background(), writeCommandRockspec(t, cfg, "fine", "true"), client.LintOpts{}),
		"Lint after the cancellations")
}

// TestLuaEngine_CancelKillsBuildCommand — a cancelled Make or Build of the
// lua backend ran its build command to the end. Cancelling must kill it: in
// the caller's process group the command alone, which here is the sleep the
// shell execs, and with IsolateBuilds its whole process group, the sleep the
// shell started included.
func TestLuaEngine_CancelKillsBuildCommand(t *testing.T) {
	t.Parallel()

	self, err := syscall.Getpgid(0)
	require.NoError(t, err)

	for _, isolate := range []bool{false, true} {
		for _, method := range []string{"Make", "Build"} {
			t.Run(method+"/"+groupName(isolate), func(t *testing.T) {
				t.Parallel()

				cfg := newLuaStubCfg(t, t.TempDir())
				cfg.IsolateBuilds = isolate
				pidFile := filepath.Join(t.TempDir(), "pids")

				command := `echo $$ > '` + pidFile + `.tmp' && mv '` + pidFile + `.tmp' '` + pidFile + `' && exec sleep 30`
				if isolate {
					command = `sleep 30 & echo "$$ $!" > '` + pidFile + `.tmp' && mv '` + pidFile + `.tmp' '` + pidFile + `'; wait`
				}

				name := "slow" + strings.ToLower(method+groupName(isolate))
				spec := writeCommandRockspec(t, cfg, name, command)

				r, err := client.New(cfg, client.WithBackend(client.BackendLua))
				require.NoError(t, err, "New")

				var pids []int

				callAndCancel(t, func(ctx context.Context) error {
					if method == "Make" {
						return r.Make(ctx, client.MakeOpts{RockspecPath: spec})
					}

					return r.Build(ctx, spec, client.BuildOpts{})
				}, func() {
					n := 1
					if isolate {
						n = 2
					}

					pids = waitPids(t, pidFile, n)
					for _, pid := range pids {
						killOnCleanup(t, pid)
					}

					pgid, err := syscall.Getpgid(pids[0])
					require.NoError(t, err)

					if isolate {
						assert.Equal(t, pids[0], pgid, "the build command leads a process group of its own")
					} else {
						assert.Equal(t, self, pgid, "the build command is in the caller's process group")
					}
				})

				if isolate {
					assert.Eventually(t, func() bool { return gone(-pids[0]) }, time.Second, 10*time.Millisecond,
						"no process of the build command's group is left")
				} else {
					assert.True(t, gone(pids[0]), "the build command is killed")
				}

				// LuaRocks' cleanup ran and rolled the partial install back,
				// which, left in the tree, fails every later build there.
				assert.NoDirExists(t, filepath.Join(cfg.Tree, "share", "tarantool", "rocks", name, "1.0-1"),
					"the install dir of the cancelled build")

				// The same client builds again.
				require.NoError(t, r.Make(context.Background(), client.MakeOpts{
					RockspecPath: writeCommandRockspec(t, cfg, "quick", "true"),
				}), "Make after the cancellation")
			})
		}
	}
}

// TestLuaEngine_CancelKillsChildren — a child LuaRocks started through
// os.execute or io.popen ran on after the call was cancelled, and the call
// waited for it. Cancelling must kill it and return at once: in the caller's
// process group the shell alone, even though the sleep it started lives on
// and, for io.popen, holds the pipe the call reads; with IsolateBuilds the
// shell's whole process group.
func TestLuaEngine_CancelKillsChildren(t *testing.T) {
	t.Parallel()

	for _, isolate := range []bool{false, true} {
		for _, fn := range []string{"os.execute", "io.popen"} {
			t.Run(fn+"/"+groupName(isolate), func(t *testing.T) {
				t.Parallel()

				cfg := newLuaStubCfg(t, t.TempDir())
				cfg.IsolateBuilds = isolate
				pidFile := filepath.Join(t.TempDir(), "pids")

				command := `sleep 30 & echo "$$ $!" > '` + pidFile + `.tmp' && mv '` + pidFile + `.tmp' '` + pidFile + `'; wait`

				chunk := "os.execute([[" + command + "]])"
				if fn == "io.popen" {
					chunk = "io.popen([[" + command + "]]):read('*a')"
				}

				e := client.NewLuaEngine(cfg, nil, nil)

				var pids []int

				callAndCancel(t, func(ctx context.Context) error { return e.Run(ctx, chunk) }, func() {
					pids = waitPids(t, pidFile, 2)
					for _, pid := range pids {
						killOnCleanup(t, pid)
					}
				})

				shell, sleep := pids[0], pids[1]

				if isolate {
					assert.Eventually(t, func() bool { return gone(-shell) }, time.Second, 10*time.Millisecond,
						"no process of the shell's group is left")
				} else {
					assert.True(t, gone(shell), "the shell is killed")
					assert.False(t, gone(sleep), "the process the shell started is not")
				}

				require.NoError(t, e.Run(context.Background(), "assert(io.popen('echo ok'):read('*l') == 'ok')"),
					"the engine runs commands after the cancellation")
			})
		}
	}
}

// TestLuaEngine_CancelInstall — a cancelled Install of the lua backend
// waited for its download from a server that does not answer. Cancelling must
// return at once, kill the downloader (with IsolateBuilds, which reaches the
// downloader the shell started; in the caller's process group only the shell
// is killed), and leave the client able to install and no goroutine behind.
//
//nolint:paralleltest // counts the goroutines of the process
func TestLuaEngine_CancelInstall(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		if _, err := exec.LookPath("curl"); err != nil {
			testenv.Missingf(t, "neither wget nor curl is on PATH; LuaRocks downloads with one of them: %v", err)
		}
	}

	repo := installTestRepo(t)

	type target struct {
		r    *client.Rocks
		tree string
	}

	targets := map[bool]target{}

	for _, isolate := range []bool{false, true} {
		cfg := newLuaStubCfg(t, t.TempDir())
		cfg.Servers = []string{repo}
		cfg.IsolateBuilds = isolate

		r, err := client.New(cfg, client.WithBackend(client.BackendLua))
		require.NoError(t, err, "New")

		targets[isolate] = target{r, cfg.Tree}
	}

	baseline := runtime.NumGoroutine()

	requested := make(chan struct{}, 1)
	disconnected := make(chan struct{}, 1)
	release := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case requested <- struct{}{}:
		default:
		}

		select {
		case <-r.Context().Done():
			select {
			case disconnected <- struct{}{}:
			default:
			}
		case <-release:
		}

		http.NotFound(w, r)
	}))

	stopServer := sync.OnceFunc(func() {
		close(release)
		srv.Close()
	})
	t.Cleanup(stopServer)

	for _, isolate := range []bool{false, true} {
		r := targets[isolate].r

		callAndCancel(t, func(ctx context.Context) error {
			return r.Install(ctx, "dep", client.InstallOpts{Servers: []string{srv.URL}})
		}, func() {
			select {
			case <-requested:
			case <-time.After(30 * time.Second):
				t.Fatal("LuaRocks did not reach the server")
			}
		})

		if isolate {
			select {
			case <-disconnected:
			case <-time.After(luaCancelBound):
				t.Error("the downloader is not killed")
			}
		}

		require.NoError(t, r.Install(context.Background(), "dep", client.InstallOpts{}), "Install after the cancellation")

		m, err := manif.ReadTreeManifest(filepath.Join(targets[isolate].tree, "share", "tarantool", "rocks"))
		require.NoError(t, err, "the tree's manifest")
		assert.Contains(t, m.Repository, "dep", "the installed rock")
	}

	stopServer()

	// Polled here rather than with assert.Eventually, whose own goroutine
	// would count.
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	assert.LessOrEqual(t, runtime.NumGoroutine(), baseline, "the goroutines of the cancelled calls are gone")
}
