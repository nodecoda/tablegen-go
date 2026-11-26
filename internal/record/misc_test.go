package record_test

import (
	"strings"
	"testing"

	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

// parseOnly parses and returns the AST plus the parse diagnostics.
func parseOnly(t *testing.T, src string) (*syntax.File, []diag.Diagnostic) {
	t.Helper()
	f, d := syntax.Parse("t.td", src, nil, nil)
	for _, x := range d {
		if x.Severity == diag.Error {
			t.Fatalf("parse: %v", d)
		}
	}
	return f, d
}

// Top-level assertions run while parsing (TGParser::addEntry ->
// CheckAssert), not at record completion.
func TestTopLevelAssert(t *testing.T) {
	diags := lowerDiags(t, "assert 1, \"ok\";\nassert 0, \"boom\";\n")
	var msgs []string
	for _, d := range diags {
		if d.Severity == diag.Error {
			msgs = append(msgs, d.Message)
		}
	}
	if len(msgs) != 1 || msgs[0] != "assertion failed: boom" {
		t.Fatalf("messages = %q", msgs)
	}
}

// A class-body assertion is copied to every def that inherits the class and
// rebound to the caller's template arguments (appendAssertions).
func TestClassAssertionRebind(t *testing.T) {
	diags := lowerDiags(t, `class Chk<int n> { assert !eq(n, 1), "bad"; }
def A : Chk<1>;
def B : Chk<2>;
`)
	var msgs []string
	for _, d := range diags {
		if d.Severity == diag.Error {
			msgs = append(msgs, d.Message)
		}
	}
	want := []string{"assertion failed: bad", "assertion failed in this record"}
	if len(msgs) != len(want) {
		t.Fatalf("messages = %q, want %q", msgs, want)
	}
	for i := range want {
		if msgs[i] != want[i] {
			t.Errorf("msg %d = %q, want %q", i, msgs[i], want[i])
		}
	}
}

// LowerNames resolves record names and loop-generated names without evaluating
// values; LoopDefs then reports the defs each statement-level foreach created.
func TestLowerNamesAndLoopDefs(t *testing.T) {
	src := `def A;
foreach i = 1...2 in def N#i;
`
	f, _ := parseOnly(t, src)
	k, diags := record.LowerNames(f, "t.td")
	for _, d := range diags {
		if d.Severity == diag.Error {
			t.Fatalf("LowerNames: %v", diags)
		}
	}
	if k.Def("N1") == nil || k.Def("N2") == nil {
		t.Fatalf("loop defs not named: %v", k.Defs())
	}
	var names []string
	for _, defs := range k.LoopDefs() {
		for _, d := range defs {
			names = append(names, d.Name)
		}
	}
	got := strings.Join(names, ",")
	if got != "N1,N2" {
		t.Errorf("LoopDefs = %q, want N1,N2", got)
	}
}

// A body-level `let` re-stamps the field location (OverrideDefLoc,
// TGParser.cpp:326), which is what the detailed records report prints.
func TestLetOverridesFieldLocation(t *testing.T) {
	src := `def D {
  int x = 1;
  let x = 2;
}
`
	f, _ := parseOnly(t, src)
	k, diags := record.Lower(f, "t.td")
	for _, d := range diags {
		if d.Severity == diag.Error {
			t.Fatalf("lower: %v", diags)
		}
	}
	rv := k.Def("D").Val("x")
	if rv == nil || rv.Loc.Line != 3 {
		t.Fatalf("field loc = %+v, want line 3", rv)
	}
}
