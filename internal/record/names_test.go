package record_test

import (
	"testing"

	"tblgen/internal/diag"
	"tblgen/internal/record"
	"tblgen/internal/syntax"
)

// A statement-level foreach inside a top-level `let ... in { }` block still
// creates records the parser must know for later statements
// (TGParser.cpp:426-430). LowerNames learns them without instantiating the
// parents, so a `let Field` that names a class field must not abort the name
// pass.
func TestLowerNamesLearnsDynamicNamesUnderLet(t *testing.T) {
	src := `class Op;
def add : Op;
class RC<dag l> { dag L = l; int Q = 0; }
let Q = 5 in {
  foreach I = 1-2 in {
    defvar Idx = !shl(I, 1);
    defvar IdxP1 = !add(Idx, 1);
    def "P" # Idx # "_" # IdxP1 : RC<(add)>;
  }
}
def GP : RC<(add P2_3)>;
`
	// The first parse reports the names it cannot know yet; that is exactly
	// what makes the driver lower once and parse again.
	file, _ := syntax.Parse("t.td", src, nil, nil)
	k, _ := record.LowerNames(file, "t.td")
	names := map[string]bool{}
	for _, defs := range k.LoopDefs() {
		for _, d := range defs {
			names[d.Name] = true
		}
	}
	for _, want := range []string{"P2_3", "P4_5"} {
		if !names[want] {
			t.Errorf("LowerNames did not learn %q (got %v)", want, names)
		}
	}
}

// The instance-site chain on a generated record follows TGParser::resolve:
// a loop that is unrolled while the multiclass is parsed keeps every site, one
// that only becomes concrete after substituting the defm's arguments drops
// that defm's site, and one that stays unresolved loses all of them
// (TGParser.cpp:511, 561).
func TestLoopInstanceLocs(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []int32
	}{
		{
			// Inner's foreach unrolls at the Inner<["a"]> call: the loop
			// drops that site (line 8) but the enclosing defm (line 10) is
			// appended when the record becomes its entry.
			name: "deferred at the inner call",
			src: `class Pat;
multiclass Inner<list<string> L> {
  foreach x = L in {
    def : Pat;
  }
}
multiclass Outer {
  defm A : Inner<["a"]>;
}
defm Y : Outer;
`,
			want: []int32{4, 10},
		},
		{
			// A literal list unrolls while the multiclass definition is
			// parsed, so no site is dropped.
			name: "literal list",
			src: `class Pat;
multiclass Inner<list<string> L> {
  foreach x = ["a"] in {
    def : Pat;
  }
}
multiclass Outer {
  defm A : Inner<["a"]>;
}
defm Y : Outer;
`,
			want: []int32{4, 8, 10},
		},
		{
			// The condition names a template argument of the enclosing
			// multiclass, so Inst defers the if to Outer's resolve and the
			// record keeps only the alias defm's own site.
			name: "deferred to the outer defm",
			src: `class Pat;
multiclass Alias { def : Pat; }
multiclass Inst<string mnemonic> {
  if !eq(mnemonic, "x") then {
    defm : Alias;
  }
}
multiclass Multi<string m> {
  defm _H : Inst<m>;
}
defm Y : Multi<"x">;
`,
			want: []int32{2, 5},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file, pdiags := syntax.Parse("t.td", c.src, nil, nil)
			for _, d := range pdiags {
				if d.Severity == diag.Error {
					t.Fatalf("parse: %v", pdiags)
				}
			}
			k, ldiags := record.Lower(file, "t.td")
			for _, d := range ldiags {
				if d.Severity == diag.Error {
					t.Fatalf("lower: %v", ldiags)
				}
			}
			rec := k.Def("anonymous_0")
			if rec == nil {
				t.Fatalf("no anonymous_0")
			}
			if len(rec.Locs) != len(c.want) {
				t.Fatalf("locs = %v, want lines %v", rec.Locs, c.want)
			}
			for i, p := range rec.Locs {
				if int32(p.Line) != c.want[i] {
					t.Fatalf("locs = %v, want lines %v", rec.Locs, c.want)
				}
			}
		})
	}
}
