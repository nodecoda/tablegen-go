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
	if len(got) == 0 || got[0] != "error: template argument with the same name has already been defined" {
		t.Fatalf("got %q", got)
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
