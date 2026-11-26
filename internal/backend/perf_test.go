package backend

import (
	"os"
	"testing"

	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

// a64Root is the LLVM checkout used for the end-to-end performance benchmark.
// The benchmark skips when it is not present, so `go test ./...` never
// depends on a local llvm-project.
const a64Root = "/home/ubuntu/llvm/llvm-project/llvm"

func lowerAArch64(b *testing.B) *record.Keeper {
	b.Helper()
	file := a64Root + "/lib/Target/AArch64/AArch64.td"
	src, err := os.ReadFile(file)
	if err != nil {
		b.Skip("llvm-project not checked out at " + a64Root)
	}
	f, diags := syntax.ParseWith(file, string(src), nil,
		[]string{a64Root + "/lib/Target/AArch64", a64Root + "/include"}, nil, syntax.Options{})
	for _, d := range diags {
		if d.Severity == 2 {
			b.Fatalf("parse: %v", d)
		}
	}
	k, ldiags := record.Lower(f, file)
	for _, d := range ldiags {
		if d.Severity == 2 {
			b.Fatalf("lower: %v", d)
		}
	}
	return k
}

// BenchmarkAArch64Records times the full pipeline for the largest real input
// in the tree: parse, lower, and render --print-records.
func BenchmarkAArch64Records(b *testing.B) {
	for i := 0; i < b.N; i++ {
		k := lowerAArch64(b)
		out, err := PrintRecords(k)
		if err != nil {
			b.Fatal(err)
		}
		if len(out) == 0 {
			b.Fatal("empty output")
		}
	}
}

// BenchmarkAArch64Lower isolates parse+lower from rendering.
func BenchmarkAArch64Lower(b *testing.B) {
	for i := 0; i < b.N; i++ {
		lowerAArch64(b)
	}
}

// BenchmarkAArch64Parse isolates lexing + parsing.
func BenchmarkAArch64Parse(b *testing.B) {
	file := a64Root + "/lib/Target/AArch64/AArch64.td"
	src, err := os.ReadFile(file)
	if err != nil {
		b.Skip("llvm-project not checked out at " + a64Root)
	}
	for i := 0; i < b.N; i++ {
		f, _ := syntax.ParseWith(file, string(src), nil,
			[]string{a64Root + "/lib/Target/AArch64", a64Root + "/include"}, nil, syntax.Options{})
		if f == nil {
			b.Fatal("no file")
		}
	}
}
