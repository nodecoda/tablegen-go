# tblgen v1.0.0

A zero-dependency Go implementation of LLVM TableGen's **language core** and
**record-level backends**, validated by differential testing against
`llvm-tblgen 24.0.0git`.

## Supported surface

- Lexer, preprocessor (`-D`, `-I`, `include`), parser, `defm`/multiclass
  expansion, `dump`, static type checking, and the record graph.
- `--dump-json`, `--print-records`, `--print-detailed-records`, `-o`, `-d`,
  `-E`, `--null-backend`, `--dump-tokens`, `--dump-ast`, `--version`.
- Diagnostics in LLVM's `file:line:col: error: msg` form with caret and notes.
- Everything else — the `-gen-*` target emitters — is out of scope and exits 2
  with `E_UNSUPPORTED`. It never silently prints nothing.

## Evidence

Measured with `make score` against `llvm-tblgen 24.0.0git` on
`llvm/test/TableGen` (392 tests, 385 in scope, 7 need `split-file`):

| check | result |
|---|---|
| `--dump-json` | 330 byte-identical, 0 diffs |
| error tests | 55 rejected at the oracle's first-error line |
| diagnostics | 62/62 same message, 61/62 same full rendering |
| `RUN:` matrix | 295 identical, 0 output/error/exit diffs, 19 note-wording |

Beyond the corpus, `lib/Target/AArch64/AArch64.td` is byte-identical through
both `--print-records` and `--dump-json` (106 MB, 88073 records).

## Known deviations

1. **RISCV / ARM**: record names that only exist after a multiclass expression
   is resolved (`def Ext.Suffix` under `let ... in { defm ... }`,
   `MVE_VMLADAVu32`) are not in the parser's name table when a later statement
   references them, so those targets fail with `Variable not defined`.
2. **X86**: parses fully, but 135 of 88538 records differ in one inherited
   field (`ExeDomain` and the `TSFlags` bits derived from it).
3. `preprocess-include.td`: same message and exit class, different caret line.

None of these shapes occurs in the test corpus; they are tracked as the next
work items. `make ci` (fmt, vet, tests, coverage floor, ratchet) is green at
this tag.

## Replay

```console
$ make score     # corpus numbers + delta against the previous run
$ make ci        # fmt-check, vet, go test, coverage floor, ratchet
```
