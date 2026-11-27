# Conformance scoreboards

This directory holds generated results; the JSON files are deliberately not
committed.

- `baseline.json` — what the oracle does for every in-scope corpus file. One
  per oracle version; compare/check reuse it to avoid re-running the oracle.
- `status.json` — the last compare/check run. `make check` reads it as the
  ratchet.
- `stderr.json`, `matrix.json` — diagnostic and `RUN:`-matrix scoreboards.
- `score.json` — the combined summary written by `make score`.

All of them contain the absolute oracle and corpus paths, which is why they are
generated instead of checked in. Regenerate with:

```console
$ export LLVM_SRC=/path/to/llvm-project
$ export ORACLE=/path/to/build/bin/llvm-tblgen
$ make baseline
$ make score
```

The methodology and the current numbers are in
[../docs/CONFORMANCE.md](../docs/CONFORMANCE.md).
