package record

import (
	"strings"
	"testing"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
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
