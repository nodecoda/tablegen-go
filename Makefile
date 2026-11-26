GO      ?= go
BIN     := tblgen
ORACLE  ?= /home/ubuntu/llvm/builds/record-loader/bin/llvm-tblgen
CORPUS  ?= /home/ubuntu/llvm/llvm-project/llvm/test/TableGen
JOBS    ?= 12

COVER_MIN ?= 60
FUZZ      ?= FuzzParse
FUZZTIME  ?= 30s

.PHONY: build test baseline compare check stderr matrix show ast fmt ci vet fmt-check score report \
	cover cover-check bench fuzz

build:
	$(GO) build -o $(BIN) ./cmd/tblgen

test:
	$(GO) test ./...

# One command a reviewer or a hook can run: formatting, vet, unit tests, the
# coverage floor, and the ratchet against the committed status.json.
ci: fmt-check vet test cover-check check

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	$(GO) vet ./...

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

check: build
	$(GO) run ./cmd/conformance -mode check -oracle $(ORACLE) -corpus $(CORPUS)

# One number per question, plus a delta against the previous run.
score: build
	$(GO) run ./cmd/conformance -mode compare -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -stderr -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -matrix -jobs $(JOBS) -oracle $(ORACLE) -corpus $(CORPUS)
	$(GO) run ./cmd/conformance -summary

# Print the last score table without re-running anything.
report:
	$(GO) run ./cmd/conformance -summary

show:
	$(GO) run ./cmd/conformance -show $(F) -oracle $(ORACLE)

ast: build
	./$(BIN) --dump-ast $(F)

fmt:
	gofmt -w .

# Coverage of the language core. cover-check is the ratchet: it fails when
# internal/record drops below COVER_MIN (was 31.8% at the start of W6).
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

# AArch64 is the largest real input; the benchmarks pin parse, lower, records
# and JSON throughput on it.
bench:
	$(GO) test -run '^$$' -bench . -benchmem ./internal/backend

# The parser and lexer fuzz targets. FUZZ=FuzzLex, FUZZTIME=2m to vary.
fuzz:
	$(GO) test -run '^$$' -fuzz '^$(FUZZ)$$' -fuzztime $(FUZZTIME) ./internal/syntax
