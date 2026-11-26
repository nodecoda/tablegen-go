package record

import (
	"strconv"
	"strings"

	"tblgen/internal/syntax"
)

// InitText renders a parsed expression the way Init::getAsString does. Class
// bodies are never resolved, so `--print-records` shows their initialisers as
// Init trees: `P<0: n>` for a class application, `!add(n, 1)` for an operator,
// `{ b{3}, 1 }` for a bit list, and so on.
//
// want is the declared type of the destination. TGParser casts every
// initialiser to it while parsing, which is visible in the text: an int field
// initialised with `b{2}` prints as `!cast<int>({ b{2} })`, and a bits<4>
// field initialised with an int prints one `!cast<bits<4>>(ii){i}` per bit.

// initText renders v with the conversion the parser would apply for want.
func initText(v syntax.Value, env *Env, want *Ty) string {
	if v == nil {
		return unsetText(want)
	}
	if want != nil {
		src := v.Type()
		switch want.Kind {
		case BitsTy:
			if s, ok := bitBraces(v, env, want.Bits); ok {
				return s
			}
		case IntTy:
			if src.Kind == syntax.BitType {
				// BitRecTy is convertible to IntRecTy only through !cast;
				// a bit is not an IntInit.
				return "!cast<int>(" + rawText(v, env) + ")"
			}
			if src.Kind == syntax.BitsType && src.Bits == 1 {
				return "!cast<int>(" + bitsInitForm(v, env) + ")"
			}
		case BitTy:
			// IntInit::convertInitializerTo accepts exactly 0 and 1; anything
			// else keeps the cast node. `true`/`false` are IntInits too, so a
			// default `bit b = true` prints as `1`.
			if n, ok := constIntV(v, env); ok && (n == 0 || n == 1) {
				return strconv.FormatInt(n, 10)
			}
			if src.Kind == syntax.IntType {
				return "!cast<bit>(" + rawText(v, env) + ")"
			}
			if src.Kind == syntax.BitsType && src.Bits == 1 {
				// A BitsInit of one bit converts by taking that bit; only a
				// variable/operator reference keeps the cast.
				if isBitsLiteral(v) {
					if bits, ok := bitTexts(v, env, 1, 0); ok {
						return bits[0]
					}
				}
				return "!cast<bit>(" + bitsInitForm(v, env) + ")"
			}
		}
	}
	return rawText(v, env)
}

// bitBraces renders v as the BitsInit RecordVal::setValue stores for a
// bits<N> destination: the value is cast to bits<N> first (an int that does
// not fit keeps a !cast node), and anything that is not already a BitsInit is
// expanded one VarBitInit per bit.
func bitBraces(v syntax.Value, env *Env, n int) (string, bool) {
	bits, ok := bitTexts(v, env, n, 0)
	if !ok {
		return "", false
	}
	// BitsInit stores bits least significant first and prints them reversed.
	return "{ " + strings.Join(reverseBits(bits), ", ") + " }", true
}

// isBitsLiteral reports whether v is stored as a BitsInit: a `{ ... }` bit
// list or a bit slice.
func isBitsLiteral(v syntax.Value) bool {
	switch v.(type) {
	case *syntax.BitsValue, *syntax.BitRangeValue:
		return true
	}
	return false
}

// fitsBits is IntInit::convertInitializerTo's canFitInBitfield gate.
func fitsBits(n int64, w int) bool {
	if w >= 64 {
		return true
	}
	if w <= 0 {
		return n == 0
	}
	return (n >= 0 && n>>uint(w) == 0) || (n < 0 && n>>uint(w-1) == -1)
}

// constBits renders the low w bits of n, least significant first.
func constBits(n int64, w int) []string {
	out := make([]string, w)
	for i := 0; i < w; i++ {
		v := int64(0)
		if i < 64 {
			v = (n >> uint(i)) & 1
		}
		out[i] = strconv.FormatInt(v, 10)
	}
	return out
}

// castBits is the Init tree of `!cast<bits<w>>(expr)`: one VarBitInit per bit.
func castBits(expr string, w int) []string {
	cast := "!cast<bits<" + strconv.Itoa(w) + ">>(" + expr + ")"
	out := make([]string, w)
	for i := range out {
		out[i] = cast + "{" + strconv.Itoa(i) + "}"
	}
	return out
}

// resolveVar mirrors rawText's VarValue case. It returns either the reference
// text to print or the Init a binding substitutes for the name (a caller's
// argument, or a defvar literal), which is what the Init tree actually holds.
func resolveVar(x *syntax.VarValue, env *Env) (string, syntax.Value, *Env) {
	if x.Subst && x.Lit != nil {
		return "", x.Lit, env
	}
	if x.GlobalRef {
		// TGParser::ParseIDValue returns the Init stored for a global
		// (RecordKeeper::getGlobal), so a reference to a defvar is the
		// defvar's value, not a VarInit naming it. A def keeps its name: the
		// DefInit prints as the record reference. See TGParser.cpp:1180-1216.
		if x.Lit != nil {
			return "", x.Lit, env
		}
		return x.Name, nil, nil
	}
	if x.Lexical {
		// Loop variables (and other lexical bindings) print bare: they are not
		// class fields, so TGParser never qualified them.
		return x.Name, nil, nil
	}
	q := x.ArgQual
	if q == "" {
		var ok bool
		q, _, ok = templateArgKey(env, x.Name, x.ArgRef)
		if !ok {
			q = ""
		}
	}
	if q != "" {
		if b, ok := env.Lookup(q); ok {
			// Dynamic bindings are a class's own template arguments, left as
			// VarInits for the concrete instantiation. A binding installed by
			// AddParentClass for another record's application is already the
			// caller's Init in the tree and must be substituted here.
			// A dynamic binding is the class's own argument; it stays a
			// VarInit unless its value is already complete, in which case
			// TGParser's AddParentClass would have substituted it (a defaulted
			// argument prints as its default, not as `Class:arg`).
			if b.V != nil && (!b.Dynamic || b.Owner != env.owner || b.Mapped) {
				return "", b.V, env.StackOn(b.Env)
			}
			if b.Val != nil {
				return printableOf(b.Val), nil, nil
			}
		}
		return q, nil, nil
	}
	// An unbound name inside a record body prints qualified: LLVM stores it
	// as a VarInit whose name was QualifyName'd by the owning record
	// (`Foo:NAME`), which is how --print-records shows an unresolved name.
	// Names that do resolve -- a lambda variable or a field of the same
	// record -- print bare, so check both before qualifying.
	if env != nil {
		if _, ok := env.Lookup(x.Name); ok {
			return x.Name, nil, nil
		}
		if env.owner != nil && env.owner.hasVal(x.Name) {
			return x.Name, nil, nil
		}
		if env.owner != nil {
			sep := env.ownerSep
			if sep == "" {
				sep = ":"
			}
			owner := env.owner.Name
			if x.Name == "NAME" && env.implicitTo != "" {
				owner = env.implicitTo
			}
			return owner + sep + x.Name, nil, nil
		}
	}
	return x.Name, nil, nil
}

// bitTexts renders v as the BitsInit RecordVal::setValue stores for a bits<w>
// destination, one text per bit, least significant first. ok=false means the
// value's width or kind is not statically known here.
func bitTexts(v syntax.Value, env *Env, w int, depth int) ([]string, bool) {
	if depth > 32 {
		return nil, false
	}
	switch x := v.(type) {
	case nil:
		return unsetBits(w), true
	case *syntax.UnsetValue:
		return unsetBits(w), true
	case *syntax.IntValue:
		if fitsBits(int64(x.Num), w) {
			return constBits(int64(x.Num), w), true
		}
		return castBits(strconv.FormatInt(int64(x.Num), 10), w), true
	case *syntax.BangValue:
		if n, ok := foldIntV(x, env, depth); ok {
			if fitsBits(n, w) {
				return constBits(n, w), true
			}
			return castBits(strconv.FormatInt(n, 10), w), true
		}
		// An operator that is already bits<N> is a BitsInit-like Init of
		// width N; its bits are VarBitInits of the operator itself.
		if bits, ok := typedBits(rawText(v, env), x.Type(), w); ok {
			return bits, true
		}
		return castBits(rawText(v, env), w), true
	case *syntax.VarValue:
		text, sub, subEnv := resolveVar(x, env)
		if sub != nil {
			if bits, ok := bitTexts(sub, subEnv, w, depth+1); ok {
				return bits, true
			}
			// A substituted value that is not itself a bits tree falls back to
			// its printed form: that is what a VarInit of it would show.
			return fillBits(rawText(sub, subEnv), w), true
		}
		return varBits(text, x.Type(), w)
	case *syntax.FieldValue:
		if x.Lit != nil && concreteInit(x.Lit) && fieldBaseIsDef(x.Base) {
			return bitTexts(x.Lit, env, w, depth+1)
		}
		text := rawText(v, env)
		return varBits(text, x.Type(), w)
	case *syntax.BitRangeValue:
		if len(x.Bits) != w {
			return nil, false
		}
		out := make([]string, w)
		for i := 0; i < w; i++ {
			bit := x.Bits[w-1-i]
			out[i] = rawText(x.Base, env) + "{" + strconv.FormatInt(int64(bit), 10) + "}"
		}
		return out, true
	case *syntax.BitsValue:
		hi, ok := bitListCandidates(x, env, depth)
		if !ok || len(hi) != w {
			return nil, false
		}
		return reverseBits(hi), true
	}
	return nil, false
}

// typedBits is TypedInit::getBit for an expression that is not a reference:
// a bits<N> operator keeps one VarBitInit per bit, a bit is itself.
func typedBits(text string, t syntax.Type, w int) ([]string, bool) {
	switch {
	case t.Kind == syntax.BitsType && t.Bits == w:
		out := make([]string, w)
		for i := range out {
			out[i] = text + "{" + strconv.Itoa(i) + "}"
		}
		return out, true
	case t.Kind == syntax.BitType && w == 1:
		return []string{text}, true
	}
	return nil, false
}

// varBits is TypedInit::getBit for a reference whose type is known: a bit is
// itself, bits<N> yields VarBitInit (`name{i}`) for every bit.
func varBits(text string, t syntax.Type, w int) ([]string, bool) {
	switch {
	case t.Kind == syntax.BitType:
		if w == 1 {
			return []string{text}, true
		}
	case t.Kind == syntax.IntType:
		return castBits(text, w), true
	case t.Kind == syntax.BitsType && t.Bits == w:
		out := make([]string, w)
		for i := range out {
			out[i] = text + "{" + strconv.Itoa(i) + "}"
		}
		return out, true
	}
	return nil, false
}

// bitListCandidates flattens a `{ ... }` bit list in written (high-to-low)
// order. A bits<N> element is stored as one Init per bit (TGParser expands
// BitsInit and VarInit elements), and a span contributes one entry per bit.
func bitListCandidates(x *syntax.BitsValue, env *Env, depth int) ([]string, bool) {
	var out []string
	for _, e := range x.Elems {
		switch el := e.(type) {
		case *syntax.BitRangeValue:
			for _, bit := range el.Bits {
				out = append(out, rawText(el.Base, env)+"{"+strconv.FormatInt(int64(bit), 10)+"}")
			}
		case *syntax.VarValue:
			t := el.Type()
			if t.Kind == syntax.BitsType {
				text, sub, subEnv := resolveVar(el, env)
				if sub != nil {
					bits, ok := bitTexts(sub, subEnv, t.Bits, depth+1)
					if !ok {
						return nil, false
					}
					for i := t.Bits - 1; i >= 0; i-- {
						out = append(out, bits[i])
					}
					continue
				}
				for i := t.Bits - 1; i >= 0; i-- {
					out = append(out, text+"{"+strconv.Itoa(i)+"}")
				}
				continue
			}
			out = append(out, rawText(e, env))
		case *syntax.BitsValue:
			inner, ok := bitListCandidates(el, env, depth+1)
			if !ok {
				return nil, false
			}
			out = append(out, inner...)
		default:
			out = append(out, rawText(e, env))
		}
	}
	return out, true
}

func reverseBits(hi []string) []string {
	out := make([]string, len(hi))
	for i := range hi {
		out[i] = hi[len(hi)-1-i]
	}
	return out
}

func unsetBits(w int) []string {
	out := make([]string, w)
	for i := range out {
		out[i] = "?"
	}
	return out
}

func fillBits(text string, w int) []string {
	out := make([]string, w)
	for i := range out {
		out[i] = text
	}
	return out
}

// constIntV is constInt for the printer: literals, defvar/field literals and
// operators over constants. LLVM folds these while parsing, so the Init tree
// carries the value rather than the operator.
func constIntV(v syntax.Value, env *Env) (int64, bool) {
	return foldIntV(v, env, 0)
}

func foldIntV(v syntax.Value, env *Env, depth int) (int64, bool) {
	if v == nil || depth > 32 {
		return 0, false
	}
	switch x := v.(type) {
	case *syntax.IntValue:
		return int64(x.Num), true
	case *syntax.VarValue:
		// A template argument stays a VarInit while parsing, so its (default)
		// literal must not be folded into an operator; only defvar and unrolled
		// loop variables were textually substituted by TGParser.
		if x.Subst && x.Lit != nil {
			return foldIntV(x.Lit, env, depth+1)
		}
		if x.Lexical && x.Lit != nil {
			return foldIntV(x.Lit, env, depth+1)
		}
		if x.GlobalRef && x.Lit != nil {
			// Same textual substitution as resolveVar (TGParser.cpp:1180).
			return foldIntV(x.Lit, env, depth+1)
		}
		// A class instance's fields hold the caller's Init once the class is
		// applied; resolveVar is the one place that decides whether a name is
		// still a VarInit (a class's own template arg) or already substituted.
		if _, sub, subEnv := resolveVar(x, env); sub != nil && sub != v {
			return foldIntV(sub, subEnv, depth+1)
		}
		return 0, false
	case *syntax.FieldValue:
		// `Foo.Bar` is a VarInit named "Foo.Bar", not the field's literal.
		return 0, false
	case *syntax.BitsValue:
		// A complete literal BitsInit converts to int.
		var n int64
		for i, e := range x.Elems {
			b, ok := foldIntV(e, env, depth+1)
			if !ok || (b != 0 && b != 1) {
				return 0, false
			}
			if b != 0 {
				n |= 1 << uint(len(x.Elems)-1-i)
			}
		}
		return n, true
	case *syntax.BangValue:
		arg := func(i int) (int64, bool) {
			if i >= len(x.Args) {
				return 0, false
			}
			return foldIntV(x.Args[i], env, depth+1)
		}
		switch x.Op {
		case syntax.XAdd, syntax.XSub, syntax.XMul, syntax.XAnd, syntax.XOr,
			syntax.XXor, syntax.XDiv, syntax.XShl, syntax.XSra, syntax.XSrl:
			a, ok1 := arg(0)
			b, ok2 := arg(1)
			if !ok1 || !ok2 {
				return 0, false
			}
			switch x.Op {
			case syntax.XAdd:
				return a + b, true
			case syntax.XSub:
				return a - b, true
			case syntax.XMul:
				return a * b, true
			case syntax.XAnd:
				return a & b, true
			case syntax.XOr:
				return a | b, true
			case syntax.XXor:
				return a ^ b, true
			case syntax.XDiv:
				if b == 0 || (a == -1<<63 && b == -1) {
					return 0, false
				}
				return a / b, true
			case syntax.XShl:
				if b < 0 || b > 63 {
					return 0, false
				}
				return a << uint(b), true
			case syntax.XSra:
				if b < 0 || b > 63 {
					return 0, false
				}
				return a >> uint(b), true
			case syntax.XSrl:
				if b < 0 || b > 63 {
					return 0, false
				}
				return int64(uint64(a) >> uint(b)), true
			}
		case syntax.XNot:
			a, ok := arg(0)
			if !ok {
				return 0, false
			}
			if len(x.Args) == 1 && x.Args[0] != nil && x.Args[0].Type().Kind == syntax.BitType {
				return 1 - a, true
			}
			return ^a, true
		case syntax.XEq, syntax.XNe, syntax.XLt, syntax.XLe, syntax.XGt, syntax.XGe:
			a, ok1 := arg(0)
			b, ok2 := arg(1)
			if !ok1 || !ok2 {
				return 0, false
			}
			r := false
			switch x.Op {
			case syntax.XEq:
				r = a == b
			case syntax.XNe:
				r = a != b
			case syntax.XLt:
				r = a < b
			case syntax.XLe:
				r = a <= b
			case syntax.XGt:
				r = a > b
			case syntax.XGe:
				r = a >= b
			}
			if r {
				return 1, true
			}
			return 0, true
		case syntax.XIf:
			c, ok := arg(0)
			if !ok || (c != 0 && c != 1) {
				return 0, false
			}
			if c != 0 {
				return arg(1)
			}
			return arg(2)
		case syntax.XCond:
			for _, c := range x.Cases {
				if c.Key == nil {
					return foldIntV(c.Value, env, depth+1)
				}
				k, ok := foldIntV(c.Key, env, depth+1)
				if !ok || (k != 0 && k != 1) {
					return 0, false
				}
				if k != 0 {
					return foldIntV(c.Value, env, depth+1)
				}
			}
			return 0, false
		case syntax.XSize:
			if len(x.Args) == 1 {
				if l, ok := constList(x.Args[0], env, depth+1); ok {
					return int64(len(l)), true
				}
				if sv, ok := x.Args[0].(*syntax.StringValue); ok {
					return int64(len(sv.Text)), true
				}
				if dv, ok := x.Args[0].(*syntax.DagValue); ok {
					return int64(len(dv.Args)), true
				}
			}
		case syntax.XEmpty:
			if len(x.Args) == 1 {
				if l, ok := constList(x.Args[0], env, depth+1); ok {
					if len(l) == 0 {
						return 1, true
					}
					return 0, true
				}
				if sv, ok := x.Args[0].(*syntax.StringValue); ok {
					if sv.Text == "" {
						return 1, true
					}
					return 0, true
				}
				if dv, ok := x.Args[0].(*syntax.DagValue); ok {
					if len(dv.Args) == 0 {
						return 1, true
					}
					return 0, true
				}
			}
		case syntax.XCast:
			if x.TypeArg.Kind == syntax.IntType || x.TypeArg.Kind == syntax.BitType {
				return arg(0)
			}
		}
	}
	return 0, false
}

// unsetText is what RecordVal::setValue stores for a declaration with no
// initialiser: UnsetInit cast to the declared type, which expands a bits<N>
// field into N unset bits.
func unsetText(want *Ty) string {
	if want != nil && want.Kind == BitsTy {
		parts := make([]string, want.Bits)
		for i := range parts {
			parts[i] = "?"
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	}
	return "?"
}

// perBitCast renders int -> bits<N>: a BitsInit whose elements are the bits of
// `!cast<bits<N>>(expr)`, high bit first.
func perBitCast(inner string, width int) string {
	cast := "!cast<bits<" + strconv.Itoa(width) + ">>(" + inner + ")"
	parts := make([]string, width)
	for i := range parts {
		parts[i] = cast + "{" + strconv.Itoa(width-1-i) + "}"
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// bitsInitForm renders a one-bit source as a BitsInit before it is cast.
func bitsInitForm(v syntax.Value, env *Env) string {
	switch v.(type) {
	case *syntax.BitsValue:
		return rawText(v, env)
	case *syntax.BitRangeValue:
		return "{ " + rawText(v, env) + " }"
	}
	return rawText(v, env)
}

// exprText renders a record value's stored initialiser. Classes keep the
// parsed Init tree; an unset value prints as its type's unset form.
func (r *Record) exprText(v *RecordVal) string {
	if v.Expr == nil {
		return unsetText(v.Ty)
	}
	return r.renderExpr(v.Expr, v.Ty)
}

func (r *Record) renderExpr(e Expr, want *Ty) string {
	switch x := e.(type) {
	case *AstExpr:
		env := x.Env
		if r.IsClass {
			env = env.WithImplicitName(r.Name)
		}
		return initText(x.V, env, want)
	case *BindExpr:
		if x.Rec != nil {
			return x.Rec.Name
		}
		return unsetText(want)
	case *AppendExpr:
		// TGParser::SetValue builds BinOpInit(LISTCONCAT/STRCONCAT/CONCAT) and
		// folds it immediately (TGParser.cpp:266-282). BinOpInit::Fold
		// (Record.cpp:1220) only folds when both operands are already concrete
		// inits, so a VarInit or compound operator keeps the !con text.
		if v, ok := r.constAppend(x); ok {
			return v.String()
		}
		base := r.renderExpr(x.Base, nil)
		add := r.renderExpr(x.Add, nil)
		if x.Prepend {
			base, add = add, base
		}
		return "!con(" + base + ", " + add + ")"
	case *BitsExpr:
		// ParseLet patches the field's current BitsInit in place, so the
		// result is the base value with this range's bits replaced, printed
		// high bit first.
		bits := r.bitTextsOf(x, x.Width)
		return "{ " + strings.Join(reverseBits(bits), ", ") + " }"
	}
	return unsetText(want)
}

// bitTextsOf renders an expression whose declared width is w to per-bit
// texts, least significant first.
func (r *Record) bitTextsOf(e Expr, w int) []string {
	switch x := e.(type) {
	case nil:
		return unsetBits(w)
	case *AstExpr:
		if bits, ok := bitTexts(x.V, x.Env, w, 0); ok {
			return bits
		}
		return fillBits(rawText(x.V, x.Env), w)
	case *BindExpr:
		if x.Rec != nil {
			return fillBits(x.Rec.Name, w)
		}
		return unsetBits(w)
	case *BitsExpr:
		bits := r.bitTextsOf(x.Base, w)
		if len(bits) != w {
			bits = unsetBits(w)
		}
		var addV syntax.Value
		var addEnv *Env
		if a, ok := x.Add.(*AstExpr); ok {
			addV, addEnv = a.V, a.Env
		}
		src, ok := bitTexts(addV, addEnv, len(x.Bits), 0)
		if !ok {
			src = fillBits(rawText(addV, addEnv), len(x.Bits))
		}
		for i, bit := range x.Bits {
			if i < len(src) && bit >= 0 && int(bit) < w {
				bits[bit] = src[i]
			}
		}
		return bits
	}
	return unsetBits(w)
}

// dagParts is a literal dag split into its operator text and rendered
// arguments, so two of them can be concatenated like DagInit::Fold does.
type dagParts struct {
	op   string
	name string
	args []string
}

func constDagParts(v syntax.Value, env *Env) (dagParts, bool) {
	d, ok := v.(*syntax.DagValue)
	if !ok {
		return dagParts{}, false
	}
	if _, unset := d.Op.(*syntax.UnsetValue); !unset {
		switch d.Op.(type) {
		case *syntax.VarValue:
			// A reference is only a literal operator once it resolves to a
			// def; anything else keeps the !con node.
			if vv := d.Op.(*syntax.VarValue); vv.GlobalRef || vv.Subst {
				break
			}
			return dagParts{}, false
		}
	}
	p := dagParts{op: rawText(d.Op, env), name: d.OpName}
	for _, a := range d.Args {
		t := rawText(a.Value, env)
		if a.Name != "" {
			t += ":$" + a.Name
		}
		p.args = append(p.args, t)
	}
	return p, true
}

func dagPartsText(l, r dagParts) string {
	op := l.op
	if op == "?" {
		op = r.op
	}
	name := l.name
	if name == "" {
		name = r.name
	}
	args := append(append([]string{}, l.args...), r.args...)
	var b strings.Builder
	b.WriteByte('(')
	b.WriteString(op)
	if name != "" {
		b.WriteString(":$")
		b.WriteString(name)
	}
	if len(args) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(args, ", "))
	}
	b.WriteByte(')')
	return b.String()
}

func rawText(v syntax.Value, env *Env) string {
	switch x := v.(type) {
	case nil:
		return "?"
	case *syntax.UnsetValue:
		return "?"
	case *syntax.IntValue:
		return strconv.FormatInt(int64(x.Num), 10)
	case *syntax.BitsValue:
		// A bit-list element may name a span (`{ y.B{1, 0} }`) or a bits<N>
		// variable; BitsInit stores one Init per bit, so both expand.
		hi, ok := bitListCandidates(x, env, 0)
		if !ok {
			return "?"
		}
		return "{ " + strings.Join(hi, ", ") + " }"
	case *syntax.StringValue:
		if x.Code {
			return "[{" + x.Text + "}]"
		}
		return `"` + x.Text + `"`
	case *syntax.ListValue:
		return listText(x.Elems, env)
	case *syntax.DagValue:
		var b strings.Builder
		b.WriteByte('(')
		b.WriteString(rawText(x.Op, env))
		if x.OpName != "" {
			b.WriteString(":$")
			b.WriteString(x.OpName)
		}
		if len(x.Args) > 0 {
			b.WriteByte(' ')
			for i, a := range x.Args {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(rawText(a.Value, env))
				if a.Name != "" {
					b.WriteString(":$")
					b.WriteString(a.Name)
				}
			}
		}
		b.WriteByte(')')
		return b.String()
	case *syntax.VarValue:
		// A defvar is substituted textually by the parser, so the Init is the
		// value it was folded to. AddParentClass resolves an inherited field
		// with a MapResolver binding the parent's arguments to the caller's
		// Init; a class's own template arguments stay VarInit (Dynamic) and
		// are qualified by their owning record (QualifyName).
		text, sub, subEnv := resolveVar(x, env)
		if sub != nil {
			return rawText(sub, subEnv)
		}
		return text
	case *syntax.FieldValue:
		// FieldInit::Fold (Record.cpp:2498): at parse time a reference to a
		// field of a def whose value is already concrete is replaced by that
		// value, so the Init tree shows the literal. Fold only looks at a
		// DefInit base; `vt.isVector` on a template argument stays a
		// FieldInit even when the class default is known.
		if x.Lit != nil && concreteInit(x.Lit) && fieldBaseIsDef(x.Base) {
			// SetValue stored the field's value converted to its declared
			// type, so a `bits<4> = 11` field yields the BitsInit literal.
			if text, ok := literalAs(x.Lit, x.Type(), env); ok {
				return text
			}
			return rawText(x.Lit, env)
		}
		return rawText(x.Base, env) + "." + x.Field
	case *syntax.SliceValue:
		return rawText(x.Base, env) + "[" + sliceText(x, env) + "]"
	case *syntax.BitRangeValue:
		bits := make([]string, len(x.Bits))
		for i, bit := range x.Bits {
			bits[i] = rawText(x.Base, env) + "{" + strconv.FormatInt(int64(bit), 10) + "}"
		}
		return "{ " + strings.Join(bits, ", ") + " }"
	case *syntax.PasteValue:
		if x.LHS.Type().Kind == syntax.ListType {
			// `#` on lists is BinOpInit::LISTCONCAT, not dag CONCAT.
			// BinOpInit::resolveReferences folds it as soon as both operands
			// are ListInits (Record.cpp:1297-1307), which is how a
			// superclass's `intr_properties # [...]` becomes one list once the
			// caller's argument is substituted.
			if la, ok := constList(x.LHS, env, 0); ok {
				if lb, ok := constList(x.RHS, env, 0); ok {
					return listText(append(append([]syntax.Value{}, la...), lb...), env)
				}
			}
			return "!listconcat(" + rawText(x.LHS, env) + ", " + rawText(x.RHS, env) + ")"
		}
		if x.LHS.Type().Kind == syntax.DagType {
			// BinOpInit::Fold's CONCAT merges two DagInits whose operators
			// agree, keeping the left name (Record.cpp:1219-1248).
			if l, lok := constDagParts(x.LHS, env); lok {
				if r, rok := constDagParts(x.RHS, env); rok && l.op == r.op {
					return dagPartsText(l, r)
				}
			}
			return "!con(" + rawText(x.LHS, env) + ", " + rawText(x.RHS, env) + ")"
		}
		if lt, lc, ok := constStrCode(x.LHS, env, 0); ok {
			if rt, rc, ok := constStrCode(x.RHS, env, 0); ok {
				return stringLiteral(lt+rt, lc || rc)
			}
		}
		if ls, ok := x.LHS.(*syntax.StringValue); ok && !ls.Code {
			if rs, ok := x.RHS.(*syntax.StringValue); ok && !rs.Code {
				return `"` + ls.Text + rs.Text + `"`
			}
		}
		return "!strconcat(" + stringOperand(x.LHS, env) + ", " + stringOperand(x.RHS, env) + ")"
	case *syntax.ClassApplyValue:
		if x.FoldName != "" {
			return x.FoldName
		}
		var b strings.Builder
		b.WriteString(x.Class)
		b.WriteByte('<')
		for i, a := range x.Args {
			if i > 0 {
				b.WriteString(", ")
			}
			if a.Name == "" {
				b.WriteString(strconv.Itoa(i))
			} else {
				// ArgumentInit::getAsString prints the qualified name as a
				// quoted string: `P<"P:i": n>`.
				b.WriteByte('"')
				b.WriteString(x.Class)
				b.WriteByte(':')
				b.WriteString(a.Name)
				b.WriteByte('"')
			}
			b.WriteString(": ")
			b.WriteString(rawText(a.Value, env))
		}
		b.WriteByte('>')
		return b.String()
	case *syntax.BangValue:
		// TGParser folds these at parse time, so the Init tree holds a literal.
		// A cast to the operand's own type is a no-op (UnOpInit::Fold).
		if x.Op == syntax.XCast && len(x.Args) == 1 && castIsIdentity(x.Args[0], x.TypeArg) {
			return rawText(x.Args[0], env)
		}
		// TernOpInit::Fold's IF case returns the taken branch unchanged
		// (Record.cpp:1806-1816), so any branch type folds, not just the ones
		// the typed helpers below understand.
		if x.Op == syntax.XIf && len(x.Args) == 3 {
			if c, ok := foldIntV(x.Args[0], env, 0); ok {
				return rawText(x.Args[boolIdx(c)], env)
			}
		}
		// CondOpInit::Fold returns the value of the first true condition
		// (Record.cpp:2580-2597).
		if x.Op == syntax.XCond {
			if v, ok := condArm(x, env, 0); ok {
				return rawText(v, env)
			}
		}
		if str, ok := constStr(x, env, 0); ok {
			return `"` + str + `"`
		}
		if elems, ok := constList(x, env, 0); ok {
			return listText(elems, env)
		}
		if x.Op == syntax.XHead && len(x.Args) == 1 {
			if elems, ok := constList(x.Args[0], env, 0); ok && len(elems) > 0 {
				return rawText(elems[0], env)
			}
		}
		if n, ok := foldIntV(x, env, 0); ok {
			return strconv.FormatInt(n, 10)
		}
		return bangText(x, env)
	}
	return "?"
}

// boolIdx maps an int condition to the index picking between the two arms of
// `!if(cond, then, else)`: TernOpInit::Fold returns MHS (args[1]) for a true
// condition and RHS (args[2]) otherwise (Record.cpp:1806-1816).
func boolIdx(n int64) int {
	if n != 0 {
		return 1
	}
	return 2
}

// condArm returns the value of the first condition that folds to a true int,
// mirroring CondOpInit::Fold (Record.cpp:2580). A nil key is the default arm
// and is always taken.
func condArm(b *syntax.BangValue, env *Env, depth int) (syntax.Value, bool) {
	for _, c := range b.Cases {
		if c.Key == nil {
			return c.Value, true
		}
		k, ok := foldIntV(c.Key, env, depth+1)
		if !ok {
			// An unresolved condition stops the fold: CondOpInit::Fold
			// returns the operator itself (Record.cpp:2587).
			return nil, false
		}
		if k != 0 {
			return c.Value, true
		}
	}
	return nil, false
}

// substValue replaces a lexical loop variable with a value, the substitution
// ForeachHelper performs per element while folding (Record.cpp:1958).
func substValue(v syntax.Value, name string, repl syntax.Value, depth int) syntax.Value {
	if v == nil || depth > 32 {
		return v
	}
	switch x := v.(type) {
	case *syntax.VarValue:
		if x.Name == name && (x.Lexical || x.Subst) {
			return repl
		}
		return v
	case *syntax.BangValue:
		cp := *x
		if x.Args != nil {
			cp.Args = make([]syntax.Value, len(x.Args))
			for i, a := range x.Args {
				cp.Args[i] = substValue(a, name, repl, depth+1)
			}
		}
		if x.Cases != nil {
			cp.Cases = make([]*syntax.BangCase, len(x.Cases))
			for i, c := range x.Cases {
				nc := *c
				nc.Key = substValue(c.Key, name, repl, depth+1)
				nc.Value = substValue(c.Value, name, repl, depth+1)
				cp.Cases[i] = &nc
			}
		}
		return &cp
	case *syntax.ListValue:
		cp := *x
		cp.Elems = make([]syntax.Value, len(x.Elems))
		for i, e := range x.Elems {
			cp.Elems[i] = substValue(e, name, repl, depth+1)
		}
		return &cp
	case *syntax.BitsValue:
		cp := *x
		cp.Elems = make([]syntax.Value, len(x.Elems))
		for i, e := range x.Elems {
			cp.Elems[i] = substValue(e, name, repl, depth+1)
		}
		return &cp
	case *syntax.DagValue:
		cp := *x
		cp.Op = substValue(x.Op, name, repl, depth+1)
		cp.Args = make([]*syntax.DagArg, len(x.Args))
		for i, a := range x.Args {
			na := *a
			na.Value = substValue(a.Value, name, repl, depth+1)
			cp.Args[i] = &na
		}
		return &cp
	case *syntax.FieldValue:
		base := substValue(x.Base, name, repl, depth+1)
		if base == x.Base {
			return v
		}
		cp := *x
		cp.Base = base
		return &cp
	case *syntax.SliceValue:
		base := substValue(x.Base, name, repl, depth+1)
		if base == x.Base {
			return v
		}
		cp := *x
		cp.Base = base
		return &cp
	case *syntax.PasteValue:
		lhs := substValue(x.LHS, name, repl, depth+1)
		rhs := substValue(x.RHS, name, repl, depth+1)
		if lhs == x.LHS && rhs == x.RHS {
			return v
		}
		cp := *x
		cp.LHS, cp.RHS = lhs, rhs
		return &cp
	case *syntax.ClassApplyValue:
		cp := *x
		cp.Args = make([]*syntax.Arg, len(x.Args))
		for i, a := range x.Args {
			na := *a
			na.Value = substValue(a.Value, name, repl, depth+1)
			cp.Args[i] = &na
		}
		return &cp
	}
	return v
}

// constAny folds an expression to the concrete Init the parser would store.
// It is the general form of the typed helpers: any literal, a foldable
// operator, or a lexical binding created by !foreach/!filter/!sort.
func constAny(v syntax.Value, env *Env, depth int) (syntax.Value, bool) {
	if v == nil || depth > 32 {
		return nil, false
	}
	switch x := v.(type) {
	case *syntax.IntValue, *syntax.StringValue, *syntax.BitsValue,
		*syntax.ListValue, *syntax.DagValue:
		return v, true
	case *syntax.VarValue:
		if x.Lit != nil && (x.Subst || x.Lexical || x.GlobalRef) {
			return constAny(x.Lit, env, depth+1)
		}
		return nil, false
	case *syntax.FieldValue:
		if x.Lit != nil && concreteInit(x.Lit) && fieldBaseIsDef(x.Base) {
			return constAny(x.Lit, env, depth+1)
		}
		return nil, false
	case *syntax.BangValue:
		switch x.Op {
		case syntax.XIf:
			if len(x.Args) == 3 {
				if c, ok := foldIntV(x.Args[0], env, depth+1); ok {
					return constAny(x.Args[boolIdx(c)], env, depth+1)
				}
			}
			return nil, false
		case syntax.XCond:
			if arm, ok := condArm(x, env, depth); ok {
				return constAny(arm, env, depth+1)
			}
			return nil, false
		case syntax.XCast:
			if len(x.Args) == 1 && castIsIdentity(x.Args[0], x.TypeArg) {
				return constAny(x.Args[0], env, depth+1)
			}
		}
		if n, ok := foldIntV(x, env, depth+1); ok {
			return intNode(n, x.Span()), true
		}
		if str, ok := constStr(x, env, depth+1); ok {
			return &syntax.StringValue{
				ValueBase: syntax.ValueBase{Base: syntax.Base{At: x.Span()}, Ty: syntax.StringTy},
				Text:      str,
			}, true
		}
		if elems, ok := constList(x, env, depth+1); ok {
			return listNode(elems, x.Span(), elemTypeOf(x)), true
		}
		return nil, false
	}
	return nil, false
}

// fieldBaseIsDef reports whether a field reference's base is a DefInit, the
// only shape FieldInit::Fold folds (Record.cpp:2498). A template argument or
// any other VarInit keeps its FieldInit even when the class default is known.
func fieldBaseIsDef(v syntax.Value) bool {
	for depth := 0; depth < 32; depth++ {
		x, ok := v.(*syntax.VarValue)
		if !ok {
			return false
		}
		if x.GlobalRef && !x.GlobalVar {
			return true
		}
		if x.Lit != nil && (x.Subst || x.Lexical || x.GlobalRef) {
			v = x.Lit
			continue
		}
		return false
	}
	return false
}

// derefInit follows the bindings the parser substituted textually (defvars,
// loop variables) to the value the Init tree really holds.
func derefInit(v syntax.Value) syntax.Value {
	for depth := 0; depth < 32; depth++ {
		x, ok := v.(*syntax.VarValue)
		if !ok {
			return v
		}
		if x.Lit == nil || !(x.Subst || x.Lexical || x.GlobalRef) {
			return v
		}
		v = x.Lit
	}
	return v
}

// castIsIdentity reports whether `!cast<T>(v)` folds away: UnOpInit::Fold's
// CAST case ends with LHS->convertInitializerTo(getType()), which returns LHS
// unchanged when the value already has that type (Record.cpp:803-856).
func castIsIdentity(v syntax.Value, want syntax.Type) bool {
	v = derefInit(v)
	if v == nil || want.Kind == syntax.InvalidType {
		return false
	}
	got := v.Type()
	if want.Kind == syntax.ListType {
		if got.Kind != syntax.ListType || got.Elem == nil || want.Elem == nil {
			return false
		}
		return sameSyntaxType(*got.Elem, *want.Elem)
	}
	return sameSyntaxType(got, want)
}

// sameSyntaxType compares parsed types the way RecTy::typeIsA does for the
// identity conversions: `code` and `string` are the same StringRecTy, and a
// record type is an unordered intersection of class names.
func sameSyntaxType(a, b syntax.Type) bool {
	isString := func(t syntax.Type) bool {
		return t.Kind == syntax.StringType || t.Kind == syntax.CodeType
	}
	if isString(a) || isString(b) {
		return isString(a) && isString(b)
	}
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case syntax.BitsType:
		return a.Bits == b.Bits
	case syntax.ListType:
		if a.Elem == nil || b.Elem == nil {
			return a.Elem == nil && b.Elem == nil
		}
		return sameSyntaxType(*a.Elem, *b.Elem)
	case syntax.RecordType:
		if len(a.Recs) != len(b.Recs) {
			return false
		}
		for _, r := range a.Recs {
			found := false
			for _, o := range b.Recs {
				if r == o {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return true
}

// sliceText renders the inside of `base[...]`. A single index reads as `i`; a
// single non-literal range keeps RANGEC's `lo...hi` form; anything that the
// parser folds into a list is a ListInit, printed with its own brackets.
func sliceText(x *syntax.SliceValue, env *Env) string {
	if !x.ListResult && len(x.Index) == 1 && x.Index[0].Hi == nil {
		return rawText(x.Index[0].Lo, env)
	}
	if len(x.Index) == 1 && x.Index[0].Hi != nil && !literalRange(x.Index[0]) {
		return rawText(x.Index[0].Lo, env) + "..." + rawText(x.Index[0].Hi, env)
	}
	var parts []string
	for _, el := range x.Index {
		if el.Hi == nil {
			parts = append(parts, rawText(el.Lo, env))
			continue
		}
		if lo, hi, ok := literalBounds(el); ok {
			for i := lo; i <= hi; i++ {
				parts = append(parts, strconv.FormatInt(i, 10))
			}
			continue
		}
		parts = append(parts, rawText(el.Lo, env)+"..."+rawText(el.Hi, env))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func literalRange(el *syntax.SliceElem) bool {
	_, _, ok := literalBounds(el)
	return ok
}

func literalBounds(el *syntax.SliceElem) (int64, int64, bool) {
	if el.Hi == nil {
		return 0, 0, false
	}
	lo, ok1 := el.Lo.(*syntax.IntValue)
	hi, ok2 := el.Hi.(*syntax.IntValue)
	if !ok1 || !ok2 {
		return 0, 0, false
	}
	return int64(lo.Num), int64(hi.Num), true
}

// stringLiteral renders a folded string with the format ConcatStringInits
// keeps: a code operand makes the result code (Record.cpp:1064).
func stringLiteral(text string, code bool) string {
	if code {
		return "[{" + text + "}]"
	}
	return `"` + text + `"`
}

// constStrCode folds a string expression to its text and format, following the
// substitutions rawText would print (defvars, loop variables, a caller's
// substituted arguments).
func constStrCode(v syntax.Value, env *Env, depth int) (string, bool, bool) {
	if v == nil || depth > 32 {
		return "", false, false
	}
	switch x := v.(type) {
	case *syntax.StringValue:
		return x.Text, x.Code, true
	case *syntax.VarValue:
		if x.Lit != nil && (x.Subst || x.Lexical || x.GlobalRef) {
			if t, c, ok := constStrCode(x.Lit, env, depth+1); ok && (x.Subst || x.GlobalRef) {
				return t, c, ok
			}
		}
		if _, sub, subEnv := resolveVar(x, env); sub != nil && sub != v {
			return constStrCode(sub, subEnv, depth+1)
		}
	case *syntax.BangValue:
		if x.Op == syntax.XStrConcat {
			var out string
			code := false
			for _, a := range x.Args {
				t, c, ok := constStrCode(a, env, depth+1)
				if !ok {
					return "", false, false
				}
				out += t
				code = code || c
			}
			return out, code, true
		}
	case *syntax.PasteValue:
		lt, lc, ok1 := constStrCode(x.LHS, env, depth+1)
		rt, rc, ok2 := constStrCode(x.RHS, env, depth+1)
		if ok1 && ok2 {
			return lt + rt, lc || rc, true
		}
	}
	return "", false, false
}

// stringOperand renders a paste operand: non-string operands are cast to
// string while parsing (`"n" # !add(i, 1)` prints the cast).
func stringOperand(v syntax.Value, env *Env) string {
	text := rawText(v, env)
	if v.Type().Kind == syntax.StringType {
		return text
	}
	return "!cast<string>(" + text + ")"
}

func listText(elems []syntax.Value, env *Env) string {
	parts := make([]string, len(elems))
	for i, e := range elems {
		parts[i] = rawText(e, env)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// assocOps are the operators TGParser nests right-associatively when written
// with more than two operands, folding each pair as it goes.
func assocOp(op syntax.Kind) bool {
	switch op {
	case syntax.XStrConcat, syntax.XListConcat, syntax.XConcat,
		syntax.XAdd, syntax.XAnd, syntax.XOr, syntax.XXor, syntax.XMul:
		return true
	}
	return false
}

func bangNode(op syntax.Kind, a, b syntax.Value) *syntax.BangValue {
	return &syntax.BangValue{
		ValueBase: syntax.ValueBase{Base: syntax.Base{At: a.Span()}, Ty: syntax.InvalidTy},
		Op:        op,
		Args:      []syntax.Value{a, b},
	}
}

func intNode(n int64, at syntax.Span) *syntax.IntValue {
	return &syntax.IntValue{ValueBase: syntax.ValueBase{Base: syntax.Base{At: at}, Ty: syntax.IntTy}, Num: uint64(n)}
}

func listNode(elems []syntax.Value, at syntax.Span, elem *syntax.Type) *syntax.ListValue {
	t := syntax.Type{Kind: syntax.ListType}
	if elem != nil {
		t.Elem = elem
	}
	return &syntax.ListValue{ValueBase: syntax.ValueBase{Base: syntax.Base{At: at}, Ty: t}, Elems: elems}
}

// foldBin folds one binary step of an operator over constant operands. nil
// means the Init survives (BinOpInit::Fold returned this).
func foldBin(op syntax.Kind, a, b syntax.Value, env *Env, depth int) syntax.Value {
	if depth > 32 {
		return nil
	}
	if n, ok := foldIntV(bangNode(op, a, b), env, depth+1); ok {
		return intNode(n, a.Span())
	}
	switch op {
	case syntax.XListConcat:
		la, ok1 := constList(a, env, depth+1)
		lb, ok2 := constList(b, env, depth+1)
		if ok1 && ok2 {
			elems := make([]syntax.Value, 0, len(la)+len(lb))
			elems = append(elems, la...)
			elems = append(elems, lb...)
			return listNode(elems, a.Span(), elemTypeOf(a))
		}
	case syntax.XListRemove:
		la, ok1 := constList(a, env, depth+1)
		lb, ok2 := constList(b, env, depth+1)
		if ok1 && ok2 {
			var elems []syntax.Value
			for _, e := range la {
				found := false
				for _, r := range lb {
					if initText(e, env, nil) == initText(r, env, nil) {
						found = true
						break
					}
				}
				if !found {
					elems = append(elems, e)
				}
			}
			return listNode(elems, a.Span(), elemTypeOf(a))
		}
	case syntax.XListSplat:
		if n, ok := foldIntV(b, env, depth+1); ok && n >= 0 && n <= 1<<20 {
			elems := make([]syntax.Value, n)
			for i := range elems {
				elems[i] = a
			}
			return listNode(elems, a.Span(), elemTypeOf(a))
		}
	case syntax.XStrConcat:
		as, ok1 := a.(*syntax.StringValue)
		bs, ok2 := b.(*syntax.StringValue)
		if ok1 && ok2 && !as.Code && !bs.Code {
			return &syntax.StringValue{
				ValueBase: syntax.ValueBase{Base: syntax.Base{At: a.Span()}, Ty: syntax.StringTy},
				Text:      as.Text + bs.Text,
			}
		}
	}
	return nil
}

// sortRange is a stable insertion sort over the parallel slices the caller
// keeps in sync; the corpus has tiny lists, and stability matches
// llvm::stable_sort in SortHelper.
func sortRange(n int, less func(i, j int) bool, swap func(i, j int)) {
	for i := 1; i < n; i++ {
		for j := i; j > 0 && less(j, j-1); j-- {
			swap(j, j-1)
		}
	}
}

func elemTypeOf(v syntax.Value) *syntax.Type {
	if t := v.Type(); t.Kind == syntax.ListType {
		return t.Elem
	}
	return nil
}

// chainText renders an associative operator with the parser's right nesting.
func chainText(op syntax.Kind, args []syntax.Value, env *Env) string {
	right := args[len(args)-1]
	for i := len(args) - 2; i >= 1; i-- {
		if f := foldBin(op, args[i], right, env, 0); f != nil {
			right = f
		} else {
			right = bangNode(op, args[i], right)
		}
	}
	return binText(op, args[0], right, env)
}

func binText(op syntax.Kind, a, b syntax.Value, env *Env) string {
	if f := foldBin(op, a, b, env, 0); f != nil {
		return rawText(f, env)
	}
	return op.String() + "(" + rawText(a, env) + ", " + rawText(b, env) + ")"
}

// constList returns the elements of a list expression whose Init is a
// ListInit. TGParser folds list operators over ListInits regardless of whether
// their elements are concrete.
func constList(v syntax.Value, env *Env, depth int) ([]syntax.Value, bool) {
	if v == nil || depth > 32 {
		return nil, false
	}
	switch x := v.(type) {
	case *syntax.ListValue:
		return x.Elems, true
	case *syntax.VarValue:
		if (x.Subst || x.Lexical) && x.Lit != nil {
			return constList(x.Lit, env, depth+1)
		}
		if x.GlobalRef && x.Lit != nil {
			// A defvar reference is its value (see resolveVar).
			return constList(x.Lit, env, depth+1)
		}
		if _, sub, subEnv := resolveVar(x, env); sub != nil && sub != v {
			return constList(sub, subEnv, depth+1)
		}
		return nil, false
	case *syntax.FieldValue:
		// FieldInit::Fold replaced a reference into a concrete def field with
		// its value while parsing (Record.cpp:2498).
		if x.Lit != nil && concreteInit(x.Lit) && fieldBaseIsDef(x.Base) {
			return constList(x.Lit, env, depth+1)
		}
		return nil, false
	case *syntax.BangValue:
		switch x.Op {
		case syntax.XForEach:
			// ForeachHelper evaluates the expression once per element
			// (Record.cpp:1958), so both the list and every result must fold.
			if len(x.Args) == 2 && len(x.VarNames) == 1 {
				l, ok := constList(x.Args[0], env, depth+1)
				if !ok {
					return nil, false
				}
				out := make([]syntax.Value, 0, len(l))
				for _, e := range l {
					cv, ok := constAny(substValue(x.Args[1], x.VarNames[0], e, 0), env, depth+1)
					if !ok {
						return nil, false
					}
					out = append(out, cv)
				}
				return out, true
			}
		case syntax.XFilter:
			// FilterHelper keeps the elements whose predicate folds to a
			// nonzero int (Record.cpp:1968).
			if len(x.Args) == 2 && len(x.VarNames) == 1 {
				l, ok := constList(x.Args[0], env, depth+1)
				if !ok {
					return nil, false
				}
				var out []syntax.Value
				for _, e := range l {
					pred := substValue(x.Args[1], x.VarNames[0], e, 0)
					n, ok := foldIntV(pred, env, depth+1)
					if !ok {
						return nil, false
					}
					if n != 0 {
						out = append(out, e)
					}
				}
				return out, true
			}
		case syntax.XSort:
			// SortHelper orders the list by the folded key (Record.cpp:1985).
			if len(x.Args) == 2 && len(x.VarNames) == 1 {
				l, ok := constList(x.Args[0], env, depth+1)
				if !ok {
					return nil, false
				}
				keys := make([]int64, len(l))
				strs := make([]string, len(l))
				useInt := true
				for i, e := range l {
					key, ok := constAny(substValue(x.Args[1], x.VarNames[0], e, 0), env, depth+1)
					if !ok {
						return nil, false
					}
					if n, ok := foldIntV(key, env, depth+1); ok {
						keys[i] = n
					} else {
						useInt = false
					}
					if sv, ok := key.(*syntax.StringValue); ok {
						strs[i] = sv.Text
					} else {
						useInt = false
					}
				}
				out := append([]syntax.Value{}, l...)
				sortRange(len(out), func(i, j int) bool {
					if useInt {
						return keys[i] < keys[j]
					}
					return strs[i] < strs[j]
				}, func(i, j int) {
					keys[i], keys[j] = keys[j], keys[i]
					strs[i], strs[j] = strs[j], strs[i]
				})
				return out, true
			}
		case syntax.XListConcat:
			if len(x.Args) == 2 {
				la, ok1 := constList(x.Args[0], env, depth+1)
				lb, ok2 := constList(x.Args[1], env, depth+1)
				if ok1 && ok2 {
					return append(append([]syntax.Value{}, la...), lb...), true
				}
				return nil, false
			}
			if len(x.Args) > 2 {
				right, ok := constList(x.Args[len(x.Args)-1], env, depth+1)
				if !ok {
					return nil, false
				}
				for i := len(x.Args) - 2; i >= 0; i-- {
					left, ok := constList(x.Args[i], env, depth+1)
					if !ok {
						return nil, false
					}
					right = append(append([]syntax.Value{}, left...), right...)
				}
				return right, true
			}
		case syntax.XListRemove:
			if len(x.Args) == 2 {
				la, ok1 := constList(x.Args[0], env, depth+1)
				lb, ok2 := constList(x.Args[1], env, depth+1)
				if ok1 && ok2 {
					var out []syntax.Value
					for _, e := range la {
						found := false
						for _, r := range lb {
							if initText(e, env, nil) == initText(r, env, nil) {
								found = true
								break
							}
						}
						if !found {
							out = append(out, e)
						}
					}
					return out, true
				}
			}
		case syntax.XListFlatten:
			if len(x.Args) == 1 {
				inner, ok := constList(x.Args[0], env, depth+1)
				if !ok {
					return nil, false
				}
				if et := elemTypeOf(x.Args[0]); et != nil && et.Kind == syntax.ListType {
					var out []syntax.Value
					for _, e := range inner {
						l, ok := constList(e, env, depth+1)
						if !ok {
							return nil, false
						}
						out = append(out, l...)
					}
					return out, true
				}
				return inner, true
			}
		case syntax.XTail:
			if len(x.Args) == 1 {
				l, ok := constList(x.Args[0], env, depth+1)
				if !ok || len(l) == 0 {
					return nil, false
				}
				return l[1:], true
			}
		case syntax.XIf:
			if len(x.Args) == 3 {
				// TernOpInit::Fold returns MHS for a true condition and RHS
				// otherwise (Record.cpp:1810-1816).
				if c, ok := foldIntV(x.Args[0], env, depth+1); ok && (c == 0 || c == 1) {
					return constList(x.Args[2-int(c)], env, depth+1)
				}
			}
		case syntax.XCond:
			for _, c := range x.Cases {
				if c.Key == nil {
					return constList(c.Value, env, depth+1)
				}
				if k, ok := foldIntV(c.Key, env, depth+1); ok && (k == 0 || k == 1) {
					if k == 1 {
						return constList(c.Value, env, depth+1)
					}
					continue
				}
				return nil, false
			}
		}
	}
	return nil, false
}

// constStr folds !strconcat/!if chains over string literals.
func constStr(v syntax.Value, env *Env, depth int) (string, bool) {
	if v == nil || depth > 32 {
		return "", false
	}
	switch x := v.(type) {
	case *syntax.StringValue:
		if x.Code {
			return "", false
		}
		return x.Text, true
	case *syntax.VarValue:
		if (x.Subst || x.Lexical) && x.Lit != nil {
			return constStr(x.Lit, env, depth+1)
		}
		if x.GlobalRef && x.Lit != nil {
			// A defvar reference is its value (see resolveVar).
			return constStr(x.Lit, env, depth+1)
		}
		if _, sub, subEnv := resolveVar(x, env); sub != nil && sub != v {
			return constStr(sub, subEnv, depth+1)
		}
	case *syntax.PasteValue:
		// BinOpInit::getStrConcat folds as soon as both operands are string
		// literals (Record.cpp:1297-1307); a paste inside !strconcat is
		// already a literal by the time the outer operator folds.
		if t, c, ok := constStrCode(x, env, depth+1); ok && !c {
			return t, true
		}
	case *syntax.BangValue:
		switch x.Op {
		case syntax.XStrConcat:
			var out string
			for _, a := range x.Args {
				s, ok := constStr(a, env, depth+1)
				if !ok {
					return "", false
				}
				out += s
			}
			return out, true
		case syntax.XIf:
			if len(x.Args) == 3 {
				// Same branch order as constList (Record.cpp:1810-1816).
				if c, ok := foldIntV(x.Args[0], env, depth+1); ok && (c == 0 || c == 1) {
					return constStr(x.Args[2-int(c)], env, depth+1)
				}
			}
		case syntax.XCond:
			for _, c := range x.Cases {
				if c.Key == nil {
					return constStr(c.Value, env, depth+1)
				}
				if k, ok := foldIntV(c.Key, env, depth+1); ok && (k == 0 || k == 1) {
					if k == 1 {
						return constStr(c.Value, env, depth+1)
					}
					continue
				}
				return "", false
			}
		}
	}
	return "", false
}

func bangText(b *syntax.BangValue, env *Env) string {
	if assocOp(b.Op) && len(b.Args) >= 2 {
		return chainText(b.Op, b.Args, env)
	}
	name := b.Op.String()
	switch b.Op {
	case syntax.XCast, syntax.XGetDagArg:
		if b.TypeArg.Kind != syntax.InvalidType {
			name += "<" + b.TypeArg.String() + ">"
		}
	case syntax.XIsA, syntax.XExists, syntax.XInstances:
		if b.TypeArg.Kind != syntax.InvalidType {
			name += "<" + b.TypeArg.String() + ">"
		}
	}
	switch b.Op {
	case syntax.XCond:
		parts := make([]string, len(b.Cases))
		for i, c := range b.Cases {
			parts[i] = rawText(c.Key, env) + ": " + rawText(c.Value, env)
		}
		return name + "(" + strings.Join(parts, ", ") + ")"
	case syntax.XSwitch:
		// TGParser desugars !switch into !cond while parsing, so that is the
		// text an Init tree carries.
		parts := make([]string, len(b.Cases))
		for i, c := range b.Cases {
			key := rawText(c.Key, env)
			if c.Key == nil {
				key = "true"
			}
			parts[i] = key + ": " + rawText(c.Value, env)
		}
		return "!cond(" + strings.Join(parts, ", ") + ")"
	case syntax.XForEach, syntax.XFilter, syntax.XSort:
		// The variable is a StringInit printed unquoted (UnquotedLHS).
		head := ""
		sub := bindNames(env, b.VarNames)
		if len(b.VarNames) > 0 {
			head = b.VarNames[0] + ", "
		}
		return name + "(" + head + rawText(b.Args[0], sub) + ", " + rawText(b.Args[1], sub) + ")"
	case syntax.XFoldl:
		// FoldOpInit prints (init, list, var1, var2, expr): the lambda names
		// sit before the expression, unlike the source syntax.
		sub := bindNames(env, b.VarNames)
		parts := make([]string, 0, len(b.Args)+len(b.VarNames))
		if len(b.Args) >= 2 {
			parts = append(parts, rawText(b.Args[0], env), rawText(b.Args[1], env))
			parts = append(parts, b.VarNames...)
			for _, a := range b.Args[2:] {
				parts = append(parts, rawText(a, sub))
			}
		} else {
			for _, a := range b.Args {
				parts = append(parts, rawText(a, env))
			}
		}
		return name + "(" + strings.Join(parts, ", ") + ")"
	}
	parts := make([]string, len(b.Args))
	for i, a := range b.Args {
		parts[i] = rawText(a, env)
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// bindNames installs comprehension lambda variables into a child environment so
// that a bare `a` in `!foldl(..., !add(a, b), a, b)` is not mistaken for an
// unresolved name and qualified with the record's name.
func bindNames(env *Env, names []string) *Env {
	if len(names) == 0 {
		return env
	}
	vars := make(map[string]Binding, len(names))
	for _, n := range names {
		vars[n] = Binding{}
	}
	return env.With(vars)
}

// Text renders a top-level defvar initializer the way Init::getAsString does,
// which is what DetailedRecordsBackend prints for global variables.
func (g *Global) Text() string {
	return (&Record{}).renderExpr(g.Expr, nil)
}

// constAppend mirrors the parse-time fold of `let append/prepend`: it evaluates
// only syntactic literals, so a class body shows [1, 2, 3] where LLVM's Init
// tree holds the folded list, and keeps !con(...) where LLVM keeps a BinOpInit.
func (r *Record) constAppend(x *AppendExpr) (Value, bool) {
	base, ok := r.constExpr(x.Base)
	if !ok {
		return nil, false
	}
	add, ok := r.constExpr(x.Add)
	if !ok {
		return nil, false
	}
	// TGParser::SetValue assigns directly when the current value is unset.
	if _, unset := base.(*Unset); unset {
		return add, true
	}
	if x.Prepend {
		base, add = add, base
	}
	switch x.Kind {
	case StringTy, CodeTy:
		a, ok1 := base.(*Str)
		b, ok2 := add.(*Str)
		if !ok1 || !ok2 {
			return nil, false
		}
		return &Str{S: a.S + b.S, Code: a.Code || b.Code}, true
	case ListTy:
		a, ok1 := base.(*List)
		b, ok2 := add.(*List)
		if !ok1 || !ok2 {
			return nil, false
		}
		out := &List{Elem: a.Elem}
		out.Elems = append(append([]Value(nil), a.Elems...), b.Elems...)
		return out, true
	}
	return nil, false
}

// constExpr is constAppend's view of an expression: literals and chains of
// append/prepend over them.
func (r *Record) constExpr(e Expr) (Value, bool) {
	switch x := e.(type) {
	case *AstExpr:
		return constLiteral(x.V)
	case *AppendExpr:
		return r.constAppend(x)
	}
	return nil, false
}

// constLiteral converts the literal forms of a parsed value. Anything that
// would need a record (a VarValue, a field reference, a class application, or
// a compound operator) reports false: LLVM's Fold leaves those as inits.
func constLiteral(v syntax.Value) (Value, bool) {
	switch t := v.(type) {
	case *syntax.UnsetValue:
		return &Unset{}, true
	case *syntax.IntValue:
		return &Int{N: int64(t.Num)}, true
	case *syntax.StringValue:
		return &Str{S: t.Text, Code: t.Code}, true
	case *syntax.BitsValue:
		out := make([]Value, 0, len(t.Elems))
		for i := len(t.Elems) - 1; i >= 0; i-- {
			ev, ok := constLiteral(t.Elems[i])
			if !ok {
				return nil, false
			}
			if bits, ok := ev.(*Bits); ok {
				out = append(out, bits.B...)
				continue
			}
			b, ok := ev.(*Int)
			if !ok || (b.N != 0 && b.N != 1) {
				return nil, false
			}
			out = append(out, &Bit{B: b.N == 1})
		}
		return &Bits{B: out}, true
	case *syntax.ListValue:
		out := &List{}
		for _, e := range t.Elems {
			ev, ok := constLiteral(e)
			if !ok {
				return nil, false
			}
			out.Elems = append(out.Elems, ev)
		}
		return out, true
	case *syntax.PasteValue:
		// `#` on two strings is folded by BinOpInit::getStrConcat as soon as
		// the operands parse (TGParser.cpp:3429), before any resolver runs.
		ls, ok1 := constLiteral(t.LHS)
		rs, ok2 := constLiteral(t.RHS)
		if !ok1 || !ok2 {
			return nil, false
		}
		a, ok1 := ls.(*Str)
		b, ok2 := rs.(*Str)
		if !ok1 || !ok2 {
			return nil, false
		}
		return &Str{S: a.S + b.S, Code: a.Code || b.Code}, true
	}
	return nil, false
}

// literalAs prints a concrete literal the way RecordVal::setValue stores it
// for a destination of type want: an int assigned to bits<N> becomes a BitsInit
// of N one-bit entries, and an int assigned to bit becomes that bit.
func literalAs(v syntax.Value, want syntax.Type, env *Env) (string, bool) {
	switch want.Kind {
	case syntax.BitsType:
		bits, ok := bitTexts(v, env, want.Bits, 0)
		if !ok {
			return "", false
		}
		return "{ " + strings.Join(reverseBits(bits), ", ") + " }", true
	case syntax.BitType:
		bits, ok := bitTexts(v, env, 1, 0)
		if !ok {
			return "", false
		}
		return bits[0], true
	}
	return "", false
}
