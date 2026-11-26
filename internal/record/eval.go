package record

import (
	"strconv"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// UnsupportedError is a feature the Go implementation has not reached yet. It
// is deliberately distinct from a semantic error: the driver turns it into
// E_UNSUPPORTED and exit code 2, never into a silently wrong record.
type UnsupportedError struct {
	Feature string
	Span    diag.Span
}

func (e *UnsupportedError) Error() string { return "unsupported: " + e.Feature }

func unsupported(feature string, span diag.Span) error {
	return &UnsupportedError{Feature: feature, Span: span}
}

const maxEvalDepth = 400

// eval evaluates one expression in the context of rec. want is the field type
// the result will be stored in; it is a hint for empty lists and unset values,
// not a coercion applied here (that happens in Record.evalVal).
func (k *Keeper) eval(e Expr, rec *Record, want *Ty) (Value, error) {
	if e == nil {
		return &Unset{}, nil
	}
	switch x := e.(type) {
	case *AstExpr:
		return k.evalSyntax(x.V, x.Env, rec)
	case *BindExpr:
		return k.evalBinding(x.B, x.Env, x.Rec)
	case *AppendExpr:
		return k.evalAppend(x, rec)
	case *BitsExpr:
		return k.evalBits(x, rec)
	default:
		return nil, &EvalError{Msg: "internal: unknown expression"}
	}
}

func (k *Keeper) evalAppend(x *AppendExpr, rec *Record) (Value, error) {
	base, err := k.eval(x.Base, rec, nil)
	if err != nil {
		return nil, err
	}
	add, err := k.eval(x.Add, rec, nil)
	if err != nil {
		return nil, err
	}
	if _, unset := base.(*Unset); unset {
		return add, nil
	}
	switch x.Kind {
	case StringTy, CodeTy:
		a, ok1 := base.(*Str)
		b, ok2 := add.(*Str)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "Cannot append to field of type 'string' with a non-string value"}
		}
		if x.Prepend {
			return &Str{S: b.S + a.S, Code: a.Code || b.Code}, nil
		}
		return &Str{S: a.S + b.S, Code: a.Code || b.Code}, nil
	case ListTy:
		a, ok1 := base.(*List)
		b, ok2 := add.(*List)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "Cannot append to field of type 'list' with a non-list value"}
		}
		var elems []Value
		if x.Prepend {
			elems = append(append(elems, b.Elems...), a.Elems...)
		} else {
			elems = append(append(elems, a.Elems...), b.Elems...)
		}
		return &List{Elems: elems, Elem: a.Elem}, nil
	case DagTy:
		a, ok1 := base.(*Dag)
		b, ok2 := add.(*Dag)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "Cannot append to field of type 'dag' with a non-dag value"}
		}
		if x.Prepend {
			return concatDag(b, a)
		}
		return concatDag(a, b)
	default:
		return nil, &EvalError{Msg: "Cannot append to this field type"}
	}
}

func (k *Keeper) evalBits(x *BitsExpr, rec *Record) (Value, error) {
	base, err := k.eval(x.Base, rec, nil)
	if err != nil {
		return nil, err
	}
	add, err := k.eval(x.Add, rec, nil)
	if err != nil {
		return nil, err
	}
	var bb *Bits
	switch b := base.(type) {
	case *Bits:
		bb = b
	case *Unset:
		// A declared-but-unassigned bits<N> field is all-unknown, matching
		// RecordVal::setValue turning UnsetInit into N unset bits.
		bb = &Bits{B: make([]Value, x.Width)}
		for i := range bb.B {
			bb.B[i] = &Unset{}
		}
	default:
		conv, cerr := k.coerce(base, &Ty{Kind: BitsTy, Bits: x.Width})
		if cerr != nil {
			return nil, &EvalError{Msg: "Value is not a bits type"}
		}
		bb = conv.(*Bits)
	}
	ab, err := k.coerce(add, &Ty{Kind: BitsTy, Bits: len(x.Bits)})
	if err != nil {
		return nil, &EvalError{Msg: "Initializer is not compatible with bit range"}
	}
	abits, ok := ab.(*Bits)
	if !ok {
		return nil, &EvalError{Msg: "Initializer is not compatible with bit range"}
	}
	out := Bits{B: append([]Value(nil), bb.B...)}
	for i, bit := range x.Bits {
		if int(bit) < 0 || int(bit) >= len(out.B) {
			return nil, &EvalError{Msg: "Bit range index out of bounds"}
		}
		out.B[bit] = abits.B[i]
	}
	return &out, nil
}

func (k *Keeper) evalSyntax(v syntax.Value, env *Env, rec *Record) (Value, error) {
	k.depth++
	defer func() { k.depth-- }()
	if k.depth > maxEvalDepth {
		return nil, &EvalError{Msg: "expression nesting too deep (possible cycle)", Span: v.Span()}
	}
	switch t := v.(type) {
	case *syntax.UnsetValue:
		return &Unset{}, nil
	case *syntax.IntValue:
		return &Int{N: int64(t.Num)}, nil
	case *syntax.BitsValue:
		return k.evalBitsLiteral(t, env, rec)
	case *syntax.StringValue:
		return &Str{S: t.Text, Code: t.Code}, nil
	case *syntax.ListValue:
		return k.evalListLiteral(t, env, rec)
	case *syntax.DagValue:
		return k.evalDagLiteral(t, env, rec)
	case *syntax.VarValue:
		return k.evalVar(t, env, rec)
	case *syntax.FieldValue:
		return k.evalField(t, env, rec)
	case *syntax.SliceValue:
		return k.evalSlice(t, env, rec)
	case *syntax.BitRangeValue:
		return k.evalBitRange(t, env, rec)
	case *syntax.PasteValue:
		return k.evalPaste(t, env, rec)
	case *syntax.ClassApplyValue:
		cl := k.Class(t.Class)
		if cl == nil {
			return nil, &EvalError{Msg: "Couldn't find class '" + t.Class + "'", Span: t.Span()}
		}
		r, err := k.instantiate(cl, t.Args, env, rec, t)
		if err != nil {
			return nil, err
		}
		return &DefVal{Rec: r}, nil
	case *syntax.BangValue:
		return k.evalBang(t, env, rec)
	default:
		return nil, unsupported("value expression", v.Span())
	}
}

func (k *Keeper) evalBitsLiteral(t *syntax.BitsValue, env *Env, rec *Record) (Value, error) {
	var out []Value
	for i := len(t.Elems) - 1; i >= 0; i-- {
		ev, err := k.evalSyntax(t.Elems[i], env, rec)
		if err != nil {
			return nil, err
		}
		if bits, ok := ev.(*Bits); ok {
			out = append(out, bits.B...)
			continue
		}
		bit, err := k.coerce(ev, &Ty{Kind: BitTy})
		if err != nil {
			return nil, &EvalError{Msg: "Element #" + itoa(len(t.Elems)-1-i) + " (" + ev.String() + ") is not convertable to a bit", Span: t.Elems[i].Span()}
		}
		out = append(out, bit)
	}
	return &Bits{B: out}, nil
}

func (k *Keeper) evalListLiteral(t *syntax.ListValue, env *Env, rec *Record) (Value, error) {
	var elem *Ty
	if t.ElemType != nil {
		elem = k.tyFromSyntax(*t.ElemType)
	} else if t.Type().Kind == syntax.ListType && t.Type().Elem != nil {
		elem = k.tyFromSyntax(*t.Type().Elem)
	}
	out := &List{Elem: elem}
	for _, e := range t.Elems {
		ev, err := k.evalSyntax(e, env, rec)
		if err != nil {
			return nil, err
		}
		if elem != nil {
			ev, err = k.coerce(ev, elem)
			if err != nil {
				return nil, err
			}
		}
		out.Elems = append(out.Elems, ev)
	}
	return out, nil
}

func (k *Keeper) evalDagLiteral(t *syntax.DagValue, env *Env, rec *Record) (Value, error) {
	op, err := k.evalSyntax(t.Op, env, rec)
	if err != nil {
		return nil, err
	}
	d := &Dag{Op: op, OpName: t.OpName}
	for _, a := range t.Args {
		av, err := k.evalSyntax(a.Value, env, rec)
		if err != nil {
			return nil, err
		}
		d.Args = append(d.Args, DagArg{Value: av, Name: a.Name})
	}
	return d, nil
}

func (k *Keeper) evalVar(t *syntax.VarValue, env *Env, rec *Record) (Value, error) {
	if t.Subst && t.Lit != nil {
		return k.evalSyntax(t.Lit, env, rec)
	}
	if t.GlobalRef {
		// A name that named a top-level definition when the file was parsed is
		// a DefInit in LLVM and is never re-bound by a record field of the
		// same name.
		if val, ok, err := k.Global(t.Name); ok {
			return val, err
		}
		if d := k.defs[t.Name]; d != nil {
			return &DefVal{Rec: d}, nil
		}
	}
	if !t.Lexical {
		if q, ok := env.Qualify(t.Name); ok {
			if b, ok := env.Lookup(q); ok {
				return k.evalBinding(b, env, rec)
			}
		}
	}
	if b, ok := env.Lookup(t.Name); ok {
		return k.evalBinding(b, env, rec)
	}
	if rec != nil {
		if rv := rec.Val(t.Name); rv != nil {
			v, err := rec.evalVal(rv)
			if err != nil {
				return nil, err
			}
			return unresolvedRef(t.Name, rv, v), nil
		}
	}
	if k.parseFold && t.Lit != nil && !t.Subst && !t.Lexical {
		// The parser inlines a field's initializer into Lit so type checking
		// works, but LLVM's parse-time resolver still sees VarInit(field):
		// VarDefInit::Fold uses an empty resolver and AddParentClass's
		// MapResolver maps only the class's template arguments. (A record's
		// own fields are looked up above, so assert checking still works.)
		return nil, &EvalError{Msg: "Variable not defined: '" + t.Name + "'", Span: t.Span()}
	}
	if t.Lit != nil {
		return k.evalSyntax(t.Lit, env, rec)
	}
	if val, ok, err := k.Global(t.Name); ok {
		return val, err
	}
	if d := k.defs[t.Name]; d != nil {
		return &DefVal{Rec: d}, nil
	}
	if rec != nil && rec.Name == t.Name {
		return &DefVal{Rec: rec}, nil
	}
	if rec != nil && t.Name == "NAME" {
		return &Str{S: rec.Name}, nil
	}
	return nil, &EvalError{Msg: "Variable not defined: '" + t.Name + "'", Span: t.Span()}
}

// unresolvedRef turns "I referenced a field that is still unset" into the
// VarInit (or VarBitInit) llvm-tblgen keeps. A chain of references collapses
// to the innermost name, matching RecordResolver::resolve walking the chain.
// Only references go through here; a field's own unset value stays Unset so
// --dump-json prints null for it.
func unresolvedRef(name string, rv *RecordVal, v Value) Value {
	switch x := v.(type) {
	case *Unset:
		u := &Unresolved{Text: name, Kind: "var", Var: name, Ty: rv.Ty}
		if rv.Ty != nil && rv.Ty.Kind == BitsTy {
			u.Width = rv.Ty.Bits
		}
		return u
	case *Bits:
		// A bits<N> reference expands to one VarBitInit per bit; the bits that
		// are still unset keep the reference, the others become their value
		// (BitsInit::resolveReferences with keepUnsetBits).
		out := Bits{B: make([]Value, len(x.B))}
		for i, b := range x.B {
			if _, unset := b.(*Unset); unset {
				out.B[i] = &BitRef{Var: name, Index: i, Value: b}
			} else {
				out.B[i] = b
			}
		}
		return &out
	case *Unresolved:
		return x
	}
	return v
}

func (k *Keeper) evalBinding(b Binding, env *Env, rec *Record) (Value, error) {
	v, err := k.evalBindingInner(b, env, rec)
	if err != nil {
		return nil, err
	}
	if b.Ty != nil {
		return k.coerce(v, b.Ty)
	}
	return v, nil
}

func (k *Keeper) evalBindingInner(b Binding, env *Env, rec *Record) (Value, error) {
	if b.Val != nil {
		return b.Val, nil
	}
	if b.V == nil {
		return &Unset{}, nil
	}
	if b.Dynamic {
		e := env.With(nil)
		e.owner = b.Owner
		e.ownerSep = b.Sep
		if b.Self != "" {
			if e.hidden == nil {
				e.hidden = map[string]bool{}
			}
			e.hidden[b.Self] = true
		}
		return k.evalSyntax(b.V, e, rec)
	}
	ctx := rec
	if b.Rec != nil {
		ctx = b.Rec
	}
	return k.evalSyntax(b.V, b.Env, ctx)
}

func (k *Keeper) evalField(t *syntax.FieldValue, env *Env, rec *Record) (Value, error) {
	base, err := k.evalSyntax(t.Base, env, rec)
	if err != nil {
		return nil, err
	}
	switch b := base.(type) {
	case *DefVal:
		if rec != nil && b.Rec == rec && !rec.IsClass {
			return nil, &EvalError{
				Msg:      "Attempting to access field '" + t.Field + "' of '" + rec.Name + "' is a forbidden self-reference",
				Span:     t.FieldSpan,
				AtRecord: true,
			}
		}
		v, err := b.Rec.Field(t.Field)
		if err != nil {
			if ee, ok := err.(*EvalError); ok && ee.Span.Start.File == "" {
				ee.Span = t.FieldSpan
			}
			return nil, err
		}
		return v, nil
	case *Unset:
		return nil, &EvalError{Msg: "Value of field '" + t.Field + "' is unknown", Span: t.FieldSpan}
	case *Unresolved:
		return &Unresolved{Text: b.Text + "." + t.Field}, nil
	default:
		return nil, &EvalError{Msg: "Cannot access field '" + t.Field + "' on a non-record value", Span: t.FieldSpan}
	}
}

func (k *Keeper) evalSlice(t *syntax.SliceValue, env *Env, rec *Record) (Value, error) {
	base, err := k.evalSyntax(t.Base, env, rec)
	if err != nil {
		return nil, err
	}
	// Single index on a list or string.
	if len(t.Index) == 1 && t.Index[0].Hi == nil && !t.ListResult {
		if ls, ok := base.(*List); ok {
			idx, err := k.evalSyntax(t.Index[0].Lo, env, rec)
			if err != nil {
				return nil, err
			}
			if u, ok := idx.(*Unresolved); ok {
				// ListInit::convertInitializerToIndex makes a LISTELEM, which
				// stays as an Init when the index is unresolved.
				return &Unresolved{Text: printableOf(base) + "[" + u.Text + "]"}, nil
			}
			vals, err := k.evalIndexEntryValue(idx, ls, t.Index[0].Span())
			if err != nil {
				return nil, err
			}
			if len(vals) == 1 {
				return vals[0], nil
			}
			return &List{Elems: vals, Elem: ls.Elem}, nil
		}
		idx, err := k.evalInt(t.Index[0].Lo, env, rec)
		if err != nil {
			return nil, err
		}
		return indexValue(base, idx, t.Span())
	}
	// General slice: a list of indices/ranges.
	ls, ok := base.(*List)
	if !ok {
		return nil, unsupported("slice on non-list value", t.Span())
	}
	out := &List{Elem: ls.Elem}
	for _, se := range t.Index {
		if se.Hi == nil {
			vals, err := k.evalIndexEntry(se.Lo, ls, se.Span(), env, rec)
			if err != nil {
				return nil, err
			}
			out.Elems = append(out.Elems, vals...)
			continue
		}
		lo, err := k.evalInt(se.Lo, env, rec)
		if err != nil {
			return nil, err
		}
		hi, err := k.evalInt(se.Hi, env, rec)
		if err != nil {
			return nil, err
		}
		step := int64(1)
		if hi < lo {
			step = -1
		}
		for i := lo; ; i += step {
			v, err := indexValue(ls, i, se.Span())
			if err != nil {
				return nil, err
			}
			out.Elems = append(out.Elems, v)
			if i == hi {
				break
			}
		}
	}
	return out, nil
}

// evalIndexEntry evaluates one entry inside a list slice. The entry is either
// an int index or a list of int indices (list[i, j] / list[someList]).
func (k *Keeper) evalIndexEntry(v syntax.Value, ls *List, span diag.Span, env *Env, rec *Record) ([]Value, error) {
	ev, err := k.evalSyntax(v, env, rec)
	if err != nil {
		return nil, err
	}
	return k.evalIndexEntryValue(ev, ls, span)
}

func (k *Keeper) evalIndexEntryValue(ev Value, ls *List, span diag.Span) ([]Value, error) {
	if idxs, ok := ev.(*List); ok {
		out := make([]Value, 0, len(idxs.Elems))
		for _, e := range idxs.Elems {
			n, err := k.asInt(e)
			if err != nil {
				return nil, err
			}
			x, err := indexValue(ls, n, span)
			if err != nil {
				return nil, err
			}
			out = append(out, x)
		}
		return out, nil
	}
	n, err := k.asInt(ev)
	if err != nil {
		return nil, err
	}
	x, err := indexValue(ls, n, span)
	if err != nil {
		return nil, err
	}
	return []Value{x}, nil
}

func indexValue(base Value, idx int64, span diag.Span) (Value, error) {
	switch b := base.(type) {
	case *List:
		if idx < 0 || int(idx) >= len(b.Elems) {
			return nil, &EvalError{Msg: "list index out of range", Span: span}
		}
		return b.Elems[idx], nil
	case *Str:
		if idx < 0 || int(idx) >= len(b.S) {
			return nil, &EvalError{Msg: "string index out of range", Span: span}
		}
		return &Str{S: b.S[idx : idx+1], Code: b.Code}, nil
	default:
		return nil, &EvalError{Msg: "value is not indexable", Span: span}
	}
}

func (k *Keeper) evalBitRange(t *syntax.BitRangeValue, env *Env, rec *Record) (Value, error) {
	base, err := k.evalSyntax(t.Base, env, rec)
	if err != nil {
		return nil, err
	}
	if u, ok := base.(*Unresolved); ok && u.Kind == "var" && u.Width > 0 {
		out := Bits{B: make([]Value, len(t.Bits))}
		for i := len(t.Bits) - 1; i >= 0; i-- {
			n := int(t.Bits[i])
			if n < 0 || n >= u.Width {
				return nil, &EvalError{Msg: "Bit range index out of bounds", Span: t.Span()}
			}
			out.B[len(t.Bits)-1-i] = &Unresolved{
				Text:  u.Var + "{" + strconv.Itoa(n) + "}",
				Kind:  "varbit",
				Var:   u.Var,
				Index: n,
				Width: u.Width,
				Ty:    &Ty{Kind: BitTy},
			}
		}
		return &out, nil
	}
	bits, ok := base.(*Bits)
	if !ok {
		return nil, &EvalError{Msg: "Value is not a bits type", Span: t.Span()}
	}
	out := Bits{B: make([]Value, len(t.Bits))}
	for i := len(t.Bits) - 1; i >= 0; i-- {
		bit := t.Bits[i]
		if int(bit) < 0 || int(bit) >= len(bits.B) {
			return nil, &EvalError{Msg: "Bit range index out of bounds", Span: t.Span()}
		}
		out.B[len(t.Bits)-1-i] = bits.B[bit]
	}
	return &out, nil
}

func (k *Keeper) evalPaste(t *syntax.PasteValue, env *Env, rec *Record) (Value, error) {
	lhs, err := k.evalSyntax(t.LHS, env, rec)
	if err != nil {
		return nil, err
	}
	rhs, err := k.evalSyntax(t.RHS, env, rec)
	if err != nil {
		return nil, err
	}
	// TGParser: a list on the LHS makes this a list concat; everything else
	// is string concat after casting both sides to string.
	if a, ok := lhs.(*List); ok {
		b, ok := rhs.(*List)
		if !ok {
			if isUnresolved(rhs) {
				return &Unresolved{
					Text: "!listconcat(" + printableOf(lhs) + ", " + printableOf(rhs) + ")",
					Ty:   &Ty{Kind: ListTy, Elem: a.Elem},
				}, nil
			}
			return nil, &EvalError{Msg: "Cannot paste list with non-list", Span: t.Span()}
		}
		elems := append(append([]Value(nil), a.Elems...), b.Elems...)
		return &List{Elems: elems, Elem: a.Elem}, nil
	}
	if isUnresolved(lhs) || isUnresolved(rhs) {
		if isListLike(lhs) || isListLike(rhs) {
			// Paste of lists is BinOpInit::LISTCONCAT, not a string concat.
			return &Unresolved{
				Text: "!listconcat(" + printableOf(lhs) + ", " + printableOf(rhs) + ")",
				Ty:   &Ty{Kind: ListTy},
			}, nil
		}
		// TGParser wraps each non-string paste operand in !cast<string>; only
		// when that cast could not fold does the BinOpInit survive.
		left := pasteOperand(lhs)
		right := pasteOperand(rhs)
		return &Unresolved{Text: "!strconcat(" + left + ", " + right + ")", Ty: &Ty{Kind: StringTy}}, nil
	}
	ls, ok1 := stringValue(lhs)
	rs, ok2 := stringValue(rhs)
	if !ok1 || !ok2 {
		return nil, &EvalError{Msg: "Cannot paste this value", Span: t.Span()}
	}
	return &Str{S: ls + rs, Code: isCode(lhs) || isCode(rhs)}, nil
}

// pasteOperand renders one side of `#` the way the surviving Init prints:
// strings as-is, unresolved values inside the !cast<string> the parser added.
func pasteOperand(v Value) string {
	if u, ok := v.(*Unresolved); ok {
		return "!cast<string>(" + u.Text + ")"
	}
	return v.String()
}

func isListLike(v Value) bool {
	switch x := v.(type) {
	case *List:
		return true
	case *Unresolved:
		return x.Ty != nil && x.Ty.Kind == ListTy
	}
	return false
}

func stringValue(v Value) (string, bool) {
	switch x := v.(type) {
	case *Str:
		return x.S, true
	case *Int:
		return x.String(), true
	case *Bit:
		return x.String(), true
	case *Bits:
		return x.String(), true
	case *DefVal:
		return x.Rec.Name, true
	}
	return "", false
}

func isCode(v Value) bool {
	s, ok := v.(*Str)
	return ok && s.Code
}

func concatDag(a, b *Dag) (Value, error) {
	ad, aok := a.Op.(*DefVal)
	bd, bok := b.Op.(*DefVal)
	if aok && bok && ad.Rec != bd.Rec {
		return nil, &EvalError{Msg: "Concatenated Dag operators do not match"}
	}
	op := a.Op
	if !aok && bok {
		op = b.Op
	}
	out := &Dag{Op: op, OpName: a.OpName}
	if out.OpName == "" {
		out.OpName = b.OpName
	}
	out.Args = append(append([]DagArg(nil), a.Args...), b.Args...)
	return out, nil
}

func (k *Keeper) evalInt(v syntax.Value, env *Env, rec *Record) (int64, error) {
	val, err := k.evalSyntax(v, env, rec)
	if err != nil {
		return 0, err
	}
	n, err := k.asInt(val)
	if err != nil {
		if ee, ok := err.(*EvalError); ok && ee.Span.Start.File == "" {
			ee.Span = v.Span()
		}
		return 0, err
	}
	return n, nil
}

func (k *Keeper) asInt(v Value) (int64, error) {
	switch x := v.(type) {
	case *Int:
		return x.N, nil
	case *Bit:
		if x.B {
			return 1, nil
		}
		return 0, nil
	case *Bits:
		if n, ok := bitsToInt(*x); ok {
			return n, nil
		}
		return 0, &EvalError{Msg: "bits value has unresolved bits"}
	default:
		return 0, &EvalError{Msg: "expected int, got " + v.Type().String()}
	}
}

// instantiate creates an anonymous def from a class application. It mirrors
// VarDefInit::instantiate: copy the class fields, bind the class template
// arguments, resolve, then register.
func (k *Keeper) instantiate(class *Record, args []*syntax.Arg, env *Env, caller *Record, ca *syntax.ClassApplyValue) (*Record, error) {
	provided, err := k.bindArgs(class, args, env, caller, ca.Span().Start, false)
	if err != nil {
		return nil, err
	}
	// VarDefInit is uniqued by class and resolved argument values, so two
	// textual applications with equal arguments share one anonymous def.
	key := class.Name
	for i, a := range args {
		name := a.Name
		pos := "n"
		if name == "" {
			name = class.TArgs[i].Name
			pos = strconv.Itoa(i)
		}
		v, err := k.evalBinding(provided[class.Name+":"+name], env, caller)
		if err != nil {
			return nil, err
		}
		key += "\x00" + pos + ":" + name + "=" + valueKey(v)
	}
	if cached := k.anonCache[key]; cached != nil {
		return cached, nil
	}
	rec := k.newAnonymous(k.siteFor(ca, env, key))
	k.anonCache[key] = rec
	rec.Super = append(rec.Super, class)
	defaults := classDefaultBindings(class, provided)
	for _, f := range class.Vals {
		nv := &RecordVal{Name: f.Name, Ty: f.Ty, Nonconcrete: f.Nonconcrete, Loc: f.Loc}
		nv.Expr = rebindFrames(f.Expr, defaults, provided)
		rec.addVal(nv)
	}
	for _, as := range class.Asserts {
		rec.Asserts = append(rec.Asserts, rebindAssertFrames(as, defaults, provided))
	}
	if err := rec.resolveAll(); err != nil {
		// A pre-parse fold may have swallowed this error; do not leave a
		// half-built instance behind for the real evaluation to reuse.
		delete(k.anonCache, key)
		return nil, err
	}
	if err := k.addDef(rec); err != nil {
		delete(k.anonCache, key)
		return nil, err
	}
	// VarDefInit::instantiate checks assertions after the record is added.
	for _, as := range rec.Asserts {
		k.checkAssertion(as, rec)
	}
	return rec, nil
}

// classDefaultBindings collects the class's own defaulted template arguments
// so an instance resolves them through the class frame. A field copied from a
// parent class keeps the environment it was written in, which does not contain
// this class's frame; without these bindings `class Spelling<...> : Versioned<min>`
// cannot resolve `min` during a strict pre-fold.
func classDefaultBindings(class *Record, provided map[string]Binding) map[string]Binding {
	if class.Env == nil {
		return nil
	}
	var out map[string]Binding
	prefix := class.Name + ":"
	for _, f := range class.Env.frames {
		for k, b := range f {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if _, ok := provided[k]; ok {
				continue
			}
			if out == nil {
				out = map[string]Binding{}
			}
			out[k] = b
		}
	}
	return out
}

// applyClass copies a class's fields into an existing def or class and records
// the direct superclass.
func (k *Keeper) applyClass(target *Record, class *Record, args []*syntax.Arg, env *Env, caller *Record, loc diag.Pos) error {
	provided, err := k.bindArgs(class, args, env, caller, loc, target.IsClass)
	if err != nil {
		return err
	}
	for _, f := range class.Vals {
		nv := &RecordVal{Name: f.Name, Ty: f.Ty, Nonconcrete: f.Nonconcrete, Loc: f.Loc}
		nv.Expr = rebind(f.Expr, provided)
		target.addVal(nv)
	}
	// Record::appendAssertions: the parent's assertions become the child's and
	// are only evaluated once a concrete def is finished, so that lets applied
	// after the parent list are visible to them (Target.td's SubRegs check
	// reads SubRegIndices, which targets usually set with `let`).
	for _, as := range class.Asserts {
		target.Asserts = append(target.Asserts, rebindAssert(as, provided))
	}
	// AddParentClass resolves the values it just copied with the class
	// arguments bound, right here and before the body is parsed. That is when
	// a concrete `Class<...>` application inside the class body instantiates
	// its anonymous record (and gets its anonymous_N name), even if a later
	// `let` replaces the field. Reproduce the side effect only; the copied
	// fields stay lazy so the final values are resolved as before.
	stand := k.preFoldArgs(target.Name, class, provided)
	for _, f := range class.Vals {
		// Unconditional: the class application we care about may sit inside a
		// binding this field only names (e.g. Intrinsic's IntrProperties is
		// `intr_properties`, whose argument expression mentions
		// AMDGPUImageDimIntrinsicEval), so a syntactic check on the field
		// expression itself is not enough.
		k.preFoldExprBound(rebind(f.Expr, provided), stand, false)
	}
	target.Super = append(target.Super, class)
	return nil
}

// preFoldArgs builds the record that AddParentClass's MapResolver exposes to a
// class body: NAME plus every template argument under its bare name, evaluated
// through the binding that bindArgs produced. Passing a real record (instead
// of an Env frame) lets references resolve even when they were captured in a
// different environment, e.g. inside the argument expression of a parent
// class applied while the class itself was parsed.
func (k *Keeper) preFoldArgs(name string, class *Record, provided map[string]Binding) *Record {
	rec := &Record{Name: name, keeper: k}
	for _, ta := range class.TArgs {
		b, ok := provided[class.Name+":"+ta.Name]
		if !ok {
			continue
		}
		rec.addVal(&RecordVal{Name: ta.Name, Ty: ta.Ty, Loc: ta.Loc, Expr: &BindExpr{B: b, Env: b.Env, Rec: b.Rec}})
	}
	return rec
}

// preFoldExpr is preFold for an already-bound expression: evaluate it just far
// enough to let a concrete class application instantiate, and throw the result
// away. name carries the record's name so that `NAME` resolves the way
// MapResolver resolves the class's implicit name; fields stay unresolved.
func (k *Keeper) preFoldExpr(e Expr, rec *Record) {
	var v syntax.Value
	var env *Env
	switch x := e.(type) {
	case *AstExpr:
		v, env = x.V, x.Env
	case *AppendExpr:
		k.preFoldExpr(x.Base, rec)
		k.preFoldExpr(x.Add, rec)
		return
	case *BitsExpr:
		k.preFoldExpr(x.Base, rec)
		k.preFoldExpr(x.Add, rec)
		return
	default:
		return
	}
	if !hasClassApply(v) {
		return
	}
	name := ""
	if rec != nil {
		name = rec.Name
	}
	k.preFoldExprBound(&AstExpr{V: v, Env: env}, &Record{Name: name, keeper: k}, true)
}

// preFoldExprBound is preFoldExpr against a caller-supplied stand-in record.
// A class application buried in a binding is invisible to a syntactic check,
// so callers can pass requireApply=false to always evaluate.
func (k *Keeper) preFoldExprBound(e Expr, rec *Record, requireApply bool) {
	var v syntax.Value
	var env *Env
	switch x := e.(type) {
	case *AstExpr:
		v, env = x.V, x.Env
	case *AppendExpr:
		k.preFoldExprBound(x.Base, rec, requireApply)
		k.preFoldExprBound(x.Add, rec, requireApply)
		return
	case *BitsExpr:
		k.preFoldExprBound(x.Base, rec, requireApply)
		k.preFoldExprBound(x.Add, rec, requireApply)
		return
	default:
		return
	}
	if requireApply && !hasClassApply(v) {
		return
	}
	prev := k.parseFold
	k.parseFold = true
	_, _ = k.evalSyntax(v, env, rec)
	k.parseFold = prev
}

// checkAssertion evaluates one deferred class assertion against a concrete
// record. LLVM prints the failed expression at the assert site and a second
// error at the record that inherited it.
func (k *Keeper) checkAssertion(as Assertion, rec *Record) {
	cond, err := k.evalSyntax(as.Cond, as.Env, rec)
	if err != nil {
		k.diags.Error("E_ASSERT", err.Error(), as.Cond.Span())
		return
	}
	n, err := k.asInt(cond)
	if err != nil {
		k.diags.Error("E_ASSERT", err.Error(), as.Cond.Span())
		return
	}
	if n != 0 {
		return
	}
	msg := "(assert message is not a string)"
	if mv, err := k.evalSyntax(as.Message, as.Env, rec); err == nil {
		if s, ok := mv.(*Str); ok {
			msg = s.S
		}
	}
	k.diags.Error("E_ASSERT", "assertion failed: "+msg, as.Cond.Span())
	if len(rec.Locs) > 0 {
		at := rec.Locs[0]
		k.diags.Error("E_ASSERT_RECORD", "assertion failed in this record", diag.Span{Start: at, End: at})
	}
}

// bindArgs turns an argument list into a frame of lexical bindings. Missing
// arguments fall back to the class's own dynamic defaults, which live in the
// class expression's Env.
func (k *Keeper) bindArgs(class *Record, args []*syntax.Arg, env *Env, caller *Record, loc diag.Pos, dynamic bool) (map[string]Binding, error) {
	if len(args) > len(class.TArgs) {
		return nil, &EvalError{Msg: "Too many template arguments for class '" + class.Name + "'", Span: diag.Span{Start: loc, End: loc}}
	}
	provided := map[string]Binding{}
	for i, a := range args {
		name := a.Name
		if name == "" {
			if i >= len(class.TArgs) {
				return nil, &EvalError{Msg: "Too many template arguments for class '" + class.Name + "'", Span: a.Span()}
			}
			name = class.TArgs[i].Name
		}
		key := class.Name + ":" + name
		if _, dup := provided[key]; dup {
			return nil, &EvalError{Msg: "We can only specify the template argument '" + name + "' once", Span: a.Span()}
		}
		b := Binding{V: a.Value, Env: env, Rec: caller, Dynamic: dynamic, Ty: class.TArgs[i].Ty}
		if name != class.TArgs[i].Name {
			if ta := findTArg(class, name); ta != nil {
				b.Ty = ta.Ty
			}
		}
		if env != nil {
			b.Owner = env.owner
			b.Sep = env.ownerSep
		}
		if err := k.checkValueType(b.Ty, a.Value, caller); err != nil {
			return nil, err
		}
		// A class application passed as a template argument is a VarDefInit:
		// CheckTemplateArgValues casts it to the declared type, which
		// instantiates the anonymous record right here (and gives it its
		// anonymous_N name) rather than on first use.
		if ca, ok := a.Value.(*syntax.ClassApplyValue); ok && !dynamic {
			if cl := k.Class(ca.Class); cl != nil {
				val, err := k.evalSyntax(ca, env, caller)
				if err != nil {
					return nil, err
				}
				b.V, b.Env, b.Rec = nil, nil, nil
				b.Val = val
			}
		}
		provided[key] = b
	}
	for _, ta := range class.TArgs {
		if _, ok := provided[class.Name+":"+ta.Name]; ok {
			continue
		}
		// Defaults are resolved when the class is inherited, so an anonymous
		// class application in a default exists before any field uses it.
		if ax, ok := ta.Expr.(*AstExpr); ok {
			if ca, ok := ax.V.(*syntax.ClassApplyValue); ok && k.Class(ca.Class) != nil {
				val, err := k.evalSyntax(ca, ax.Env, caller)
				if err != nil {
					return nil, err
				}
				provided[class.Name+":"+ta.Name] = Binding{Val: val, Ty: ta.Ty}
				continue
			}
		}
		if ta.Expr == nil {
			return nil, &EvalError{Msg: "value not specified for template argument '" + ta.Name + "'", Span: diag.Span{Start: loc, End: loc}}
		}
		if ax, ok := ta.Expr.(*AstExpr); ok {
			if _, unset := ax.V.(*syntax.UnsetValue); unset {
				return nil, &EvalError{Msg: "value not specified for template argument '" + ta.Name + "'", Span: diag.Span{Start: loc, End: loc}}
			}
		}
	}
	return provided, nil
}

// unwrapBitRef resolves a VarBitInit to the bit it points at, which is what
// VarBitInit::resolveReferences leaves behind (an unset bit stays Unset).
func unwrapBitRef(v Value) Value {
	if br, ok := v.(*BitRef); ok {
		return br.Value
	}
	return v
}

// castUnresolved applies the implicit `!cast<T>` TGParser inserts when a
// non-concrete value is stored in a field of another type (TypedInit::
// getCastTo): a bits<N> target becomes one VarBitInit per bit over the cast, a
// scalar target keeps a single cast node. When the types already agree the
// cast folds away, exactly like UnOpInit::Fold(CAST).
func (k *Keeper) castUnresolved(u *Unresolved, want *Ty) (Value, error) {
	if want == nil || want.Kind == AnyTy || u.Ty == nil || u.Ty.Kind == AnyTy {
		return u, nil
	}
	if u.Ty.IsA(want) {
		return u, nil
	}
	if !u.Ty.ConvertibleTo(want) {
		// checkValueType has already rejected the mismatched declared types;
		// keep the value so the diagnostic points at the initializer.
		return u, nil
	}
	if want.Kind == BitsTy {
		if u.Ty.Kind == BitTy || u.Ty.Kind == BitsTy {
			// VarInit::getBit on a bit-typed var returns the var itself, so
			// `bits<1> x = p` is a BitsInit holding the VarInit. The JSON
			// prints the reference, not a cast.
			out := &Bits{B: make([]Value, want.Bits)}
			for i := range out.B {
				out.B[i] = u
			}
			return out, nil
		}
		// An int stored in bits<N> goes through !cast<bits<N>>, and each bit
		// of that cast is a VarBitInit whose base is the cast, not a var.
		cast := "!cast<" + want.String() + ">(" + u.Text + ")"
		out := &Bits{B: make([]Value, want.Bits)}
		for i := range out.B {
			out.B[i] = &Unresolved{Text: cast + "{" + itoa(i) + "}", Ty: &Ty{Kind: BitTy}}
		}
		return out, nil
	}
	cast := "!cast<" + want.String() + ">(" + u.Text + ")"
	return &Unresolved{Text: cast, Ty: want}, nil
}

// coerce converts v to the declared field type. It is the small, explicit
// subset of Init::convertInitializerTo that TableGen fields actually use.
func (k *Keeper) coerce(v Value, want *Ty) (Value, error) {
	if want == nil || want.Kind == AnyTy {
		return v, nil
	}
	if u, unresolved := v.(*Unresolved); unresolved {
		return k.castUnresolved(u, want)
	}
	if br, ok := v.(*BitRef); ok && want != nil && want.Kind != BitsTy {
		// VarBitInit::resolveReferences replaces the reference with the bit it
		// points at; an unset bit reads as Unset.
		return br.Value, nil
	}
	if _, unset := v.(*Unset); unset {
		if want.Kind == BitsTy {
			// RecordVal::setValue expands an unset bits field into one unset
			// bit per declared bit, which is also what the JSON backend prints.
			out := &Bits{B: make([]Value, want.Bits)}
			for i := range out.B {
				out.B[i] = &Unset{}
			}
			return out, nil
		}
		return v, nil
	}
	if v.Type().IsA(want) {
		return v, nil
	}
	switch want.Kind {
	case BitTy:
		switch x := v.(type) {
		case *Int:
			if x.N == 0 || x.N == 1 {
				return &Bit{B: x.N == 1}, nil
			}
		case *Bits:
			// BitsInit::convertInitializerTo(BitRecTy): only a single bit.
			if len(x.B) == 1 {
				return unwrapBitRef(x.B[0]), nil
			}
		}
	case IntTy:
		switch x := v.(type) {
		case *Bit:
			if x.B {
				return &Int{N: 1}, nil
			}
			return &Int{N: 0}, nil
		case *Bits:
			if n, ok := bitsToInt(*x); ok {
				return &Int{N: n}, nil
			}
		}
	case BitsTy:
		switch x := v.(type) {
		case *Int:
			b, ok := bitsFromInt(x.N, want.Bits)
			if ok {
				return &b, nil
			}
		case *Bit:
			// BitInit::convertInitializerTo(BitsRecTy) only accepts width 1.
			if want.Bits == 1 {
				return &Bits{B: []Value{x}}, nil
			}
		case *Bits:
			if len(x.B) == want.Bits {
				return x, nil
			}
		}
	case StringTy, CodeTy:
		if x, ok := v.(*Str); ok {
			return x, nil
		}
	case ListTy:
		if x, ok := v.(*List); ok {
			elem := want.Elem
			if elem == nil {
				return x, nil
			}
			out := &List{Elem: elem}
			for _, e := range x.Elems {
				ce, err := k.coerce(e, elem)
				if err != nil {
					return nil, err
				}
				out.Elems = append(out.Elems, ce)
			}
			return out, nil
		}
	case DagTy:
		if x, ok := v.(*Dag); ok {
			return x, nil
		}
	case RecordTy:
		if x, ok := v.(*DefVal); ok {
			if x.Type().ConvertibleTo(want) {
				return x, nil
			}
		}
	}
	// TypedInit::getCastTo (Record.cpp:2262) does not fail when the value
	// cannot be converted *today* but its type is convertible: it keeps a
	// !cast node instead. RecordVal::setValue then expands that node into one
	// VarBitInit per declared bit, which is why an out-of-range literal is
	// reported later by checkConcrete as an unresolved initializer rather than
	// here as a type error.
	if want.Kind == BitsTy && v.Type().ConvertibleTo(want) {
		text := "!cast<" + want.String() + ">(" + v.String() + ")"
		out := &Bits{B: make([]Value, want.Bits)}
		for i := range out.B {
			out.B[i] = &Unresolved{Text: text + "{" + itoa(i) + "}"}
		}
		return out, nil
	}
	return nil, &EvalError{Msg: "value of type '" + v.Type().String() + "' is incompatible with '" + want.String() + "'"}
}

// findTArg locates a template argument by name (named arguments may be given
// out of order).
func findTArg(class *Record, name string) *RecordVal {
	for _, ta := range class.TArgs {
		if ta.Name == name {
			return ta
		}
	}
	return nil
}

// valueKey is a deterministic structural key used to emulate VarDefInit
// uniquing: equal resolved argument values mean one anonymous def.
func valueKey(v Value) string {
	var b strings.Builder
	writeValueKey(&b, v)
	return b.String()
}

func writeValueKey(b *strings.Builder, v Value) {
	switch x := v.(type) {
	case nil:
		b.WriteString("nil")
	case *Unset:
		b.WriteString("?")
	case *Int:
		b.WriteString("i")
		b.WriteString(strconv.FormatInt(x.N, 10))
		b.WriteByte(';')
	case *Bit:
		if x.B {
			b.WriteString("b1")
		} else {
			b.WriteString("b0")
		}
	case *Bits:
		b.WriteString("B")
		b.WriteString(strconv.Itoa(len(x.B)))
		b.WriteByte('[')
		for _, e := range x.B {
			writeValueKey(b, e)
		}
		b.WriteByte(']')
	case *Str:
		if x.Code {
			b.WriteString("c")
		} else {
			b.WriteString("s")
		}
		b.WriteString(strconv.Itoa(len(x.S)))
		b.WriteByte(':')
		b.WriteString(x.S)
	case *List:
		b.WriteString("L<")
		if x.Elem != nil {
			b.WriteString(x.Elem.String())
		}
		b.WriteByte('[')
		for _, e := range x.Elems {
			writeValueKey(b, e)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case *Dag:
		b.WriteString("D(")
		writeValueKey(b, x.Op)
		b.WriteByte(':')
		b.WriteString(x.OpName)
		for _, a := range x.Args {
			b.WriteByte('|')
			writeValueKey(b, a.Value)
			b.WriteByte(':')
			b.WriteString(a.Name)
		}
		b.WriteByte(')')
	case *DefVal:
		b.WriteString("R")
		b.WriteString(x.Rec.Name)
	default:
		b.WriteString("<unknown>")
	}
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
