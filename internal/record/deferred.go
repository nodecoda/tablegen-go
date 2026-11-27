package record

import (
	"sort"
	"strings"

	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// Deferred-value support: llvm-tblgen does not evaluate records eagerly. It
// keeps Init trees and folds what it can, so an operator with an operand that
// is still unknown survives into the record and --dump-json prints it as a
// "complex" Init (a reference prints as "var"/"varbit"). This file mirrors
// that: when an operand cannot be folded, the operator returns an Unresolved
// carrying the same printing llvm-tblgen would produce.
//
// The rules below follow Record.cpp's *Init::Fold / *Init::resolveReferences:
//   - operands are evaluated first, so anything that folds prints folded
//     (!add(!mul(2,3), u) prints "!add(6, u)");
//   - !and/!or short-circuit before looking at the right operand;
//   - !if/!cond/!switch only defer when the selector itself is unknown;
//   - !foreach folds over a concrete list even when the element values are
//     unknown, but !filter/!sort keep the whole operator when a body value
//     does not evaluate to int/string.

func isUnresolved(v Value) bool {
	_, ok := v.(*Unresolved)
	return ok
}

func anyUnresolved(vals []Value) bool {
	for _, v := range vals {
		if isUnresolved(v) {
			return true
		}
	}
	return false
}

// printableOf is Init::getAsString for an evaluated Value.
func printableOf(v Value) string {
	if u, ok := v.(*Unresolved); ok {
		return u.Text
	}
	return v.String()
}

// deferBang handles the operators whose value cannot be folded because an
// operand is unresolved. handled=false means the caller must run the normal
// evaluator (the value folded normally). vals holds operands this call already
// evaluated so the caller does not evaluate them again; it is nil when they
// are unavailable or not reusable.
func (k *Keeper) deferBang(b *syntax.BangValue, env *Env, rec *Record) (Value, []Value, bool, error) {
	eval := func(i int) (Value, error) { return k.evalSyntax(b.Args[i], env, rec) }

	switch b.Op {
	case syntax.XIf:
		cond, err := eval(0)
		if err != nil {
			return nil, nil, true, err
		}
		if !isUnresolved(cond) {
			return nil, nil, false, nil
		}
		mhs, err := eval(1)
		if err != nil {
			return nil, nil, true, err
		}
		rhs, err := eval(2)
		if err != nil {
			return nil, nil, true, err
		}
		return deferredCall(b, []Value{cond, mhs, rhs}, nil), nil, true, nil

	case syntax.XCond:
		v, handled, err := k.deferCond(b, env, rec)
		return v, nil, handled, err

	case syntax.XForEach, syntax.XFilter, syntax.XSort:
		list, err := eval(0)
		if err != nil {
			return nil, nil, true, err
		}
		deferred := isUnresolved(list)
		if !deferred && b.Op != syntax.XForEach {
			// !filter/!sort need every body result to be an int or string;
			// otherwise TernOpInit::Fold bails out and the operator survives.
			// The body has to be evaluated per element either way, so compute
			// the folded value here instead of leaving it to the normal path:
			// that would evaluate every body twice.
			ls, ok := list.(*List)
			if !ok {
				return nil, nil, false, nil // the normal path reports the type error
			}
			out, undecidable, err := k.foldFilterSort(b, ls, env, rec)
			if err != nil {
				return nil, nil, true, err
			}
			if !undecidable {
				return out, nil, true, nil
			}
			deferred = true
		}
		if !deferred {
			return nil, nil, false, nil
		}
		// Only evaluate the body with an unknown loop variable once we know
		// the operator survives; doing it speculatively would surface errors
		// from expressions that the folded path never needs.
		shadow, err := k.shadowBody(b, env, rec)
		if err != nil {
			return nil, nil, true, err
		}
		return &Unresolved{Text: comprehensionText(b, list, shadow)}, nil, true, nil

	case syntax.XFoldl:
		acc, err := eval(0)
		if err != nil {
			return nil, nil, true, err
		}
		list, err := eval(1)
		if err != nil {
			return nil, nil, true, err
		}
		if !isUnresolved(list) && !isUnresolved(acc) {
			return nil, nil, false, nil
		}
		aName, bName := foldNames(b)
		sub := env.With(map[string]Binding{
			aName: {Val: &Unresolved{Text: aName, Kind: "var", Var: aName}},
			bName: {Val: &Unresolved{Text: bName, Kind: "var", Var: bName}},
		})
		body, err := k.evalSyntax(b.Args[2], sub, rec)
		if err != nil {
			return nil, nil, true, err
		}
		text := "!foldl(" + printableOf(acc) + ", " + printableOf(list) + ", " +
			aName + ", " + bName + ", " + printableOf(body) + ")"
		return &Unresolved{Text: text}, nil, true, nil

	case syntax.XCast:
		v, err := eval(0)
		if err != nil {
			return nil, nil, true, err
		}
		u, ok := v.(*Unresolved)
		if !ok {
			return nil, nil, false, nil
		}
		target := k.tyFromSyntax(b.TypeArg)
		if target != nil && u.Ty != nil && u.Ty.IsA(target) {
			// UnOpInit::Fold(CAST): convertInitializerTo the value's own type
			// returns the value, so `!cast<int>(x)` on an int var is just x.
			return u, nil, true, nil
		}
		text := "!cast<" + b.TypeArg.String() + ">(" + u.Text + ")"
		return &Unresolved{Text: text, Ty: target}, nil, true, nil

	case syntax.XDag, syntax.XIsA, syntax.XExists, syntax.XInstances:
		// These either build structured values from unresolved parts (a dag
		// keeps a var operator/argument) or take names/types, not values.
		return nil, nil, false, nil
	}

	// Generic case: evaluate the arguments in order, then keep the operator as
	// an Init tree if any of them is unresolved.
	vals := make([]Value, len(b.Args))
	for i := range b.Args {
		v, err := eval(i)
		if err != nil {
			return nil, nil, true, err
		}
		vals[i] = v
		// !and/!or fold on a zero / all-ones left operand before touching the
		// right side (BinOpInit::resolveReferences).
		if b.Op == syntax.XAnd || b.Op == syntax.XOr {
			if n, ok := intValueOf(v); ok && i == 0 && ((b.Op == syntax.XAnd && n == 0) || (b.Op == syntax.XOr && n == -1)) {
				return nil, nil, false, nil
			}
		}
	}
	if !anyUnresolved(vals) {
		// The operands resolved after all; hand them to the caller so the
		// normal evaluator does not evaluate every argument a second time.
		return nil, vals, false, nil
	}
	return deferredCall(b, vals, k.bangResultTy(b)), nil, true, nil
}

// deferredCall renders `!op(a, b, ...)` with the already-evaluated operands.
func deferredCall(b *syntax.BangValue, vals []Value, ty *Ty) *Unresolved {
	name := bangOpName(b)
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = printableOf(v)
	}
	return &Unresolved{Text: name + "(" + strings.Join(parts, ", ") + ")", Ty: ty}
}

// bangOpName is UnOpInit/BinOpInit/TernOpInit::getAsString for the operators
// that carry a type argument.
func bangOpName(b *syntax.BangValue) string {
	name := b.Op.String()
	switch b.Op {
	case syntax.XCast, syntax.XGetDagArg, syntax.XGetDagOp:
		if b.TypeArg.Kind != syntax.InvalidType {
			name += "<" + b.TypeArg.String() + ">"
		}
	}
	return name
}

// bangResultTy is the static result type of a bang operator, needed for the
// implicit !cast<...> the parser inserts when a value is stored in a field of
// another type. Nil means "unknown".
func (k *Keeper) bangResultTy(b *syntax.BangValue) *Ty {
	bit := &Ty{Kind: BitTy}
	integer := &Ty{Kind: IntTy}
	str := &Ty{Kind: StringTy}
	switch b.Op {
	case syntax.XEq, syntax.XNe, syntax.XLe, syntax.XLt, syntax.XGe, syntax.XGt,
		syntax.XIsA, syntax.XExists, syntax.XMatch:
		return bit
	case syntax.XAdd, syntax.XSub, syntax.XMul, syntax.XDiv, syntax.XAnd,
		syntax.XOr, syntax.XXor, syntax.XShl, syntax.XSra, syntax.XSrl,
		syntax.XNot, syntax.XLog2, syntax.XSize, syntax.XEmpty,
		syntax.XInitialized:
		return integer
	case syntax.XStrConcat, syntax.XSubstr, syntax.XFind, syntax.XToLower,
		syntax.XToUpper, syntax.XRepr, syntax.XGetDagName:
		return str
	case syntax.XCast:
		return k.tyFromSyntax(b.TypeArg)
	}
	return nil
}

// shadowBody evaluates a comprehension body with its loop variable replaced by
// an unresolved VarInit, which is what ShadowResolver leaves behind.
func (k *Keeper) shadowBody(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	name := ""
	if len(b.VarNames) > 0 {
		name = b.VarNames[0]
	}
	if name == "" {
		name = "x"
	}
	sub := env.WithBinding(name, Binding{Val: &Unresolved{Text: name, Kind: "var", Var: name}})
	return k.evalSyntax(b.Args[1], sub, rec)
}

func comprehensionText(b *syntax.BangValue, list, body Value) string {
	name := "x"
	if len(b.VarNames) > 0 && b.VarNames[0] != "" {
		name = b.VarNames[0]
	}
	return bangOpName(b) + "(" + name + ", " + printableOf(list) + ", " + printableOf(body) + ")"
}

// foldFilterSort evaluates !filter/!sort in one pass. undecidable reports that
// a body value is not an int (filter) or a key the operator can compare (sort),
// which is when TernOpInit::Fold returns nullptr and the operator stays an
// Init; the caller then keeps the deferred form.
func (k *Keeper) foldFilterSort(b *syntax.BangValue, ls *List, env *Env, rec *Record) (out *List, undecidable bool, err error) {
	name := "x"
	if len(b.VarNames) > 0 && b.VarNames[0] != "" {
		name = b.VarNames[0]
	}
	body := func(e Value) (Value, error) {
		return k.evalSyntax(b.Args[1], env.WithBinding(name, Binding{Val: e}), rec)
	}

	if b.Op == syntax.XFilter {
		// FilterHelper requires an IntInit; intValueOf is the same test the
		// deferral analysis used, so the folded/unfolded split is unchanged.
		out = &List{Elem: ls.Elem, Elems: make([]Value, 0, len(ls.Elems))}
		for _, e := range ls.Elems {
			v, err := body(e)
			if err != nil {
				return nil, false, err
			}
			n, ok := intValueOf(v)
			if !ok {
				return nil, true, nil
			}
			if n != 0 {
				out.Elems = append(out.Elems, e)
			}
		}
		return out, false, nil
	}

	// SortHelper takes the key type from the first element and then requires
	// every key to agree: int when the first converts to an int, StringInit
	// otherwise.
	type kv struct {
		key Value
		val Value
	}
	items := make([]kv, 0, len(ls.Elems))
	useInt := false
	for i, e := range ls.Elems {
		v, err := body(e)
		if err != nil {
			return nil, false, err
		}
		if isUnresolved(v) {
			return nil, true, nil
		}
		_, isInt := intValueOf(v)
		if i == 0 {
			useInt = isInt
		}
		if useInt && !isInt {
			return nil, true, nil
		}
		if !useInt {
			if _, isStr := v.(*Str); !isStr {
				return nil, true, nil
			}
		}
		items = append(items, kv{key: v, val: e})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if useInt {
			a, _ := valueInt(items[i].key)
			b, _ := valueInt(items[j].key)
			return a < b
		}
		as, _ := items[i].key.(*Str)
		bs, _ := items[j].key.(*Str)
		return as.S < bs.S
	})
	out = &List{Elem: ls.Elem}
	for _, it := range items {
		out.Elems = append(out.Elems, it.val)
	}
	return out, false, nil
}

func foldNames(b *syntax.BangValue) (string, string) {
	a, c := "a", "b"
	if len(b.VarNames) > 0 && b.VarNames[0] != "" {
		a = b.VarNames[0]
	}
	if len(b.VarNames) > 1 && b.VarNames[1] != "" {
		c = b.VarNames[1]
	}
	return a, c
}

func intValueOf(v Value) (int64, bool) {
	switch x := v.(type) {
	case *Int:
		return x.N, true
	case *Bit:
		if x.B {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// deferCond mirrors CondOpInit::resolveReferences/Fold: an unresolved
// condition before the first true one keeps the whole !cond as an Init, printed
// with every condition and value it resolved.
func (k *Keeper) deferCond(b *syntax.BangValue, env *Env, rec *Record) (Value, bool, error) {
	texts := make([]string, 0, len(b.Cases))
	deferred := false
	for _, c := range b.Cases {
		if c.Key == nil {
			continue
		}
		kv, err := k.evalSyntax(c.Key, env, rec)
		if err != nil {
			return nil, true, err
		}
		vv, err := k.evalSyntax(c.Value, env, rec)
		if err != nil {
			return nil, true, err
		}
		texts = append(texts, printableOf(kv)+": "+printableOf(vv))
		if isUnresolved(kv) {
			deferred = true
			continue
		}
		// A concrete true condition ends the scan; Fold then only sees the
		// cases up to here.
		if n, ok := intValueOf(kv); ok && n != 0 {
			break
		}
	}
	if !deferred {
		return nil, false, nil
	}
	return &Unresolved{Text: "!cond(" + strings.Join(texts, ", ") + ")"}, true, nil
}
