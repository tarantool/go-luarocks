package rocks

import "log/slog"

// Config carries every input a Rocks operation needs.
//
// WorkingDir is explicit — the facade never reads process cwd; callers pass
// WorkingDir directly.
//
// Tarantool fields come from one source of truth on the caller side; the
// library does not parse `tarantool --version` itself.
type Config struct {
	// Tree is the root of the Tarantool rocks tree to read from and install into.
	Tree string
	// WorkingDir is the base directory for relative paths and build staging;
	// the facade never reads the process cwd.
	WorkingDir string
	// Tarantool describes the Tarantool installation rocks are built against.
	Tarantool TarantoolConfig
	// Servers are the rocks-repository base URLs queried for remote operations.
	Servers []string
	// Rockspec governs the sandboxed rockspec evaluator.
	Rockspec RockspecConfig
	// Logger receives structured operation logs; nil disables logging.
	Logger *slog.Logger

	// InsecureServers lists URL hosts (no scheme) for which TLS certificate
	// verification is skipped by the native backend's HTTP requests. Escape
	// hatch: upstream luarocks disables certs for `rocks.tarantool.org`; we
	// ALLOW that opt-in via this field but default to verifying. It applies
	// to each request by the request's own host, every redirect hop
	// included, so a listed host redirecting elsewhere does not lift the
	// check there; a redirect from https to http is refused.
	InsecureServers []string

	// IsolateBuilds, on unix, makes the native backend start every build
	// tool — make, cmake, the C compiler, the shell of a command build — in
	// a process group of its own, and makes cancelling the build's context
	// kill that whole group, the compilers a make started included, so none
	// of them runs on after the build is cancelled. Signals sent to the
	// caller's process group, the terminal's Ctrl-C (SIGINT) among them, no
	// longer reach the build tools: a caller that sets it must cancel the
	// context on SIGINT and SIGTERM, e.g. with signal.NotifyContext, or the
	// build tools survive the caller and keep writing into the tree.
	//
	// By default the build tools stay in the caller's process group, so
	// Ctrl-C reaches them, and cancelling the context kills only the tool
	// the build is running, not the processes it started. Either way a
	// cancelled build returns within about a second, even if a process it
	// started still holds its output open. IsolateBuilds is a no-op on other
	// platforms and in the lua backend.
	IsolateBuilds bool
}

// TarantoolConfig describes the Tarantool installation rocks are built against.
// Executable is the path to the `tarantool` binary; Prefix and IncludeDir
// come from the caller (typically tt's GetTarantoolPrefix). Version is the
// "x.y.z" string for diagnostic / golden-file headers.
type TarantoolConfig struct {
	Executable string
	Prefix     string
	IncludeDir string
	Version    string
}

// RockspecConfig governs the sandboxed evaluator. It carries no options today
// (the evaluator runs the rockspec in an all-nil environment matching upstream,
// with no host env access); it is retained as the Eval configuration seam.
type RockspecConfig struct{}
