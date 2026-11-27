package record

import (
	"strings"

	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// TyKind is the record-level type tag. It mirrors RecTyKind in
// llvm/include/llvm/TableGen/Record.h (Bit/Bits/Int/String/List/Dag/Record);
// code is a StringInit format, not a separate RecTy, but keeping the tag makes
// diagnostics and the JSON path simpler.
type TyKind uint8

const (
	AnyTy TyKind = iota
	BitTy
	BitsTy
	IntTy
	StringTy
	CodeTy
	DagTy
	ListTy
	RecordTy
)

// Ty is a pure-data record type. Record types keep both the class records and
// their names: the names are usable before the class table is complete, the
// pointers make subtype checks cheap.
type Ty struct {
	Kind  TyKind
	Bits  int
	Elem  *Ty
	Recs  []*Record
	Names []string
}

func (t *Ty) String() string {
	if t == nil {
		return "any"
	}
	switch t.Kind {
	case BitTy:
		return "bit"
	case BitsTy:
		return "bits<" + itoa(t.Bits) + ">"
	case IntTy:
		return "int"
	case StringTy:
		return "string"
	case CodeTy:
		return "code"
	case DagTy:
		return "dag"
	case ListTy:
		if t.Elem == nil {
			return "list<?>"
		}
		return "list<" + t.Elem.String() + ">"
	case RecordTy:
		names := t.recordNames()
		if len(names) == 1 {
			return names[0]
		}
		return "{" + strings.Join(names, ", ") + "}"
	default:
		return "any"
	}
}

// Equal is structural equality. Record types compare by class identity; when a
// class is not resolved yet, names are the fallback.
func (t *Ty) Equal(u *Ty) bool {
	if t == nil || u == nil {
		return t == u
	}
	if t.Kind != u.Kind {
		return false
	}
	switch t.Kind {
	case BitsTy:
		return t.Bits == u.Bits
	case ListTy:
		if t.Elem == nil || u.Elem == nil {
			return t.Elem == u.Elem
		}
		return t.Elem.Equal(u.Elem)
	case RecordTy:
		return sameRecordSet(t, u)
	}
	return true
}

func sameRecordSet(a, b *Ty) bool {
	if len(a.Recs) != 0 && len(b.Recs) != 0 {
		if len(a.Recs) != len(b.Recs) {
			return false
		}
		for i := range a.Recs {
			if a.Recs[i] != b.Recs[i] {
				return false
			}
		}
		return true
	}
	an, bn := a.recordNames(), b.recordNames()
	if len(an) != len(bn) {
		return false
	}
	for i := range an {
		if an[i] != bn[i] {
			return false
		}
	}
	return true
}

func (t *Ty) recordNames() []string {
	if len(t.Names) > 0 {
		return t.Names
	}
	out := make([]string, 0, len(t.Recs))
	for _, r := range t.Recs {
		out = append(out, r.Name)
	}
	return out
}

// IsA reports whether every value of type t is also a value of type u. It is
// the subset relation, not the conversion relation: bit is not int, bits<8> is
// not int.
func (t *Ty) IsA(u *Ty) bool {
	if t == nil || u == nil || u.Kind == AnyTy {
		return true
	}
	if t.Equal(u) {
		return true
	}
	if t.Kind == RecordTy && u.Kind == RecordTy {
		for _, want := range u.recordNames() {
			if !recordSetContains(t, want) {
				return false
			}
		}
		return true
	}
	if t.Kind == ListTy && u.Kind == ListTy && t.Elem != nil && u.Elem != nil {
		return t.Elem.IsA(u.Elem)
	}
	return false
}

func recordSetContains(t *Ty, name string) bool {
	for _, n := range t.recordNames() {
		if n == name {
			return true
		}
	}
	for _, r := range t.Recs {
		if r != nil && r.IsSubClassOf(name) {
			return true
		}
	}
	return false
}

// ConvertibleTo is RecTy::typeIsConvertibleTo: which implicit conversions
// TableGen accepts when assigning a value to a typed field.
func (t *Ty) ConvertibleTo(u *Ty) bool {
	if t == nil || u == nil || u.Kind == AnyTy || t.Kind == AnyTy {
		// AnyTy is this port's stand-in for "not inferred"; LLVM has no such
		// RecTy, so being permissive here cannot hide a real LLVM error.
		return true
	}
	if t.IsA(u) {
		return true
	}
	switch u.Kind {
	case IntTy:
		return t.Kind == BitTy || t.Kind == BitsTy
	case BitTy:
		return t.Kind == IntTy || (t.Kind == BitsTy && t.Bits == 1)
	case BitsTy:
		return t.Kind == IntTy || t.Kind == BitTy
	case StringTy, CodeTy:
		return t.Kind == StringTy || t.Kind == CodeTy
	case ListTy:
		return t.Kind == ListTy && (t.Elem == nil || u.Elem == nil || t.Elem.ConvertibleTo(u.Elem))
	case DagTy:
		return t.Kind == DagTy
	case RecordTy:
		// RecordRecTy::typeIsConvertibleTo == typeIsA: only subclasses convert.
		return false
	}
	return false
}

// Small, immutable types repeat for every field and every template argument,
// so they are shared instead of allocated per conversion. The arrays are
// built once, which keeps tyFromSyntax read-only.
var (
	bitsTys = func() [129]*Ty {
		var a [129]*Ty
		for i := range a {
			a[i] = &Ty{Kind: BitsTy, Bits: i}
		}
		return a
	}()
	// listOfKind[i] is list<scalar> for the scalar kind with value i; entries
	// for composite kinds stay nil and take the allocating path.
	listOfKind = func() [64]*Ty {
		var a [64]*Ty
		for _, k := range []syntax.TypeKind{syntax.IntType, syntax.BitType,
			syntax.StringType, syntax.CodeType, syntax.DagType} {
			a[int(k)] = &Ty{Kind: ListTy, Elem: scalarTy(k)}
		}
		return a
	}()
	listOfUnset = &Ty{Kind: ListTy}
)

func scalarTy(k syntax.TypeKind) *Ty {
	switch k {
	case syntax.IntType:
		return intTy
	case syntax.BitType:
		return bitTy
	case syntax.StringType:
		return stringTy
	case syntax.CodeType:
		return codeTy
	case syntax.DagType:
		return dagTy
	}
	return anyTy
}

// recordTypeKey identifies a record-type spelling. The names are kept in the
// parser's order: Equality falls back to them, and `{A, B}` is not the same
// spelling as `{B, A}` in a diagnostic.
func recordTypeKey(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names, "\x00")
}

// tyFromSyntax converts a parser Type into a record Ty, resolving class names
// through the keeper when possible.
func (k *Keeper) tyFromSyntax(st syntax.Type) *Ty {
	switch st.Kind {
	case syntax.IntType:
		return intTy
	case syntax.BitType:
		return bitTy
	case syntax.BitsType:
		if st.Bits >= 0 && st.Bits < len(bitsTys) {
			return bitsTys[st.Bits]
		}
		return &Ty{Kind: BitsTy, Bits: st.Bits}
	case syntax.StringType:
		return stringTy
	case syntax.CodeType:
		return codeTy
	case syntax.DagType:
		return dagTy
	case syntax.ListType:
		if st.Elem == nil {
			return listOfUnset
		}
		if int(st.Elem.Kind) < len(listOfKind) && listOfKind[st.Elem.Kind] != nil {
			return listOfKind[st.Elem.Kind]
		}
		return &Ty{Kind: ListTy, Elem: k.tyFromSyntax(*st.Elem)}
	case syntax.RecordType:
		// A record type repeats for every field and for every def that names
		// the class as a parent: dag-isel-subregs.td converts about 1.5M of
		// them. Intern the type once all names resolve. A type built while a
		// class is still undefined must not enter the cache: its Recs would
		// stay empty even after the class table is complete.
		key := recordTypeKey(st.Recs)
		if k.recTys != nil {
			if t := k.recTys[key]; t != nil {
				return t
			}
		}
		t := &Ty{
			Kind:  RecordTy,
			Names: make([]string, 0, len(st.Recs)),
			Recs:  make([]*Record, 0, len(st.Recs)),
		}
		resolved := true
		for _, name := range st.Recs {
			t.Names = append(t.Names, name)
			if r := k.Class(name); r != nil {
				t.Recs = append(t.Recs, r)
			} else {
				resolved = false
			}
		}
		if resolved {
			if k.recTys == nil {
				k.recTys = make(map[string]*Ty, 8)
			}
			k.recTys[key] = t
		}
		return t
	default:
		return anyTy
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// sameRecTy is RecTy::typeIsA for the record phase. LLVM has no CodeRecTy:
// `code` is a StringInit format, so string and code are the same type there,
// and list<code> typeIsA list<string>.
func sameRecTy(a, b *Ty) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind == StringTy || a.Kind == CodeTy {
		return b.Kind == StringTy || b.Kind == CodeTy
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case BitsTy:
		return a.Bits == b.Bits
	case ListTy:
		return sameRecTy(a.Elem, b.Elem)
	case RecordTy:
		return a.Equal(b)
	}
	return true
}

// syntaxTypeOf is the inverse of tyFromSyntax, for a synthesized cast.
func syntaxTypeOf(t *Ty) syntax.Type {
	if t == nil {
		return syntax.Type{Kind: syntax.InvalidType}
	}
	switch t.Kind {
	case BitTy:
		return syntax.Type{Kind: syntax.BitType}
	case BitsTy:
		return syntax.Type{Kind: syntax.BitsType, Bits: t.Bits}
	case IntTy:
		return syntax.Type{Kind: syntax.IntType}
	case StringTy:
		return syntax.Type{Kind: syntax.StringType}
	case CodeTy:
		return syntax.Type{Kind: syntax.CodeType}
	case DagTy:
		return syntax.Type{Kind: syntax.DagType}
	case ListTy:
		out := syntax.Type{Kind: syntax.ListType}
		if t.Elem != nil {
			e := syntaxTypeOf(t.Elem)
			out.Elem = &e
		}
		return out
	case RecordTy:
		return syntax.Type{Kind: syntax.RecordType, Recs: t.recordNames()}
	}
	return syntax.Type{Kind: syntax.InvalidType}
}
