# Scope matrix — Go TableGen

Status legend: `todo` · `wip` · `done` · `out` (deliberate non-goal)

Reference: LLVM 24.0.0git at `/home/ubuntu/llvm/llvm-project`.
Oracle: `/home/ubuntu/llvm/builds/record-loader/bin/llvm-tblgen` (`--dump-json`).

## Lexical

| Feature | Status | Milestone | C++ ref |
|---|---|---|---|
| identifiers, keywords (24) | done | M1 | `TGLexer.cpp:422-447` |
| decimal / hex / binary integers, 64-bit | done | M1 | `TGLexer.cpp` `LexNumber` |
| strings + escapes (`\n \t \" \\ \xNN`) | done | M1 | `TGLexer.cpp` `LexString` |
| `[{ ... }]` code fragments | done | M1 | `TGLexer.cpp:601-627` |
| `!operator` lexing (58) | done | M1 | `TGLexer.cpp:631-673` |
| `#` name concat token | done | M1 | `TGLexer.h` |
| comments `//` and `/* */` | done | M1 | `TGLexer.cpp` |
| spans (file:line:col) on every token | done | M1 | — |
| `include "..."` recursion, search path, cycle/depth guard | done | M5 | `TGLexer.cpp` `LexInclude`, `SourceMgr::OpenIncludeFile` |

`include` is expanded in the lexer, as in LLVM: the parser never sees an
include token, macros are global across files, and the conditional stack is
per-file.

## Preprocessor (lexed and executed during lexing)

| Feature | Status | C++ ref |
|---|---|---|
| `#ifdef` / `#ifndef` | done | `TGLexer.cpp` `lexPreprocessor` |
| `#else` / `#endif`, double-`#else` rejection | done | same |
| `#define` and `-D` macros | done | `TGLexer.cpp` `DefinedMacros` |
| nested conditionals, control stack | done | `PrepIncludeStack` |
| skipped-region scan (handles comments and nested directives) | done | `prepSkipRegion` |
| directive word must be followed by space/EOL/comment | done | `prepIsDirective` |
| unterminated conditional, bad macro name, trailing junk | done | `prepSkipMacroName`, `prepSkipDirectiveEnd` |
| `include "..."` expansion | done | `LexInclude`, `processEOF` |

## Syntactic

| Feature | Status | Milestone | C++ ref |
|---|---|---|---|
| `class Name<args> : Base<args> { ... }` | done | M2 | `TGParser.cpp:4709` |
| `def Name : Class<args> { ... }`, `def Name;` | done | M2 | `TGParser.cpp:4689` |
| `defm` / `multiclass` | done | M2/M3 | `TGParser.cpp:4691,4715` |
| `defvar`, `defset`, `deftype` | done | M2/M3 | `TGParser.cpp:4693,4695,4705` |
| `let x = v;` (body) and `let x = v in { ... }` | done | M2 | `TGParser.cpp:4703` |
| `foreach x = [...] in { ... }` | done | M2 | `TGParser.cpp:4701` |
| `if cond then { ... } else { ... }` | done | M2 | `TGParser.cpp:4699` |
| `assert cond, msg;` / `dump x;` | done | M2 | `TGParser.cpp:4687,4697` |
| types `string int bit bits<N> list<T> code dag` + record types | done | M2 | `Record.h:134-235` |
| values: `?`, `[...]`, `{...}`, `(dag ...)`, `Class<...>`, `#` concat, slices | done | M2 | `TGParser.cpp` `ParseValue` |
| `CheckTemplateArgValues` (class/multiclass/anon argument types) | done | M5 | `TGParser.cpp:4749` |
| error recovery (one bad def does not kill the file) | done | M2 | — |

## Record graph / evaluation

| Feature | Status | Milestone | C++ ref |
|---|---|---|---|
| RecTy: bits/int/string/list/dag/record/code | done | M3 | `Record.h:134-235` |
| Init tree: 22 concrete classes | done (value-level; see deviations) | M3 | `Record.h:470-1580` |
| inheritance chain + field inheritance | done | M3 | `Record.cpp` |
| template argument substitution | done | M3 | `TGParser.cpp` `ParseClass` |
| `let` stacking / override order | done | M3 | `TGParser.cpp` `ParseLet` |
| anonymous `Class<...>` naming (`anonymous_<n>`) | done (allocation order may differ from LLVM) | M3 | `Record.cpp` |
| `defm` name mangling and suffixes | done | M3 | `TGParser.cpp` `ParseDefm` |
| class-body `assert` checked when a def is finished | done | M5 | `Record::checkRecordAssertions` |
| `!instanceof` grouping in JSON | done | M3 | `JSONBackend.cpp` |
| one pass, all diagnostics, no early exit | done | M3 | — |
| deterministic record order (source order) | done | M3 | — |
| names created by a dynamic statement-level `foreach` visible afterwards | done (second parse with names learned from one lowering pass) | M5 | `TGParser.cpp` `addEntry`/`resolve` |

## Bang operators (58)

| Group | Operators | Status | Milestone |
|---|---|---|---|
| compare/logic | `eq ne le lt gt ge and or xor not if cond switch` | done | M4 |
| list | `head tail size empty listconcat listflatten listsplat listremove range sort interleave foreach filter foldl` | done | M4 |
| string | `strconcat subst substr tolower toupper repr find` | done | M4 |
| bit/int | `add sub mul div logtwo shl sra srl` | done | M4 |
| dag | `dag setdagop getdagop setdagopname getdagopname getdagarg getdagname setdagarg setdagname` | done | M4 |
| record/reflect | `isa cast instances exists initialized match con` | done | M4 |

## Backends / CLI

| Feature | Status | Milestone |
|---|---|---|
| `--dump-json` | done | M3 |
| `--null-backend` | done | M3 |
| `-I` include dirs, `-D` macros | done | M5 |
| `--print-records` | todo | M6 |
| `--print-detailed-records` | todo | M6 |
| `-o` / dependency file | todo | M6 |
| `--version`, `--dump-tokens`, `--dump-ast` | done | M0/M2 |
| diagnostic format `file:line:col: error: msg` + caret + note | done | M5 |
| `--gen-*` target emitters | out | — |
| C++ backend plugin ABI | out | — |
| byte-identical diagnostic wording | done (measured by `make stderr`) | — |

## Known deviations (measured, not guessed)

1. **Anonymous instantiation.** LLVM instantiates a class application with
   concrete arguments at *parse* time (`VarDefInit::Fold`), even inside a class
   that is never used; this implementation instantiates on evaluation. A def
   whose class body contains such a value is therefore dumped with different
   `anonymous_N` indices, and an unused class contributes no anonymous def at
   all. This is the only remaining source of `diff` (145/392) and only names
   differ, never types or values. Unresolved values themselves are reproduced:
   `field` values print the same `{"kind":"var"/"varbit"/"complex"}` Init shapes
   as `JSONBackend.cpp` (`JSON.td` and `UnsetBitInit.td` are byte-identical).
2. **Diagnostic wording/columns** are close but not byte-identical; conformance
   compares the exit class and the line of the first error.

## Non-negotiables

1. Unsupported input exits non-zero with `E_UNSUPPORTED` + span. Never silently
   parse it into a wrong record.
2. `--dump-json` output is compared structurally against the oracle.
3. Two runs on the same input are byte-identical.
4. No third-party Go dependencies.

## Test assets: what LLVM's own suite gives us

`llvm/test/TableGen` is four layers of evidence, not one corpus:

| Layer | Size | How this project uses it |
|---|---|---|
| `.td` inputs | 494 files (509 with `Common/`) | `-mode compare` (JSON) and `-stderr` (diagnostics), one fixed invocation per file |
| `// RUN:` matrix | 211 language-only files, **329** invocations | `-matrix`: every `-D` variant, `stdin`, `-print-records`, `-o`/`-d` |
| `// CHECK:` assertions | 205 files use DAG/NOT/NEXT | **not used** — replaced by byte comparison against the oracle binary, which is strictly stronger |
| `llvm/unittests/TableGen` | 563 lines | not used: 40 of those lines are `ParserEntryPointTest`; the rest test emitters |

Backend tests (283 files with `-gen-*`) are out of scope; `-matrix` reports each
as `skipped` with the flag that caused it, so the gap is visible rather than
silent.

Three scores, all measured against the same 24.0.0git binary:

```bash
make compare   # JSON output, one invocation per file
make stderr    # diagnostic text: msg-match (wording) and full-match (incl. caret)
make matrix    # every RUN invocation
```
