package record_test

import (
	"testing"

	"tblgen/internal/backend"
	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

// --print-records prints class bodies as unresolved Init trees (Init::getAsString),
// so every initialiser form goes through internal/record/inittxt.go. The
// expected text is llvm-tblgen 24.0.0git's output for the same source.
func TestPrintRecordsInitText(t *testing.T) {
	src := `def op;
class App<int n> { int v = n; }
class C<bits<4> b, bit f, int n, list<int> l, string s, code c, dag d> {
  bits<4> B = b;
  bit F = f;
  int N = n;
  int FromBit = f;
  bits<2> FromInt = {1, 1};
  list<int> L = l;
  string S = s;
  code Cc = c;
  dag D = d;
  App A = App<n>;
  int Un = ?;
  bits<3> UB;
  list<int> UBL;
  string Cat = "a" # "b";
  int Neg = -1;
}
def D : C<{1, 0, 1, 0}, 1, 7, [1, 2], "hi", [{int x;}], (op 1)>;
`
	want := `------------- Classes -----------------
class App<int App:n = ?> {
  int v = App:n;
}
class C<bits<4> C:b = { ?, ?, ?, ? }, bit C:f = ?, int C:n = ?, list<int> C:l = ?, string C:s = ?, string C:c = ?, dag C:d = ?> {
  bits<4> B = { C:b{3}, C:b{2}, C:b{1}, C:b{0} };
  bit F = C:f;
  int N = C:n;
  int FromBit = !cast<int>(C:f);
  bits<2> FromInt = { 1, 1 };
  list<int> L = C:l;
  string S = C:s;
  string Cc = C:c;
  dag D = C:d;
  App A = App<0: C:n>;
  int Un = ?;
  bits<3> UB = { ?, ?, ? };
  list<int> UBL = ?;
  string Cat = "ab";
  int Neg = -1;
}
------------- Defs -----------------
def D {	// C
  bits<4> B = { 1, 0, 1, 0 };
  bit F = 1;
  int N = 7;
  int FromBit = 1;
  bits<2> FromInt = { 1, 1 };
  list<int> L = [1, 2];
  string S = "hi";
  code Cc = [{int x;}];
  dag D = (op 1);
  App A = anonymous_0;
  int Un = ?;
  bits<3> UB = { ?, ?, ? };
  list<int> UBL = ?;
  string Cat = "ab";
  int Neg = -1;
}
def anonymous_0 {	// App
  int v = 7;
}
def op {
}
`
	file, pdiags := syntax.Parse("t.td", src, nil, nil)
	for _, d := range pdiags {
		if d.Severity == diag.Error {
			t.Fatalf("parse: %v", pdiags)
		}
	}
	k, ldiags := record.Lower(file, "t.td")
	for _, d := range ldiags {
		if d.Severity == diag.Error {
			t.Fatalf("lower: %v", ldiags)
		}
	}
	out, err := backend.PrintRecords(k)
	if err != nil {
		t.Fatalf("print-records: %v", err)
	}
	if string(out) != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}
