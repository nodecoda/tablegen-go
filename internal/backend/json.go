// Package backend renders a resolved record graph.
package backend

import (
	"path/filepath"
	"sort"
	"strconv"

	"tblgen/internal/record"
)

// JSON emits the same document as llvm-tblgen --dump-json: a compact object
// with sorted keys, version 1, one entry per def, and the !instanceof map.
//
// The document is streamed in the sorted key order encoding/json would use for
// a map, so no intermediate tree is built. json_ref_test.go holds the old
// map+encoding/json encoder and asserts both produce identical bytes.
func JSON(k *record.Keeper) ([]byte, error) {
	defs := k.Defs()
	classes := k.Classes()

	instances := make(map[string][]string, len(classes))
	for _, c := range classes {
		instances[c.Name] = []string{}
	}
	for _, d := range defs {
		for _, sc := range d.SuperClasses() {
			instances[sc.Name] = append(instances[sc.Name], d.Name)
		}
	}

	w := &jsonBuf{b: make([]byte, 0, 1<<20)}
	w.ch('{')
	first := true
	key := func(k string) {
		if !first {
			w.ch(',')
		}
		first = false
		w.str(k)
		w.ch(':')
	}
	// The two synthetic keys sort among the def names; both start with '!'
	// (0x21), which is below every identifier character, so they only need to
	// be placed before the first def name they precede.
	synthetic := []string{"!instanceof", "!tablegen_json_version"}
	si := 0
	emitSynthetic := func(name string) {
		key(name)
		if name == "!tablegen_json_version" {
			w.num(1)
			return
		}
		names := make([]string, 0, len(instances))
		for n := range instances {
			names = append(names, n)
		}
		sort.Strings(names)
		w.ch('{')
		for i, n := range names {
			if i > 0 {
				w.ch(',')
			}
			w.str(n)
			w.ch(':')
			w.ch('[')
			for j, def := range instances[n] {
				if j > 0 {
					w.ch(',')
				}
				w.str(def)
			}
			w.ch(']')
		}
		w.ch('}')
	}
	for _, d := range defs {
		for si < len(synthetic) && synthetic[si] < d.Name {
			emitSynthetic(synthetic[si])
			si++
		}
		key(d.Name)
		if err := writeJSONDef(w, d); err != nil {
			return nil, err
		}
	}
	for ; si < len(synthetic); si++ {
		emitSynthetic(synthetic[si])
	}
	w.raw("}\n")
	return w.b, nil
}

// writeJSONDef emits one def object. Its keys sort as: the five !-prefixed
// keys, then the field names (no field name can start below '!').
func writeJSONDef(w *jsonBuf, d *record.Record) error {
	type field struct {
		name string
		val  record.Value
	}
	fields := make([]field, 0, len(d.Vals))
	nonconcrete := make([]string, 0, 4)
	for _, v := range d.Vals {
		val, err := d.Field(v.Name)
		if err != nil {
			return err
		}
		if v.Nonconcrete {
			nonconcrete = append(nonconcrete, v.Name)
		}
		fields = append(fields, field{v.Name, val})
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].name < fields[j].name })

	w.ch('{')
	w.raw(`"!anonymous":`)
	w.boolean(d.IsAnonymous)
	w.raw(`,"!fields":`)
	w.ch('[')
	for i, n := range nonconcrete {
		if i > 0 {
			w.ch(',')
		}
		w.str(n)
	}
	w.raw(`],"!locs":[`)
	for i, loc := range d.Locs {
		if i > 0 {
			w.ch(',')
		}
		w.str(filepath.Base(loc.File) + ":" + strconv.Itoa(loc.Line))
	}
	w.raw(`],"!name":`)
	w.str(d.Name)
	w.raw(`,"!superclasses":[`)
	for i, sc := range d.SuperClasses() {
		if i > 0 {
			w.ch(',')
		}
		w.str(sc.Name)
	}
	w.ch(']')
	for _, f := range fields {
		w.ch(',')
		w.str(f.name)
		w.ch(':')
		writeJSONValue(w, f.val)
	}
	w.ch('}')
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
