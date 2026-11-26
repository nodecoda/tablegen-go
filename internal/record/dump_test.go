package record_test

import (
	"strings"
	"testing"

	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

// lowerDiags parses and lowers without failing on errors, returning the full
// diagnostic stream (dump notes included).
func lowerDiags(t *testing.T, src string) []diag.Diagnostic {
	t.Helper()
	file, pdiags := syntax.Parse("t.td", src, nil, nil)
	for _, d := range pdiags {
		if d.Severity == diag.Error {
			t.Fatalf("parse: %v", pdiags)
		}
	}
	_, ldiags := record.Lower(file, "t.td")
	return ldiags
}

// dump emits a note at the statement, once per concrete record: the body dump
// of a def, a parent-class dump inherited by a def, and a multiclass dump run
// once per defm (TGParser.cpp:4816, Record.cpp:3125, TGParser.cpp:363).
func TestDumpNotes(t *testing.T) {
	src := `def op;
class A { string A = "some text"; dag X = (op op); }
def a : A;
dump "The Value of A is: \n" # !repr(a);
multiclass MC { dump "one"; def D; }
defm X : MC;
class Dumper<int n> { dump "n = " # !repr(n); }
multiclass DMC<int m> { def _a : Dumper<m>; }
defm Y : DMC<2>;
`
	diags := lowerDiags(t, src)
	var notes []string
	for _, d := range diags {
		if d.Severity == diag.Error {
			t.Fatalf("unexpected error: %v", d)
		}
		if d.Severity == diag.NoteSev {
			notes = append(notes, d.Message)
		}
	}
	want := []string{
		"The Value of A is: \na {\t// A\n  string A = \"some text\";\n  dag X = (op op);\n}\n",
		"one",
		"n = 2",
	}
	if len(notes) != len(want) {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	for i := range want {
		if notes[i] != want[i] {
			t.Errorf("note %d = %q, want %q", i, notes[i], want[i])
		}
	}
}

// A dump whose message is not a string is a PrintError at the statement
// (Error.cpp:178-186).
func TestDumpNotString(t *testing.T) {
	diags := lowerDiags(t, "def D { int x = 1; dump x; }\n")
	found := false
	for _, d := range diags {
		if d.Severity == diag.Error && strings.Contains(d.Message, "dump value is not of type string") {
			found = true
			if d.Span.Start.Line != 1 {
				t.Errorf("dump error line = %d, want 1", d.Span.Start.Line)
			}
		}
	}
	if !found {
		t.Fatalf("no dump type error: %v", diags)
	}
}

// checkConcrete reports every incomplete value in one def, the way
// TGParser.cpp:99-116 loops over Record::getValues.
func TestCheckConcreteReportsEveryValue(t *testing.T) {
	src := `class A { int x; list<int> xs; }
def D : A {
  list<int> f = !filter(e, [1, 2], x);
  list<int> s = !sort(e, [2, 1], x);
  list<int> fo = !foreach(e, xs, e);
}
`
	diags := lowerDiags(t, src)
	var msgs []string
	for _, d := range diags {
		if d.Severity == diag.Error && strings.Contains(d.Message, "could not be fully resolved") {
			msgs = append(msgs, d.Message)
			if !d.Soft {
				t.Errorf("checkConcrete diagnostic must not abort: %v", d)
			}
		}
	}
	want := []string{
		"Initializer of 'f' in 'D' could not be fully resolved: !filter(e, [1, 2], x)",
		"Initializer of 's' in 'D' could not be fully resolved: !sort(e, [2, 1], x)",
		"Initializer of 'fo' in 'D' could not be fully resolved: !foreach(e, xs, e)",
	}
	if len(msgs) != len(want) {
		t.Fatalf("msgs = %q, want %q", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg %d = %q, want %q", i, msgs[i], want[i])
		}
	}
}
