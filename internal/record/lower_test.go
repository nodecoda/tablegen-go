package record_test

import (
	"strings"
	"testing"

	"tblgen/internal/backend"
	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
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
