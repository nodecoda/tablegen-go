# Conformance testing

The reference is a real `llvm-tblgen` binary. A baseline run records what it
does for every self-contained test; compare/check replay the same inputs
through `./tblgen` and classify each case.

## Prerequisites

- Go 1.22+
- An `llvm-project` checkout and an `llvm-tblgen` built from it. The published
  numbers use LLVM `24.0.0git` and the oracle's JSON includes `!locs`.
- `LLVM_SRC` = the directory that contains `llvm/`
- `ORACLE` = the matching `llvm-tblgen` binary

```console
$ export LLVM_SRC=/path/to/llvm-project
$ export ORACLE=/path/to/build/bin/llvm-tblgen
$ make baseline    # once per oracle version
$ make compare     # JSON output, one invocation per file
$ make stderr      # diagnostic wording + caret rendering
$ make matrix      # every RUN: invocation, including -D variants and stdin
$ make score       # all three, plus the delta against the previous run
$ make check       # ratchet against the previous status.json
```

The scoreboards (`baseline.json`, `status.json`, `stderr.json`,
`matrix.json`, `score.json`) are generated and ignored by git: they embed the
oracle and corpus paths. `make baseline` is the expensive step; after it,
compare/check reuse 383 of 392 oracle results.

## Corpus layers

`llvm/test/TableGen` provides four kinds of evidence:

| layer | size | used for |
|---|---|---|
| `.td` inputs | 392 top-level files | JSON compare, diagnostics, matrix |
| `RUN:` lines | 607 invocations | every `-D`/stdin/`-print-records` variant |
| `CHECK:` lines | 205 files | **not used** — byte comparison against the oracle is stronger for the outputs we produce |
| emitters | 283 files | skipped and reported by name, never silently dropped |

`split-file`-based tests (7) cannot be replayed as a single invocation and are
reported as skipped.

## Reading the scoreboard

- `match`: oracle and ours both exit 0 with identical stdout.
- `reject-match`: both reject the input at the same first-error line.
- `same-report`: diagnostics are identical and both exit 0; only the trailing
  allocator statistics differ (see below).
- `diff`: a real output difference.
- `skipped`: outside what this binary can run, with the reason recorded.

Current numbers (LLVM 24.0.0git):

| check | result |
|---|---|
| `--dump-json` | 330 match, 0 diff, 55 reject-match, 7 skipped |
| diagnostics | msg-match 62/62, full-match 62/62, 0 diff |
| matrix | 314 same + 2 same-report, exit/out/err diff 0, 291 skipped |

## Known deviations

1. **RISCV `--dump-json` `!locs`.** 266,768 records with the same names, field
   values, types, and superclasses. 164,416 `!locs` lists are identical;
   102,352 differ — 101,833 miss trailing `defm` sites, 519 miss a middle one.
   Example: `ReadVIMovX_MF8` is defined in a multiclass `foreach` at
   `RISCVScheduleV.td:168` and instantiated at `RISCVScheduleV.td:675`; the
   oracle lists both, we list only the first. LLVM unrolls a multiclass
   `foreach` with a parse-time-concrete list during inheritance
   (`TGParser.cpp:415`), so its records take the `defm` location; we keep the
   loop deferred and the loop path does not carry `Loc` (`TGParser.cpp:530-531`).
   `--print-records` is byte-identical.
2. **Invalid UTF-8 in string values.** The oracle's JSON value constructor
   asserts on invalid UTF-8 (`JSON.h:333`), so an assertions build aborts
   instead of producing reference bytes. We escape each invalid byte as
   U+FFFD instead: valid JSON and deterministic, with no oracle output to
   match. This does not occur in the corpus.
3. **`--print-detailed-records` tail.** The C++ backend ends with
   `dumpAllocationStats`, which counts LLVM's internal uniquing pools. Those
   numbers do not depend on the TableGen program, so this implementation stops
   before them; every emitted byte is a prefix of the oracle's output. The
   matrix records this as `same-report` (`anonymous-location.td`,
   `detailed-records.td`).

## Real targets

The scoreboards cover the test corpus. The four large targets were additionally
checked by hand (`cmp` on both backends): AArch64, ARM, X86, RISCV. See the
table in the README; the measurements were taken with the same oracle version.

## Performance

```console
$ /usr/bin/time -f "%e s %M KB" ./tblgen \
    -I "$LLVM_SRC/llvm/include" -I "$LLVM_SRC/llvm/test/TableGen/Common" \
    "$LLVM_SRC/llvm/test/TableGen/dag-isel-subregs.td" --dump-json > /dev/null
```

2.90–3.09 s across 11 runs (median ~3.00 s), 584–591 MB peak RSS, for a
28,826,421-byte JSON dump. The oracle takes 1.48–1.54 s and 1.11 GB.
