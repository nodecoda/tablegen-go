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

M5: **language core complete for the whole LLVM TableGen test corpus.** Lexing
(including `include` expansion and the preprocessor), parsing, `defm`/multiclass
expansion, the record graph, static type checking, and `--dump-json` all run.
`--print-records`, `--print-detailed-records` and `-o` are still `E_UNSUPPORTED`.

Differential state (`make compare`, 392 in-scope files):

| | oracle valid | oracle invalid |
|---|---|---|
| ours valid | 330 (187 byte-identical) | 0 |
| ours invalid | 0 | 62 (60 same first-error line) |

```
$ make compare
mode=compare  files=392  in-scope=392  needs-include=0
  diff           145
  match          187
  reject-match   60

$ make check                    # ratchet: no previously-matching case regressed
$ go test ./...                 # lexer/parser/include units + corpus ratchet
```

All 145 `diff` cases are byte differences on inputs both sides *accept*; there
is no accept/reject disagreement left. The cause is anonymous record
instantiation. llvm-tblgen instantiates a class application with concrete
arguments **while parsing the value** (`VarDefInit::Fold`), even inside a class
that is never used:

```tablegen
class X<int n> { int v = n; }
class C { X x = X<1>; }   // C never instantiated
def D { int v = 1; }
```

`llvm-tblgen --dump-json` still emits `anonymous_0` for `X<1>`; this
implementation only instantiates when a record is evaluated, so the anonymous
record (and its `anonymous_N` index) is missing. The rest of the output — types,
values, `!instanceof`, field order, unresolved `var`/`varbit`/`complex` JSON
shapes — matches.

## Commands

```bash
make build       # go build -o tblgen ./cmd/tblgen
make test        # go test ./...
make baseline    # record what llvm-tblgen does on llvm/test/TableGen (deterministic)
make compare     # replay the corpus through our binary; writes conformance/status.json
make check       # ratchet: a case that matched before must still match
make show F=path/to.td   # both outputs side by side
```

## Corpus

`/home/ubuntu/llvm/llvm-project/llvm/test/TableGen/*.td` — 392 top-level tests,
all run in-scope: `RUN:` lines are scanned for `-I` and the same search path is
given to both binaries. 330 are accepted by the oracle under `--dump-json`, 62
are deliberate error tests — those are compared by the **line of the first
error**, so `reject-match` means "rejected the same construct", not merely
"rejected something".

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
