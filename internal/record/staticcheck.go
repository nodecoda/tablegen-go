package record

import (
	"tblgen/internal/syntax"
)

// checkValueType mirrors the static half of RecordVal::setValue: when an
// initializer's type is already known (a BitsInit, a TypedInit such as !cond,
// or a variable whose declared type is known), it must be convertible to the
// field's declared type. The value itself is never evaluated here.
//
// This is what rejects `bits<2> x = { 0, 1, 1 }` or `C x = !cond(cc: b, 1: c)`
// while the class is parsed, long before any def instantiates it.
func (k *Keeper) checkValueType(want *Ty, v syntax.Value, self *Record) error {
	return k.checkFieldValue(want, v, self, nil, "")
}

// checkFieldValue is checkValueType with the field name and environment that
// TGParser::SetValue has on hand; both are needed to reproduce its diagnostic.
func (k *Keeper) checkFieldValue(want *Ty, v syntax.Value, self *Record, env *Env, field string) error {
	if want == nil || v == nil || want.Kind == AnyTy {
		return nil
	}
	st := v.Type()
	if st.Kind == syntax.InvalidType || st.Kind == syntax.UnsetType {
		return nil
	}
	got := k.staticTy(st, self)
	if got == nil || got.Kind == AnyTy {
		return nil
	}
	if got.ConvertibleTo(want) {
		return nil
	}
	// TGParser.cpp:328-339 builds this message from three pieces: the field
	// name, the declared type, and the value's own text. A BitsInit has no
	// RecTy to name, so LLVM prints its length instead.
	msg := "Field '" + field + "' of type '" + want.String() +
		"' is incompatible with value '" + initText(v, env, nil) + "'"
	if n, ok := bitsLiteralWidth(v, env); ok {
		return &EvalError{Msg: msg + " of type bit initializer with length " + itoa(n), Span: v.Span()}
	}
	return &EvalError{Msg: msg + " of type '" + got.String() + "'", Span: v.Span()}
}

// bitsLiteralWidth reports whether v is a BitsInit in LLVM's sense (a `{...}`
// literal, a `0b...` literal, or a let-patched bit range) and how many bits it
// holds. Such a value has no RecTy, so it cannot be named in a diagnostic.
func bitsLiteralWidth(v syntax.Value, env *Env) (int, bool) {
	if b, ok := v.(*syntax.BitsValue); ok {
		// `{ opc, opc2 }` is two elements that expand to four bits: LLVM's
		// BitsInit is already flat, so the length is the expanded count.
		if hi, ok := bitListCandidates(b, env, 0); ok && len(hi) > 0 {
			return len(hi), true
		}
		// A 0b literal keeps its raw width even when no elements were
		// materialised; fall back to the value type.
		if b.Type().Kind == syntax.BitsType {
			return b.Type().Bits, true
		}
		return 0, true
	}
	return 0, false
}

// staticTy is tyFromSyntax plus one TableGen rule the syntax type cannot
// express: a reference to a def has the def's direct superclasses as its type
// (Record::getType), so `Base b = D;` works when D : Intermediate : Base but a
// forward reference to D is typed `{}` and rejected.
func (k *Keeper) staticTy(st syntax.Type, self *Record) *Ty {
	switch st.Kind {
	case syntax.ListType:
		var elem *Ty
		if st.Elem != nil {
			elem = k.staticTy(*st.Elem, self)
		}
		return &Ty{Kind: ListTy, Elem: elem}
	case syntax.RecordType:
		if st.IsClass {
			// A class name (type position or Class<...> application) is not a
			// def reference; resolve it in the class namespace.
			return k.tyFromSyntax(st)
		}
		if len(st.Recs) == 1 {
			name := st.Recs[0]
			// A record type names classes: either a class used as a type or
			// the direct superclasses of a def. A def may share its name with
			// a class (generic-tables.td's `class CEnum;` and
			// `def CEnum : GenericEnum`), and the set still means the class.
			if k.Class(name) != nil {
				return k.tyFromSyntax(st)
			}
			var d *Record
			switch {
			case self != nil && !self.IsClass && self.Name == name:
				d = self
			default:
				d = k.defs[name]
			}
			if d != nil && !d.IsClass {
				t := &Ty{Kind: RecordTy}
				for _, sc := range d.Super {
					t.Names = append(t.Names, sc.Name)
					t.Recs = append(t.Recs, sc)
				}
				return t
			}
		}
	}
	return k.tyFromSyntax(st)
}
