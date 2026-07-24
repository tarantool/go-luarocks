# Contribution Guide

## Filing issues

Before starting work, open an issue describing the bug or the feature you
want to add, or find an existing one. This gives maintainers and other
contributors a chance to weigh in before you invest time in an
implementation.

## First steps

The project requires Go 1.25 or later. Clone the repository and build it.

```sh
$ git clone https://github.com/tarantool/go-luarocks
$ cd go-luarocks
$ go build ./...
```

## Running tests

There is no Makefile; run `go test` directly against the root package and
every internal package:

```sh
go test ./build/... ./client/... ./deps/... ./fetch/... ./manif/... \
  ./remote/... ./rockspec/... ./tree/... .
```

A single package, e.g. `deps`:

```sh
go test ./deps/...
```

Some `client` tests require a real `tarantool` binary and its `lua.h` on the
build path; they are gated behind the `integration_luaengine` build tag and
skipped otherwise:

```sh
go test ./client/ -tags integration_luaengine -run TestBackendParity -v
```

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
golangci-lint run ./build/... ./client/... ./deps/... ./fetch/... ./manif/... \
  ./remote/... ./rockspec/... ./tree/... .
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
