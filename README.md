# tablegen-go

A Go implementation of LLVM's TableGen **language core**: lexer, parser, record
graph, and the record-level backends (`--dump-json`, `--print-records`,
`--print-detailed-records`). Target instruction-selection emitters are out of
scope.

Why from scratch: there is no usable Go implementation. The only Python one
(`daeken/PyTableGen`) is Python 2, unmaintained since 2016, and has no license.
The grammar and semantics are taken from the C++ source, and correctness is
enforced by **differential testing against the real `llvm-tblgen`**.

```
.td ──► lexer ──► parser ──► record graph ──► backend (json / records)
                                │
                                └── resolved against the C++ oracle
```

## Status

v1.0.0: the **language core and the record-level backends** are complete.
Lexing (including `include` expansion and the preprocessor), parsing, `defm`/
multiclass expansion, `dump`, the record graph, static type checking,
`--dump-json`, `--print-records`, `--print-detailed-records`, `-o` and `-d` all
run. Target instruction-selection emitters are out of scope and exit 2 with
`E_UNSUPPORTED`.

Differential state against llvm-tblgen 24.0.0git (392 top-level tests, 385
in-scope, 7 skipped because they need `split-file`):

| | count |
|---|---|
| `--dump-json` outputs byte-identical | 330 (match) |
| byte differences | 0 |
| rejected like the oracle | 55 (`reject-match`: same first-error line) |
| diagnostics with the same message | 62/62 |
| diagnostics rendered identically (message + caret) | 61/62 |

`make matrix` replays every `RUN:` line (including `-D` variants). It reports
295 identical runs, 2 identical diagnostic reports, 0 output / error / exit
differences, and 19 cases where the message matches but the wording of a
secondary note differs. `make score` prints all four numbers and a delta
against the previous run; `make check` is the ratchet.

Real-target evidence, beyond the corpus:

| input | `--print-records` | `--dump-json` |
|---|---|---|
| `lib/Target/AArch64/AArch64.td` | byte-identical (`d973796c…`) | byte-identical (`6b71e881…`, 106 MB / 88073 records) |
| `lib/Target/ARM/ARM.td` | exactly one line differs (below) | byte-identical (`52a31c5c…`, 49200 records) |
| `lib/Target/X86/X86.td` | byte-identical (`19343d7d…`) | byte-identical (`2d66a922…`, 88538 records) |
| `lib/Target/RISCV/RISCV.td` | same records, different anonymous numbers | exit 0, 266845 records (oracle 266770) |

The JSON run exercises `dump`, deferred `foreach`/`if` inside multiclasses,
anonymous instantiation order and the `!locs` chain; the site rules are pinned
by `internal/record/names_test.go`.

Known deviations (measured, not guessed):

1. **RISCV anonymous numbering / `!locs`.** All 45906 named records exist; 1302
   names differ only in their `anonymous_N` suffix, 13452 records differ only in
   `!locs`, and 172 more reference the renumbered instances. No field value,
   type, or superclass differs. The offset starts in SiFive7's
   `foreach model = [...] in defm model.Name` + SchedVariant instantiation
   order, where we create 75 extra anonymous instances.
2. **ARM `--print-records`, one line.** `MVE_VLDRSTR_rq_b`'s `InOperandList`
   prints `mve_addr_rq_shift<0: 0>` where llvm-tblgen, resolving the inherited
   field, already instantiated `anonymous_19464`. Display only; the JSON dump is
   byte-identical.
3. `preprocess-include.td` renders one diagnostic with a different caret line,
   and `make matrix` reports 19 runs whose secondary note wording differs.

## Commands

```bash
make build       # go build -o tblgen ./cmd/tblgen
make test        # go test ./...
make baseline    # record what llvm-tblgen does on llvm/test/TableGen (deterministic)
make compare     # replay the corpus through our binary; writes conformance/status.json
make check       # ratchet: a case that matched before must still match
make stderr      # diagnostic text + caret rendering
make matrix      # every RUN: line of every test
make score       # all four numbers, with a delta against the previous run
make cover       # coverage; make cover-check is the floor (60% for internal/record)
make bench       # AArch64 parse / lower / records / JSON benchmarks
make fuzz        # FuzzParse (FUZZ=FuzzLex, FUZZTIME=2m to vary)
make show F=path/to.td   # both outputs side by side
```

## Corpus

`/home/ubuntu/llvm/llvm-project/llvm/test/TableGen/*.td` — 392 top-level tests
(385 in scope): `RUN:` lines are scanned for `-I` and the same search path is
given to both binaries. 330 are accepted by the oracle under `--dump-json` and
produce byte-identical output; 55 are deliberate error tests, compared by the
**line of the first error**, so `reject-match` means "rejected the same
construct", not merely "rejected something".

Oracle: `/home/ubuntu/llvm/builds/record-loader/bin/llvm-tblgen` (24.0.0git).
LLVM 24's JSON includes `!locs`; LLVM 15's does not. The version is recorded in
`conformance/baseline.json`.

## Layout

```
cmd/tblgen/          CLI
cmd/conformance/     differential runner (baseline / compare / check)
internal/syntax/     token, lexer (include + preprocessor), AST, parser
internal/record/     types, values, record graph, lowering, bang operators
internal/backend/    json
docs/SCOPE.md        feature matrix with milestones and C++ references
conformance/         baseline.json (oracle truth), status.json (ratchet)
```

## Invariants

1. **Init is a pure data tree** — no closures, no lazy getters; dumpable and
   comparable.
2. **One pass, all diagnostics** — no early exit, no exceptions.
3. **Every node carries a span** — tokens, AST, records, diagnostics.
4. **Deterministic** — source order, sorted JSON keys, byte-identical reruns.
5. **Unsupported is loud** — `E_UNSUPPORTED` + span, never a silent wrong record.

## Non-goals

`--gen-*` emitters, C++ plugin ABI, byte-identical diagnostic wording. See
`docs/SCOPE.md`.
