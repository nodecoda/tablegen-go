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
	if got := printRecords(t, src); got != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// printRecords parses and lowers src and renders --print-records.
func printRecords(t *testing.T, src string) string {
	t.Helper()
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
	return string(out)
}

// A class inherits a parent with concrete arguments, and TGParser resolves the
// copied fields right there (AddParentClass -> Record::resolveReferences,
// TGParser.cpp:346-374): a bit-select on an int argument folds to the bit
// (VarBitInit::resolveReferences, Record.cpp:2321-2327), !cast<string> and
// !eq fold (UnOpInit::Fold Record.cpp:803-813), and a !strconcat of two
// StringInits collapses (BinOpInit::Fold Record.cpp:1373-1378). The expected
// text is llvm-tblgen 24.0.0git's.
func TestPrintRecordsResolvedClassFields(t *testing.T) {
	src := `class I<bits<8> o> {
  bits<8> Op = o;
}
class ITy<bits<8> o, bit t> : I<{o{7}, o{6}, o{5}, o{4}, o{3}, o{2}, o{1}, !if(t, 0, o{0})}>;
class M<bit t> : ITy<211, t>;
class Ops { string c = ",x"; }
def R : Ops { let c = ",y"; }
class N<string s, Ops v> { string C = !strconcat(s, v.c); }
class P : N<"", R>;
class Q<string n, bit even> { bits<2> K = { even, !if(even, 0, 1) }; }
class S<bit s> {
  bits<4> Inst = { 0, s, 0, 0 };
}
class T<bits<2> size> : S<size{0}>;
`
	want := `------------- Classes -----------------
class I<bits<8> I:o = { ?, ?, ?, ?, ?, ?, ?, ? }> {
  bits<8> Op = { I:o{7}, I:o{6}, I:o{5}, I:o{4}, I:o{3}, I:o{2}, I:o{1}, I:o{0} };
}
class ITy<bits<8> ITy:o = { ?, ?, ?, ?, ?, ?, ?, ? }, bit ITy:t = ?> {	// I
  bits<8> Op = { ITy:o{7}, ITy:o{6}, ITy:o{5}, ITy:o{4}, ITy:o{3}, ITy:o{2}, ITy:o{1}, !cast<bit>(!if(ITy:t, 0, { ITy:o{0} })) };
}
class M<bit M:t = ?> {	// I ITy
  bits<8> Op = { 1, 1, 0, 1, 0, 0, 1, !cast<bit>(!if(M:t, 0, { 1 })) };
}
class N<string N:s = ?, Ops N:v = ?> {
  string C = !strconcat(N:s, N:v.c);
}
class Ops {
  string c = ",x";
}
class P {	// N
  string C = ",y";
}
class Q<string Q:n = ?, bit Q:even = ?> {
  bits<2> K = { Q:even, !cast<bit>(!if(Q:even, 0, 1)) };
}
class S<bit S:s = ?> {
  bits<4> Inst = { 0, S:s, 0, 0 };
}
class T<bits<2> T:size = { ?, ? }> {	// S
  bits<4> Inst = { 0, T:size{0}, 0, 0 };
}
------------- Defs -----------------
def R {	// Ops
  string c = ",y";
}
`
	if got := printRecords(t, src); got != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
