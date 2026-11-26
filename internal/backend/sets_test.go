package backend

import (
	"testing"

	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

func setsSrc(t *testing.T, src string) *record.Keeper {
	t.Helper()
	f, pdiags := syntax.Parse("t.td", src, nil, nil)
	for _, d := range pdiags {
		if d.Severity == diag.Error {
			t.Fatalf("parse: %v", pdiags)
		}
	}
	k, ldiags := record.Lower(f, "t.td")
	for _, d := range ldiags {
		if d.Severity == diag.Error {
			t.Fatalf("lower: %v", ldiags)
		}
	}
	return k
}

// TestPrintSetsOperators covers every SetTheory operator at once; the expected
// text is the output of llvm-tblgen -print-sets for the same input.
func TestPrintSetsOperators(t *testing.T) {
	k := setsSrc(t, `
class Set<dag d> { dag Elements = d; }
def a; def b; def c;
def add; def sub; def and; def shl; def trunc;
def rotl; def rotr; def decimate; def interleave; def sequence;
def S0 : Set<(add a, b, a, c)>;
def S1 : Set<(sub S0, b)>;
def S2 : Set<(and S0, S1)>;
def S3 : Set<(shl S0, 2)>;
def S4 : Set<(trunc S0, 2)>;
def S5 : Set<(rotl S0, 1)>;
def S6 : Set<(rotr S0, 1)>;
def S7 : Set<(decimate S0, 2)>;
def S8 : Set<(interleave S1, S3)>;
def e0; def e1; def e2;
def S9 : Set<(sequence "e%u", 0, 2)>;
`)
	out, err := PrintSets(k)
	if err != nil {
		t.Fatal(err)
	}
	want := `S0 = [ a b c ]
S1 = [ a c ]
S2 = [ a c ]
S3 = [ c ]
S4 = [ a b ]
S5 = [ b c a ]
S6 = [ c a b ]
S7 = [ a c ]
S8 = [ a c ]
S9 = [ e0 e1 e2 ]
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// TestPrintSetsEmpty checks the `[ ]` spacing the driver prints for no
// elements, and that a def with no expander but not a Set is ignored.
func TestPrintSetsEmpty(t *testing.T) {
	k := setsSrc(t, `
class Set<dag d> { dag Elements = d; }
def add;
def e;
def S : Set<(add)>;
`)
	out, err := PrintSets(k)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "S = [ ]\n" {
		t.Fatalf("got %q", out)
	}
}
