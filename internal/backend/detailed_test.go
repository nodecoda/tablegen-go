package backend

import (
	"strings"
	"testing"
)

// TestDetailedReport is the --print-detailed-records report for a small file.
// The expected text is llvm-tblgen's output up to (and including) the Memory
// Allocation Stats heading; the allocator numbers after it are LLVM internals
// that this implementation deliberately does not emit.
func TestDetailedReport(t *testing.T) {
	k := setsSrc(t, `defvar Greeting = "Hello there.";
defvar Answer = 42;

class Base {
  int b = 1;
}

class Derived<int Arg> : Base {
  int d = Arg;
}

def Rec : Derived<7>;

def : Base;
`)
	out, err := Detailed(k)
	if err != nil {
		t.Fatal(err)
	}
	want := `DETAILED RECORDS for file t.td

-------------------- Global Variables (2) --------------------

Answer = 42
Greeting = "Hello there."

-------------------- Classes (2) --------------------

Base  |t.td:4|
  Template args: (none)
  Superclasses: (none)
  Fields:
    int b = 1  |t.td:5|

Derived  |t.td:8|
  Template args:
    int Derived:Arg = ?  |t.td:8|
  Superclasses: Base
  Fields:
    int b = 1  |t.td:5|
    int d = Derived:Arg  |t.td:9|

-------------------- Records (2) --------------------

Rec  |t.td:12|
  Superclasses: (Base) Derived
  Fields:
    int b = 1  |t.td:5|
    int d = 7  |t.td:9|

anonymous_0  |t.td:14|
  Superclasses: Base
  Fields:
    int b = 1  |t.td:5|

-------------------- Memory Allocation Stats --------------------
`
	if string(out) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out, want)
	}
}

// TestDetailedEmptySections covers the (none) rendering and the empty global
// section, which still prints a blank line after its heading.
func TestDetailedEmptySections(t *testing.T) {
	k := setsSrc(t, "def x;\n")
	out, err := Detailed(k)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"-------------------- Global Variables (0) --------------------\n\n\n",
		"-------------------- Classes (0) --------------------\n",
		"\nx  |t.td:1|\n  Superclasses: (none)\n  Fields: (none)\n",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}
