GO      ?= go
BIN     := tblgen
ORACLE  ?= /home/ubuntu/llvm/builds/record-loader/bin/llvm-tblgen
CORPUS  ?= /home/ubuntu/llvm/llvm-project/llvm/test/TableGen

.PHONY: build test baseline compare check stderr matrix show ast fmt ci vet fmt-check

build:
	$(GO) build -o $(BIN) ./cmd/tblgen

test:
	$(GO) test ./...

# One command a reviewer or a hook can run: formatting, vet, unit tests, and the
# ratchet against the committed status.json.
ci: fmt-check vet test check

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

show:
	$(GO) run ./cmd/conformance -show $(F) -oracle $(ORACLE)

ast: build
	./$(BIN) --dump-ast $(F)

fmt:
	gofmt -w .
