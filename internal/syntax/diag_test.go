package syntax

import (
	"testing"

	"tblgen/internal/diag"
)

func msgs(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Severity.String()+": "+d.Message)
	}
	return out
}

// The warning text and its qualification are llvm-tblgen's, and they are the
// reason cond-type.td/if-type.td stderr finally matches: 12 corpus files carry
// this warning next to a real error.
func TestUnusedTemplateArgWarning(t *testing.T) {
	_, diags := Parse("t.td", `
class A<int dummy> {}
multiclass M<int used> {
  def NAME { int v = used; }
}
`, nil, nil)
	got := msgs(diags)
	want := []string{
		"warning: unused template argument: A:dummy",
	}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// A field shadows a template argument with the same name: TGParser keeps
// template arguments in Record::TemplateArgs and looks a bare name up with
// Record::getValue first (TGParser.cpp:173-183), so the argument becomes
// unreachable and is reported unused. ARMInstrMVE.td:2965 writes
// `let Inst{20-16} = imm{4-0}` in exactly this shape.
func TestFieldShadowsTemplateArg(t *testing.T) {
	f, diags := Parse("t.td", `
class Operand;
class A<Operand imm> {
  bits<5> imm;
  bits<5> inst;
  let inst{4-0} = imm{4-0};
}
def O : Operand;
def X : A<O>;
`, nil, nil)
	got := msgs(diags)
	want := []string{"warning: unused template argument: A:imm"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("got %q, want %q", got, want)
	}
	class := f.Objects[1].(*ClassStmt)
	let := class.Body[2].(*LetItem)
	v := let.Bind.Value.(*BitRangeValue).Base.(*VarValue)
	if v.ArgRef || v.ArgQual != "" {
		t.Fatalf("imm is still the template argument: %+v", v)
	}
	if v.Ty.Kind != BitsType || v.Ty.Bits != 5 {
		t.Fatalf("imm has type %+v, want bits<5>", v.Ty)
	}
}

func TestNoWarnUnusedTemplateArgs(t *testing.T) {
	_, diags := ParseWith("t.td", `class A<int dummy> {}`, nil, nil, nil,
		Options{NoWarnUnusedTArgs: true})
	if len(diags) != 0 {
		t.Fatalf("expected silence, got %q", msgs(diags))
	}
}

func TestDuplicateTemplateArg(t *testing.T) {
	_, diags := Parse("t.td", `class C<int a, int a>;`, nil, nil)
	got := msgs(diags)
	if len(got) != 1 || got[0] != "error: template argument with the same name has already been defined" {
		t.Fatalf("got %q, want exactly one duplicate error", got)
	}
	if p := diags[0].Span.Start; p.Line != 1 || p.Col != 16 {
		t.Fatalf("diagnostic at %d:%d, want 1:16", p.Line, p.Col)
	}
}

func TestDefvarRedeclaration(t *testing.T) {
	cases := []struct {
		src, want string
	}{
		{"defvar x = 1;\ndefvar x = 2;\n", "error: def or global variable of this name already exists"},
		{"multiclass M { defvar x = 1; defvar x = 2; def NAME; }", "error: local variable of this name already exists"},
		{"class C { int x = 1; defvar x = 2; }", "error: field of this name already exists"},
	}
	for _, c := range cases {
		_, diags := Parse("t.td", c.src, nil, nil)
		got := msgs(diags)
		found := false
		for _, g := range got {
			if g == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: got %q", c.src, got)
		}
	}
}

// TGParser::ParseOperatorType reports the code type but keeps building the
// operator (TGParser.cpp:2369-2390), so the value parses normally and no
// cascade of follow-on diagnostics is produced.
func TestCodeTypeInBangOperatorStillParses(t *testing.T) {
	_, diags := Parse("t.td", `def Z {
  code Code = !cast<code>("i = 0;");
}`, nil, nil)
	got := msgs(diags)
	want := "error: the 'code' type is not allowed in bang operators; use 'string'"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want [%q]", got, want)
	}
	if p := diags[0].Span.Start; p.Line != 2 || p.Col != 21 {
		t.Fatalf("diagnostic at %d:%d, want 2:21", p.Line, p.Col)
	}
}

// TGParser.cpp:1310-1325 reports a non-record type for !getdagop but keeps
// parsing to consume the operand; the operator still has the given type.
func TestGetDagOpNonRecordTypeKeepsParsing(t *testing.T) {
	_, diags := Parse("t.td", `class Base;
def foo: Base;
def test {
  dag orig = (foo 1);
  int x = !getdagop<int>(orig);
}`, nil, nil)
	got := msgs(diags)
	want := "error: type for !getdagop must be a record type"
	if len(got) == 0 || got[0] != want {
		t.Fatalf("got %q, want first %q", got, want)
	}
	if p := diags[0].Span.Start; p.Line != 5 || p.Col != 25 {
		t.Fatalf("diagnostic at %d:%d, want 5:25", p.Line, p.Col)
	}
}

// ParseIDValue represents a direct self-reference as !cast<{}>("Name")
// (TGParser.cpp:1206-1211), and RecordRecTy::getAsString prints the empty
// record type as "{}" (Record.cpp:272-284).
func TestSelfReferencePrintsUnresolvedCast(t *testing.T) {
	_, diags := Parse("t.td", `class Cl<Cl rec> {
  Cl Arec = rec;
}
def Rec0 : Cl<Rec0>;
`, nil, nil)
	got := msgs(diags)
	want := `error: Value specified for template argument 'Cl:rec' is of type {}; expected type Cl: !cast<{}>("Rec0")`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("got %q, want [%q]", got, want)
	}
}

// !sort type-checks the list after the comma following it has been consumed,
// so the diagnostic points at the next token (TGParser.cpp:2649-2677).
func TestSortRejectsNonListAtNextToken(t *testing.T) {
	_, diags := Parse("t.td", `def myop;
defvar mydag = (myop);
def test {
  list<string> bad = !sort(x, mydag, !cast<string>(x));
}`, nil, nil)
	got := msgs(diags)
	if len(got) == 0 || got[0] != "error: !sort must have a list argument" {
		t.Fatalf("got %q", got)
	}
	if p := diags[0].Span.Start; p.Line != 4 || p.Col != 38 {
		t.Fatalf("diagnostic at %d:%d, want 4:38", p.Line, p.Col)
	}
}
