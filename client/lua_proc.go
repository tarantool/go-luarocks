package client

// The engine's os.execute and io.popen. gopher-lua runs both through a
// context-free os.StartProcess / exec.Command, so a child process LuaRocks
// started — the downloader, unzip, make, cmake, the compiler, the shell of a
// command build — ran on after the call that started it was cancelled, and
// the call with it. The replacements below run the same shell command line
// through exec.CommandContext under the context of the VM (lua.LState.Context,
// set per dispatch by luaVM.run), and otherwise return exactly what gopher-lua's
// own functions return, which is what LuaRocks is written against: fs/tools.lua
// and fs/lua.lua take os.execute's 0 for success, and read a pipe with read
// ("*a", "*l", no argument), lines and close.
//
// Process groups follow the native backend's build tools (build/subprocess.go):
// by default a child stays in the caller's process group, so a terminal's
// Ctrl-C reaches it, and cancelling the context kills the child alone; with
// rocks.Config.IsolateBuilds, on unix, it leads a process group of its own and
// cancelling kills the whole group.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	lua "github.com/yuin/gopher-lua"
)

const (
	// luaWaitDelay bounds how long a cancelled child's Wait waits for the
	// child to go once it was killed, as build's waitDelay does.
	luaWaitDelay = time.Second

	// luaPipeBufferSize is the read and write buffer size of a pipe, the one
	// gopher-lua gives its files.
	luaPipeBufferSize = 4096

	// luaErrno is the errno gopher-lua returns as the third result of a
	// failed file operation, for C-Lua compatibility.
	luaErrno = 1

	// luaPipeClass names the metatable of the engine's io.popen files.
	luaPipeClass = "go-luarocks.process"

	// luaModeArg is the argument position of the mode io.popen and
	// file:setvbuf take; the formats of file:read are named argument 2 too,
	// whichever they are, as gopher-lua names them.
	luaModeArg = 2
)

// luaPopenModes are the modes io.popen accepts, gopher-lua's.
var luaPopenModes = []string{"r", "w"}

// luaPipeVBufModes are the buffering modes file:setvbuf accepts, gopher-lua's.
var luaPipeVBufModes = []string{"no", "full"}

// luaProcs starts and tracks the child processes of one VM. It holds no
// reference to the engine: see the comment on os.getenv in newVM.
type luaProcs struct {
	// isolate is rocks.Config.IsolateBuilds.
	isolate bool

	// pipes are the io.popen files the Lua code has not closed yet; reap
	// collects their processes. Only the goroutine running the VM touches
	// it.
	pipes map[*luaPipe]struct{}
}

func newLuaProcs(isolate bool) *luaProcs {
	return &luaProcs{isolate: isolate, pipes: map[*luaPipe]struct{}{}}
}

// install replaces os.execute and io.popen in L, and wraps io.type and
// io.close so they know the files io.popen returns, as gopher-lua's know its
// own.
func (p *luaProcs) install(L *lua.LState, osTbl, ioTbl *lua.LTable) error { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	origType, ok := L.GetField(ioTbl, "type").(*lua.LFunction)
	if !ok {
		return errors.New("luaEngine: io.type missing from Lua environment")
	}

	origClose, ok := L.GetField(ioTbl, "close").(*lua.LFunction)
	if !ok {
		return errors.New("luaEngine: io.close missing from Lua environment")
	}

	mt := L.NewTypeMetatable(luaPipeClass)
	methods := L.SetFuncs(L.NewTable(), map[string]lua.LGFunction{
		"close":   p.fileClose,
		"flush":   withPipe((*luaPipe).flush),
		"lines":   withPipe((*luaPipe).lines),
		"read":    withPipe((*luaPipe).read),
		"seek":    withPipe((*luaPipe).seek),
		"setvbuf": withPipe((*luaPipe).setvbuf),
		"write":   withPipe((*luaPipe).write),
	})
	L.SetField(mt, "__index", methods)
	L.SetField(mt, "__tostring", L.NewFunction(withPipe((*luaPipe).toString)))

	L.SetField(osTbl, "execute", L.NewFunction(p.execute))
	L.SetField(ioTbl, "popen", L.NewFunction(func(L *lua.LState) int { return p.popen(L, mt) }))

	L.SetField(ioTbl, "type", L.NewFunction(func(L *lua.LState) int {
		pipe, ok := toPipe(L.Get(1))
		if !ok {
			return callThrough(L, origType)
		}

		if pipe.closed {
			L.Push(lua.LString("closed file"))
		} else {
			L.Push(lua.LString("file"))
		}

		return 1
	}))

	L.SetField(ioTbl, "close", L.NewFunction(func(L *lua.LState) int {
		if _, ok := toPipe(L.Get(1)); ok {
			return p.fileClose(L)
		}

		return callThrough(L, origClose)
	}))

	return nil
}

// callThrough calls fn with the arguments of the running Go function and
// returns all its results.
func callThrough(L *lua.LState, fn *lua.LFunction) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	top := L.GetTop()

	L.Push(fn)

	for i := 1; i <= top; i++ {
		L.Push(L.Get(i))
	}

	L.Call(top, lua.MultRet)

	return L.GetTop() - top
}

// luaContext returns the context the VM runs under, which a thread of it
// inherits; a VM that runs under none can be cancelled by nothing.
func luaContext(L *lua.LState) context.Context { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if ctx := L.Context(); ctx != nil {
		return ctx
	}

	return context.Background()
}

// raiseIfDone raises the error the VM raises at its next instruction once
// ctx is done, so a Go function the cancellation interrupted ends the call
// as promptly as the VM would rather than return a result made up from the
// interruption.
func raiseIfDone(L *lua.LState, ctx context.Context) { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if err := ctx.Err(); err != nil {
		L.RaiseError("%s", err.Error())
	}
}

// command returns the exec.Cmd that runs shellCmd the way gopher-lua's
// os.execute and io.popen do, under ctx.
func (p *luaProcs) command(ctx context.Context, shellCmd string) *exec.Cmd {
	name, args := "/bin/sh", []string{"-c", shellCmd}
	if lua.LuaOS == "windows" {
		name, args = `C:\Windows\system32\cmd.exe`, []string{"/c", shellCmd}
	}

	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the command line LuaRocks runs, as upstream's os.execute and io.popen run it
	if p.isolate {
		killGroupOnCancel(cmd)
	}

	cmd.WaitDelay = luaWaitDelay

	return cmd
}

// execute is os.execute. Like gopher-lua's, it runs the command line with
// the process's stdin, stdout and stderr and returns 0 when it exits with 0
// and 1 otherwise — when it cannot start, exits non-zero or dies of a
// signal — which fs/tools.lua and fs/lua.lua test against 0.
func (p *luaProcs) execute(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	shellCmd := L.CheckString(1)
	ctx := luaContext(L)

	cmd := p.command(ctx, shellCmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	err := cmd.Run()

	raiseIfDone(L, ctx)

	if err != nil {
		L.Push(lua.LNumber(1))

		return 1
	}

	L.Push(lua.LNumber(0))

	return 1
}

// popen is io.popen. Like gopher-lua's, it takes mode "r" (the default) or
// "w", connects the pipe to the child's stdout or stdin and the child's
// other standard streams to the null device, and returns the file, or nil
// and a message when the command cannot start.
func (p *luaProcs) popen(L *lua.LState, mt lua.LValue) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	shellCmd := L.CheckString(1)
	if L.GetTop() < luaModeArg || L.Get(luaModeArg) == lua.LNil {
		L.SetTop(1)
		L.Push(lua.LString("r"))
	}

	writable := luaPopenModes[L.CheckOption(luaModeArg, luaPopenModes)] == "w"
	ctx := luaContext(L)

	pipe, err := p.start(ctx, shellCmd, writable)
	if err != nil {
		raiseIfDone(L, ctx)
		L.Push(lua.LNil)
		L.Push(lua.LString(err.Error()))

		return 2 //nolint:mnd // the two values pushed above
	}

	ud := L.NewUserData()
	ud.Value = pipe
	L.SetMetatable(ud, mt)
	L.Push(ud)

	return 1
}

// start starts shellCmd with a pipe to its stdin (writable) or from its
// stdout. Once ctx is done, the parent's end of the pipe is closed, so a read
// or write blocked on it returns even when a process the child started, and
// the cancellation did not kill, still holds the child's end.
func (p *luaProcs) start(ctx context.Context, shellCmd string, writable bool) (*luaPipe, error) {
	cmd := p.command(ctx, shellCmd)

	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	pipe := &luaPipe{ctx: ctx, cmd: cmd}

	childEnd := w
	if writable {
		cmd.Stdin, childEnd = r, r
		pipe.raw, pipe.writer = w, w
	} else {
		cmd.Stdout = w
		pipe.raw, pipe.reader = r, bufio.NewReaderSize(r, luaPipeBufferSize)
	}

	err = cmd.Start()

	_ = childEnd.Close() // the child holds its own copy now

	if err != nil {
		_ = pipe.raw.Close()

		return nil, err
	}

	pipe.stopWatch = context.AfterFunc(ctx, func() { _ = pipe.raw.Close() })
	p.pipes[pipe] = struct{}{}

	return pipe, nil
}

// fileClose is file:close and io.close(file) for a pipe: it closes the pipe
// and waits for the child, and returns the child's exit status, -1 when a
// signal killed it, as gopher-lua's does. Closing a closed pipe returns 0,
// what gopher-lua's second Wait makes of it.
func (p *luaProcs) fileClose(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	pipe := checkPipe(L)
	if pipe.closed {
		L.Push(lua.LNumber(0))

		return 1
	}

	pipe.closed = true

	if bw, ok := pipe.writer.(*bufio.Writer); ok {
		if err := bw.Flush(); err != nil {
			_ = p.finish(pipe)

			raiseIfDone(L, pipe.ctx)
			L.RaiseError("%s", err.Error())
		}
	}

	err := p.finish(pipe)

	raiseIfDone(L, pipe.ctx)

	status := 0

	if exitErr := (*exec.ExitError)(nil); errors.As(err, &exitErr) {
		status = exitErr.ExitCode()
	}

	L.Push(lua.LNumber(status))

	return 1
}

// finish closes the parent's end of the pipe — which gopher-lua leaves open
// for a "w" pipe, so a child reading to the end of its input never exits —
// and waits for the child.
func (p *luaProcs) finish(pipe *luaPipe) error {
	delete(p.pipes, pipe)

	pipe.closed = true
	pipe.stopWatch()

	_ = pipe.raw.Close()

	return pipe.cmd.Wait()
}

// reap kills and collects the process of every pipe the Lua code left open,
// so none outlives the chunk that started it: an unclosed pipe, such as the
// one fetch/git.lua reads git's version from, is otherwise closed only by the
// collector of a C Lua.
func (p *luaProcs) reap() {
	for pipe := range p.pipes {
		if pipe.cmd.Cancel != nil {
			_ = pipe.cmd.Cancel()
		}

		_ = p.finish(pipe)
	}
}

// luaPipe is a file io.popen returned: gopher-lua's process file, whose
// methods it mirrors, over a process the engine started.
type luaPipe struct {
	ctx       context.Context //nolint:containedctx // the context the pipe's process runs under, which its blocking methods honor
	cmd       *exec.Cmd
	raw       *os.File
	reader    *bufio.Reader
	writer    io.Writer
	closed    bool
	stopWatch func() bool
}

// toPipe returns the pipe v holds, if it is one.
func toPipe(v lua.LValue) (*luaPipe, bool) {
	ud, ok := v.(*lua.LUserData)
	if !ok {
		return nil, false
	}

	pipe, ok := ud.Value.(*luaPipe)

	return pipe, ok
}

// checkPipe returns the pipe that is the first argument, or raises the error
// gopher-lua raises for a file method called on something else.
func checkPipe(L *lua.LState) *luaPipe { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	pipe, ok := toPipe(L.Get(1))
	if !ok {
		L.ArgError(1, "file expected")
	}

	return pipe
}

// withPipe adapts a pipe method to a Lua function taking the pipe first.
func withPipe(method func(*luaPipe, *lua.LState) int) lua.LGFunction {
	return func(L *lua.LState) int { return method(checkPipe(L), L) }
}

// name is how gopher-lua's messages name a process file.
func (pipe *luaPipe) name() string { return "process " + pipe.cmd.Path }

// failed returns the nil, message, errno triple of a failed file operation.
func failed(L *lua.LState, msg string) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	L.Push(lua.LNil)
	L.Push(lua.LString(msg))
	L.Push(lua.LNumber(luaErrno))

	return 3 //nolint:mnd // the three values pushed above
}

func (pipe *luaPipe) checkOpen(L *lua.LState) { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.closed {
		L.ArgError(1, "file is closed")
	}
}

func (pipe *luaPipe) toString(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.closed {
		L.Push(lua.LString("process (closed)"))
	} else {
		L.Push(lua.LString("process"))
	}

	return 1
}

// ioError returns the failure of an I/O operation on the pipe, or raises the
// cancellation that caused it.
func (pipe *luaPipe) ioError(L *lua.LState, err error) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	raiseIfDone(L, pipe.ctx)

	return failed(L, err.Error())
}

func (pipe *luaPipe) write(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.writer == nil {
		return failed(L, pipe.name()+" is opened for only reading.")
	}

	pipe.checkOpen(L)

	for i := 2; i <= L.GetTop(); i++ {
		L.CheckTypes(i, lua.LTNumber, lua.LTString)

		if _, err := io.WriteString(pipe.writer, lua.LVAsString(L.Get(i))); err != nil {
			return pipe.ioError(L, err)
		}
	}

	L.Push(lua.LTrue)

	return 1
}

func (pipe *luaPipe) flush(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.writer == nil {
		return failed(L, pipe.name()+" is opened for only reading.")
	}

	pipe.checkOpen(L)

	if bw, ok := pipe.writer.(*bufio.Writer); ok {
		if err := bw.Flush(); err != nil {
			raiseIfDone(L, pipe.ctx)
			L.Push(lua.LNil)
			L.Push(lua.LString(err.Error()))

			return 2 //nolint:mnd // the two values pushed above
		}
	}

	L.Push(lua.LTrue)

	return 1
}

func (pipe *luaPipe) seek(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	L.Push(lua.LNil)
	L.Push(lua.LString("can not seek a process."))

	return 2 //nolint:mnd // the two values pushed above
}

// setvbuf switches a "w" pipe between unbuffered writes, the default, and a
// buffer of the given size. gopher-lua's fails for every process file.
func (pipe *luaPipe) setvbuf(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.writer == nil {
		return failed(L, pipe.name()+" is opened for only reading.")
	}

	if bw, ok := pipe.writer.(*bufio.Writer); ok {
		if err := bw.Flush(); err != nil {
			return pipe.ioError(L, err)
		}
	}

	switch luaPipeVBufModes[L.CheckOption(luaModeArg, luaPipeVBufModes)] {
	case "no":
		pipe.writer = pipe.raw
	default:
		pipe.writer = bufio.NewWriterSize(pipe.raw, L.OptInt(3, luaPipeBufferSize)) //nolint:mnd // setvbuf's size argument
	}

	L.Push(lua.LTrue)

	return 1
}

// lines returns an iterator over the lines of a "r" pipe and nothing for a
// "w" one, as gopher-lua's does. Like it, the iterator splits a line longer
// than the read buffer.
func (pipe *luaPipe) lines(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.reader == nil {
		return 0
	}

	L.Push(L.NewFunction(func(L *lua.LState) int {
		buf, _, err := pipe.reader.ReadLine()
		if err != nil {
			raiseIfDone(L, pipe.ctx)

			if errors.Is(err, io.EOF) {
				L.Push(lua.LNil)

				return 1
			}

			L.RaiseError("%s", err.Error())
		}

		L.Push(lua.LString(buf))

		return 1
	}))

	return 1
}

// read reads a "r" pipe in the formats gopher-lua's file:read takes: a byte
// count, or "*" followed by any of n (a number), a (the rest) and l (a line,
// the default). It returns a value per format, up to a nil at the end of the
// output, or nil, a message and an errno on an error.
func (pipe *luaPipe) read(L *lua.LState) int { //nolint:gocritic // L is the conventional gopher-lua LState name used throughout
	if pipe.reader == nil {
		return failed(L, pipe.name()+" is opened for only writing.")
	}

	pipe.checkOpen(L)

	if L.GetTop() == 1 {
		L.Push(lua.LString("*l"))
	}

	top := L.GetTop()

	for i := 2; i <= top; i++ {
		var formats []rune

		switch lv := L.Get(i).(type) {
		case lua.LNumber:
			formats = []rune{0}
		case lua.LString:
			opts := string(lv)
			if opts != "" && opts[0] != '*' {
				L.ArgError(luaModeArg, "invalid options:"+opts)
			}

			if opts != "" {
				formats = []rune(opts[1:])
			}
		}

		for _, format := range formats {
			v, eof, err := pipe.readFormat(L, format, L.Get(i))

			switch {
			case err != nil:
				return pipe.ioError(L, err)
			case eof:
				L.Push(lua.LNil)

				return L.GetTop() - top
			}

			L.Push(v)
		}
	}

	return L.GetTop() - top
}

// readFormat reads one value in format, 0 for the byte count arg. eof
// reports that the output ended before the value.
func (pipe *luaPipe) readFormat(L *lua.LState, format rune, arg lua.LValue) (lua.LValue, bool, error) { //nolint:gocritic,ireturn // L is the conventional gopher-lua LState name; a format reads a Lua value of its own type
	r := pipe.reader

	switch format {
	case 0:
		size, _ := arg.(lua.LNumber)

		return readSize(r, int64(size))
	case 'n':
		var v lua.LNumber

		_, err := fmt.Fscanf(r, lua.LNumberScanFormat, &v)
		if errors.Is(err, io.EOF) {
			return nil, true, nil
		}

		return v, false, err
	case 'a':
		buf, err := io.ReadAll(r)

		return lua.LString(buf), false, err
	case 'l':
		return readLine(r)
	default:
		L.ArgError(luaModeArg, "invalid options:"+string(format))

		return nil, false, nil
	}
}

// readSize reads size bytes, fewer at the end of the output, the way
// gopher-lua does: 0 bytes is "" unless the output has ended.
func readSize(r *bufio.Reader, size int64) (lua.LValue, bool, error) { //nolint:ireturn // nil at the end of the output, a string otherwise
	size = max(size, 0)

	if size == 0 {
		if _, err := r.ReadByte(); errors.Is(err, io.EOF) {
			return nil, true, nil
		}

		_ = r.UnreadByte()
	}

	var (
		result []byte
		err    error
	)

	for int64(len(result)) != size {
		buf := make([]byte, size-int64(len(result)))

		var n int

		n, err = r.Read(buf)
		if err != nil {
			break
		}

		result = append(result, buf[:n]...)
	}

	if errors.Is(err, io.EOF) {
		if len(result) == 0 {
			return nil, true, nil
		}

		err = nil
	}

	return lua.LString(result), false, err
}

// readLine reads a line without its end of line.
func readLine(r *bufio.Reader) (lua.LValue, bool, error) { //nolint:ireturn // nil at the end of the output, a string otherwise
	var (
		result []byte
		err    error
	)

	for prefix := true; prefix; {
		var buf []byte

		buf, prefix, err = r.ReadLine()
		if err != nil {
			break
		}

		result = append(result, buf...)
	}

	if errors.Is(err, io.EOF) {
		if len(result) == 0 {
			return nil, true, nil
		}

		err = nil
	}

	return lua.LString(result), false, err
}
