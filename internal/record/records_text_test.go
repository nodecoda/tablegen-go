package record_test

import (
	"testing"

	"github.com/nodecoda/tablegen-go/internal/backend"
	"github.com/nodecoda/tablegen-go/internal/diag"
	"github.com/nodecoda/tablegen-go/internal/record"
	"github.com/nodecoda/tablegen-go/internal/syntax"
)

func lowerForPrint(t *testing.T, file string, src string) *record.Keeper {
	t.Helper()
	f, pdiags := syntax.Parse(file, src, nil, nil)
	for _, d := range pdiags {
		if d.Severity == diag.Error {
			t.Fatalf("parse: %v", pdiags)
		}
	}
	keeper, ldiags := record.Lower(f, file)
	for _, d := range ldiags {
		if d.Severity == diag.Error {
			t.Fatalf("lower: %v", ldiags)
		}
	}
	return keeper
}

// let append/prepend folds ListInit and DagInit even when elements are still
// VarInits, survives as !strconcat for strings, !listconcat for a referenced
// list and !con for a referenced dag (Record.cpp:1219-1248). The field's
// source location is the `let` line (TGParser.cpp:326, 3851). The expected
// text is llvm-tblgen 24.0.0git's --print-records output.
func TestPrintRecordsAppendForms(t *testing.T) {
	src := `def op;
class C<int n, string pre, list<int> L, dag D> {
  list<int> xs = [0];
  let append xs = [n];
  list<int> ys = [1];
  let prepend ys = [n, 2];
  string s = "a";
  let append s = pre;
  dag d = (op);
  let append d = (op 1);
  list<int> zs = L;
  let append zs = [1];
  dag e = D;
  let append e = (op 2);
  code c = [{int a;}];
  let append c = [{int b;}];
  list<int> us = ?;
  let append us = [9];
  string ss = ?;
  let append ss = "z";
}
`
	want := `------------- Classes -----------------
class C<int C:n = ?, string C:pre = ?, list<int> C:L = ?, dag C:D = ?> {
  list<int> xs = [0, C:n];
  list<int> ys = [C:n, 2, 1];
  string s = !strconcat("a", C:pre);
  dag d = (op 1);
  list<int> zs = !listconcat(C:L, [1]);
  dag e = !con(C:D, (op 2));
  code c = [{int a;int b;}];
  list<int> us = [9];
  string ss = "z";
}
------------- Defs -----------------
def op {
}
`
	out, err := backend.PrintRecords(lowerForPrint(t, "ap.td", src))
	if err != nil {
		t.Fatalf("print-records: %v", err)
	}
	if string(out) != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// --print-detailed-records: globals (including a defset, whose value has no
// Expr), classes, records and the let-updated field locations. Our backend
// stops before LLVM's allocator statistics, so the oracle output is truncated
// at that heading and compared as a prefix.
func TestDetailedRecordsPrefix(t *testing.T) {
	src := `def op;
defvar v1 = 1;
defvar v2 = [1, 2];
defvar v3 = {0, 1};
defvar v4 = (op 1);
defvar v5 = "s" # "t";
defvar v6 = !add(1, 2);
defvar v7 = ?;
class C<int n> {
  list<int> xs = [0];
  let append xs = [n];
  string s = "x" # "y";
}
def D : C<1>;
defset list<C> S = {
  def D2 : C<2>;
}
`
	want := `DETAILED RECORDS for file det2.td

-------------------- Global Variables (8) --------------------

S = [D2]
v1 = 1
v2 = [1, 2]
v3 = { 0, 1 }
v4 = (op 1)
v5 = "st"
v6 = 3
v7 = ?

-------------------- Classes (1) --------------------

C  |det2.td:9|
  Template args:
    int C:n = ?  |det2.td:9|
  Superclasses: (none)
  Fields:
    list<int> xs = [0, C:n]  |det2.td:11|
    string s = "xy"  |det2.td:12|

-------------------- Records (3) --------------------

D  |det2.td:14|
  Superclasses: C
  Fields:
    list<int> xs = [0, 1]  |det2.td:11|
    string s = "xy"  |det2.td:12|

D2  |det2.td:16|
  Superclasses: C
  Fields:
    list<int> xs = [0, 2]  |det2.td:11|
    string s = "xy"  |det2.td:12|

op  |det2.td:1|
  Superclasses: (none)
  Fields: (none)

-------------------- Memory Allocation Stats --------------------
`
	out, err := backend.Detailed(lowerForPrint(t, "det2.td", src))
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if string(out) != want {
		t.Errorf("detailed mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// Every operator and initialiser form in a class body, which --print-records
// renders through Init::getAsString. Covers the !dag fold (Record.cpp:1815),
// bit lists with bit-refs, slices, and the untouched operator nodes.
func TestPrintRecordsAllOperatorForms(t *testing.T) {
	src := `def op;
def op2;
defvar gv = 7;
defvar gl = [1, 2, 3];
def D0 { int f = 3; bits<4> fb = {1, 0, 1, 0}; }
class E;
class Big<int n, bits<2> b, list<int> L, dag D, string S, code C, bit f> {
  int i1 = n;
  bits<2> b1 = b;
  bits<4> b2 = { b{0}, f, 1, 0 };
  bits<3> b3 = 5;
  bits<4> ub;
  bits<4> ib = n;
  int qb = b;
  bits<4> g4 = D0.fb;
  int gi = D0.fb;
  bits<2> b4 = !cast<bits<2>>(n);
  int i2 = !cast<int>(b);
  bit f1 = !cast<bit>(n);
  int i3 = !add(n, 1);
  int i4 = !if(n, 1, 2);
  int i5 = !cond(n: 1, 1: 2);
  int i6 = !switch(n, 1: 2, 3);
  int i7 = !foldl(0, L, a, b, !add(a, b));
  list<int> l1 = !foreach(x, L, !add(x, n));
  list<int> l2 = !filter(x, L, !gt(x, n));
  list<int> l3 = !sort(x, L, x);
  list<int> l4 = !listconcat(L, [n]);
  list<int> l5 = !listsplat(n, 2);
  list<int> l6 = !listflatten([[1], [n]]);
  list<int> l7 = !listremove(L, [1]);
  list<int> l8 = !range(n, 3);
  list<int> l9 = L[1...n];
  int h = !head(L);
  list<int> t = !tail(L);
  int sz = !size(L);
  int em = !empty(L);
  string s1 = !strconcat(S, "x");
  string s2 = S # "y";
  string s3 = !substr(S, n, 2);
  int f2 = !find(S, "a");
  string s4 = !toupper(S);
  string s5 = !tolower(S);
  string s6 = !repr(n);
  string s7 = !subst("a", "b", S);
  bit m1 = !match(S, "a");
  int q1 = !eq(n, 1);
  int q2 = !lt(n, 1);
  int q3 = !and(n, 1);
  int q4 = !not(n);
  int q5 = !xor(n, 1);
  int q6 = !shl(n, 1);
  int q7 = !sra(n, 1);
  int q8 = !srl(n, 1);
  int q9 = !logtwo(n);
  int q10 = !initialized(L);
  dag d1 = D;
  dag d2 = !con(D, (op 1));
  dag d3 = !dag(op, [1], ["a"]);
  string dg = !getdagname(D, 1);
  int da = !getdagarg<int>(D, 0);
  dag ds = !setdagarg(D, 0, 2);
  dag dsn = !setdagname(D, 0, "z");
  dag dso = !setdagop(D, op);
  string dson = !getdagopname(D);
  int ex = !exists<E>("x");
  int isa = !isa<int>(n);
  code c1 = C;
  int g1 = gv;
  list<int> g2 = gl;
  int g3 = D0.f;
}
`
	want := `------------- Classes -----------------
class Big<int Big:n = ?, bits<2> Big:b = { ?, ? }, list<int> Big:L = ?, dag Big:D = ?, string Big:S = ?, string Big:C = ?, bit Big:f = ?> {
  int i1 = Big:n;
  bits<2> b1 = { Big:b{1}, Big:b{0} };
  bits<4> b2 = { Big:b{0}, Big:f, 1, 0 };
  bits<3> b3 = { 1, 0, 1 };
  bits<4> ub = { ?, ?, ?, ? };
  bits<4> ib = { !cast<bits<4>>(Big:n){3}, !cast<bits<4>>(Big:n){2}, !cast<bits<4>>(Big:n){1}, !cast<bits<4>>(Big:n){0} };
  int qb = !cast<int>(Big:b);
  bits<4> g4 = { 1, 0, 1, 0 };
  int gi = 10;
  bits<2> b4 = { !cast<bits<2>>(Big:n){1}, !cast<bits<2>>(Big:n){0} };
  int i2 = !cast<int>(Big:b);
  bit f1 = !cast<bit>(Big:n);
  int i3 = !add(Big:n, 1);
  int i4 = !if(Big:n, 1, 2);
  int i5 = !cond(Big:n: 1, 1: 2);
  int i6 = !cond(!eq(Big:n, 1): 2, 1: 3);
  int i7 = !foldl(0, Big:L, a, b, !add(a, b));
  list<int> l1 = !foreach(x, Big:L, !add(x, Big:n));
  list<int> l2 = !filter(x, Big:L, !gt(x, Big:n));
  list<int> l3 = !sort(x, Big:L, x);
  list<int> l4 = !listconcat(Big:L, [Big:n]);
  list<int> l5 = [Big:n, Big:n];
  list<int> l6 = [1, Big:n];
  list<int> l7 = !listremove(Big:L, [1]);
  list<int> l8 = !range(Big:n, 3, 1);
  list<int> l9 = Big:L[1...Big:n];
  int h = !head(Big:L);
  list<int> t = !tail(Big:L);
  int sz = !size(Big:L);
  int em = !empty(Big:L);
  string s1 = !strconcat(Big:S, "x");
  string s2 = !strconcat(Big:S, "y");
  string s3 = !substr(Big:S, Big:n, 2);
  int f2 = !find(Big:S, "a", 0);
  string s4 = !toupper(Big:S);
  string s5 = !tolower(Big:S);
  string s6 = !repr(Big:n);
  string s7 = !subst("a", "b", Big:S);
  bit m1 = !match(Big:S, "a");
  int q1 = !cast<int>(!eq(Big:n, 1));
  int q2 = !cast<int>(!lt(Big:n, 1));
  int q3 = !and(Big:n, 1);
  int q4 = !not(Big:n);
  int q5 = !xor(Big:n, 1);
  int q6 = !shl(Big:n, 1);
  int q7 = !sra(Big:n, 1);
  int q8 = !srl(Big:n, 1);
  int q9 = !logtwo(Big:n);
  int q10 = !initialized(Big:L);
  dag d1 = Big:D;
  dag d2 = !con(Big:D, (op 1));
  dag d3 = (op 1:$a);
  string dg = !getdagname(Big:D, 1);
  int da = !getdagarg<int>(Big:D, 0);
  dag ds = !setdagarg(Big:D, 0, 2);
  dag dsn = !setdagname(Big:D, 0, "z");
  dag dso = !setdagop(Big:D, op);
  string dson = !getdagopname(Big:D);
  int ex = !exists<E>("x");
  int isa = 1;
  string c1 = Big:C;
  int g1 = 7;
  list<int> g2 = [1, 2, 3];
  int g3 = 3;
}
class E {
}
------------- Defs -----------------
def D0 {
  int f = 3;
  bits<4> fb = { 1, 0, 1, 0 };
}
def op {
}
def op2 {
}
`
	out, err := backend.PrintRecords(lowerForPrint(t, "big.td", src))
	if err != nil {
		t.Fatalf("print-records: %v", err)
	}
	if string(out) != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}

// Parse-time folding of list comprehensions and bit casts. LLVM folds
// !range, !foreach and !foldl while parsing whenever their list operand is a
// ListInit, even if the bodies stay unresolved, and a bits<N> cast of an int
// becomes a BitsInit, so --print-records shows the folded shape. Expected
// text is llvm-tblgen 24.0.0git's output for the same source.
func TestPrintRecordsComprehensionAndBitFolds(t *testing.T) {
	src := `class FoldRange<int nf> {
  list<int> R = !foldl([]<int>, !listconcat(!range(8, 12), !range(1, 3)), L, i, !listconcat(L, !if(!lt(i, nf), [i], [])));
}
class NestedForeach<list<string> names> {
  list<string> V = !foldl([]<string>, !foreach(name, names, !foreach(mx, ["a", "b"], !strconcat(name, !strconcat("_", mx)))), all, w, !listconcat(all, w));
}
class SubstChain<string Pseudo> {
  string V = !foldl(Pseudo, [["Pseudo", ""], ["_E64", ""], ["_MASK", ""]], Acc, Sub, !subst(Sub[0], Sub[1], Acc));
}
class Pick<string mx> {
  defvar Cycles = [[0, 0, 0, 3], [0, 0, 3, 5]];
  int c = !cond(!eq(mx, "MF8"): Cycles[0][!if(!eq(mx, "MF8"), 1, 0)], !eq(mx, "MF4"): Cycles[1][0]);
}
class BitsHolder<bits<3> S> {
  bits<3> L = S;
}
class BitsArg<int n> : BitsHolder<!logtwo(8)> {
  int UsesN = n;
}
class ImmLeaf<code pred> {
  code ImmediateCode = pred;
}
class SImmLeafOp<int bitsNum> : ImmLeaf<"return isInt<" # bitsNum # ">(Imm);"> {
  int UsesN = bitsNum;
}
class Simm12 : SImmLeafOp<12> {
}
class ConcreteShape {
  list<int> R = !if(!not(!empty(!foreach(x, [1, 2, 3], !add(x, 1)))), !tail(!foreach(x, [1, 2, 3], !add(x, 1))), !foreach(x, [1, 2, 3], !add(x, 1)));
}
`
	want := `------------- Classes -----------------
class BitsArg<int BitsArg:n = ?> {	// BitsHolder
  bits<3> L = { 0, 1, 1 };
  int UsesN = BitsArg:n;
}
class BitsHolder<bits<3> BitsHolder:S = { ?, ?, ? }> {
  bits<3> L = { BitsHolder:S{2}, BitsHolder:S{1}, BitsHolder:S{0} };
}
class ConcreteShape {
  list<int> R = [3, 4];
}
class FoldRange<int FoldRange:nf = ?> {
  list<int> R = !listconcat(!listconcat(!listconcat(!listconcat(!listconcat(!listconcat([], !if(!lt(8, FoldRange:nf), [8], [])), !if(!lt(9, FoldRange:nf), [9], [])), !if(!lt(10, FoldRange:nf), [10], [])), !if(!lt(11, FoldRange:nf), [11], [])), !if(!lt(1, FoldRange:nf), [1], [])), !if(!lt(2, FoldRange:nf), [2], []));
}
class ImmLeaf<string ImmLeaf:pred = ?> {
  string ImmediateCode = ImmLeaf:pred;
}
class NestedForeach<list<string> NestedForeach:names = ?> {
  list<string> V = !foldl([], !foreach(name, NestedForeach:names, [!strconcat(name, "_a"), !strconcat(name, "_b")]), all, w, !listconcat(all, w));
}
class Pick<string Pick:mx = ?> {
  int c = !cond(!eq(Pick:mx, "MF8"): [0, 0, 0, 3][!if(!eq(Pick:mx, "MF8"), 1, 0)], !eq(Pick:mx, "MF4"): 0);
}
class SImmLeafOp<int SImmLeafOp:bitsNum = ?> {	// ImmLeaf
  string ImmediateCode = !strconcat("return isInt<", !strconcat(!cast<string>(SImmLeafOp:bitsNum), ">(Imm);"));
  int UsesN = SImmLeafOp:bitsNum;
}
class Simm12 {	// ImmLeaf SImmLeafOp
  string ImmediateCode = "return isInt<12>(Imm);";
  int UsesN = 12;
}
class SubstChain<string SubstChain:Pseudo = ?> {
  string V = !subst("_MASK", "", !subst("_E64", "", !subst("Pseudo", "", SubstChain:Pseudo)));
}
------------- Defs -----------------
`
	out, err := backend.PrintRecords(lowerForPrint(t, "fold.td", src))
	if err != nil {
		t.Fatalf("print-records: %v", err)
	}
	if string(out) != want {
		t.Errorf("print-records mismatch:\n--- got ---\n%s\n--- want ---\n%s", out, want)
	}
}
