package syntax

import (
	"strings"
	"testing"

	"github.com/nodecoda/tablegen-go/internal/diag"
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

// TokError reports at the current token. For an empty !switch the parser has
// already consumed the closing ')' when it checks the case count, so the caret
// lands on the following token (TGParser.cpp:2888-2891).
func TestSwitchEmptyCaseCaret(t *testing.T) {
	line := "  int bad = !switch(1, 99);"
	_, diags := Parse("t.td", "def D {\n"+line+"\n}\n", nil, nil)
	var d *diag.Diagnostic
	for i := range diags {
		if diags[i].Message == "there should be at least 1 'case: value' in the !switch operator" {
			d = &diags[i]
			break
		}
	}
	if d == nil {
		t.Fatalf("missing !switch diagnostic: %v", msgs(diags))
	}
	want := strings.Index(line, ";") + 1
	if d.Span.Start.Line != 2 || d.Span.Start.Col != want {
		t.Fatalf("at %d:%d, want 2:%d", d.Span.Start.Line, d.Span.Start.Col, want)
	}
}

// A body-ending semicolon prints the error and then a standalone note
// (TGParser.cpp:3877-3882, 4545-4550).
func TestSpuriousSemicolonNote(t *testing.T) {
	_, diags := Parse("t.td", "def D {\n};\nclass C {\n};\n", nil, nil)
	if len(diags) != 4 {
		t.Fatalf("got %d diagnostics, want 2 errors + 2 notes: %v", len(diags), msgs(diags))
	}
	for i := 0; i < len(diags); i += 2 {
		if diags[i].Severity != diag.Error || !strings.Contains(diags[i].Message, "should not end with a semicolon") {
			t.Fatalf("diag[%d] = %q, want the semicolon error", i, msgs(diags)[i])
		}
		n := diags[i+1]
		if n.Severity != diag.NoteSev || n.Message != "Semicolon ignored; remove to eliminate this error" {
			t.Fatalf("diag[%d] = %q, want the standalone note", i+1, msgs(diags)[i+1])
		}
		if n.Span.Start.File != "" || !strings.HasPrefix(n.Render(""), "note: ") {
			t.Fatalf("note carries a location: %+v", n.Span)
		}
	}
}

// prepLexMacroName moves TokStart to the macro name, so a duplicate #define is
// reported there rather than at the '#' (TGLexer.cpp:888-894, 968-976).
func TestDuplicateMacroWarningAtName(t *testing.T) {
	_, diags := Tokenize("t.td", "#define ENABLED1\n#define ENABLED1\n", nil)
	if len(diags) != 1 {
		t.Fatalf("got %d diagnostics, want 1: %v", len(diags), msgs(diags))
	}
	if p := diags[0].Span.Start; p.Line != 2 || p.Col != 9 {
		t.Fatalf("warning at %d:%d, want 2:9", p.Line, p.Col)
	}
}
