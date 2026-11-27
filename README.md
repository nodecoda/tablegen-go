# tablegen-go

[![CI](https://github.com/nodecoda/tablegen-go/actions/workflows/ci.yml/badge.svg)](https://github.com/nodecoda/tablegen-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/nodecoda/tablegen-go.svg)](https://pkg.go.dev/github.com/nodecoda/tablegen-go)
[![License: Apache-2.0 WITH LLVM-exception](https://img.shields.io/badge/license-Apache--2.0%20WITH%20LLVM--exception-blue.svg)](LICENSE)

A zero-dependency Go implementation of LLVM TableGen's **language core** and
**record-level backends**: lexer, preprocessor, parser, record graph, and
`--dump-json` / `--print-records` / `--print-detailed-records`. Target
instruction-selection emitters (`-gen-*`) are deliberately out of scope.

Correctness is defined by differential testing against the real
`llvm-tblgen`, not by hand-written expectations:

```
.td ──► lexer ──► parser ──► record graph ──► backend (json / records)
                                │
                                └── compared byte-for-byte with llvm-tblgen
```

## Status

v1.0.1: the language core and the record-level backends are complete.

Differential state against `llvm-tblgen 24.0.0git` on `llvm/test/TableGen`
(392 top-level tests, 385 in scope, 7 need `split-file`):

| check | result |
|---|---|
| `--dump-json` | 330 byte-identical, 0 differences |
| error tests | 55 rejected at the oracle's first-error line (`reject-match`) |
| diagnostics | 62/62 same message, 62/62 same full rendering (caret included) |
| `RUN:` matrix | 314 identical + 2 identical-report, 0 exit/output/error differences |

Beyond the corpus (oracle and this implementation, `cmp` clean):

| input | `--print-records` | `--dump-json` |
|---|---|---|
| AArch64.td | byte-identical | byte-identical (106 MB / 88,071 records) |
| ARM.td | byte-identical | byte-identical (49,198 records) |
| X86.td | byte-identical | byte-identical (88,536 records) |
| RISCV.td | byte-identical | 266,768 records, same names; only `!locs` differs |

The only known output deviation is the RISCV `!locs` metadata: 102,352 of
266,768 records list fewer `defm` instantiation sites than the oracle, while
every name, field value, type, and superclass matches. `--print-records` is
byte-identical. The cause is a phase difference in foreach expansion; see
[docs/CONFORMANCE.md](docs/CONFORMANCE.md#known-deviations).

## Build and run

```console
$ go build -o tblgen ./cmd/tblgen
$ ./tblgen --print-records file.td
$ ./tblgen -I llvm/include -I llvm/lib/Target/RISCV \
      --dump-json llvm/lib/Target/RISCV/RISCV.td
$ ./tblgen --version
```

Go 1.22 or newer. There are no third-party dependencies.

## Conformance testing

The oracle and the LLVM test corpus live outside this repository:

```console
$ export LLVM_SRC=/path/to/llvm-project          # the directory containing llvm/
$ export ORACLE=/path/to/build/bin/llvm-tblgen   # built from the same tree
$ make baseline   # record the oracle's behaviour once per oracle version
$ make compare    # replay the corpus through ./tblgen
$ make check      # ratchet: a case that matched before must still match
$ make score      # JSON + diagnostics + RUN matrix, with a delta
$ make ci         # offline gate: gofmt, vet, tests, coverage floor
```

Scoreboards are generated and not checked in: they embed the oracle path and
the machine's corpus path. See [docs/CONFORMANCE.md](docs/CONFORMANCE.md) for
what each layer covers and how the numbers above were measured.

## Layout

```
cmd/tblgen/          CLI
cmd/conformance/     differential runner (baseline / compare / check / stderr / matrix)
internal/syntax/     token, lexer (include + preprocessor), AST, parser
internal/record/     types, values, record graph, lowering, bang operators
internal/backend/    json, records, detailed records, sets
internal/diag/       diagnostics and rendering
conformance/         generated scoreboards (ignored; see conformance/README.md)
docs/                architecture, conformance, roadmap
```

## Design principles

1. **Init is a pure data tree** — no closures, no lazy getters; class bodies
   stay as expressions until a `def` instantiates them.
2. **Phase fidelity** — LLVM's resolve order decides anonymous numbering,
   `!locs`, and folded forms, so the lowering mirrors `TGParser`/`Record`
   phases rather than "simplifying" them.
3. **One pass, all diagnostics** — errors are collected, never thrown.
4. **Every node carries a span** — tokens, AST, records, diagnostics.
5. **Deterministic** — source order, sorted JSON keys, byte-identical reruns.
6. **Unsupported is loud** — `E_UNSUPPORTED` plus a span and exit 2, never a
   silently wrong record.

More detail: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

## Non-goals

`-gen-*` target emitters, the C++ backend plugin ABI, and byte-identical
wording for diagnostics that are not exercised by the corpus. See
[docs/ROADMAP.md](docs/ROADMAP.md) for the known gaps.

## Contributing

Issues and pull requests are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md)
first; the short version is: no new dependencies, cite the LLVM C++ source for
every non-obvious rule, and keep the oracle scoreboards green.

## License

Apache License 2.0 with LLVM Exceptions. See [LICENSE](LICENSE) and
[NOTICE](NOTICE). This project is independent and is not affiliated with the
LLVM project.
