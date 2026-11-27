# Changelog

All notable changes to this project are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [1.0.0] - 2026-11-27

First public release: the TableGen language core and the record-level
backends.

### Added

- Lexer with `include` expansion, `#ifdef`/`#ifndef`/`#define`, `-D` macros,
  code blocks, and all 58 bang-operator tokens.
- Parser for `class`, `def`, `defm`, `multiclass`, `defvar`, `defset`,
  `deftype`, `let`, `foreach`, `if`, `assert`, and `dump`, with error
  recovery.
- Record graph: inheritance, template argument substitution, `let` stacking,
  `defm` name mangling, anonymous instances, static type checking, deferred
  folding, and class assertions.
- Backends: `--dump-json`, `--print-records`, `--print-detailed-records`,
  `--print-sets`, `--null-backend`, plus `-o`, `-d`, and `-E`.
- LLVM-compatible diagnostics (`file:line:col: error: message`, caret line,
  notes, include chains).
- `cmd/conformance`: baseline / compare / check / stderr / matrix / score,
  including a ratchet against a previous run.
- `E_UNSUPPORTED` for out-of-scope `-gen-*` actions: a stable message and exit
  code 2, never a silently wrong record.

### Evidence

Measured against `llvm-tblgen 24.0.0git` on `llvm/test/TableGen`:

- `--dump-json`: 330 byte-identical, 0 differences; 55 error tests rejected at
  the oracle's first-error line.
- Diagnostics: 62/62 same message and 62/62 same full rendering.
- `RUN:` matrix: 314 identical + 2 identical-report, 0 exit/output/error
  differences.
- Real targets: `--print-records` byte-identical for AArch64, ARM, X86, and
  RISCV; `--dump-json` byte-identical for AArch64, ARM, and X86.
- `internal/record` coverage: 67.1% (floor 60%).

### Known deviations

- RISCV `--dump-json`: the same 266,768 records and field values; 102,352
  `!locs` lists miss `defm` instantiation sites (metadata only).
- `--print-detailed-records`: the trailing LLVM-internal allocator statistics
  are not emitted; the output is a byte prefix of the oracle's.

[Unreleased]: https://github.com/nodecoda/tablegen-go/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/nodecoda/tablegen-go/releases/tag/v1.0.0
