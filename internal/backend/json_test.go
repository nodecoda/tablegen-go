package backend

import (
	"strings"
	"testing"

	"github.com/nodecoda/tablegen-go/internal/record"
	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// jsonOf is the whole pipeline for one source string: parse, lower, emit.
func jsonOf(t *testing.T, src string) (string, []string) {
	t.Helper()
	file, parseDiags := syntax.Parse("t.td", src, nil, nil)
	var msgs []string
	for _, d := range parseDiags {
		if d.Severity.String() == "error" {
			msgs = append(msgs, d.Message)
		}
	}
	keeper, lowerDiags := record.Lower(file, "t.td")
	for _, d := range lowerDiags {
		if d.Severity.String() == "error" {
			msgs = append(msgs, d.Message)
		}
	}
	out, err := JSON(keeper)
	if err != nil {
		t.Fatalf("emit: %v", err)
	}
	return strings.TrimSpace(string(out)), msgs
}

// These pin the Init shapes llvm-tblgen's JSON backend emits for values that
// never resolve. Each case is copied from a probe run against the C++ oracle;
// changing one of these means the Go output no longer matches --dump-json.
func TestJSONUnresolvedValues(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{
			name: "var reference inside a field",
			src:  "def B { int u; field int c = !add(u, 2); }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":["c"],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"c":{"kind":"complex","printable":"!add(u, 2)"},"u":null}}`,
		},
		{
			name: "bits reference expands to varbits",
			src:  "def B { bits<2> ub; bits<4> x; let x{1...0} = ub{1...0}; }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":[],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"ub":[null,null],"x":[{"index":0,"kind":"varbit","printable":"ub{0}","var":"ub"},{"index":1,"kind":"varbit","printable":"ub{1}","var":"ub"},null,null]}}`,
		},
		{
			name: "list keeps an unresolved element",
			src:  "def B { int u; field list<int> l = [u, 2]; }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":["l"],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"l":[{"kind":"var","printable":"u","var":"u"},2],"u":null}}`,
		},
		{
			name: "implicit cast from int to bit",
			src:  "def B { int u; field bit g = !not(u); }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":["g"],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"g":{"kind":"complex","printable":"!cast<bit>(!not(u))"},"u":null}}`,
		},
		{
			name: "paste with an unset operand",
			src:  "def B { int u; field string s = \"a\" # u; }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":["s"],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"s":{"kind":"complex","printable":"!strconcat(\"a\", !cast<string>(u))"},"u":null}}`,
		},
		{
			name: "fully set bits read as int",
			src:  "def B { bits<2> ub = {1, 0}; field int n = ub; }",
			want: `{"!instanceof":{},"!tablegen_json_version":1,"B":{"!anonymous":false,"!fields":["n"],"!locs":["t.td:1"],"!name":"B","!superclasses":[],"n":2,"ub":[0,1]}}`,
		},
	}
	for _, c := range cases {
		got, msgs := jsonOf(t, c.src)
		if len(msgs) > 0 {
			t.Errorf("%s: unexpected diagnostics: %v", c.name, msgs)
			continue
		}
		if got != c.want {
			t.Errorf("%s: got\n  %s\nwant\n  %s", c.name, got, c.want)
		}
	}
}

// A reference outside `field` must not survive into the record graph.
func TestCheckConcreteRejectsReference(t *testing.T) {
	_, msgs := jsonOf(t, "def D { int u; int b = u; }")
	found := false
	for _, m := range msgs {
		if strings.Contains(m, "could not be fully resolved: u") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a could-not-be-fully-resolved diagnostic, got %v", msgs)
	}
}

// A plain unassigned declaration is concrete (UnsetInit::isConcrete) and must
// stay legal.
func TestCheckConcreteAllowsUnset(t *testing.T) {
	got, msgs := jsonOf(t, "def D { int u; }")
	if len(msgs) > 0 {
		t.Fatalf("unexpected diagnostics: %v", msgs)
	}
	if !strings.Contains(got, `"u":null`) {
		t.Fatalf("want u:null, got %s", got)
	}
}
