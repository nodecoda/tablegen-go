package record

import (
	"strconv"
	"strings"

	"tblgen/internal/syntax"
)

// Value is a fully evaluated TableGen initializer. The set is deliberately
// small: everything the JSON backend can print without consulting the record
// graph. Expression nodes live in expr.go; they produce Values.
type Value interface {
	isValue()
	Type() *Ty
	String() string
}

type Unset struct{}

type Int struct{ N int64 }

type Bit struct{ B bool }

// Bits stores bits least-significant first, matching BitsInit::getBit(i) and
// the order the JSON backend emits. An element is *Bit or *Unset: TableGen
// keeps unresolved bits as data, and JSON prints them as null.
type Bits struct{ B []Value }

type Str struct {
	S    string
	Code bool
}

type List struct {
	Elems []Value
	Elem  *Ty
}

type Dag struct {
	Op     Value
	OpName string
	// OpSrc keeps the operator as written when the dag came from a literal.
	// Record.cpp:914-916 prints the Init tree, so a foldable operator such as
	// !cast<TestInstruction>(NAME) is shown unresolved in a fatal diagnostic.
	// Rendering is deferred to SourceString so the hot path stores a pointer.
	OpSrc syntax.Value
	Args  []DagArg
}

type DagArg struct {
	Value Value
	Name  string
	// HasName distinguishes an empty StringInit from a missing name:
	// DagInit::getAsString prints `:$` for every non-null ArgNames entry,
	// including one whose value is the empty string (Record.cpp:2719-2724).
	HasName bool
}

// BitRef is VarBitInit: one bit of another field, kept as data. LLVM never
// inlines an unset bit, so a bits<N> that references another bits<N> prints
// `{ ub{1}, ub{0} }` and keeps the reference in the JSON as a "varbit". Value
// is the referenced bit as it stands now (*Bit once it has been assigned,
// *Unset while it is still unknown), which is what a scalar read resolves to.
type BitRef struct {
	Var   string
	Index int
	Value Value
}

type DefVal struct{ Rec *Record }

// Unresolved is a value llvm-tblgen keeps as an Init tree instead of folding
// it to a concrete value: a reference to a field that was never assigned
// (VarInit), one bit of such a field (VarBitInit), or an operator with an
// unresolved operand (a "complex" Init). Keeping it as data rather than an
// error is what lets `field int x = u;` survive to --dump-json, while the
// concreteness check still rejects the same value outside a `field`.
type Unresolved struct {
	Text  string // TableGen printing, e.g. "!add(u, 2)"
	Kind  string // "", "var", "varbit"
	Var   string // base name for var / varbit
	Index int    // bit index for varbit
	Width int    // declared bit width when Var names a bits<N>
	Ty    *Ty    // static type, when the Init has one (VarInit does)
}

func (*Unset) isValue()      {}
func (*Int) isValue()        {}
func (*Bit) isValue()        {}
func (*Bits) isValue()       {}
func (*Str) isValue()        {}
func (*List) isValue()       {}
func (*Dag) isValue()        {}
func (*DefVal) isValue()     {}
func (*Unresolved) isValue() {}
func (*BitRef) isValue()     {}

// Scalar values have a fixed type, so they hand out shared immutable Ty
// values: Type() is on the hot path of every conversion check.
var (
	anyTy    = &Ty{Kind: AnyTy}
	anyRecTy = &Ty{Kind: RecordTy}
	bitTy    = &Ty{Kind: BitTy}
	intTy    = &Ty{Kind: IntTy}
	stringTy = &Ty{Kind: StringTy}
	codeTy   = &Ty{Kind: CodeTy}
	dagTy    = &Ty{Kind: DagTy}
)

func (*Unset) Type() *Ty  { return anyTy }
func (*Int) Type() *Ty    { return intTy }
func (*Bit) Type() *Ty    { return bitTy }
func (v *Bits) Type() *Ty { return &Ty{Kind: BitsTy, Bits: len(v.B)} }
func (v *Str) Type() *Ty {
	if v.Code {
		return codeTy
	}
	return stringTy
}
func (v *List) Type() *Ty {
	elem := v.Elem
	if elem == nil {
		elem = commonElem(v.Elems)
	}
	return &Ty{Kind: ListTy, Elem: elem}
}
func (*Dag) Type() *Ty      { return dagTy }
func (v *DefVal) Type() *Ty { return v.Rec.Type() }

// Type is what the Init's static type would be; unknown when the reference was
// never given one.
func (v *Unresolved) Type() *Ty {
	if v.Ty != nil {
		return v.Ty
	}
	return &Ty{Kind: AnyTy}
}

func commonElem(elems []Value) *Ty {
	if len(elems) == 0 {
		return nil
	}
	t := elems[0].Type()
	for _, e := range elems[1:] {
		if !t.Equal(e.Type()) {
			return &Ty{Kind: AnyTy}
		}
	}
	return t
}

func (*Unset) String() string { return "?" }
func (v *Int) String() string { return strconv.FormatInt(v.N, 10) }
func (v *Bit) String() string {
	if v.B {
		return "1"
	}
	return "0"
}

func (v *Bits) String() string {
	parts := make([]string, len(v.B))
	for i := range v.B {
		parts[i] = v.B[len(v.B)-1-i].String()
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

func (v *Str) String() string {
	if v.Code {
		return "[{" + v.S + "}]"
	}
	// StringInit::getAsString wraps the value in quotes without escaping it,
	// so a tab in the source prints as a tab and a backslash prints once.
	return `"` + v.S + `"`
}

func (v *List) String() string {
	parts := make([]string, len(v.Elems))
	for i, e := range v.Elems {
		parts[i] = e.String()
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func (v *Dag) String() string {
	return v.render(v.Op.String())
}

// SourceString renders the dag with the operator text as written, for
// diagnostics that print an Init tree before it has been folded.
func (v *Dag) SourceString() string {
	op := v.Op.String()
	if v.OpSrc != nil {
		op = syntax.InitText(v.OpSrc)
	}
	return v.render(op)
}

func (v *Dag) render(op string) string {
	var b strings.Builder
	b.WriteByte('(')
	b.WriteString(op)
	if v.OpName != "" {
		b.WriteString(":$")
		b.WriteString(v.OpName)
	}
	for i, a := range v.Args {
		if i == 0 {
			b.WriteByte(' ')
		} else {
			b.WriteString(", ")
		}
		b.WriteString(a.Value.String())
		if a.HasName {
			b.WriteString(":$")
			b.WriteString(a.Name)
		}
	}
	b.WriteByte(')')
	return b.String()
}

func (v *DefVal) String() string { return v.Rec.Name }

func (v *Unresolved) String() string { return v.Text }

// Type is the referenced bit's type; the value itself is not folded yet.
func (v *BitRef) Type() *Ty { return &Ty{Kind: BitTy} }

// String prints VarBitInit::getAsString. A reference whose bit has been
// assigned prints that bit instead, which is what resolution leaves behind.
func (v *BitRef) String() string {
	if b, ok := v.Value.(*Bit); ok {
		return b.String()
	}
	return v.Var + "{" + itoa(v.Index) + "}"
}

// bitsFromInt produces bits<N> from an integer, least-significant bit first.
// ok mirrors IntInit::convertInitializerTo(BitsRecTy): a value fits when
// canFitInBitfield says so, so bits<4> accepts [-8, 15].
func bitsFromInt(n int64, width int) (Bits, bool) {
	if width == 0 {
		return Bits{B: []Value{}}, n == 0
	}
	if width < 64 && n>>uint(width) != 0 && n>>uint(width-1) != -1 {
		return Bits{}, false
	}
	out := Bits{B: make([]Value, width)}
	for i := 0; i < width; i++ {
		// LLVM masks off bits past 63, so a wide negative value leaves its
		// upper bits cleared rather than sign-extended.
		v := i < 64 && n&(int64(1)<<uint(i)) != 0
		out.B[i] = &Bit{B: v}
	}
	return out, true
}

// bitsToInt converts only when every bit is resolved; Unset bits make the
// value non-convertible, matching BitsInit::convertInitializerToInt.
func bitsToInt(b Bits) (int64, bool) {
	var u uint64
	for i, raw := range b.B {
		v := unwrapBitRef(raw)
		bit, ok := v.(*Bit)
		if !ok {
			return 0, false
		}
		if i < 64 && bit.B {
			u |= 1 << uint(i)
		}
	}
	return int64(u), true
}
