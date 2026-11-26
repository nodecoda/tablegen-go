// Package backend renders a resolved record graph.
package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"tblgen/internal/record"
)

// JSON emits the same document as llvm-tblgen --dump-json: a compact object
// with sorted keys, version 1, one entry per def, and the !instanceof map.
func JSON(k *record.Keeper) ([]byte, error) {
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
			obj[v.Name] = translate(val)
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

func itoa(n int) string { return strconv.Itoa(n) }

func translate(v record.Value) any {
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
			out[i] = translate(bit)
		}
		return out
	case *record.Int:
		return x.N
	case *record.Str:
		return x.S
	case *record.List:
		out := make([]any, len(x.Elems))
		for i, e := range x.Elems {
			out[i] = translate(e)
		}
		return out
	case *record.Dag:
		obj := map[string]any{
			"printable": x.String(),
			"kind":      "dag",
			"operator":  translate(x.Op),
		}
		if x.OpName != "" {
			obj["name"] = x.OpName
		}
		args := make([]any, len(x.Args))
		for i, a := range x.Args {
			pair := []any{translate(a.Value), nil}
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
		return translate(x.Value)
	case *record.Unresolved:
		// JSONBackend.cpp prints the Init tree that survived resolution: a
		// variable reference, one bit of one, or an unfolded operator.
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
