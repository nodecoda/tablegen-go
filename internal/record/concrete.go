package record

import "tblgen/internal/diag"

// checkConcrete mirrors TGParser.cpp's checkConcrete: once a def is complete,
// every value declared without `field` must be fully concrete. An unassigned
// declaration is fine (UnsetInit::isConcrete is true, and --dump-json prints
// null); what is rejected is a value that is still a VarInit/VarBitInit
// reference or an unfolded operator. `field` opts out because targets
// legitimately keep non-concrete values in helper defs.
//
// LLVM prints one error per offending value; we stop at the first, which is
// enough to classify the input and keeps the driver's one-error contract.
func (k *Keeper) checkConcrete(rec *Record) error {
	for _, v := range rec.Vals {
		if v.Nonconcrete {
			continue
		}
		val, err := rec.evalVal(v)
		if err != nil {
			return err
		}
		if initIncomplete(val) {
			return &EvalError{
				Msg:  "Initializer of '" + v.Name + "' in '" + rec.Name + "' could not be fully resolved: " + printableOf(val),
				Span: rec.locSpan(),
			}
		}
	}
	return nil
}

// initIncomplete is !Init::isConcrete for our evaluated values. A bare
// reference to an existing field is kept as data by LLVM's bit check
// (checkBitsConcrete), so VarInit/VarBitInit bits do not make a bits<N>
// incomplete; only an unfolded operator does.
func initIncomplete(v Value) bool {
	switch x := v.(type) {
	case *Unset, *Int, *Bit, *Str, *DefVal:
		return false
	case *Unresolved:
		// VarInit and VarBitInit are not concrete; only a bit stored *inside*
		// a bits<N> may be such a reference (checkBitsConcrete).
		return true
	case *BitRef:
		// checkBitsConcrete treats a VarBitInit of an existing field as a
		// reference, not as an incomplete value.
		return false
	case *Bits:
		for _, b := range x.B {
			if initIncompleteBit(b) {
				return true
			}
		}
		return false
	case *List:
		for _, e := range x.Elems {
			if initIncomplete(e) {
				return true
			}
		}
		return false
	case *Dag:
		if initIncomplete(x.Op) {
			return true
		}
		for _, a := range x.Args {
			if initIncomplete(a.Value) {
				return true
			}
		}
		return false
	}
	return false
}

// initIncompleteBit is one element of checkBitsConcrete: UnsetInit is
// concrete, and a VarInit/VarBitInit of an existing field is a reference the
// check accepts. Anything else that is not a bit is incomplete.
func initIncompleteBit(v Value) bool {
	switch v.(type) {
	case *Bit, *Unset, *BitRef:
		return false
	case *Unresolved:
		// A VarInit/VarBitInit reference to an existing field is accepted by
		// checkBitsConcrete; an unfolded operator is not.
		u := v.(*Unresolved)
		return u.Kind != "var" && u.Kind != "varbit"
	}
	return true
}

// locSpan lets checkConcrete report the def name, as LLVM's Record::getLoc
// does.
func (r *Record) locSpan() diag.Span {
	if len(r.Locs) > 0 {
		return diag.Span{Start: r.Locs[0], End: r.Locs[0]}
	}
	return diag.Span{}
}
