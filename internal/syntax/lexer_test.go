package syntax

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tblgen/internal/diag"
)

func lex(t *testing.T, src string) ([]Token, []diag.Diagnostic) {
	t.Helper()
	return Tokenize("t.td", src, nil)
}

func kinds(toks []Token) []Kind {
	out := make([]Kind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

func wantKinds(t *testing.T, got []Kind, want ...Kind) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("kind count = %d, want %d\ngot:  %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("token %d = %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
}

func codes(ds []diag.Diagnostic) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Code
	}
	return out
}

func hasCode(ds []diag.Diagnostic, code string) bool {
	for _, d := range ds {
		if d.Code == code {
			return true
		}
	}
	return false
}

func TestPunctuationAndKeywords(t *testing.T) {
	toks, ds := lex(t, `class C<string s> { string x = ?; } def d : C<"v">;`)
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	wantKinds(t, kinds(toks),
		Class, Id, Less, String, Id, Greater, LBrace,
		String, Id, Equal, Question, Semi, RBrace,
		Def, Id, Colon, Id, Less, StrVal, Greater, Semi, Eof)
}

func TestIdentifierMayStartWithDigit(t *testing.T) {
	// `foo#8i` must be Id, Paste, Id — not a number followed by garbage.
	toks, ds := lex(t, `foo#8i`)
	if len(ds) != 0 {
		t.Fatalf("unexpected diagnostics: %v", ds)
	}
	wantKinds(t, kinds(toks), Id, Paste, Id, Eof)
	if toks[0].Text != "foo" || toks[2].Text != "8i" {
		t.Fatalf("texts = %q,%q", toks[0].Text, toks[2].Text)
	}
}

func TestNumbers(t *testing.T) {
	cases := []struct {
		src  string
		kind Kind
		num  uint64
		text string
	}{
		{"0", IntVal, 0, "0"},
		{"42", IntVal, 42, "42"},
		{"-5", IntVal, ^uint64(4), "-5"},
		{"+7", IntVal, 7, "+7"},
		{"0x1f", IntVal, 31, "0x1f"},
		{"0b1010", BinaryIntVal, 10, "0b1010"},
		{"18446744073709551615", IntVal, ^uint64(0), "18446744073709551615"},
	}
	for _, c := range cases {
		toks, ds := lex(t, c.src)
		if len(ds) != 0 {
			t.Fatalf("%q: diagnostics %v", c.src, ds)
		}
		if toks[0].Kind != c.kind || toks[0].Num != c.num || toks[0].Text != c.text {
			t.Errorf("%q: got kind=%s num=%d text=%q", c.src, toks[0].Kind, toks[0].Num, toks[0].Text)
		}
	}

	// `0x` with no digits is an *identifier* in LLVM, not a number: the
	// [0-9][xb] heuristic only commits when a digit follows the prefix.
	if toks, ds := lex(t, "0x"); len(ds) != 0 || toks[0].Kind != Id || toks[0].Text != "0x" {
		t.Errorf("0x: got kind=%s text=%q diags=%v", toks[0].Kind, toks[0].Text, codes(ds))
	}

	for _, bad := range []struct{ src, code string }{
		{"18446744073709551616", "E_LEX_NUMBER_RANGE"},
		{"-9223372036854775809", "E_LEX_NUMBER_RANGE"},
	} {
		_, ds := lex(t, bad.src)
		if !hasCode(ds, bad.code) {
			t.Errorf("%q: want %s, got %v", bad.src, bad.code, codes(ds))
		}
	}
}

func TestStrings(t *testing.T) {
	toks, ds := lex(t, `"a\tb\nc\\d\"e\'f"`)
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	if toks[0].Kind != StrVal || toks[0].Text != "a\tb\nc\\d\"e'f" {
		t.Fatalf("got %q", toks[0].Text)
	}

	for _, c := range []struct{ src, code string }{
		{`"a\qb"`, "E_LEX_ESCAPE"},
		{"\"unterminated\n", "E_LEX_STRING_EOL"},
		{"\"unterminated", "E_LEX_STRING_EOF"},
		{"\"a\\\nb\"", "E_LEX_ESCAPED_NEWLINE"},
	} {
		_, ds := lex(t, c.src)
		if !hasCode(ds, c.code) {
			t.Errorf("%q: want %s, got %v", c.src, c.code, codes(ds))
		}
	}
}

func TestCodeFragment(t *testing.T) {
	toks, ds := lex(t, "[{ int x = 1; }]")
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	if toks[0].Kind != CodeFragment || toks[0].Text != " int x = 1; " {
		t.Fatalf("got %s %q", toks[0].Kind, toks[0].Text)
	}

	_, ds = lex(t, "[{ unterminated")
	if !hasCode(ds, "E_LEX_CODE_EOF") {
		t.Fatalf("want E_LEX_CODE_EOF, got %v", codes(ds))
	}
}

func TestAllBangOperatorsLex(t *testing.T) {
	var names []string
	for name := range bangOps {
		names = append(names, name)
	}
	// Order is irrelevant; each must lex as its own kind.
	for _, name := range names {
		toks, ds := lex(t, "!"+name)
		if len(ds) != 0 {
			t.Errorf("!%s: diagnostics %v", name, ds)
			continue
		}
		if toks[0].Kind != bangOps[name] {
			t.Errorf("!%s lexed as %s, want %s", name, toks[0].Kind, bangOps[name])
		}
	}
	if _, ds := lex(t, "!nope"); !hasCode(ds, "E_LEX_BANG") {
		t.Errorf("unknown bang operator not rejected: %v", codes(ds))
	} else if len(ds) == 1 && ds[0].Message != "unknown operator" {
		// TGLexer.cpp:701 prints the message without the operator name.
		t.Errorf("unknown bang operator message = %q", ds[0].Message)
	}
	if _, ds := lex(t, "!"); !hasCode(ds, "E_LEX_BANG") {
		t.Errorf("bare ! not rejected: %v", codes(ds))
	}
}

func TestComments(t *testing.T) {
	toks, ds := lex(t, "a // comment\nb /* x /* nested */ y */ c")
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	wantKinds(t, kinds(toks), Id, Id, Id, Eof)

	_, ds = lex(t, "/* nope")
	if !hasCode(ds, "E_LEX_COMMENT_EOF") {
		t.Fatalf("want E_LEX_COMMENT_EOF, got %v", codes(ds))
	}
}

func TestDotDotDot(t *testing.T) {
	toks, ds := lex(t, "... .")
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	wantKinds(t, kinds(toks), DotDotDot, Dot, Eof)

	if _, ds := lex(t, ".."); !hasCode(ds, "E_LEX_DOT") {
		t.Fatalf("want E_LEX_DOT, got %v", codes(ds))
	}
}

func TestPositions(t *testing.T) {
	toks, _ := lex(t, "def\n  x")
	def, x := toks[0], toks[1]
	if def.Span.Start.Line != 1 || def.Span.Start.Col != 1 {
		t.Errorf("def at %v", def.Span.Start)
	}
	if x.Span.Start.Line != 2 || x.Span.Start.Col != 3 {
		t.Errorf("x at %v", x.Span.Start)
	}
}

func TestCRLFCountsAsOneLine(t *testing.T) {
	toks, _ := lex(t, "a\r\nb")
	if toks[1].Span.Start.Line != 2 {
		t.Fatalf("b at %v", toks[1].Span.Start)
	}
}

func TestPreprocessor(t *testing.T) {
	// Undefined macro: the body disappears, the tail survives.
	toks, ds := lex(t, "#ifdef A\ndead\ndef dead2\n#endif\nlive")
	if len(ds) != 0 {
		t.Fatalf("diagnostics: %v", ds)
	}
	if len(toks) != 2 || toks[0].Text != "live" {
		t.Fatalf("got %v", kinds(toks))
	}

	// Defined macro: body survives, else branch does not.
	{
		l := NewLexer("t.td", "#ifdef A\nlive\n#else\ndead\n#endif\n", []string{"A"})
		toks, ds := l.Lex()
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %v", ds)
		}
		if len(toks) != 2 || toks[0].Text != "live" {
			t.Fatalf("got %v", kinds(toks))
		}
	}

	// #ifndef inverts; #define inside a live region takes effect.
	{
		l := NewLexer("t.td", "#ifndef B\n#define B\na\n#endif\n#ifdef B\nb\n#endif\n", nil)
		toks, ds := l.Lex()
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %v", ds)
		}
		var texts []string
		for _, tok := range toks[:len(toks)-1] {
			texts = append(texts, tok.Text)
		}
		if strings.Join(texts, ",") != "a,b" {
			t.Fatalf("got %v", texts)
		}
	}

	// Nested conditionals.
	{
		l := NewLexer("t.td", "#ifdef A\n#ifdef B\nab\n#endif\n#endif\n", []string{"A"})
		toks, ds := l.Lex()
		if len(ds) != 0 {
			t.Fatalf("diagnostics: %v", ds)
		}
		if len(toks) != 1 || toks[0].Kind != Eof {
			t.Fatalf("got %v", kinds(toks))
		}
	}
}

// TestDirectiveAtEOF pins an LLVM quirk: peekNextChar reads the buffer's NUL
// terminator, which is not EOF(-1), so a directive immediately followed by the
// end of the file is not recognized. The oracle then reports an unmatched
// #ifdef at EOF; we must do the same.
func TestDirectiveAtEOF(t *testing.T) {
	_, ds := lex(t, "#ifdef FOO\nendif-not-a-directive\n#endif")
	if !hasCode(ds, "E_LEX_PREP_UNCLOSED") {
		t.Fatalf("want E_LEX_PREP_UNCLOSED, got %v", codes(ds))
	}
	if ds[0].Span.Start.Line != 3 || ds[0].Span.Start.Col != 7 {
		t.Fatalf("first diagnostic at %v, want 3:7", ds[0].Span.Start)
	}
}

func TestPreprocessorErrors(t *testing.T) {
	cases := []struct{ src, code string }{
		{"#ifdef\n#endif\n", "E_LEX_PREP_MACRO"},
		{"#endif\n", "E_LEX_PREP_ENDIF"},
		{"#else\n", "E_LEX_PREP_ELSE"},
		{"#ifdef A\n#else\n#else\n#endif\n", "E_LEX_PREP_ELSE"},
		{"#ifdef A", "E_LEX_PREP_UNCLOSED"},
		{"#ifdef A junk\n#endif\n", "E_LEX_PREP_TRAILING"},
	}
	for _, c := range cases {
		_, ds := lex(t, c.src)
		if !hasCode(ds, c.code) {
			t.Errorf("%q: want %s, got %v", c.src, c.code, codes(ds))
		}
	}
}

// The lexer must swallow every file in LLVM's TableGen test suite without a
// panic; only files that are *meant* to be broken may report a lexical error.
func TestCorpusLexes(t *testing.T) {
	dir := "/home/ubuntu/llvm/llvm-project/llvm/test/TableGen"
	llvmIncludeDir := "/home/ubuntu/llvm/llvm-project/llvm/include"
	// Include directories the lit RUN lines use for the corpus: the test
	// directory itself, shared Inputs, and target .td trees.
	incDirs := []string{
		dir,
		filepath.Join(dir, "Common"),
		"/home/ubuntu/llvm/llvm-project/llvm/lib/Target/AMDGPU",
		"/home/ubuntu/llvm/llvm-project/llvm/lib/Target/AArch64",
		llvmIncludeDir,
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.td"))
	if err != nil || len(files) == 0 {
		t.Skip("LLVM TableGen test corpus not available")
	}
	// Files that exist to test a *lexical* diagnostic. Every one of them must
	// still report an error, and no other file may.
	expectsLexError := func(base string) bool {
		return base == "UnterminatedComment.td" ||
			base == "unterminated-c-comment.td" ||
			strings.HasPrefix(base, "prep-diag") ||
			strings.HasPrefix(base, "prep-ifndef-diag") ||
			base == "eof-after-endif.td" ||
			base == "unterminated-code-block.td" ||
			// split-file tests: the .td on disk is a container, the real
			// inputs (and `a.td`) only exist after `split-file` runs.
			base == "escape-dependency-filenames.td" ||
			base == "preprocess-depfile.td" ||
			base == "preprocess-include.td" ||
			base == "AMDGPUTargetDefGenericFeatures.td" ||
			base == "AMDGPUTargetDefLDSAllocGranularity.td" ||
			base == "RegClassByHwModeErrors.td" ||
			base == "RegClassEmptyAllocationOrder.td"
	}
	sawError := map[string]bool{}
	var unexpected []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		toks, ds := TokenizeWithDirs(f, string(b), nil, incDirs)
		if len(toks) == 0 || toks[len(toks)-1].Kind != Eof {
			t.Errorf("%s: token stream does not end in Eof", f)
		}
		lexErr := false
		for _, d := range ds {
			if d.Severity == diag.Error && strings.HasPrefix(d.Code, "E_LEX_") {
				lexErr = true
			}
		}
		base := filepath.Base(f)
		if lexErr {
			sawError[base] = true
		}
		if lexErr && !expectsLexError(base) {
			unexpected = append(unexpected, base)
		}
	}
	if len(unexpected) > 0 {
		t.Errorf("%d unexpected lexical failures: %v", len(unexpected), unexpected)
	}
	for _, f := range files {
		base := filepath.Base(f)
		if expectsLexError(base) && !sawError[base] {
			t.Errorf("%s is expected to report a lexical error but did not", base)
		}
	}
}

func FuzzLex(f *testing.F) {
	f.Add("")
	f.Add(`def x : C<1, "s"> { let a = [{ code }]; }`)
	f.Add("#ifdef A\n#else\n#endif\n")
	f.Add("foo#8i 0x1f 0b1010 /* /* */ */ ...")
	f.Fuzz(func(t *testing.T, src string) {
		toks, _ := Tokenize("fuzz.td", src, nil)
		if len(toks) == 0 || toks[len(toks)-1].Kind != Eof {
			t.Fatal("token stream does not end in Eof")
		}
		for _, tok := range toks {
			if tok.Span.Start.Line < 1 || tok.Span.Start.Col < 1 {
				t.Fatalf("bad span %v for %s", tok.Span.Start, tok.Kind)
			}
			if tok.Span.End.Line < tok.Span.Start.Line {
				t.Fatalf("end before start: %v", tok.Span)
			}
		}
	})
}
