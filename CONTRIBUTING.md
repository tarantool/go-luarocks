# Contribution Guide

## Filing issues

Before starting work, open an issue describing the bug or the feature you
want to add, or find an existing one. This gives maintainers and other
contributors a chance to weigh in before you invest time in an
implementation.

## First steps

The project requires Go 1.26 or later. Clone the repository and build it.

```sh
$ git clone https://github.com/tarantool/go-luarocks
$ cd go-luarocks
$ go build ./...
```

## Running tests

There is no Makefile; run `go test` directly against the root package and
every internal package:

```sh
go test ./build/... ./client/... ./deps/... ./fetch/... ./internal/... \
  ./manif/... ./remote/... ./rockspec/... ./tree/... .
```

A single package, e.g. `deps`:

```sh
go test ./deps/...
```

Tests that need more than Go are gated behind build tags. The `integration`
tag enables the `build`, `fetch`, and native `client` tests that run `cc`,
`make`, `cmake`, `git`, and `tarantool` or reach `luarocks.org` and GitHub.
The `integration_luaengine` tag enables the `client` tests of the lua backend
and the backend-parity harness (`TestBackendParity_*`); they need a real
`tarantool` on `PATH` and its `lua.h` at `<prefix>/include/tarantool/lua.h`,
where `<prefix>` is the directory above the one holding `tarantool` (the
`tarantool-dev` package puts it at `/usr/include/tarantool/lua.h`):

```sh
go test ./client/ -tags integration_luaengine -run TestBackendParity -v
```

A tagged test whose prerequisite is missing is skipped, so the suite runs on
a machine that has only some of them. Set `GO_LUAROCKS_INTEGRATION_STRICT=1`
to make a missing prerequisite fail the test instead, so a run that is meant
to exercise the whole suite cannot pass by skipping it. The network is the
exception: `GO_LUAROCKS_OFFLINE=1` declares a run offline by design, and the
tests that need the network are then skipped even in strict mode. The CI
`integration` job runs exactly this:

```sh
GO_LUAROCKS_INTEGRATION_STRICT=1 go test -tags integration,integration_luaengine \
  ./build/... ./client/... ./deps/... ./fetch/... ./internal/... \
  ./manif/... ./remote/... ./rockspec/... ./tree/... .
```

The tests keep LuaRocks out of your home and system configuration: the
`client` package runs with `HOME` (and `XDG_CACHE_HOME`, `XDG_CONFIG_HOME`,
`XDG_DATA_HOME` inside it) and `LUAROCKS_SYSCONFDIR` pointing at fresh empty
directories, set up by its `TestMain`, and the tagged `build` tests do the
same per test. So nothing lands in `~/.cache/luarocks`, and neither
`~/.luarocks` nor `/etc/luarocks` changes what a test sees. `TMPDIR`, `PATH`,
and the toolchain variables (`CC`, `CFLAGS`, …) are passed through. The helpers
are in `internal/testenv`; `TestLuaEngine_Install_LeavesRealHomeAlone`
(`integration_luaengine`, which also needs `unzip`) checks that a lua-backend
install leaves the real home alone.

### Sandboxed runner

The integration and lua-engine suites run real build tools and LuaRocks
code. `scripts/test-in-docker.sh` runs them in a Docker container built from
`test/docker/Dockerfile`, which pins Go, Tarantool (with its headers), CMake,
make, a C compiler, and git. The sandbox protects your machine and turns
stray writes into test failures:

- the root filesystem and the source tree are read-only, and `HOME` points
  to a read-only directory, so writing there fails with `EROFS` or `EACCES`;
- tests run as an unprivileged user without capabilities;
- the only writable places are `/tmp` (a tmpfs that holds `TMPDIR`) and the
  Go build cache volume; anything left in `/tmp` or `/dev/shm` after the run
  fails it with a list of the leftovers;
- there is no network unless you pass `--net`; the module cache volume is
  filled by a separate `go mod download` step, the only one ever given
  network for that. An offline run sets `GO_LUAROCKS_OFFLINE=1`, so the
  tests that need luarocks.org or GitHub are skipped rather than failed;
- `GO_LUAROCKS_INTEGRATION_STRICT=1` is set, since the image provides every
  prerequisite of the integration tests.

A canary test in `internal/sandbox` checks these guarantees from the inside,
so a weakened sandbox fails the run; outside the sandbox it skips.

Without a command, the script runs `go test -count=1 -tags
integration,integration_luaengine` over every package; `-count=1` keeps
results from coming out of the test cache instead of running in the sandbox.

```sh
scripts/test-in-docker.sh                # the whole suite, offline
scripts/test-in-docker.sh --net          # also the tests that reach luarocks.org
scripts/test-in-docker.sh go test -count=1 -v ./build/...   # a custom command
```

Override the pinned versions with `GO_VERSION`, `DEBIAN_SUITE`, or
`TARANTOOL_VERSION`, e.g. `GO_VERSION=1.26.8 scripts/test-in-docker.sh`.

### Differential tests

The pure functions are checked against upstream LuaRocks itself: the
`diff_fuzz_test.go` files in `deps`, `rockspec`, `manif` and `client` run each
input through the Go implementation and through the vendored upstream Lua code
(version and constraint parsing, dependency strings, rockspec loading,
manifest reading and writing, source-directory and rock-name parsing), and the
two must agree. The upstream side runs in a bare gopher-lua VM set up by
`internal/luaref`, offline; its package comment lists the stubs it uses.

Every target is a Go fuzz target, so a plain `go test` runs its seed corpus.
To explore, fuzz one target at a time:

```sh
go test ./deps/ -run '^$' -fuzz '^FuzzParseVersion$' -fuzztime 60s -fuzzminimizetime 10s
```

A failing input is written to `<package>/testdata/fuzz/<target>/` and from
then on runs with the seeds; commit it with the fix as the regression test.
The `fuzz` workflow fuzzes every target weekly. `go test ./internal/luaref/`
checks the harness itself.

## Examples

Public APIs are documented with runnable `ExampleXxx` functions that carry a
verified `// Output:` block. Run them with:

```sh
go test -v -run Example ./...
```

## Linting

The project uses [golangci-lint](https://golangci-lint.run/), configured in
`.golangci.yml` at the repository root:

```sh
golangci-lint run ./build/... ./client/... ./deps/... ./fetch/... \
  ./internal/... ./manif/... ./remote/... ./rockspec/... ./tree/... .
```

## Formatting

Code is formatted with `gofmt` and `goimports` (both enforced by the linter):

```sh
gofmt -l .
goimports -l .
```

## Code review checklist

- The public API exposes only what external users need; everything else
  stays unexported or under `internal/`.
- Every exported function, type, variable, and constant has a doc comment.
- Code is DRY.
- New features come with tests.
- Bug-fix commits include a regression test built from the reproducer, and
  the test fails without the fix.
- There are no changes to files unrelated to the issue.
- There are no obviously flaky tests.
- A `CHANGELOG.md` entry is present.
- New public methods carry an executable example with reference output where
  practical.
- Comments, commit titles, and identifiers are grammatically correct — start
  with a capital letter, end with a period.

## Commit message guidelines

A commit message has three parts: a header, a body, and links to related
issues.

```
prefix: commit title

The commit body, explaining what changed and why.

Closes #17
```

- The header is `prefix: subject` — a short prefix, a colon, a space, and a
  lowercase subject in the imperative mood: it must complete the sentence "If
  applied, this commit will …". Keep it within ~50 characters, no trailing
  period. Typical prefixes are the package name (`deps`, `rockspec`,
  `manif`, `fetch`, `tree`, `build`, `remote`, `client`, …) or a shared scope
  (`ci`, `tests`, `doc`, `all`).
- Do not put issue numbers in the header — links go in the body.
- Separate the body from the header with a blank line; wrap body lines at 72
  characters. The body explains *what* and *why*, not *how*.
- Put issue links on the last lines of the body. Use `Part of #NNN` on
  intermediate commits of a task and `Closes #NNN` on the final commit of the
  pull request.
- Use your real name and working email.
- Each commit is atomic and self-contained.

## Pull request process

1. Fork the repository and create a feature branch.
2. Self-review before requesting reviewers: all changes belong to the pull
   request, every line is explainable, tests pass, and new features have
   documentation.
3. Every patch needs tests: new behavior gets new tests, and bug fixes get a
   regression test.
4. Update the documentation (README.md and package doc comments) for any
   user-visible change.
5. Add a `CHANGELOG.md` entry under `[Unreleased]` describing the
   user-facing effect of your change.
6. Open the pull request with a clear description of the change and its
   purpose.
7. A pull request merges after approval; the reviewer, not the author,
   resolves review threads.
