package record

import (
	"strings"
	"testing"

	"github.com/nodecoda/tablegen-go/internal/diag"
	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// The arithmetic operators must trap where llvm-tblgen traps. Go silently
// wraps INT64_MIN / -1 and gives 0 for a negative shift count, so these are
// exactly the places where a port can look right and be wrong.
func TestIntegerOperatorTraps(t *testing.T) {
	cases := []struct{ src, want string }{
		{"def D { int v = !div(-9223372036854775808, -1); }", "Illegal operation: INT64_MIN / -1"},
		{"def D { int v = !div(1, 0); }", "Illegal operation: division by zero"},
		{"def D { int v = !shl(1, 64); }", "Illegal operation: out of bounds shift"},
		{"def D { list<string> v = !listsplat(\"\", -1); }", "!listsplat count -1 is negative"},
	}
	for _, c := range cases {
		_, diags := lowerSrc(t, c.src)
		found := false
		for _, d := range diags {
			if strings.Contains(d.Message, c.want) {
				found = true
			}
		}
		if !found {
			var got []string
			for _, d := range diags {
				got = append(got, d.Message)
			}
			t.Errorf("%s: got %q, want %q", c.src, got, c.want)
		}
	}
}

// !find out of range is a PrintError: the message is emitted, the fold still
// yields -1, and the run fails.
func TestFindStartOutOfRange(t *testing.T) {
	_, diags := lowerSrc(t, `def D { int v = !find("abc", "b", 100); }`)
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, "!find start position is out of range 0...3: 100") {
			found = true
			if !d.Soft {
				t.Errorf("!find diagnostic should be soft (PrintError), abort=%v", !d.Soft)
			}
		}
	}
	if !found {
		t.Fatalf("no !find diagnostic: %v", diags)
	}
}

func lowerSrc(t *testing.T, src string) (*Keeper, []diag.Diagnostic) {
	t.Helper()
	f, pdiags := syntax.Parse("t.td", src, nil, nil)
	for _, d := range pdiags {
		if d.Severity == diag.Error {
			t.Fatalf("parse: %v", pdiags)
		}
	}
	k, ldiags := Lower(f, "t.td")
	return k, ldiags
}

// Folding operators that fail while a def's field is resolved report at
// CurRec->getLoc(), the def's name token, not at the operator
// (Record.cpp:930-936, 1474-1506, 1410, 1914).
func TestFoldErrorsAnchorAtRecord(t *testing.T) {
	cases := []struct {
		src, want string
		line, col int
	}{
		{"def D { int v = !div(1, 0); }", "Illegal operation: division by zero", 1, 5},
		{"def D { int v = !logtwo(0); }", "Illegal operation: logtwo is undefined", 1, 5},
		{"def D { int v = !shl(1, 64); }", "Illegal operation: out of bounds shift", 1, 5},
		{"def op;\ndef D {\n  dag d = (op 1);\n  int v = !getdagarg<int>(d, -1);\n}",
			"!getdagarg index -1 is negative", 2, 5},
	}
	for _, c := range cases {
		_, diags := lowerSrc(t, c.src)
		found := false
		for _, d := range diags {
			if !strings.Contains(d.Message, c.want) {
				continue
			}
			found = true
			if d.Span.Start.Line != c.line || d.Span.Start.Col != c.col {
				t.Errorf("%s: at %d:%d, want %d:%d", c.want, d.Span.Start.Line, d.Span.Start.Col, c.line, c.col)
			}
		}
		if !found {
			t.Errorf("%s: diagnostic missing: %v", c.want, diags)
		}
	}
}

// TGParser type-checks the middle argument of !setdagarg/!setdagname and
// reports at that argument's own location (TGParser.cpp:2192-2199).
func TestSetDagArgKeyTypeAnchoredAtArgument(t *testing.T) {
	src := "def op;\ndef D {\n  dag d = (op 1);\n  dag e = !setdagarg(d, op, 1);\n}"
	_, diags := lowerSrc(t, src)
	want := "expected integer index or string name"
	for _, d := range diags {
		if !strings.Contains(d.Message, want) {
			continue
		}
		if d.Span.Start.Line != 4 || d.Span.Start.Col != 25 {
			t.Fatalf("at %d:%d, want 4:25", d.Span.Start.Line, d.Span.Start.Col)
		}
		return
	}
	t.Fatalf("no %q diagnostic: %v", want, diags)
}

// !instances returns derived definitions in RecordKeeper order, which sorts
// with LessRecord -> StringRef::compare_numeric (Record.h:2185,
// StringRef.cpp:57): a digit run compares by length before byte value, so
// VLE8 precedes VLE16. Plain byte order would reverse them.
func TestInstancesOrderIsNumeric(t *testing.T) {
	k, diags := lowerSrc(t, `class B;
def VLE16 : B;
def VLE8 : B;
def VLE2 : B;
def X { list<B> L = !instances<B>("VLE.*"); }
`)
	for _, d := range diags {
		if !d.Soft {
			t.Fatalf("lower: %v", diags)
		}
	}
	var rec *Record
	for _, r := range k.Defs() {
		if r.Name == "X" {
			rec = r
		}
	}
	if rec == nil {
		t.Fatal("def X not found")
	}
	txt, err := rec.ValueText(rec.Val("L"))
	if err != nil {
		t.Fatalf("X.L: %v", err)
	}
	if !strings.HasSuffix(txt, "= [VLE2, VLE8, VLE16]") {
		t.Fatalf("X.L = %q, want [VLE2, VLE8, VLE16]", txt)
	}
}
