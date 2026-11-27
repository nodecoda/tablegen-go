# Roadmap and known gaps

The language core and record-level backends are complete for v1.0. The
remaining gaps, in rough priority order:

## 1. Emitters (`-gen-*`) — out of scope

276 of the 291 skipped `RUN:` invocations are target emitters (global isel,
register info, assembler matcher/writer, disassembler, intrinsics, subtarget,
DAG isel, ...). Without them the binary cannot replace `llvm-tblgen` in an LLVM
build. This is a deliberate scope boundary, not a bug.

## 2. Record loading (`--load-records`)

The oracle used for the scoreboards is built from a patched LLVM tree that adds
`--load-records`: it appends a serialized definition table to the parsed
input. That is the natural bridge for an external DSL that wants to reuse
TableGen's backends without printing `.td`. This implementation does not support
it, and the upstream test corpus does not contain its test. `--dump-json` is
also not a round-trip format: it has no template-argument or `defm` sequence.

## 3. `--print-enums --class=<name>`

A record-level action that prints the names derived from one class. It is not
implemented (unknown flags currently exit 2 through the flag parser).

## 4. RISCV `!locs`

266,768 records with identical names, fields, types, and superclasses; the only
difference is that 102,352 `!locs` lists miss `defm` instantiation sites
(101,833 trailing, 519 in the middle). Root cause: the phase at which a
multiclass `foreach` with a parse-time-concrete list is unrolled. Fixing it
must keep AArch64/ARM/X86 byte-identical.

## 5. Driver options

`--write-if-changed`, `--time-phases`, `--color`, and `--help-list` are not
implemented.

## 6. Test and tooling coverage

- 7 corpus tests need `split-file` and are skipped.
- The matrix skips 276 emitter runs plus 15 harness-only runs (`rm`,
  `split-file`, `cat`/`echo`/`printf`, two unknown lit substitutions).
- The oracle is pinned to one version (`24.0.0git`); other LLVM versions have
  different diagnostics and, before LLVM 24, no `!locs` in JSON.
- Real-target checks are a manual workflow, not part of `make ci`.
- Diagnostics are byte-identical for the corpus, not guaranteed outside it.
- Invalid UTF-8 in a string value has no oracle reference (the assertions
  build aborts at `JSON.h:333`); we emit U+FFFD escapes.

## 7. Performance

2.9–3.1 s versus 1.5 s on a 28.8 MB JSON dump. Memory is already lower than the
oracle; the remaining cost is allocation and GC.
