package record_test

import (
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/nodecoda/tablegen-go/internal/backend"
	"github.com/nodecoda/tablegen-go/internal/diag"
	"github.com/nodecoda/tablegen-go/internal/record"
	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// lowerJSON parses and lowers a source snippet and renders --dump-json. The
// expected strings are the exact bytes llvm-tblgen 24.0.0git prints, so these
// tests double as anonymous-name ordering probes (VarDefInit::Fold at parse
// time, AddParentClass resolution, defvar scoping).
// errorDiags drops warnings: these snippets are about record shape, and an
// unused template argument is a legitimate warning that must not fail them.
func errorDiags(in []diag.Diagnostic) []diag.Diagnostic {
	var out []diag.Diagnostic
	for _, d := range in {
		if d.Severity == diag.Error {
			out = append(out, d)
		}
	}
	return out
}

func lowerJSON(t *testing.T, src string) string {
	t.Helper()
	file, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(file, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	out, err := backend.JSON(k)
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	return strings.TrimSuffix(string(out), "\n")
}

// A `let` written in a multiclass body around a nested defm belongs to that
// defm's own ParseDefm: the class's trailing classes are added and then its
// LetStack is applied, all while the multiclass body is parsed. The enclosing
// defm adds its own trailing classes later, so those win. AVX512XDIi8Base's
// `ExeDomain = SSEPackedInt` overriding `let ExeDomain = _.ExeDomain` in
// avx512_unary_fp_packed_imm is this exact shape (X86.td).
func TestNestedDefmLetBeforeEnclosingParents(t *testing.T) {
	src := `class D<bits<2> v> { bits<2> Value = v; }
def A : D<1>;
def B : D<3>;
class Has { D F = A; }
class Base { D F = B; }
multiclass Leaf { def m : Has; }
multiclass Inner { let F = A in { defm r : Leaf; } }
multiclass Outer { defm Z : Inner; }
defm T : Outer, Base;
`
	want := `{"!instanceof":{"Base":["TZrm"],"D":["A","B"],"Has":["TZrm"]},"!tablegen_json_version":1,"A":{"!anonymous":false,"!fields":[],"!locs":["t.td:2"],"!name":"A","!superclasses":["D"],"Value":[1,0]},"B":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"B","!superclasses":["D"],"Value":[1,1]},"TZrm":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7","t.td:8","t.td:9"],"!name":"TZrm","!superclasses":["Has","Base"],"F":{"def":"B","kind":"def","printable":"B"}}}`
	if got := lowerJSON(t, src); got != want {
		t.Errorf("dump-json mismatch\n got: %s\nwant: %s", got, want)
	}
}

// A field in a class body shadows a template argument of the same name.
// TGVarScope::getVar looks the name up in the record's fields (Record::getValue)
// before its template arguments (TGParser.cpp:173-183), so `imm{4-0}` is the
// bits field, not the Operand argument, and the argument is reported unused.
// ARMInstrMVE.td:2965 (`let Inst{20-16} = imm{4-0}`) is this shape; without
// the shadow it fails with "Value is not a bits type".
func TestFieldShadowsTemplateArgument(t *testing.T) {
	src := `class Operand;
class A<Operand imm> {
  bits<5> imm;
  bits<5> inst;
  let inst{4-0} = imm{4-0};
}
def O : Operand;
def X : A<O>;
`
	want := `{"!instanceof":{"A":["X"],"Operand":["O"]},"!tablegen_json_version":1,"O":{"!anonymous":false,"!fields":[],"!locs":["t.td:7"],"!name":"O","!superclasses":["Operand"]},"X":{"!anonymous":false,"!fields":[],"!locs":["t.td:8"],"!name":"X","!superclasses":["A"],"imm":[null,null,null,null,null],"inst":[{"index":0,"kind":"varbit","printable":"imm{0}","var":"imm"},{"index":1,"kind":"varbit","printable":"imm{1}","var":"imm"},{"index":2,"kind":"varbit","printable":"imm{2}","var":"imm"},{"index":3,"kind":"varbit","printable":"imm{3}","var":"imm"},{"index":4,"kind":"varbit","printable":"imm{4}","var":"imm"}]}}`
	if got := lowerJSON(t, src); got != want {
		t.Errorf("dump-json mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestParseTimeAnonymousNames(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			// TGParser unrolls a concrete foreach when its closing brace is
			// parsed (addEntry -> resolve, TGParser.cpp:426) and resolves the
			// copies with the loop substitutions, so the class application in
			// a parent's argument list takes its anonymous name before the
			// next statement in the body is parsed (VarDefInit::
			// resolveReferences, Record.cpp:2423). T therefore numbers 1 and 2,
			// between the two H defs.
			name: "multiclass body loop unrolled where it closes",
			src: `def OP;

class T<int V> {
  int P = V;
}

class H<dag D> {
  dag Y = D;
}

multiclass MC {
  foreach V = [1, 2] in {
    def : H<(OP (T<V>.P))>;
  }
  foreach W = [3] in {
    def : H<(OP 3)>;
  }
}

defm D : MC;
`,
			want: `{"!instanceof":{"H":["anonymous_0","anonymous_3","anonymous_4"],"T":["anonymous_1","anonymous_2"]},"!tablegen_json_version":1,"OP":{"!anonymous":false,"!fields":[],"!locs":["t.td:1"],"!name":"OP","!superclasses":[]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:13","t.td:20"],"!name":"anonymous_0","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":1,"printable":"(1)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1))"}},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:13"],"!name":"anonymous_1","!superclasses":["T"],"P":1},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:13"],"!name":"anonymous_2","!superclasses":["T"],"P":2},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:16","t.td:20"],"!name":"anonymous_3","!superclasses":["H"],"Y":{"args":[[3,null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP 3)"}},"anonymous_4":{"!anonymous":true,"!fields":[],"!locs":["t.td:13","t.td:20"],"!name":"anonymous_4","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":2,"printable":"(2)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2))"}}}`,
		},
		{
			// TGParser parses a loop body once: the two anonymous `def`s take
			// their names before any iteration runs, so the H copy of the first
			// iteration keeps 0 and the second keeps 1, while T<1> - created
			// while that copy resolves - lands on 2 (addEntry ->
			// resolve(ForeachLoop), TGParser.cpp:426-505).
			name: "loop body parses once, then unrolls",
			src: `def OP;

class T<int V> {
  int Half = V;
  int Quarter = V;
}

class H<dag D> {
  dag Y = D;
}

foreach V = [1, 2] in {
  foreach idx = [0, 4] in
    def : H<(OP (T<V>.Half idx))>;
  foreach idx = [0, 2] in
    def : H<(OP (T<V>.Quarter idx))>;
}
`,
			want: `{"!instanceof":{"H":["anonymous_0","anonymous_1","anonymous_3","anonymous_4","anonymous_6","anonymous_7","anonymous_8","anonymous_9"],"T":["anonymous_2","anonymous_5"]},"!tablegen_json_version":1,"OP":{"!anonymous":false,"!fields":[],"!locs":["t.td:1"],"!name":"OP","!superclasses":[]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_0","!superclasses":["H"],"Y":{"args":[[{"args":[[0,null]],"kind":"dag","operator":1,"printable":"(1 0)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1 0))"}},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:16"],"!name":"anonymous_1","!superclasses":["H"],"Y":{"args":[[{"args":[[0,null]],"kind":"dag","operator":1,"printable":"(1 0)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1 0))"}},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_2","!superclasses":["T"],"Half":1,"Quarter":1},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_3","!superclasses":["H"],"Y":{"args":[[{"args":[[4,null]],"kind":"dag","operator":1,"printable":"(1 4)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1 4))"}},"anonymous_4":{"!anonymous":true,"!fields":[],"!locs":["t.td:16"],"!name":"anonymous_4","!superclasses":["H"],"Y":{"args":[[{"args":[[2,null]],"kind":"dag","operator":1,"printable":"(1 2)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1 2))"}},"anonymous_5":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_5","!superclasses":["T"],"Half":2,"Quarter":2},"anonymous_6":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_6","!superclasses":["H"],"Y":{"args":[[{"args":[[0,null]],"kind":"dag","operator":2,"printable":"(2 0)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2 0))"}},"anonymous_7":{"!anonymous":true,"!fields":[],"!locs":["t.td:14"],"!name":"anonymous_7","!superclasses":["H"],"Y":{"args":[[{"args":[[4,null]],"kind":"dag","operator":2,"printable":"(2 4)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2 4))"}},"anonymous_8":{"!anonymous":true,"!fields":[],"!locs":["t.td:16"],"!name":"anonymous_8","!superclasses":["H"],"Y":{"args":[[{"args":[[0,null]],"kind":"dag","operator":2,"printable":"(2 0)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2 0))"}},"anonymous_9":{"!anonymous":true,"!fields":[],"!locs":["t.td:16"],"!name":"anonymous_9","!superclasses":["H"],"Y":{"args":[[{"args":[[2,null]],"kind":"dag","operator":2,"printable":"(2 2)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2 2))"}}}`,
		},
		{
			// An anonymous defm inside a loop keeps the prefix ParseDefm gave it
			// (TGParser.cpp:4573), so each iteration reuses it and only the
			// generated records collide.
			name: "defm in a loop keeps its anonymous prefix",
			src: `def OP;

class T<int V> {
  int P = V;
}

class H<dag D> {
  dag Y = D;
}

multiclass MC<int V> {
  def : H<(OP (T<V>.P))>;
}

foreach V = [1, 2] in {
  defm : MC<V>;
  defm : MC<V>;
}
`,
			want: `{"!instanceof":{"H":["anonymous_0","anonymous_4","anonymous_6","anonymous_7"],"T":["anonymous_3","anonymous_5"]},"!tablegen_json_version":1,"OP":{"!anonymous":false,"!fields":[],"!locs":["t.td:1"],"!name":"OP","!superclasses":[]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:12","t.td:16"],"!name":"anonymous_0","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":1,"printable":"(1)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1))"}},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:12"],"!name":"anonymous_3","!superclasses":["T"],"P":1},"anonymous_4":{"!anonymous":true,"!fields":[],"!locs":["t.td:12","t.td:17"],"!name":"anonymous_4","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":1,"printable":"(1)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (1))"}},"anonymous_5":{"!anonymous":true,"!fields":[],"!locs":["t.td:12"],"!name":"anonymous_5","!superclasses":["T"],"P":2},"anonymous_6":{"!anonymous":true,"!fields":[],"!locs":["t.td:12","t.td:16"],"!name":"anonymous_6","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":2,"printable":"(2)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2))"}},"anonymous_7":{"!anonymous":true,"!fields":[],"!locs":["t.td:12","t.td:17"],"!name":"anonymous_7","!superclasses":["H"],"Y":{"args":[[{"args":[],"kind":"dag","operator":2,"printable":"(2)"},null]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP (2))"}}}`,
		},
		{
			// LLVM instantiates `X<1>` while parsing the (unused) class body.
			name: "unused class body",
			src: `class X<int n> { int v = n; }
class C { X x = X<1>; }
def D { int v = 1; }
`,
			want: `{"!instanceof":{"C":[],"X":["anonymous_0"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"D","!superclasses":[],"v":1},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:2"],"!name":"anonymous_0","!superclasses":["X"],"v":1}}`,
		},
		{
			// Parent values resolve before the body is parsed, so Y<1> takes the first name even though the let replaces the field.
			name: "parent application before body",
			src: `class Y<int n> { int v = n; }
class C<int n> { Y y = Y<n>; }
def D : C<1> { let y = Y<5>; }
`,
			want: `{"!instanceof":{"C":["D"],"Y":["anonymous_0","anonymous_1"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"D","!superclasses":["C"],"y":{"def":"anonymous_1","kind":"def","printable":"anonymous_1"}},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:2"],"!name":"anonymous_0","!superclasses":["Y"],"v":1},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:3"],"!name":"anonymous_1","!superclasses":["Y"],"v":5}}`,
		},
		{
			// Both the declaration and the let of an unused class body count.
			name: "unused class decl and let",
			src: `class Y<int n> { int v = n; }
class C { Y x = Y<1>; let x = Y<2>; }
def D { int v = 1; }
`,
			want: `{"!instanceof":{"C":[],"Y":["anonymous_0","anonymous_1"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"D","!superclasses":[],"v":1},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:2"],"!name":"anonymous_0","!superclasses":["Y"],"v":1},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:2"],"!name":"anonymous_1","!superclasses":["Y"],"v":2}}`,
		},
		{
			// A defaulted template argument used by an inherited parent field must
			// resolve through the class frame while the anonymous instance is
			// being built. It used to fall back to the parser's inlined literal,
			// which the strict pre-fold rejects: the failed instantiation still
			// consumed anonymous_0, so the kept record was numbered anonymous_1.
			name: "defaulted arg through parent",
			src: `class Versioned<int min = 0, int max = 0x7FFFFFFF> { int minVersion = min; int maxVersion = max; }
class Spelling<string s, int min = 0, int max = 0x7FFFFFFF> : Versioned<min, max> { string spelling = s; }
class LoopModifier<list<Spelling> ss> { list<Spelling> spellings = ss; }
def LM : LoopModifier<[Spelling<"flat">]> {}
`,
			want: `{"!instanceof":{"LoopModifier":["LM"],"Spelling":["anonymous_0"],"Versioned":["anonymous_0"]},"!tablegen_json_version":1,"LM":{"!anonymous":false,"!fields":[],"!locs":["t.td:4"],"!name":"LM","!superclasses":["LoopModifier"],"spellings":[{"def":"anonymous_0","kind":"def","printable":"anonymous_0"}]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_0","!superclasses":["Versioned","Spelling"],"maxVersion":2147483647,"minVersion":0,"spelling":"flat"}}`,
		},
		{
			// A default may name an earlier template argument; the instance sees
			// the caller's concrete value (b = a = 7), not a zero default.
			name: "default refers to earlier arg",
			src: `class Base<int n> { int v = n; }
class C<int a, int b = a> : Base<b> { }
def D : C<7> {}
`,
			want: `{"!instanceof":{"Base":["D"],"C":["D"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"D","!superclasses":["Base","C"],"v":7}}`,
		},
		{
			// TGParser parses a loop body once and only then resolves the
			// iterations, so the literal parent arguments of both defs fold
			// before the first def's fields are resolved. Lowering element by
			// element would create M<ts> between P<p1> and P<p2>.
			name: "loop parses before resolving",
			src: `class P<string s> { string a = s; }
class M<list<int> xs> { int q = !size(xs); }
class B<P p> { }
class Has { list<int> ts = [1, 2]; M m = M<ts>; }
foreach k = [1] in {
  def d1_ # k : B<P<"p1">>, Has;
  def d2_ # k : B<P<"p2">>;
}
`,
			want: `{"!instanceof":{"B":["d1_1","d2_1"],"Has":["d1_1"],"M":["anonymous_2"],"P":["anonymous_0","anonymous_1"]},"!tablegen_json_version":1,"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_0","!superclasses":["P"],"a":"p1"},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:7"],"!name":"anonymous_1","!superclasses":["P"],"a":"p2"},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_2","!superclasses":["M"],"q":2},"d1_1":{"!anonymous":false,"!fields":[],"!locs":["t.td:6"],"!name":"d1_1","!superclasses":["B","Has"],"m":{"def":"anonymous_2","kind":"def","printable":"anonymous_2"},"ts":[1,2]},"d2_1":{"!anonymous":false,"!fields":[],"!locs":["t.td:7"],"!name":"d2_1","!superclasses":["B"]}}`,
		},
		{
			// TGParser lowers each arm of an if to a foreach over a 0/1-element
			// list and parses both bodies before resolving either condition, so
			// the discarded arm still gets its anonymous names (in source order).
			name: "if parses both arms",
			src: `class P<string s> { string a = s; }
class B<P p> { }
if 0 then { def x : B<P<"then">>; } else { def y : B<P<"else">>; }
`,
			want: `{"!instanceof":{"B":["y"],"P":["anonymous_0","anonymous_1"]},"!tablegen_json_version":1,"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:3"],"!name":"anonymous_0","!superclasses":["P"],"a":"then"},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:3"],"!name":"anonymous_1","!superclasses":["P"],"a":"else"},"y":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"y","!superclasses":["B"]}}`,
		},
		{
			// A defm written inside a multiclass body is expanded while that
			// body is parsed (TGParser::ParseObject calls ParseDefm, and
			// addEntry pushes the entries into MultiClass::Entries), so the
			// concrete class applications of its foreach list take their
			// anonymous numbers there - before the following def, and long
			// before the enclosing defm is instantiated. The instances
			// themselves stay pending until then (VarDefInit::resolveReferences
			// instantiates, TGParser::resolve copies the entry and addDefOne
			// registers it under the name it already has).
			name: "defm in multiclass body expands at parse time",
			src: `class Pair<string a, string b> { string VT0 = a; string VT1 = b; }
class Anon<string x> { string X = x; }

multiclass Inner<string A> {
  foreach D = [Pair<A, "x">] in {
    def : Anon<D.VT0>;
  }
}

multiclass Outer<string U> {
  defm I : Inner<"lit">;
  def : Anon<U>;
}

def : Anon<"toplevel1">;

defm O : Outer<"u">;
`,
			want: `{"!instanceof":{"Anon":["anonymous_0","anonymous_2","anonymous_3"],"Pair":["anonymous_1"]},"!tablegen_json_version":1,"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:6","t.td:11","t.td:17"],"!name":"anonymous_0","!superclasses":["Anon"],"X":"lit"},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:5"],"!name":"anonymous_1","!superclasses":["Pair"],"VT0":"lit","VT1":"x"},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:12","t.td:17"],"!name":"anonymous_2","!superclasses":["Anon"],"X":"u"},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:15"],"!name":"anonymous_3","!superclasses":["Anon"],"X":"toplevel1"}}`,
		},
		{
			// A defvar value is never re-resolved: TGParser parses the loop
			// body with the iterator still a VarInit, and resolve() only walks
			// record entries, not defvars. So defvar m = B<k> instantiates
			// nothing, while a literal one does.
			name: "defvar in loop stays unresolved",
			src: `class B<string k, string s = ""> { string n = "x" # k # s; }
foreach k = ["a", "b"] in {
  foreach s = ["", ".1"] in {
    defvar m = B<k, s>;
    if !eq(k, "a") then {
      def foo_ # k # s : B<k, s>;
    }
  }
}
`,
			want: `{"!instanceof":{"B":["foo_a","foo_a.1"]},"!tablegen_json_version":1,"foo_a":{"!anonymous":false,"!fields":[],"!locs":["t.td:6"],"!name":"foo_a","!superclasses":["B"],"n":"xa"},"foo_a.1":{"!anonymous":false,"!fields":[],"!locs":["t.td:6"],"!name":"foo_a.1","!superclasses":["B"],"n":"xa.1"}}`,
		},
		{
			// A defvar inside a braced block is local: it must not replace the top-level value seen by later statements.
			name: "defvar scope",
			src: `defvar myvar = "foo";
let x = 1 in { defvar myvar = "bar"; }
def D { string val = myvar; }
`,
			want: `{"!instanceof":{},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"D","!superclasses":[],"val":"foo"}}`,
		},
		{
			// A concrete argument instantiated at parse time stays; an argument only read by a field resolves with the def.
			name: "unused template argument",
			src: `class Or4<bits<8> Val> { bits<8> V = Val; }
class Whatev<bits<8> x>;
def abar : Whatev<Or4<4>.V>;
def bfoo { bits<8> W = Or4<8>.V; }
`,
			want: `{"!instanceof":{"Or4":["anonymous_0","anonymous_1"],"Whatev":["abar"]},"!tablegen_json_version":1,"abar":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"abar","!superclasses":["Whatev"]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:3"],"!name":"anonymous_0","!superclasses":["Or4"],"V":[0,0,1,0,0,0,0,0]},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_1","!superclasses":["Or4"],"V":[0,0,0,1,0,0,0,0]},"bfoo":{"!anonymous":false,"!fields":[],"!locs":["t.td:4"],"!name":"bfoo","!superclasses":[],"W":[0,0,0,1,0,0,0,0]}}`,
		},
		{
			// defm resolves every entry before adding any
			name: "defm resolves every entry before adding any",
			src: `class Prof<int N> { int X = N; }
class NoProf<int N> : Prof<N> {}
class TI<int a> { int A = a; }
class Base<int rt> { int RetTypes = rt; TI T = TI<RetTypes>; }
class Intr<Prof P> : Base<P.X> {}
multiclass M<int K> { foreach d = [0, 1, 2] in { def _x # d : Intr<NoProf<!add(K,d)>>; } }
defm a : M<100>;
`,
			want: `{"!instanceof":{"Base":["a_x0","a_x1","a_x2"],"Intr":["a_x0","a_x1","a_x2"],"NoProf":["anonymous_0","anonymous_1","anonymous_2"],"Prof":["anonymous_0","anonymous_1","anonymous_2"],"TI":["anonymous_3","anonymous_4","anonymous_5"]},"!tablegen_json_version":1,"a_x0":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"a_x0","!superclasses":["Base","Intr"],"RetTypes":100,"T":{"def":"anonymous_3","kind":"def","printable":"anonymous_3"}},"a_x1":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"a_x1","!superclasses":["Base","Intr"],"RetTypes":101,"T":{"def":"anonymous_4","kind":"def","printable":"anonymous_4"}},"a_x2":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"a_x2","!superclasses":["Base","Intr"],"RetTypes":102,"T":{"def":"anonymous_5","kind":"def","printable":"anonymous_5"}},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_0","!superclasses":["Prof","NoProf"],"X":100},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_1","!superclasses":["Prof","NoProf"],"X":101},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_2","!superclasses":["Prof","NoProf"],"X":102},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_3","!superclasses":["TI"],"A":100},"anonymous_4":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_4","!superclasses":["TI"],"A":101},"anonymous_5":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_5","!superclasses":["TI"],"A":102}}`,
		},
		{
			// nested defm cast sees sibling def
			name: "nested defm cast sees sibling def",
			src: `class SD;
class Frag<SD s> { SD F = s; }
multiclass ord { def NAME # "_m" : Frag<!cast<SD>(NAME)>; }
multiclass op<int K> { foreach v = [1, 2] in { def _ # v : SD; defm _ # v : ord; } }
defm top : op<0>;
`,
			want: `{"!instanceof":{"Frag":["top_1_m","top_2_m"],"SD":["top_1","top_2"]},"!tablegen_json_version":1,"top_1":{"!anonymous":false,"!fields":[],"!locs":["t.td:4","t.td:5"],"!name":"top_1","!superclasses":["SD"]},"top_1_m":{"!anonymous":false,"!fields":[],"!locs":["t.td:3","t.td:4","t.td:5"],"!name":"top_1_m","!superclasses":["Frag"],"F":{"def":"top_1","kind":"def","printable":"top_1"}},"top_2":{"!anonymous":false,"!fields":[],"!locs":["t.td:4","t.td:5"],"!name":"top_2","!superclasses":["SD"]},"top_2_m":{"!anonymous":false,"!fields":[],"!locs":["t.td:3","t.td:4","t.td:5"],"!name":"top_2_m","!superclasses":["Frag"],"F":{"def":"top_2","kind":"def","printable":"top_2"}}}`,
		},
		{
			// defm inside statement loop adds per entry
			name: "defm inside statement loop adds per entry",
			src: `class Prof<int N> { int X = N; }
class NoProf<int N> : Prof<N> {}
class TI<int a> { int A = a; }
class Base<int rt> { int RetTypes = rt; TI T = TI<RetTypes>; }
class Intr<Prof P> : Base<P.X> {}
multiclass M<int K> { foreach d = [0, 1] in { def _x # d : Intr<NoProf<!add(K,d)>>; } }
foreach s = [5, 6] in { defm z # s : M<s>; }
`,
			want: `{"!instanceof":{"Base":["z5_x0","z5_x1","z6_x0","z6_x1"],"Intr":["z5_x0","z5_x1","z6_x0","z6_x1"],"NoProf":["anonymous_0","anonymous_2","anonymous_4"],"Prof":["anonymous_0","anonymous_2","anonymous_4"],"TI":["anonymous_1","anonymous_3","anonymous_5"]},"!tablegen_json_version":1,"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_0","!superclasses":["Prof","NoProf"],"X":5},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_1","!superclasses":["TI"],"A":5},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_2","!superclasses":["Prof","NoProf"],"X":6},"anonymous_3":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_3","!superclasses":["TI"],"A":6},"anonymous_4":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_4","!superclasses":["Prof","NoProf"],"X":7},"anonymous_5":{"!anonymous":true,"!fields":[],"!locs":["t.td:4"],"!name":"anonymous_5","!superclasses":["TI"],"A":7},"z5_x0":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"z5_x0","!superclasses":["Base","Intr"],"RetTypes":5,"T":{"def":"anonymous_1","kind":"def","printable":"anonymous_1"}},"z5_x1":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"z5_x1","!superclasses":["Base","Intr"],"RetTypes":6,"T":{"def":"anonymous_3","kind":"def","printable":"anonymous_3"}},"z6_x0":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"z6_x0","!superclasses":["Base","Intr"],"RetTypes":6,"T":{"def":"anonymous_3","kind":"def","printable":"anonymous_3"}},"z6_x1":{"!anonymous":false,"!fields":[],"!locs":["t.td:6","t.td:7"],"!name":"z6_x1","!superclasses":["Base","Intr"],"RetTypes":7,"T":{"def":"anonymous_5","kind":"def","printable":"anonymous_5"}}}`,
		},
		{
			// let around class definition
			name: "let around class definition",
			src: `class Base { string TP = ""; }
let TP = "x" in { class C : Base; }
def d : C;
`,
			want: `{"!instanceof":{"Base":["d"],"C":["d"]},"!tablegen_json_version":1,"d":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"d","!superclasses":["Base","C"],"TP":"x"}}`,
		},
		{
			// A defvar argument is the defvar's Init node, so `P<Idx>` and
			name: "defvar substitution shares one application node",
			src: `class P<int i> { int v = i; }
class A {
  list<int> xs = [1, 2];
  defvar Idx = !size(xs);
  P p = P<Idx>;
}
class B {
  list<int> xs = [1, 2, 3];
  P q = P<!size(xs)>;
}
def D : A;
def E : B;
`,
			want: `{"!instanceof":{"A":["D"],"B":["E"],"P":["anonymous_0","anonymous_1"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:11"],"!name":"D","!superclasses":["A"],"p":{"def":"anonymous_0","kind":"def","printable":"anonymous_0"},"xs":[1,2]},"E":{"!anonymous":false,"!fields":[],"!locs":["t.td:12"],"!name":"E","!superclasses":["B"],"q":{"def":"anonymous_1","kind":"def","printable":"anonymous_1"},"xs":[1,2,3]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:5"],"!name":"anonymous_0","!superclasses":["P"],"v":2},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:5"],"!name":"anonymous_1","!superclasses":["P"],"v":3}}`,
		},
		{
			// VarInit is keyed by (qualified name, type): `argNo` of two classes is
			name: "template args of different classes are distinct nodes",
			src: `class AttrIndex<int i> { int Value = i; }
class ArgIndex<int n> : AttrIndex<!add(n, 1)>;
class ArgInfo<ArgIndex idx> { int x = idx.Value; }
class Range<AttrIndex idx, int lo> { int y = idx.Value; }
class A<int argNo> { list<int> a = [ArgInfo<ArgIndex<argNo>>.x]; }
class B<int argNo> { list<int> b = [Range<ArgIndex<argNo>, 6>.y]; }
def D : A<5>;
def E : B<5>;
`,
			want: `{"!instanceof":{"A":["D"],"ArgIndex":["anonymous_0"],"ArgInfo":["anonymous_1"],"AttrIndex":["anonymous_0"],"B":["E"],"Range":["anonymous_2"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:7"],"!name":"D","!superclasses":["A"],"a":[6]},"E":{"!anonymous":false,"!fields":[],"!locs":["t.td:8"],"!name":"E","!superclasses":["B"],"b":[6]},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:5"],"!name":"anonymous_0","!superclasses":["AttrIndex","ArgIndex"],"Value":6},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:5"],"!name":"anonymous_1","!superclasses":["ArgInfo"],"x":6},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:6"],"!name":"anonymous_2","!superclasses":["Range"],"y":6}}`,
		},
		{
			// A global def's field list folds at parse time, so its loop unrolls there
			name: "defm loc through parent multiclass and global dim list",
			src: `def Dims { list<string> All = ["1", "2"]; }
class C<int n> { int V = n; }
multiclass X<int n> {
  foreach d = Dims.All in {
    def !strconcat(NAME, "_", d) : C<!add(n, 1)>;
  }
}
multiclass Y<int n> : X<n>;
defset list<C> Cs = {
  defm foo : Y<3>;
}
`,
			want: `{"!instanceof":{"C":["foo_1","foo_2"]},"!tablegen_json_version":1,"Dims":{"!anonymous":false,"!fields":[],"!locs":["t.td:1"],"!name":"Dims","!superclasses":[],"All":["1","2"]},"foo_1":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:10"],"!name":"foo_1","!superclasses":["C"],"V":4},"foo_2":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:10"],"!name":"foo_2","!superclasses":["C"],"V":4}}`,
		},
		{
			// `if` becomes a foreach over !if(cond, [...], []); with a VarInit condition
			name: "if guarded def loses the defm location",
			src: `class C { int V = 0; }
multiclass M<bit b = true> {
  foreach a = ["", "_x"] in {
    def a # "_c" : C;
    if b then
      def a # "_all" : C;
  }
}
defm foo : M<true>;
`,
			want: `{"!instanceof":{"C":["foo_all","foo_c","foo_x_all","foo_x_c"]},"!tablegen_json_version":1,"foo_all":{"!anonymous":false,"!fields":[],"!locs":["t.td:6"],"!name":"foo_all","!superclasses":["C"],"V":0},"foo_c":{"!anonymous":false,"!fields":[],"!locs":["t.td:4","t.td:9"],"!name":"foo_c","!superclasses":["C"],"V":0},"foo_x_all":{"!anonymous":false,"!fields":[],"!locs":["t.td:6"],"!name":"foo_x_all","!superclasses":["C"],"V":0},"foo_x_c":{"!anonymous":false,"!fields":[],"!locs":["t.td:4","t.td:9"],"!name":"foo_x_c","!superclasses":["C"],"V":0}}`,
		},
		{
			// `!range` only needs the list's size, which a parse-time list has.
			name: "range over defvar list keeps the defm location",
			src: `class C { int V = 0; }
multiclass M {
  defvar sfx = ["_x", "_y"];
  foreach i = !range(sfx) in
    def sfx[i] : C;
}
defm foo : M;
`,
			want: `{"!instanceof":{"C":["foo_x","foo_y"]},"!tablegen_json_version":1,"foo_x":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:7"],"!name":"foo_x","!superclasses":["C"],"V":0},"foo_y":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:7"],"!name":"foo_y","!superclasses":["C"],"V":0}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lowerJSON(t, tc.src); got != tc.want {
				t.Errorf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
}

// TestMulticlassInheritanceInstantiatesConcreteParentEntries pins the anonymous
// numbering of a class application written in an inherited multiclass body.
// TGParser::AddParentMultiClass resolves the parent's entries while the
// inheriting multiclass is parsed (TGParser.cpp:388-404), so every instance
// whose arguments are already concrete takes its anonymous number there - in
// multiclass-definition order, and even for a multiclass no defm ever expands.
// Recording them at defm-expansion time instead renumbers every later
// anonymous record; RISCVScheduleV.td's SchedSEWSet<mx, isF, isWidening> is
// this shape.
func TestMulticlassInheritanceInstantiatesConcreteParentEntries(t *testing.T) {
	src := `defvar MxList = ["MF8", "MF4", "MF2", "M1", "M2", "M4", "M8"];
defvar MxListF = !listremove(MxList, ["MF8"]);
defvar MxListW = !listremove(MxList, ["M8"]);
class SchedSEWSet<string mx, bit isF = 0, bit isWidening = 0> {
  defvar t = !cond(!eq(mx, "MF8"): [8], !eq(mx, "MF4"): [8, 16],
                   !eq(mx, "MF2"): [8, 16, 32], 1: [8, 16, 32, 64]);
  defvar remove8 = !if(isF, !listremove(t, [8]), t);
  list<int> val = !if(isWidening, !listremove(remove8, [64]), remove8);
}
multiclass Impl<string name, list<string> MxList, bit isF = 0, bit isWidening = 0> {
  foreach mx = MxList in
    foreach sew = SchedSEWSet<mx, isF, isWidening>.val in
      def name # "_" # mx # "_E" # sew;
}
multiclass A<string name> : Impl<name, MxList>;
multiclass C<string name> : Impl<name, MxListF, isF=1>;
multiclass B<string name> : Impl<name, MxListW, isWidening=1>;
defm "" : A<"W0">;
`
	file, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(file, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	// Keeper.Defs sorts by name, which puts anonymous_10 before anonymous_2;
	// the numbering is what this test is about, so order by the number.
	defs := k.Defs()
	sort.Slice(defs, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(defs[i].Name, "anonymous_"))
		b, _ := strconv.Atoi(strings.TrimPrefix(defs[j].Name, "anonymous_"))
		return a < b
	})
	var got []string
	for _, r := range defs {
		if !r.IsAnonymous {
			continue
		}
		v, err := r.Field("val")
		if err != nil {
			t.Fatalf("%s: %v", r.Name, err)
		}
		ls, ok := v.(*record.List)
		if !ok {
			t.Fatalf("%s: val is %T", r.Name, v)
		}
		var b strings.Builder
		b.WriteString(r.Name + "=[")
		for i, e := range ls.Elems {
			if i > 0 {
				b.WriteString(",")
			}
			n, ok := e.(*record.Int)
			if !ok {
				t.Fatalf("%s: element %T", r.Name, e)
			}
			b.WriteString(strconv.FormatInt(n.N, 10))
		}
		b.WriteString("]")
		got = append(got, b.String())
	}
	// The seven A instances come from the defm; C and B are created only
	// because their multiclass definitions resolve Impl's body.
	want := []string{
		"anonymous_0=[8]", "anonymous_1=[8,16]", "anonymous_2=[8,16,32]",
		"anonymous_3=[8,16,32,64]", "anonymous_4=[8,16,32,64]",
		"anonymous_5=[8,16,32,64]", "anonymous_6=[8,16,32,64]",
		"anonymous_7=[16]", "anonymous_8=[16,32]",
		"anonymous_9=[16,32,64]", "anonymous_10=[16,32,64]",
		"anonymous_11=[16,32,64]", "anonymous_12=[16,32,64]",
		"anonymous_13=[8]", "anonymous_14=[8,16]", "anonymous_15=[8,16,32]",
		"anonymous_16=[8,16,32]", "anonymous_17=[8,16,32]",
		"anonymous_18=[8,16,32]",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("anonymous instances\n got: %v\nwant: %v", got, want)
	}
}

// TestParentFieldNameStaysUnresolvedInALoop pins when a class application that
// mentions NAME instantiates. TGParser applies a def's parents while it parses
// the object body, so AddParentClass sees the name Init as written: for a def
// inside a foreach that is `"D" # f`, not a string (TGParser.cpp:356-362). The
// application stays unresolved there, and a later `let` that overrides the
// field means llvm-tblgen never instantiates it.
func TestParentFieldNameStaysUnresolvedInALoop(t *testing.T) {
	const decls = `class P2V<string P> { string VInst = P; }
class Base { string B = P2V<NAME>.VInst; }
`
	// The `let` replaces the field, so the loop copies keep no application.
	got := lowerJSON(t, decls+`foreach f = ["A", "B"] in {
  let B = "x" in
  def "D" # f : Base;
}
`)
	if strings.Contains(got, `"!anonymous":true`) {
		t.Fatalf("a let-overridden field must not instantiate:\n%s", got)
	}
	// Without the `let` the copies resolve B with their concrete name.
	got = lowerJSON(t, decls+`foreach f = ["A", "B"] in {
  def "D" # f : Base;
}
`)
	for _, want := range []string{`"VInst":"DA"`, `"VInst":"DB"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
}

// TestLoopResolveInstantiatesBeforeAddDefOne pins the phase order addEntry
// gives a foreach body: resolve() clones each entry and runs
// Record::resolveReferences with the iterator substitution, and only then
// calls addDefOne (TGParser.cpp:525-552). A class application whose arguments
// the loop substitution makes concrete instantiates during that resolve
// (VarDefInit::resolveReferences, Record.cpp:2444-2449), i.e. before addDefOne
// renames a colliding def (TGParser.cpp:585-591). Here the defm's second
// WriteRes collides, so the G instance of that iteration must come first.
func TestLoopResolveInstantiatesBeforeAddDefOne(t *testing.T) {
	const src = `class G<int x> { int c = x; }
class WR<int x> { int Latency = 0; }
multiclass MWR<int x> { def : WR<x>; }
foreach i = [0, 1] in {
  defvar Lat = G<i>.c;
  let Latency = Lat in defm "" : MWR<i>;
}
`
	f, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(f, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	var got []string
	for _, r := range k.Defs() {
		switch {
		case r.Val("Latency") != nil:
			got = append(got, r.Name+"=WR")
		case r.Val("c") != nil:
			got = append(got, r.Name+"=G")
		}
	}
	want := []string{"anonymous_0=WR", "anonymous_1=G", "anonymous_2=G", "anonymous_3=WR"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("instantiation order: got %v want %v", got, want)
	}
}

// A multiclass-body loop is resolved the moment its list is concrete, i.e.
// while the multiclass is parsed: Record::resolveReferences resolves the
// entry's name first, then its values (Record.cpp:2855-2882), and only then
// does addDefOne run (TGParser.cpp:545-552). An inherited field therefore
// instantiates its class application right after the name's, not when the defm
// later expands. The expected list is llvm-tblgen 24.0.0git's --print-records
// order for this input (G(1), K(1), G(2), K(2), G(3), K(3)).
func TestMulticlassLoopEntryResolvesInheritedFieldsWithTheName(t *testing.T) {
	const src = `class G<int x> { int s = x; }
class K<int x> { int c = x; }
class H<int x> { K k = K<x>; }
defvar LIST = [1, 2, 3];
multiclass M {
  foreach a = LIST in {
    foreach b = LIST in {
      defvar IX = G<b>.s;
      def "D" # a # "_" # b # "_" # IX : H<b>;
    }
  }
}
defm y : M;
`
	f, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(f, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	var got []string
	for _, r := range k.Defs() {
		switch {
		case r.Val("c") != nil:
			got = append(got, r.Name+"=K")
		case r.Val("s") != nil:
			got = append(got, r.Name+"=G")
		}
	}
	want := []string{"anonymous_0=G", "anonymous_1=K", "anonymous_2=G", "anonymous_3=K", "anonymous_4=G", "anonymous_5=K"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("instantiation order: got %v want %v", got, want)
	}
}

// The name of a multiclass-body loop entry still takes its anonymous number
// when the multiclass is parsed, before the statements that follow it: the
// entry's name Init is resolved with the loop substitution right away even
// though NAME is still unresolved. llvm-tblgen gives G<1> anonymous_0 and the
// later top-level `def : G<9>` anonymous_1.
func TestMulticlassLoopEntryNameTakesNumberAtDefinition(t *testing.T) {
	const src = `class G<int x> { int s = x; }
class K<int x> { int c = x; }
multiclass M { foreach b = [1] in { def "D"#G<b>.s : K<b>; } }
def : G<9>;
defm y : M;
`
	f, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(f, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	var got []string
	for _, r := range k.Defs() {
		if r.Val("s") != nil {
			got = append(got, r.Name+"=G")
		}
	}
	want := []string{"anonymous_0=G", "anonymous_1=G"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("instantiation order: got %v want %v", got, want)
	}
	if v := k.Defs(); len(v) == 0 {
		t.Fatal("no defs")
	}
}

// anonymousClasses maps every anonymous record to its first direct superclass,
// which is what llvm-tblgen's --print-records comment names. The expected maps
// are llvm-tblgen 24.0.0git's numbering for the same source.
func anonymousClasses(t *testing.T, src string) map[string]string {
	t.Helper()
	f, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(f, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	out := map[string]string{}
	for _, r := range k.Defs() {
		if r.IsAnonymous {
			if len(r.Super) == 0 {
				out[r.Name] = ""
			} else {
				out[r.Name] = r.Super[0].Name
			}
		}
	}
	return out
}

func checkAnonymousClasses(t *testing.T, src string, want map[string]string) {
	t.Helper()
	got := anonymousClasses(t, src)
	if len(got) != len(want) {
		t.Fatalf("anonymous records: got %d want %d\ngot: %v\nwant: %v", len(got), len(want), got, want)
	}
	for name, class := range want {
		if got[name] != class {
			t.Fatalf("%s: got %q want %q\nall: %v", name, got[name], class, got)
		}
	}
}

// A multiclass-body foreach over a concrete list is unrolled while the
// multiclass is parsed, and the `let` around its defm is applied to the
// generated entries there (TGParser.cpp:4638-4662). The loop then resolves
// every entry's values in field-declaration order before addDefOne
// (TGParser.cpp:525-564), so which statement comes first in the inner
// multiclass changes nothing about the enclosing loop's A/B/C/D numbering.
// llvm-tblgen 24.0.0git numbers the two WR entries first, then one C/D pair
// per loop iteration, then the second iteration's WR.
func TestMulticlassBodyLoopNumbering(t *testing.T) {
	const outer = `class C<string mx> { int c = 1; }
class D<string mx, list<string> L> { bit c = !eq(mx, "M1"); }
class WR<string n, int lat> { string N = n; int Lat = lat; }
defvar L = ["M1", "M2"];
multiclass MC<string name, string mx, bit IsWorstCase> {
%s}
multiclass Outer {
  foreach mx = L in {
    defvar A = C<mx>.c;
    defvar B = D<mx, L>.c;
    let Lat = A in defm : MC<"W", mx, B>;
  }
}
defm x : Outer;
`
	defFirst := `  def : WR<name # "_" # mx, 0>;
  if IsWorstCase then
    def : WR<name # "_WorstCase", 0>;
`
	ifFirst := `  if IsWorstCase then
    def : WR<name # "_WorstCase", 0>;
  def : WR<name # "_" # mx, 0>;
`
	wrM1 := map[string]string{
		"anonymous_0": "WR", "anonymous_1": "WR",
		"anonymous_3": "C", "anonymous_4": "D",
		"anonymous_5": "C", "anonymous_6": "D",
		"anonymous_7": "WR",
	}
	// The `if` first makes the condition's D<mx, L> instantiate before the
	// body's C<mx>, so each iteration's pair is D then C.
	wrIf := map[string]string{
		"anonymous_0": "WR", "anonymous_1": "WR",
		"anonymous_3": "D", "anonymous_4": "C",
		"anonymous_5": "D", "anonymous_6": "C",
		"anonymous_7": "WR",
	}
	checkAnonymousClasses(t, strings.Replace(outer, "%s", defFirst, 1), wrM1)
	checkAnonymousClasses(t, strings.Replace(outer, "%s", ifFirst, 1), wrIf)
}

// `defvar vreg = lmul.vrclass;` is substituted textually, and the class
// application behind it is reached through a parent's argument
// (`P<vreg>` -> `Base<(G<vreg>.R:$r)>`). resolve() instantiates it with the
// loop substitution, so both G instances take their anonymous numbers before
// the `late` def that follows the multiclass (RISCVInstrInfoVPseudos.td's
// VTypeInfo chains).
func TestDefvarChainThroughParentArgument(t *testing.T) {
	const src = `class VR;
def VRNoV0 : VR;
def VRNoV1 : VR;
class MInfo { string MX = "M1"; VR vrclass = VRNoV0; }
def V_M1 : MInfo;
class MxSet<int n> { list<MInfo> m = [V_M1]; }
class G<VR v> { VR R = v; }
class Base<dag I> { dag Ins = I; int SEW = 0; }
class P<VR v> : Base<(G<v>.R:$r)> { }
multiclass M {
  foreach i = [8] in {
    defvar vreg = V_M1.vrclass;
    def "E" : P<vreg>;
  }
}
def late : P<VRNoV1>;
defm a : M;
`
	checkAnonymousClasses(t, src, map[string]string{
		"anonymous_0": "G",
		"anonymous_1": "G",
	})
	// The loop's G sees the defvar chain reaching V_M1.vrclass, and the late
	// def's G sees its own literal argument; swapping them was the bug.
	f, diags := syntax.Parse("t.td", src, nil, nil)
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("parse: %v", errs)
	}
	k, diags := record.Lower(f, "t.td")
	if errs := errorDiags(diags); len(errs) > 0 {
		t.Fatalf("lower: %v", errs)
	}
	want := map[string]string{"anonymous_0": "VRNoV0", "anonymous_1": "VRNoV1"}
	for _, r := range k.Defs() {
		name, ok := want[r.Name]
		if !ok {
			continue
		}
		txt, err := r.ValueText(r.Val("R"))
		if err != nil {
			t.Fatalf("%s.R: %v", r.Name, err)
		}
		if !strings.HasSuffix(txt, " = "+name) {
			t.Errorf("%s.R = %q, want it to end in %q", r.Name, txt, name)
		}
	}
}

// A `let` around a defm is applied by ParseDefm (ApplyLetStack) before the
// enclosing loop resolves the entries, so its class applications take their
// numbers in the field's declaration slot, not where the `let` is written.
// The entry's Vec/Rc fields resolve before Predicates, giving GetInt before
// GetPred in every iteration (RISCVInstrInfoVPseudos.td:5814-5822).
func TestDefmLetResolvesInFieldSlot(t *testing.T) {
	const src = `class VR;
def VRx : VR;
class VTInfo { int SEW = 8; VR Vector = VRx; VR Mask = VRx; VR RegClass = VRx; }
def VF16MF4 : VTInfo;
def VF32MF4 : VTInfo;
class GetInt<VTInfo v> { VTInfo Vti = v; }
class GetPred<VTInfo v> { list<int> P = [1]; }
class VPat<VTInfo vec, VTInfo rc> { VTInfo Vec = vec; VTInfo Rc = rc; list<int> Predicates = []; }
multiclass VPatConversion<string i, VTInfo vec, VTInfo rc> { def : VPat<vec, rc>; }
multiclass ConvVI_VF<string i> {
  foreach fvti = [VF16MF4, VF32MF4] in {
    defvar ivti = GetInt<fvti>.Vti;
    let Predicates = !listconcat(GetPred<fvti>.P, GetPred<ivti>.P) in
    defm : VPatConversion<i, ivti, ivti>;
  }
}
defm : ConvVI_VF<"x">;
`
	checkAnonymousClasses(t, src, map[string]string{
		"anonymous_0": "VPat",
		"anonymous_2": "GetInt",
		"anonymous_3": "GetPred",
		"anonymous_4": "GetInt",
		"anonymous_5": "GetPred",
		"anonymous_7": "VPat",
	})
}

// A `let` around a defm that sits in a multiclass body is applied while that
// body is parsed (TGParser.cpp:4656-4662), so the enclosing defm's resolve()
// sees it. That resolve scans every entry before any of them reaches
// addDefOne (TGParser.cpp:4638-4653), so the class application in the let
// takes its anonymous name in the scan, not in addDefOne where the record is
// renamed (TGParser.cpp:585-591). Here each iteration's TI/GT pair therefore
// precedes the second and third W; interleaving them was the bug.
func TestNestedDefmLetSeenByEnclosingResolve(t *testing.T) {
	const src = `class TI<int n> { int v = n; }
class GT<TI t> { int x = t.v; }
class W<int n> { int w = n; }
multiclass Inner<int n> { def : W<n>; }
multiclass Outer<list<int> L> {
  foreach v = L in {
    defvar iv = GT<TI<v>>.x;
    let w = iv in defm : Inner<v>;
  }
}
defm : Outer<[1, 2, 3]>;
`
	checkAnonymousClasses(t, src, map[string]string{
		"anonymous_0":  "W",
		"anonymous_3":  "TI",
		"anonymous_4":  "GT",
		"anonymous_5":  "TI",
		"anonymous_6":  "GT",
		"anonymous_7":  "TI",
		"anonymous_8":  "GT",
		"anonymous_9":  "W",
		"anonymous_10": "W",
	})
}

// A foreach whose list the parser can already resolve - a literal, or a
// defvar it is in the middle of parsing - is unrolled while the multiclass
// body is parsed, so its records become plain body entries and take the
// enclosing defm's site (TGParser.cpp:426-430, 563-565). A list that only
// becomes a ListInit once this defm's own arguments are substituted is
// expanded by resolve(), which passes no site down (TGParser.cpp:511).
// llvm-tblgen 24.0.0git reports exactly that difference in !locs.
func TestDefmLoopSiteFollowsListResolver(t *testing.T) {
	cases := []struct {
		name     string
		src      string
		defName  string
		wantSite bool
	}{
		{"literal", `multiclass P {
  foreach i = [1, 2] in
    def SubDef # i;
}
defm Y : P;
`, "YSubDef1", true},
		{"defvar", `defvar L = [1, 2];
multiclass P {
  foreach i = L in
    def SubDef # i;
}
defm Y : P;
`, "YSubDef1", true},
		{"defm argument", `class AClass<bit C> { bit Cond = C; }
def A1: AClass<1>;
class BoolToList<bit Value> { list<int> ret = !if(Value, [1]<int>, []<int>); }
multiclass P<string AStr> {
  foreach i = BoolToList<!cast<AClass>(AStr).Cond>.ret in
    def SubDef;
}
defm Y : P<"A1">;
`, "YSubDef", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, diags := syntax.Parse("t.td", c.src, nil, nil)
			if errs := errorDiags(diags); len(errs) > 0 {
				t.Fatalf("parse: %v", errs)
			}
			k, diags := record.Lower(f, "t.td")
			if errs := errorDiags(diags); len(errs) > 0 {
				t.Fatalf("lower: %v", errs)
			}
			var rec *record.Record
			for _, r := range k.Defs() {
				if r.Name == c.defName {
					rec = r
				}
			}
			if rec == nil {
				t.Fatalf("def %s not found", c.defName)
			}
			defLine := lineWith(t, c.src, "def SubDef")
			siteLine := lineWith(t, c.src, "defm Y")
			want := []int{defLine}
			if c.wantSite {
				want = append(want, siteLine)
			}
			var got []int
			for _, p := range rec.Locs {
				got = append(got, p.Line)
			}
			if len(got) != len(want) {
				t.Fatalf("locs: got lines %v want %v (def %d, defm %d)", got, want, defLine, siteLine)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("locs: got lines %v want %v (def %d, defm %d)", got, want, defLine, siteLine)
				}
			}
		})
	}
}

// lineWith returns the 1-based line number of the first line containing sub.
func lineWith(t *testing.T, src, sub string) int {
	t.Helper()
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, sub) {
			return i + 1
		}
	}
	t.Fatalf("no line contains %q", sub)
	return 0
}

// An unused template argument is never resolved: Record::resolveReferences
// only walks the template arguments the class body mentioned (RecordVal::Used
// is set by TGVarScope::getVar, TGParser.cpp:150-156), and
// VarDefInit::instantiate copies field values only (Record.cpp:2368-2404).
// The class application inside that argument is therefore a VarDefInit that
// never folds. It report a pr8330.td-shaped bug: folding the argument with the
// record's frame instantiated GetVRegNoV0 over the loop value as well, and the
// extra anonymous record shifted every later name.
func TestUnusedTemplateArgApplicationStaysLazy(t *testing.T) {
	src := `class VRc { int id = 1; }
def VR : VRc;
def VRM2 : VRc;
class LInfo<VRc v> { VRc vrclass = v; string MX = "M1"; }
def LI : LInfo<VR>;
class GetVRegNoV0<VRc VRegClass> { VRc R = VRegClass; }
class Use<VRc x> { list<int> l = [1]; }
multiclass MC {
  foreach m = [LI] in {
    def "D" : Use<GetVRegNoV0<m.vrclass>.R>;
  }
}
def : GetVRegNoV0<VR>;
defm "" : MC;
`
	want := `{"!instanceof":{"GetVRegNoV0":["anonymous_0"],"LInfo":["LI"],"Use":["D"],"VRc":["VR","VRM2"]},"!tablegen_json_version":1,"D":{"!anonymous":false,"!fields":[],"!locs":["t.td:10","t.td:14"],"!name":"D","!superclasses":["Use"],"l":[1]},"LI":{"!anonymous":false,"!fields":[],"!locs":["t.td:5"],"!name":"LI","!superclasses":["LInfo"],"MX":"M1","vrclass":{"def":"VR","kind":"def","printable":"VR"}},"VR":{"!anonymous":false,"!fields":[],"!locs":["t.td:2"],"!name":"VR","!superclasses":["VRc"],"id":1},"VRM2":{"!anonymous":false,"!fields":[],"!locs":["t.td:3"],"!name":"VRM2","!superclasses":["VRc"],"id":1},"anonymous_0":{"!anonymous":true,"!fields":[],"!locs":["t.td:13"],"!name":"anonymous_0","!superclasses":["GetVRegNoV0"],"R":{"def":"VR","kind":"def","printable":"VR"}}}`
	if got := lowerJSON(t, src); got != want {
		t.Errorf("dump-json mismatch\n got: %s\nwant: %s", got, want)
	}
}

// ParseDefm resolves its entries while an enclosing statement-level foreach is
// still only parsed, so the loop iterator is not in its MapResolver
// (TGParser.cpp:4588-4598); the iterator arrives when resolve() copies the
// records at loop-unroll time (TGParser.cpp:525-566). A `let` at the defm site
// is applied by ApplyLetStack before that copy (TGParser.cpp:4638-4659), so the
// value AddParentClass copied for the let's field is never resolved with the
// iterator bound: the class application inside it must not fold. It did, and
// the two D instances it created shifted C to anonymous_1 and anonymous_3.
func TestDefmInStatementLoopSkipsLetOverwrittenField(t *testing.T) {
	src := `class C<string mx> { int c = 1; }
class D<string mx, list<string> L> { bit c = !eq(mx, "M1"); }
defvar L = ["M1", "M2"];
class W<int lat> { int L2 = lat; }
multiclass MC<string p, int lat> { def NAME#p : W<lat>; }
foreach mx = L in {
  defvar A = C<mx>.c;
  defvar B = D<mx, L>.c;
  let L2 = A in
  defm : MC<mx, B>;
}
`
	want := `{"!instanceof":{"C":["anonymous_1","anonymous_2"],"D":[],"W":["anonymous_0M1","anonymous_0M2"]},"!tablegen_json_version":1,"anonymous_0M1":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:10"],"!name":"anonymous_0M1","!superclasses":["W"],"L2":1},"anonymous_0M2":{"!anonymous":false,"!fields":[],"!locs":["t.td:5","t.td:10"],"!name":"anonymous_0M2","!superclasses":["W"],"L2":1},"anonymous_1":{"!anonymous":true,"!fields":[],"!locs":["t.td:7"],"!name":"anonymous_1","!superclasses":["C"],"c":1},"anonymous_2":{"!anonymous":true,"!fields":[],"!locs":["t.td:7"],"!name":"anonymous_2","!superclasses":["C"],"c":1}}`
	if got := lowerJSON(t, src); got != want {
		t.Errorf("dump-json mismatch\n got: %s\nwant: %s", got, want)
	}
}
