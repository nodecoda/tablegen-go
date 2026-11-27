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
| diagnostics rendered identically (message + caret) | 62/62 |

`make matrix` replays every `RUN:` line (including `-D` variants). It reports
314 identical runs, 2 identical diagnostic reports, and 0 output, error, or
exit differences. `make score` prints all four numbers and a delta against the
previous run; `make check` is the ratchet.

Real-target evidence, beyond the corpus:

| input | `--print-records` | `--dump-json` |
|---|---|---|
| `lib/Target/AArch64/AArch64.td` | byte-identical (`d973796c…`, 63,660,222 bytes) | byte-identical (`6b71e881…`, 106 MB / 88071 records) |
| `lib/Target/ARM/ARM.td` | byte-identical (`06bccac5…`, 37,321,559 bytes) | byte-identical (`52a31c5c…`, 49198 records) |
| `lib/Target/X86/X86.td` | byte-identical (`19343d7d…`, 112,734,356 bytes) | byte-identical (`2d66a922…`, 88536 records) |
| `lib/Target/RISCV/RISCV.td` | byte-identical (`7fcf89cf…`, 153,870,634 bytes) | 266768 records, same names; only `!locs` differs (below) |

The JSON run exercises `dump`, deferred `foreach`/`if` inside multiclasses,
anonymous instantiation order and the `!locs` chain; the site rules are pinned
by `internal/record/names_test.go`. Diagnostics carry the include instance in
`diag.Pos.Inc`, because SourceMgr opens a fresh buffer per `include` and the
same file included twice has two different `Included from` chains
(`SourceMgr.cpp:58-67`); `internal/diag/diag_test.go` pins that rendering.

Known deviations (measured, not guessed):

1. **RISCV `--dump-json` `!locs` only.** Both outputs describe the same 266,768
   records with identical names, field values, types, and superclasses; the only
   byte difference is inside `!locs`: 164,416 lists are identical, 102,352 are
   not (101,833 miss trailing entries, 519 miss a middle one). The missing
   entries are `defm` instantiation sites, which the C++ parser appends through
   `Record::appendLoc` (`Record.h:1771`, from `TGParser::ParseDefm`'s
   `resolve(..., &ParentClassLoc)`, `TGParser.cpp:4593,4612-4613` →
   `TGParser.cpp:561`) and the JSON backend prints verbatim
   (`JSONBackend.cpp:161-165`). Two cases:
   - **Trailing.** LLVM unrolls a multiclass `foreach` whose list is already a
     list at parse time (`AddParentMultiClass` → `resolve(..., Final=false)`,
     `TGParser.cpp:415`), so its records become direct entries and take the
     `defm` site. We keep that loop deferred, and the loop path of `resolve`
     deliberately does not carry `Loc` (`TGParser.cpp:530-531`).
     `ReadVIMovX_MF8` is defined in the `foreach` at `RISCVScheduleV.td:168`
     and instantiated by `defm "" : LMULSchedReads<"ReadVIMovX">` at `:675`;
     the oracle lists both, we list only the first.
   - **Middle.** A `defm` inside a multiclass body gets its own site when that
     body is parsed; we drop it (`WriteVC_FPR16VV_M1`: `162,14,31` vs
     `162,31`, where `:14` is the nested `defm` in `RISCVScheduleXSf.td`).
   Record semantics are unaffected; `--print-records` is byte-identical.

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
