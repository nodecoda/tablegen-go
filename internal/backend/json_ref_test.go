package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"tblgen/internal/record"
)

// jsonRef is the original implementation: build a map[string]any tree and let
// encoding/json sort the keys. The streaming encoder must reproduce its bytes
// exactly, including key order and escaping.
func jsonRef(k *record.Keeper) ([]byte, error) {
	defs := k.Defs()
	classes := k.Classes()

	instances := map[string][]any{}
	for _, c := range classes {
		instances[c.Name] = []any{}
	}

	root := map[string]any{
		"!tablegen_json_version": int64(1),
	}
	for _, d := range defs {
		obj := map[string]any{}
		fields := []any{}
		for _, v := range d.Vals {
			val, err := d.Field(v.Name)
			if err != nil {
				return nil, err
			}
			if v.Nonconcrete {
				fields = append(fields, v.Name)
			}
			obj[v.Name] = translateRef(val)
		}
		obj["!fields"] = fields

		supers := d.SuperClasses()
		names := make([]any, 0, len(supers))
		for _, sc := range supers {
			names = append(names, sc.Name)
			instances[sc.Name] = append(instances[sc.Name], d.Name)
		}
		obj["!superclasses"] = names
		obj["!name"] = d.Name
		obj["!anonymous"] = d.IsAnonymous
		locs := make([]any, 0, len(d.Locs))
		for _, loc := range d.Locs {
			locs = append(locs, fmt.Sprintf("%s:%d", filepath.Base(loc.File), loc.Line))
		}
		obj["!locs"] = locs
		root[d.Name] = obj
	}
	instObj := map[string]any{}
	for c, list := range instances {
		instObj[c] = list
	}
	root["!instanceof"] = instObj

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func translateRef(v record.Value) any {
	switch x := v.(type) {
	case *record.Unset:
		return nil
	case *record.Bit:
		if x.B {
			return int64(1)
		}
		return int64(0)
	case *record.Bits:
		out := make([]any, len(x.B))
		for i, bit := range x.B {
			out[i] = translateRef(bit)
		}
		return out
	case *record.Int:
		return x.N
	case *record.Str:
		return x.S
	case *record.List:
		out := make([]any, len(x.Elems))
		for i, e := range x.Elems {
			out[i] = translateRef(e)
		}
		return out
	case *record.Dag:
		obj := map[string]any{
			"printable": x.String(),
			"kind":      "dag",
			"operator":  translateRef(x.Op),
		}
		if x.OpName != "" {
			obj["name"] = x.OpName
		}
		args := make([]any, len(x.Args))
		for i, a := range x.Args {
			pair := []any{translateRef(a.Value), nil}
			if a.Name != "" {
				pair[1] = a.Name
			}
			args[i] = pair
		}
		obj["args"] = args
		return obj
	case *record.BitRef:
		if _, unset := x.Value.(*record.Unset); unset {
			return map[string]any{"printable": x.Var + "{" + itoa(x.Index) + "}", "kind": "varbit", "var": x.Var, "index": int64(x.Index)}
		}
		return translateRef(x.Value)
	case *record.Unresolved:
		switch x.Kind {
		case "var":
			return map[string]any{"printable": x.Text, "kind": "var", "var": x.Var}
		case "varbit":
			return map[string]any{"printable": x.Text, "kind": "varbit", "var": x.Var, "index": int64(x.Index)}
		}
		return map[string]any{"printable": x.Text, "kind": "complex"}
	case *record.DefVal:
		return map[string]any{
			"printable": x.Rec.Name,
			"kind":      "def",
			"def":       x.Rec.Name,
		}
	default:
		return map[string]any{
			"printable": v.String(),
			"kind":      "complex",
		}
	}
}

// TestJSONMatchesMapEncoder locks the streaming encoder to the tree encoder's
// bytes across the value shapes the JSON backend can print: bits, dags with
// named arguments, unresolved operators, anonymous records, and string
// escaping.
func TestJSONMatchesMapEncoder(t *testing.T) {
	sources := []string{
		`def x;`,
		`defvar G = "he said \"hi\"\ttab";
class d;
def e : d;`,
		`def op;
def x;
class C<int n> { dag D = (op 1, x:$n); }
def i : C<7>;`,
		`class C { int n = ?; }
def a : C;
def b : C;`,
		`class S<string s> { string v = s; }
def u : S<"quote\" and \\ backslash">;`,
		`class A<int a> { int x = a; }
def y { int p = A<10>.x; }`,
		`defvar L = [1, 2, 3];
def z;`,
	}
	for i, src := range sources {
		k := setsSrc(t, src)
		want, err := jsonRef(k)
		if err != nil {
			t.Fatalf("source %d: ref: %v", i, err)
		}
		got, err := JSON(k)
		if err != nil {
			t.Fatalf("source %d: stream: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("source %d mismatch\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestAppendJSONStringMatchesEncodingJSON pins the hand-written escaper to
// encoding/json with SetEscapeHTML(false) for the characters that occur in
// TableGen strings.
func TestAppendJSONStringMatchesEncodingJSON(t *testing.T) {
	cases := []string{
		"",
		"plain",
		"quote\" back\\slash",
		"tab\tnewline\nreturn\r nul\x00 bell\x07 del\x7f",
		"high \u00e9 \u4e2d\u6587 \U0001f600",
		"line\u2028sep\u2029par",
		"invalid \xff\xfe bytes",
	}
	for _, c := range cases {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(c); err != nil {
			t.Fatal(err)
		}
		want := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
		got := appendJSONString(nil, c)
		if !bytes.Equal(got, want) {
			t.Fatalf("escape(%q)\n got: %s\nwant: %s", c, got, want)
		}
	}
}
