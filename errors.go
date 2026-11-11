package rocks

import "errors"

// Sentinel errors callers can branch on with errors.Is.
//
// These replace the "deep cc/cmake failure or generic os.exit from
// the Lua side" pattern that tt currently string-matches on. Every facade
// method documents which of these it can return.
var (
	// ErrMissingTarantoolHeaders signals that a build backend needs the
	// Tarantool development headers but Config.Tarantool.IncludeDir is
	// unset or does not contain `tarantool/module.h`.
	ErrMissingTarantoolHeaders = errors.New("rocks: tarantool headers not found")

	// ErrMissingTarantoolBinary signals that an operation requires the
	// `tarantool` binary on disk (e.g. shelling out for capability probes)
	// and Config.Tarantool.Executable is unset or does not point at an
	// executable.
	ErrMissingTarantoolBinary = errors.New("rocks: tarantool binary not found")

	// ErrUnsupportedCommand is a reserved sentinel for callers that want to
	// reject a luarocks subcommand they consider out of scope. It is not
	// returned by this package today: Exec passes argv straight to the
	// embedded dispatcher, and the typed methods use ErrNotImplemented.
	ErrUnsupportedCommand = errors.New("rocks: unsupported command")

	// ErrUnsupportedRockspecFeature signals that a rockspec used a feature
	// the evaluator does not implement (e.g. build.type outside the
	// {"builtin","cmake","make","command","none"} set, or a platforms
	// block we don't recognize). Fail loud rather than silently skip.
	ErrUnsupportedRockspecFeature = errors.New("rocks: unsupported rockspec feature")

	// ErrUnsafePath signals that a name from a rockspec, a rock_manifest, a
	// manifest, an archive or a server response would make the native
	// backend read, write, rename or remove a file outside the directory it
	// belongs to: an absolute path, one reaching above its directory through
	// "..", one passing through a symlink that points out, or a name that
	// must be a single directory and is not. Upstream LuaRocks follows such
	// names; the native backend refuses them, before anything is written
	// there.
	ErrUnsafePath = errors.New("rocks: path escapes its directory")

	// ErrNotImplemented signals that the active backend (Engine) has no
	// implementation for the invoked method. It is the SOLE error a method
	// returns in that case — no silent no-op, no zero-value success.
	// Callers branch on it with errors.Is(err, ErrNotImplemented) to
	// detect an unsupported operation for the selected backend.
	ErrNotImplemented = errors.New("rocks: method not implemented by this backend")

	// ErrChecksumMismatch signals that a file the native backend fetched
	// does not have the sha256 it was expected to have: the digest a rock
	// server's tt-rocks-index.json lists for it, or one the caller pinned.
	// The file is discarded before anything unpacks or reads it.
	ErrChecksumMismatch = errors.New("rocks: checksum mismatch")

	// ErrUnsupportedIndexFormat signals that a rock server's
	// tt-rocks-index.json declares a format newer than the one this library
	// reads, so the server cannot be used until the program is upgraded.
	ErrUnsupportedIndexFormat = errors.New("rocks: unsupported rock index format")

	// ErrArtifactNotPinned signals that the caller pinned the digests of a
	// rock's files (client.InstallOpts.Artifacts), as a lock records them,
	// and the file the native backend chose for this platform is not among
	// them: the pins were recorded without it, and the lock holding them has
	// to be resolved again. Nothing was fetched.
	ErrArtifactNotPinned = errors.New("rocks: artifact not pinned")
)
