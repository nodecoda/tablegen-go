package syntax

import (
	"reflect"
	"strings"
	"testing"

	"tblgen/internal/diag"
)

func parseSrc(t *testing.T, src string) (*File, []diag.Diagnostic) {
	t.Helper()
	return Parse("test.td", src, nil, nil)
}

func mustParse(t *testing.T, src string) *File {
	t.Helper()
	f, diags := parseSrc(t, src)
	for _, d := range diags {
		if d.Severity == diag.Error {
			t.Fatalf("unexpected error: %s", d.String())
		}
	}
	return f
}

func firstError(t *testing.T, diags []diag.Diagnostic) diag.Diagnostic {
	t.Helper()
	for _, d := range diags {
		if d.Severity == diag.Error {
			return d
		}
	}
	t.Fatalf("expected an error, got none")
	return diag.Diagnostic{}
}

func TestClass(t *testing.T) {
	f := mustParse(t, `
class Base;
class C<int a, string b = "x"> : Base {
  int f = a;
  field string g;
}
`)
	if len(f.Objects) != 2 {
		t.Fatalf("objects = %d, want 2", len(f.Objects))
	}
	c := f.Objects[1].(*ClassStmt)
	if c.Name != "C" || len(c.TemplateArgs) != 2 || c.Parents[0].Name != "Base" {
		t.Fatalf("bad class: %+v", c)
	}
	if c.TemplateArgs[0].Type.Kind != IntType || c.TemplateArgs[1].Value == nil {
		t.Fatalf("bad template args: %+v", c.TemplateArgs)
	}
	if len(c.Body) != 2 {
		t.Fatalf("body = %d", len(c.Body))
	}
	if c.Body[0].(*Decl).Name != "f" || c.Body[1].(*Decl).IsField != true {
		t.Fatalf("bad body: %+v", c.Body)
	}
}

func TestDefAndAnonymous(t *testing.T) {
	f := mustParse(t, `
class Base;
def A : Base { int x = 1; }
def { int y = 2; }
def B;
`)
	if d := f.Objects[1].(*DefStmt); d.Name == nil || len(d.Body) != 1 {
		t.Fatalf("bad def: %+v", d)
	}
	anon := f.Objects[2].(*DefStmt)
	if anon.Name != nil {
		t.Fatalf("expected anonymous def, got %v", anon.Name)
	}
	decl := f.Objects[3].(*DefStmt)
	if decl.Body != nil {
		t.Fatalf("def B; should have no body")
	}
}

func TestDefmAndMulticlass(t *testing.T) {
	f := mustParse(t, `
class Cls;
multiclass Other { def o; }
multiclass MC<int a> : Other {
  def d : Cls;
  defvar v = a;
  let a = 1 in def e;
}
defm M : MC<3>, Cls;
`)
	mc := f.Objects[2].(*MultiClassStmt)
	if mc.Name != "MC" || len(mc.Body) != 3 || mc.Parents[0].Name != "Other" {
		t.Fatalf("bad multiclass: %+v", mc)
	}
	dm := f.Objects[3].(*DefmStmt)
	if len(dm.Parents) != 2 || dm.ClassStart != 1 || dm.Parents[1].Name != "Cls" {
		t.Fatalf("bad defm: %+v", dm)
	}
}

func TestForeachRangeAndList(t *testing.T) {
	f := mustParse(t, `
foreach i = {0, 2-4, 7} in { def x#i; }
foreach j = [1, 2] in def y#j;
foreach k = 0...2 in { def z#k; }
`)
	want := []int32{0, 2, 3, 4, 7}
	got := f.Objects[0].(*ForeachStmt).Range
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("range = %v, want %v", got, want)
	}
	if f.Objects[1].(*ForeachStmt).List == nil {
		t.Fatalf("expected list foreach")
	}
	if got := f.Objects[2].(*ForeachStmt).Range; !reflect.DeepEqual(got, []int32{0, 1, 2}) {
		t.Fatalf("range = %v", got)
	}
}

func TestIfElseAndLet(t *testing.T) {
	f := mustParse(t, `
if 1 then def a; else { def b; }
let append x = [1], prepend y = "s" in { def c; }
`)
	ifs := f.Objects[0].(*IfStmt)
	if ifs.Else == nil || len(ifs.Else) != 1 {
		t.Fatalf("bad if/else: %+v", ifs)
	}
	l := f.Objects[1].(*LetStmt)
	if len(l.Binds) != 2 || l.Binds[0].Mode != LetAppend || l.Binds[1].Mode != LetPrepend {
		t.Fatalf("bad let modes: %+v", l.Binds)
	}
	if l.Binds[0].Field != "x" {
		t.Fatalf("append should name x, got %q", l.Binds[0].Field)
	}
}

func TestDefsetAndDeftype(t *testing.T) {
	f := mustParse(t, `
class R;
defset list<R> all = { def a : R; }
deftype MyInt = int;
`)
	ds := f.Objects[1].(*DefsetStmt)
	if ds.Name != "all" || ds.ElemType.Kind != RecordType || len(ds.Objects) != 1 {
		t.Fatalf("bad defset: %+v", ds)
	}
	dt := f.Objects[2].(*DeftypeStmt)
	if dt.Name != "MyInt" || dt.Type.Kind != IntType {
		t.Fatalf("bad deftype: %+v", dt)
	}
}

func TestValues(t *testing.T) {
	f := mustParse(t, `
class Op;
def X {
  dag d = (Op 1, $n, "s":$val);
  list<int> l = [1, 2, 3]<int>;
  bits<4> b = {1, 0, 1, 0};
  string p = "a" # "b";
  int r = b{0, 1};
  list<int> s = l[1, 2];
  int e = l[0];
}
`)
	body := f.Objects[1].(*DefStmt).Body
	dag := body[0].(*Decl).Value.(*DagValue)
	if len(dag.Args) != 3 || dag.Args[1].Name != "n" || dag.Args[2].Name != "val" {
		t.Fatalf("bad dag: %+v", dag.Args)
	}
	list := body[1].(*Decl).Value.(*ListValue)
	if len(list.Elems) != 3 || list.ElemType == nil {
		t.Fatalf("bad list: %+v", list)
	}
	bitsv := body[2].(*Decl).Value.(*BitsValue)
	if bitsv.Ty.Bits != 4 {
		t.Fatalf("bits width = %d", bitsv.Ty.Bits)
	}
	if _, ok := body[3].(*Decl).Value.(*PasteValue); !ok {
		t.Fatalf("expected paste")
	}
	if br, ok := body[4].(*Decl).Value.(*BitRangeValue); !ok || br.Ty.Bits != 2 {
		t.Fatalf("bad bit range: %+v", body[4].(*Decl).Value)
	}
	if sv, ok := body[5].(*Decl).Value.(*SliceValue); !ok || sv.Ty.Kind != ListType {
		t.Fatalf("bad slice: %+v", body[5].(*Decl).Value)
	}
	if sv, ok := body[6].(*Decl).Value.(*SliceValue); !ok || sv.Ty.Kind != IntType {
		t.Fatalf("single slice should be the element type: %+v", body[6].(*Decl).Value)
	}
}

func TestBangOps(t *testing.T) {
	cases := []struct {
		src  string
		want string
	}{
		{`!add(1, 2)`, "!add"},
		{`!if(1, 2, 3)`, "!if"},
		{`!cond(1: "a", 0: "b")`, "!cond"},
		{`!switch(1, 1: "a", "d")`, "!switch"},
		{`!foreach(x, [1], !mul(x, 2))`, "!foreach"},
		{`!foldl(0, [1], a, b, !add(a, b))`, "!foldl"},
		{`!substr("abc", 1)`, "!substr"},
		{`!find("abc", "b")`, "!find"},
		{`!range(0, 4)`, "!range"},
		{`!cast<string>(1)`, "!cast"},
		{`!isa<int>(1)`, "!isa"},
		{`!exists<int>("1")`, "!exists"},
		{`!instances<int>(".*")`, "!instances"},
		{`!dag("op", [1], ["a"])`, "!dag"},
	}
	for _, c := range cases {
		f := mustParse(t, "defvar v = "+c.src+";")
		v := f.Objects[0].(*DefvarStmt).Value
		if got := DumpValue(v); !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: dump %q, want prefix %q", c.src, got, c.want)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
		msg  string
	}{
		{"reserved NAME", "def X {\n  int NAME = 1;\n}", 2, "'NAME' is a reserved variable name"},
		{"defm colon", "multiclass M { def d; }\ndefm x M;\n", 2, "expected ':' after defm identifier"},
		{"empty multiclass", "multiclass M {\n}\n", 2, "multiclass must contain at least one def"},
		{"class in foreach", "foreach i = 0 in {\n  class C;\n}\n", 2, "class is not allowed inside foreach loop"},
		{"defset in multiclass", "multiclass M {\n  defset list<int> s = {}\n  def d;\n}\n", 2, "in multiclass body"},
		{"defset in nested let", "multiclass M<int a> {\n  let a = 1 in {\n    defset list<int> s = {}\n  }\n}\n", 3, "defset is not allowed inside multiclass"},
		{"unknown let field", "def X {\n  let nope = 1;\n}\n", 2, "Value 'nope' unknown!"},
		{"undefined var", "def X {\n  int v = nope;\n}\n", 2, "Variable not defined: 'nope'"},
		{"missing body brace", "def X\n  int v = 1;\n", 2, "Expected '{' to start body or ';' for declaration only"},
		{"bad type", "def X {\n  foo v = 1;\n}\n", 2, "Couldn't find class 'foo'"},
		{"foreach neg range", "foreach i = -1 - 0 in {\n}\n", 1, "invalid range, cannot be negative"},
		{"foreach bad range", "foreach i = \"x\" - 1 in {\n}\n", 1, "expected integer or bitrange"},
		{"exists type", "defvar v = !exists<int>(1);\n", 1, "expected string type argument in !exists operator"},
		{"unary list", "class C<int A> {\n  list<int> F = !listflatten(A);\n}\n", 2, "expected list type argument in unary operator"},
		{"untyped binop", "def X {\n  bit D = !eq(?, 0);\n}\n", 2, "expected value to be a typed value, got '?'"},
		{"body semicolon", "def X {\n};\n", 2, "A class or def body should not end with a semicolon"},
		{"dup class", "class C { int a; }\nclass C { int b; }\n", 2, "Class 'C' already defined"},
		{"multiclass for class", "class C;\ndefm M : C;\n", 2, "Couldn't find multiclass 'C'"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, diags := parseSrc(t, c.src)
			d := firstError(t, diags)
			if d.Span.Start.Line != c.line {
				t.Errorf("line = %d, want %d (%s)", d.Span.Start.Line, c.line, d.Message)
			}
			if !strings.Contains(d.Message, c.msg) {
				t.Errorf("message = %q, want substring %q", d.Message, c.msg)
			}
		})
	}
}

// TestSpans walks the whole tree reflectively and requires every span to be
// populated. A zero span means a node was constructed without a location, which
// would make later diagnostics point at 1:1.
func TestSpans(t *testing.T) {
	f := mustParse(t, `
class Base<int a> { int f = a; }
class Op;
multiclass Other { def o; }
multiclass MC<int a> : Other {
  def d : Base<a> { let f = 1; assert 1, "m"; dump 2; }
  foreach i = {0, 1} in def e;
}
defm M : MC<1>;
def X : Base<2> {
  dag d = (Op 1, $n);
  list<list<int>> l = [[1], [2, 3]];
  string s = "a" # "b";
  int v = !add(1, 2);
}
defset list<int> dup = { def a; }
deftype T = bits<4>;
if 1 then def yes; else def no;
let x = 1 in def y;
`)
	var walk func(v reflect.Value, path string)
	seen := 0
	walk = func(v reflect.Value, path string) {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return
			}
			walk(v.Elem(), path)
			return
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				ft := v.Type().Field(i)
				fv := v.Field(i)
				if ft.Name == "Base" && ft.Type == reflect.TypeOf(Base{}) {
					b := fv.Interface().(Base)
					seen++
					if b.At.Start.Line == 0 || b.At.End.Line == 0 {
						t.Errorf("zero span at %s.%s", path, ft.Name)
					}
				}
				if ft.Type.Kind() == reflect.Interface || ft.Type.Kind() == reflect.Slice ||
					ft.Type.Kind() == reflect.Pointer || ft.Type.Kind() == reflect.Struct {
					walk(fv, path+"."+ft.Name)
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), path)
			}
		}
	}
	walk(reflect.ValueOf(f), "File")
	if seen < 30 {
		t.Fatalf("only %d spans checked; walker missed nodes", seen)
	}
}

func TestDumpIsStable(t *testing.T) {
	f := mustParse(t, `class C<int a> { int f = a; }`+"\n"+`def X : C<1>;`)
	got := DumpFile(f)
	want := `(class C)
  (arg int a)
  (field int f = a)
(def "X")
  (parent C 1)
`
	if got != want {
		t.Fatalf("dump mismatch:\n%s\nwant:\n%s", got, want)
	}
}

func FuzzParse(f *testing.F) {
	seeds := []string{
		"def X;",
		"class C<int a> { int f = a; }",
		"multiclass M { def d; }",
		"foreach i = {0, 2-4} in def x#i;",
		"!if(!eq(1, 2), !add(1, 2), [1,2]<int>)",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, src string) {
		_, _ = Parse("fuzz.td", src, nil, nil)
	})
}
