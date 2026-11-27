GO       ?= go
BIN      := tblgen

# Conformance inputs. LLVM_SRC is the directory that contains llvm/ (an
# llvm-project checkout). Point ORACLE at a matching llvm-tblgen; the version
# recorded by the scoreboard is LLVM 24.0.0git.
LLVM_SRC ?= ../llvm-project
ORACLE   ?= llvm-tblgen
CORPUS   ?= $(LLVM_SRC)/llvm/test/TableGen
JOBS     ?= 12

COVER_MIN ?= 60
FUZZ      ?= FuzzParse
FUZZTIME  ?= 30s

.PHONY: build install test baseline compare check stderr matrix show ast fmt ci ci-full vet \
	fmt-check score report cover cover-check bench fuzz clean

build:
	$(GO) build -o $(BIN) ./cmd/tblgen

install:
	$(GO) install ./cmd/tblgen

# Offline gate: formatting, vet, unit tests, coverage floor. This is what CI
# runs; the oracle-based ratchet needs an llvm-project checkout.
ci: fmt-check vet test cover-check

# Full local gate, including the ratchet against conformance/status.json.
ci-full: ci check

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt:
	gofmt -w .

vet:
	$(GO) vet ./...

test:
	$(GO) test ./...

# Record the oracle's behaviour on the corpus. Run this once per oracle
# version; compare/check reuse it instead of re-running the oracle.
baseline:
	$(GO) run ./cmd/conformance -mode baseline -oracle $(ORACLE) -corpus $(CORPUS)

compare: build
	$(GO) run ./cmd/conformance -mode compare -oracle $(ORACLE) -corpus $(CORPUS)

# Diagnostics conformance: same error/warning text (msg-match) and same full
# rendering including caret lines (full-match).
stderr: build
	$(GO) run ./cmd/conformance -stderr -oracle $(ORACLE) -corpus $(CORPUS)

# Replay every RUN line (all -D variants, stdin, -print-records). Backend
# emitters are reported as skipped, never silently dropped.
matrix: build
	$(GO) run ./cmd/conformance -matrix -oracle $(ORACLE) -corpus $(CORPUS)

# Ratchet: a case that matched before must still match.
check: build
	$(GO) run ./cmd/conformance -mode check -oracle $(ORACLE) -corpus $(CORPUS)

# All three scoreboards, plus a delta against the previous run.
score: build
	$(GO) run ./cmd/conformance -mode compare -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -stderr -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -matrix -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -summary

# Print the last combined score table without re-running anything.
report:
	$(GO) run ./cmd/conformance -summary

show:
	$(GO) run ./cmd/conformance -show $(F) -oracle $(ORACLE)

ast: build
	./$(BIN) --dump-ast $(F)

# Coverage of the language core. cover-check is the ratchet: it fails when
# internal/record drops below COVER_MIN.
cover:
	$(GO) test -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

cover-check:
	$(GO) test -count=1 -coverprofile=coverage.out ./internal/record
	@total=$$($(GO) tool cover -func=coverage.out | awk '/^total:/{sub(/%/,"",$$3); print $$3}'); \
	if [ -z "$$total" ]; then echo "coverage: no total line"; exit 1; fi; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { \
		if (t + 0 < m + 0) { printf "internal/record coverage %.1f%% < %s%%\n", t, m; exit 1 } \
		printf "internal/record coverage %.1f%% (floor %s%%)\n", t, m }'

# Real target benchmarks. They skip unless LLVM_SRC points at an llvm-project
# checkout (LLVM_SRC is exported so the tests see it).
bench:
	LLVM_SRC=$(LLVM_SRC) $(GO) test -run '^$$' -bench . -benchmem ./internal/backend

# The parser and lexer fuzz targets. FUZZ=FuzzLex, FUZZTIME=2m to vary.
fuzz:
	$(GO) test -run '^$$' -fuzz '^$(FUZZ)$$' -fuzztime $(FUZZTIME) ./internal/syntax

clean:
	$(GO) clean
	rm -f $(BIN) coverage.out
