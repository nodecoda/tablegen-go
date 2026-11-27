# Contributing

Thanks for taking the time to improve `tablegen-go`.

## Ground rules

1. **No third-party dependencies.** The module must stay buildable with the
   standard library only.
2. **No input-specific special cases.** Do not key behaviour on a file name,
   a target name, or a test path. If the rule is real, it comes from the
   language; if it only makes one test pass, it is a bug.
3. **Cite the reference implementation.** Every non-obvious rule needs a
   comment pointing at the LLVM C++ file and line range
   (`TGParser.cpp:4612-4613`, `Record.cpp:1522-1537`, ...). We do not ship LLVM
   source, but the behaviour must be traceable.
4. **Determinism.** The same input must produce byte-identical output across
   runs. Do not iterate Go maps where order reaches the output.
5. **Unsupported is loud.** Exit 2 with `E_UNSUPPORTED` and a span; never emit a
   silently wrong record.
6. **Keep the scoreboards green.** A change that alters output is only done
   when the oracle agrees, and `make check` must stay clean.

## Setup

```console
$ go build -o tblgen ./cmd/tblgen
$ go test ./...
$ make ci            # gofmt, vet, tests, coverage floor
```

The offline tests need no LLVM checkout. The differential tests need one:

```console
$ export LLVM_SRC=/path/to/llvm-project
$ export ORACLE=/path/to/build/bin/llvm-tblgen
$ make baseline      # once per oracle version
$ make check
$ make score
```

`TBLGEN_CORPUS` / `TBLGEN_ORACLE` are the environment equivalents of
`-corpus` / `-oracle` for `cmd/conformance`.

## Adding a test

- For a language rule, add a focused `.td` fixture in a `_test.go` file and
  compare against the oracle when both accept the input.
- For a new diagnostic, assert the message, the line, and (where it matters)
  the caret rendering; `internal/syntax/diag_test.go` and
  `internal/record/*_test.go` show the pattern.
- For a backend change, prefer a byte-level assertion: the scoreboards treat
  any output difference as a failure.

## Commit messages

Use an imperative subject line (`record: ...`, `syntax: ...`, `backend: ...`,
`conformance: ...`, `docs: ...`). When the change alters measured behaviour,
put the numbers in the body: command, match/diff counts, and any remaining
deviation. Small, reviewable commits are preferred over one large change.

## Pull requests

- Describe the problem and the oracle evidence.
- List the commands you ran (`make ci`, `make check`, `make score`, real-target
  `cmp`, ...).
- Call out any output that changed and why it now matches the oracle.

By contributing you agree that your contribution is licensed under the
Apache License 2.0 with LLVM Exceptions (see `LICENSE`).
