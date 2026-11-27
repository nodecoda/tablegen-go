# Architecture

`tablegen-go` follows the same pipeline as LLVM's `llvm-tblgen`, because the
output is defined by that pipeline's observable behaviour:

```
source ──► lexer ──► parser ──► lowering ──► record graph ──► backend
           (include,    (AST)     (inherit,     (Keeper)        (json, text)
            #ifdef)               defm, let,
                                  foreach)
```

The packages:

| package | responsibility |
|---|---|
| `internal/syntax` | tokens, lexer, `include`, preprocessor, AST, parser |
| `internal/record` | types, values, record graph, lowering, bang operators, assertions |
| `internal/backend` | `--dump-json`, `--print-records`, `--print-detailed-records`, `--print-sets` |
| `internal/diag` | diagnostics, spans, include chains, rendering |
| `cmd/tblgen` | CLI, action selection, `-o`/`-d`, `-E` |
| `cmd/conformance` | differential runner against the real `llvm-tblgen` |

## Why the layers are separate

The parser never performs inheritance, template substitution, or evaluation.
It produces an AST plus per-statement information that only lowering needs.
That separation is what makes `--dump-ast`, the diagnostic positions, and the
backends independent, and it keeps the record graph dumpable.

## Init is a data tree, not code

A class body is stored as `Expr` + the lexical `Env` it was written in. Fields
are not closures and not eager values: a `def` substitutes template arguments
and evaluates inherited fields when it is finished. This mirrors LLVM's `Init`
tree and is what allows one class body to serve arbitrarily many instantiations
without re-parsing.

`internal/record/deferred.go` models LLVM's folding rules: operands are
evaluated first, `!and`/`!or` short-circuit, `!if`/`!cond`/`!switch` only defer
when the selector is unknown, and `!foreach` folds over a concrete list even
when the element values are unknown. Every rule is written to preserve the
printed form when a value cannot fold.

## Phase fidelity

Most "weird" output differences come from *when* something is resolved, not
from the final value. The implementation therefore models phases explicitly:

- parse-time versus instantiation-time loops (`parseTime`, `foldMask`);
- `defm` expansion in three phases: resolve entries, apply trailing classes,
  then apply the let stack and finalize (`addDefOne`);
- `let` precedence across nested `defm` levels;
- anonymous instance allocation and class-application sites (`sites.go`);
- a names-only lowering pass (`LowerNames`) so that later statements can see
  record names created by dynamic loops, followed by a re-parse.

Comments cite the corresponding `TGParser.cpp` / `Record.cpp` line ranges. If a
rule cannot be justified that way, it does not belong in the code.

Known limit: a multiclass `foreach` whose list is concrete at parse time is
unrolled by LLVM while the multiclass is inherited; we keep it deferred. That
changes only the `!locs` metadata of the RISCV dump, not record semantics.

## Determinism

- records are emitted in source order; classes and defs are sorted by name;
- JSON keys are sorted, and the streaming encoder reproduces
  `encoding/json`'s bytes exactly (`json_ref_test.go` keeps the old tree
  encoder as a reference);
- two runs on the same input are byte-identical.

## Diagnostics

Diagnostics are collected, not thrown. A broken object is skipped so later
objects still produce errors. Source positions carry the include instance, so
the same file included twice prints two different `Included from` chains, as
LLVM's `SourceMgr` does. Unsupported constructs use a separate error type and
surface as `E_UNSUPPORTED` with exit code 2.

## Performance

The JSON writer is streaming; types and small values are interned; the lowering
pass avoids rebuilding maps per record. On `dag-isel-subregs.td` (28.8 MB JSON)
a run takes 2.9–3.1 s and peaks at about 585 MB RSS, versus 1.5 s and 1.1 GB
for the C++ oracle. Correctness comes first; the remaining gap is mostly
allocation and GC pressure.
