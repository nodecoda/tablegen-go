# Security policy

## Supported versions

| version | supported |
|---|---|
| 1.0.x | yes |
| < 1.0 | no |

## Reporting a vulnerability

Please use GitHub's private vulnerability reporting on this repository
("Security" tab → "Report a vulnerability"). Do not open a public issue for a
suspected vulnerability.

Include the input file, the command line, and the observed behaviour. If the
issue is a crash, a Go panic trace is enough; if it is resource exhaustion,
include the input size and the memory/time limits you used.

## Scope

`tblgen` is an offline compiler: it reads `.td` files (and files reached
through `include`), writes to stdout or a path given with `-o`/`-d`, and never
opens a network connection or executes input as code. The interesting classes
of issue are therefore:

- parser or lowering panics on malformed input;
- unbounded memory or time on adversarial input (the evaluator has a depth
  limit, but very large inputs can still be expensive);
- path traversal through `include` or `-d` file names.

Out of scope: vulnerabilities in LLVM itself, and outputs that differ from
`llvm-tblgen` for constructs the project documents as unsupported.
