package syntax

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nodecoda/tablegen-go/internal/diag"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func tokenKinds(toks []Token) []string {
	var out []string
	for _, tok := range toks {
		if tok.Kind == Eof {
			break
		}
		if tok.Kind == Id || tok.Kind == Def || tok.Kind == Semi || tok.Kind == Include {
			out = append(out, tok.Kind.String()+":"+tok.Text)
		}
	}
	return out
}

// Include is resolved in the lexer: the included tokens are spliced into the
// stream and keep their own file:line spans.
func TestIncludeSplicesTokens(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	sub := filepath.Join(dir, "sub.td")
	writeFile(t, sub, "def C;\n")
	writeFile(t, main, "def A;\ninclude \"sub.td\"\ndef B;\n")

	toks, ds := TokenizeWithDirs(main, "def A;\ninclude \"sub.td\"\ndef B;\n", nil, []string{dir})
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	got := tokenKinds(toks)
	want := []string{"def:def", "id:A", "semi:", "def:def", "id:C", "semi:", "def:def", "id:B", "semi:"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("tokens = %v, want %v", got, want)
	}
	for _, tok := range toks {
		if tok.Kind == Id && tok.Text == "C" && tok.Span.Start.File != sub {
			t.Fatalf("C span file = %q, want %q", tok.Span.Start.File, sub)
		}
	}
}

// The filename is resolved the way SourceMgr does it: literal path first, then
// each -I directory in order.
func TestIncludeSearchPath(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "inc")
	main := filepath.Join(dir, "main.td")
	writeFile(t, filepath.Join(inc, "nested", "leaf.td"), "def Leaf;\n")
	writeFile(t, main, "include \"nested/leaf.td\"\n")

	l := NewLexerWithDirs(main, "include \"nested/leaf.td\"\n", nil, []string{inc})
	toks, ds := l.Lex()
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	found := false
	for _, tok := range toks {
		if tok.Kind == Id && tok.Text == "Leaf" {
			found = true
		}
	}
	if !found {
		t.Fatalf("included token missing: %v", tokenKinds(toks))
	}
}

func TestIncludeNotFound(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	src := "def A;\ninclude \"missing.td\"\n"
	writeFile(t, main, src)
	_, ds := Tokenize(main, src, nil)
	if len(ds) == 0 {
		t.Fatal("expected a diagnostic")
	}
	d := ds[0]
	if d.Code != "E_LEX_INCLUDE_NOT_FOUND" {
		t.Fatalf("code = %q, want E_LEX_INCLUDE_NOT_FOUND", d.Code)
	}
	if d.Span.Start.Line != 2 {
		t.Fatalf("line = %d, want 2", d.Span.Start.Line)
	}
	if !strings.Contains(d.Message, "missing.td") {
		t.Fatalf("message = %q", d.Message)
	}
}

// Macros are global across includes; the preprocessor control stack is not.
func TestIncludeMacrosGlobalPrepPerFile(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	writeFile(t, filepath.Join(dir, "a.td"), "#define FROM_A\ndef A;\n")
	src := "include \"a.td\"\n#ifdef FROM_A\ndef Seen;\n#endif\n"
	writeFile(t, main, src)
	toks, ds := TokenizeWithDirs(main, src, nil, []string{dir})
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	seen := false
	for _, tok := range toks {
		if tok.Kind == Id && tok.Text == "Seen" {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("macro defined in included file did not survive: %v", tokenKinds(toks))
	}
}

func TestIncludeUnbalancedPrepInside(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	writeFile(t, filepath.Join(dir, "bad.td"), "#ifdef X\ndef Hidden;\n")
	src := "include \"bad.td\"\ndef After;\n"
	writeFile(t, main, src)
	toks, ds := TokenizeWithDirs(main, src, nil, []string{dir})
	var unclosed bool
	for _, d := range ds {
		if d.Code == "E_LEX_PREP_UNCLOSED" {
			unclosed = true
		}
		if d.Severity == diag.Error && !strings.HasPrefix(d.Code, "E_LEX_PREP_") {
			t.Fatalf("unexpected diagnostic: %v", d)
		}
	}
	if !unclosed {
		t.Fatalf("expected E_LEX_PREP_UNCLOSED, got %v", ds)
	}
	for _, tok := range toks {
		if tok.Kind == Id && tok.Text == "After" {
			t.Fatalf("lexing continued past an unbalanced include: %v", tokenKinds(toks))
		}
	}
}

// Include is lexically spliced, so it may appear inside a class body.
func TestIncludeInsideClassBody(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	writeFile(t, filepath.Join(dir, "fields.td"), "int x;\n")
	src := "class C {\ninclude \"fields.td\"\n}\n"
	writeFile(t, main, src)
	toks, ds := TokenizeWithDirs(main, src, nil, []string{dir})
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	found := false
	for _, tok := range toks {
		if tok.Kind == Int && tok.Text == "int" {
			found = true
		}
	}
	if !found {
		t.Fatalf("included field missing: %v", tokenKinds(toks))
	}
}

// Each `include` opens a fresh SourceMgr buffer (SourceMgr.cpp:58-67), so the
// same file included twice gives two instances with their own IncludeLoc
// chain. Tokens carry the instance in Pos.Inc and Includes.Parents maps it
// back to the include site.
func TestIncludeInstancesAreDistinct(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "main.td")
	sub := filepath.Join(dir, "sub.td")
	writeFile(t, sub, "def C;\n")
	src := "include \"sub.td\"\ninclude \"sub.td\"\n"
	writeFile(t, main, src)

	toks, ds, _, includes := TokenizeWithDeps(main, src, nil, []string{dir})
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	var incs []int
	for _, tok := range toks {
		if tok.Kind == Id && tok.Text == "C" {
			incs = append(incs, tok.Span.Start.Inc)
		}
	}
	if len(incs) != 2 || incs[0] == 0 || incs[0] == incs[1] {
		t.Fatalf("instances = %v, want two distinct non-main ids", incs)
	}
	for i, inc := range incs {
		site, ok := includes.Parent(diag.Pos{Inc: inc})
		if !ok || site.Line != i+1 || site.Col != 1 || site.Inc != 0 {
			t.Fatalf("instance %d parent = %+v, %v; want main.td:%d:1", i, site, ok, i+1)
		}
	}
}
