package record

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// evalBang implements the `!` operators. It follows llvm/lib/TableGen/
// Record.cpp's Fold functions: operators evaluate their arguments as needed,
// and !if/!cond/!switch only evaluate the arm they select.
func (k *Keeper) evalBang(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	// An operator whose operands cannot be folded survives as an Init tree in
	// llvm-tblgen; deferred.go decides whether that is the case here. When it
	// is not deferred it still hands back the operands it evaluated, so they
	// are not evaluated twice.
	v, vals, handled, derr := k.deferBang(b, env, rec)
	if handled {
		return v, derr
	}
	eval := func(i int) (Value, error) {
		if vals != nil && i < len(vals) {
			return vals[i], nil
		}
		return k.evalSyntax(b.Args[i], env, rec)
	}
	switch b.Op {
	case syntax.XIf:
		cond, err := eval(0)
		if err != nil {
			return nil, err
		}
		n, err := k.asInt(cond)
		if err != nil {
			return nil, err
		}
		if n != 0 {
			return eval(1)
		}
		return eval(2)

	case syntax.XCond:
		for _, c := range b.Cases {
			cv, err := k.evalSyntax(c.Key, env, rec)
			if err != nil {
				return nil, err
			}
			n, err := k.asInt(cv)
			if err != nil {
				return nil, err
			}
			if n != 0 {
				return k.evalSyntax(c.Value, env, rec)
			}
		}
		// Record.cpp:2592: "<record> does not have any true condition in:<init>".
		who := ""
		if rec != nil {
			who = rec.Name
		}
		return nil, &EvalError{Msg: who + " does not have any true condition in:" + initText(b, env, nil), Span: b.Span(), AtRecord: true}

	case syntax.XSwitch:
		key, err := eval(0)
		if err != nil {
			return nil, err
		}
		for _, c := range b.Cases {
			if c.Key == nil {
				return k.evalSyntax(c.Value, env, rec)
			}
			kv, err := k.evalSyntax(c.Key, env, rec)
			if err != nil {
				return nil, err
			}
			if equalValues(key, kv) {
				return k.evalSyntax(c.Value, env, rec)
			}
		}
		return nil, &EvalError{Msg: "!switch has no matching case", Span: b.Span(), AtRecord: true}

	case syntax.XEq, syntax.XNe, syntax.XLe, syntax.XLt, syntax.XGe, syntax.XGt:
		lhs, err := eval(0)
		if err != nil {
			return nil, err
		}
		rhs, err := eval(1)
		if err != nil {
			return nil, err
		}
		cmp, ok := compareValues(b.Op, lhs, rhs)
		if !ok {
			return nil, &EvalError{Msg: "invalid operands to comparison", Span: b.Span()}
		}
		return &Bit{B: cmp}, nil

	case syntax.XAdd, syntax.XSub, syntax.XMul, syntax.XDiv,
		syntax.XAnd, syntax.XOr, syntax.XXor, syntax.XShl, syntax.XSra, syntax.XSrl:
		acc, err := eval(0)
		if err != nil {
			return nil, err
		}
		for i := 1; i < len(b.Args); i++ {
			// LLVM short-circuits !and at a zero left operand and !or at an
			// all-ones left operand before resolving the right side
			// (BinOpInit::resolveReferences).
			if b.Op == syntax.XAnd || b.Op == syntax.XOr {
				if n, err := valueInt(acc); err == nil && ((b.Op == syntax.XAnd && n == 0) || (b.Op == syntax.XOr && n == -1)) {
					return &Int{N: n}, nil
				}
			}
			rhs, err := eval(i)
			if err != nil {
				return nil, err
			}
			acc, err = intBinOp(b.Op, acc, rhs, b.Span())
			if err != nil {
				return nil, err
			}
		}
		return acc, nil

	case syntax.XNot:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		n, err := k.asInt(v)
		if err != nil {
			return nil, err
		}
		return &Int{N: boolInt(n == 0)}, nil

	case syntax.XLog2:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		n, err := k.asInt(v)
		if err != nil {
			return nil, err
		}
		if n <= 0 {
			return nil, &EvalError{Msg: "Illegal operation: logtwo is undefined on arguments less than or equal to 0", Span: b.Span()}
		}
		return &Int{N: int64(63 - leadingZeros(uint64(n)))}, nil

	case syntax.XSize:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		switch x := v.(type) {
		case *List:
			return &Int{N: int64(len(x.Elems))}, nil
		case *Dag:
			return &Int{N: int64(len(x.Args))}, nil
		case *Str:
			return &Int{N: int64(len(x.S))}, nil
		case *Bits:
			return &Int{N: int64(len(x.B))}, nil
		}
		return nil, &EvalError{Msg: "!size expects a list, dag, or string", Span: b.Span()}

	case syntax.XEmpty:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		switch x := v.(type) {
		case *List:
			return &Int{N: boolInt(len(x.Elems) == 0)}, nil
		case *Dag:
			return &Int{N: boolInt(len(x.Args) == 0)}, nil
		case *Str:
			return &Int{N: boolInt(len(x.S) == 0)}, nil
		}
		return nil, &EvalError{Msg: "!empty expects a list, dag, or string", Span: b.Span()}

	case syntax.XInitialized:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		_, unset := v.(*Unset)
		return &Int{N: boolInt(!unset)}, nil

	case syntax.XHead, syntax.XTail:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		ls, ok := v.(*List)
		if !ok {
			return nil, &EvalError{Msg: "!head/!tail expect a list", Span: b.Span()}
		}
		if len(ls.Elems) == 0 {
			return nil, &EvalError{Msg: "empty list in !head/!tail", Span: b.Span()}
		}
		if b.Op == syntax.XHead {
			return ls.Elems[0], nil
		}
		return &List{Elems: ls.Elems[1:], Elem: ls.Elem}, nil

	case syntax.XListConcat, syntax.XConcat:
		acc, err := eval(0)
		if err != nil {
			return nil, err
		}
		for i := 1; i < len(b.Args); i++ {
			v, err := eval(i)
			if err != nil {
				return nil, err
			}
			if b.Op == syntax.XListConcat {
				a, ok1 := acc.(*List)
				c, ok2 := v.(*List)
				if !ok1 || !ok2 {
					return nil, &EvalError{Msg: "!listconcat expects lists", Span: b.Span()}
				}
				acc = &List{Elems: append(append([]Value(nil), a.Elems...), c.Elems...), Elem: a.Elem}
			} else {
				a, ok1 := acc.(*Dag)
				c, ok2 := v.(*Dag)
				if !ok1 || !ok2 {
					return nil, &EvalError{Msg: "!con expects dags", Span: b.Span()}
				}
				acc, err = concatDag(a, c)
				if err != nil {
					return nil, err
				}
			}
		}
		return acc, nil

	case syntax.XStrConcat:
		var sb strings.Builder
		code := false
		for i := range b.Args {
			v, err := eval(i)
			if err != nil {
				return nil, err
			}
			s, ok := v.(*Str)
			if !ok {
				return nil, &EvalError{Msg: "!strconcat expects strings", Span: b.Span()}
			}
			sb.WriteString(s.S)
			// Record.cpp:1064 ConcatStringInits: the result keeps the code
			// format when either operand has it.
			code = code || s.Code
		}
		return &Str{S: sb.String(), Code: code}, nil

	case syntax.XListFlatten:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		ls, ok := v.(*List)
		if !ok {
			return nil, &EvalError{Msg: "!listflatten expects a list", Span: b.Span()}
		}
		out := &List{Elem: nil}
		for _, e := range ls.Elems {
			if inner, ok := e.(*List); ok {
				out.Elems = append(out.Elems, inner.Elems...)
				if out.Elem == nil {
					out.Elem = inner.Elem
				}
			} else {
				out.Elems = append(out.Elems, e)
			}
		}
		return out, nil

	case syntax.XListSplat:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		nv, err := eval(1)
		if err != nil {
			return nil, err
		}
		n, err := k.asInt(nv)
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, &EvalError{Msg: "!listsplat count " + strconv.FormatInt(n, 10) + " is negative", Span: b.Span()}
		}
		out := &List{Elem: v.Type()}
		for i := int64(0); i < n; i++ {
			out.Elems = append(out.Elems, v)
		}
		return out, nil

	case syntax.XListRemove:
		a, err := eval(0)
		if err != nil {
			return nil, err
		}
		rm, err := eval(1)
		if err != nil {
			return nil, err
		}
		al, ok1 := a.(*List)
		rl, ok2 := rm.(*List)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "!listremove expects lists", Span: b.Span()}
		}
		out := &List{Elem: al.Elem}
		for _, e := range al.Elems {
			keep := true
			for _, r := range rl.Elems {
				if equalValues(e, r) {
					keep = false
					break
				}
			}
			if keep {
				out.Elems = append(out.Elems, e)
			}
		}
		return out, nil

	case syntax.XRange:
		if len(b.Args) < 1 || len(b.Args) > 3 {
			return nil, &EvalError{Msg: "expected at most three values of integer", Span: b.Span()}
		}
		first, err := eval(0)
		if err != nil {
			return nil, err
		}
		var args []int64
		if ls, ok := first.(*List); ok {
			// !range(list) is the index range [0, !size(list)).
			if len(b.Args) != 1 {
				return nil, &EvalError{Msg: "expected one list, got extra value", Span: b.Span()}
			}
			args = []int64{0, int64(len(ls.Elems)), 1}
		} else {
			n, err := k.asInt(first)
			if err != nil {
				return nil, err
			}
			switch len(b.Args) {
			case 1:
				args = []int64{0, n, 1}
			case 2:
				mv, err := eval(1)
				if err != nil {
					return nil, err
				}
				m, err := k.asInt(mv)
				if err != nil {
					return nil, err
				}
				args = []int64{n, m, 1}
			case 3:
				mv, err := eval(1)
				if err != nil {
					return nil, err
				}
				m, err := k.asInt(mv)
				if err != nil {
					return nil, err
				}
				sv, err := eval(2)
				if err != nil {
					return nil, err
				}
				s, err := k.asInt(sv)
				if err != nil {
					return nil, err
				}
				args = []int64{n, m, s}
			}
		}
		return rangeList(args, b.Span())

	case syntax.XInterleave:
		lv, err := eval(0)
		if err != nil {
			return nil, err
		}
		dv, err := eval(1)
		if err != nil {
			return nil, err
		}
		ls, ok1 := lv.(*List)
		d, ok2 := dv.(*Str)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "!interleave expects a list and a string", Span: b.Span()}
		}
		parts := make([]string, len(ls.Elems))
		// LLVM picks the string path only for list<string>; every other list
		// converts each element to int and prints the integer.
		stringPath := ls.Elem != nil && ls.Elem.Kind == StringTy
		if len(ls.Elems) > 0 {
			if _, ok := ls.Elems[0].(*Str); ok {
				stringPath = true
			}
		}
		// Record.cpp:1076 interleaveStringList starts at SF_String and ORs in
		// the format of every element (the delimiter does not participate).
		code := false
		for i, e := range ls.Elems {
			if stringPath {
				if v, ok := e.(*Str); ok {
					parts[i] = v.S
					code = code || v.Code
					continue
				}
			}
			n, err := k.asInt(e)
			if err != nil {
				return nil, &EvalError{Msg: "!interleave element is not convertible to int", Span: b.Span()}
			}
			parts[i] = strconv.FormatInt(n, 10)
		}
		return &Str{S: strings.Join(parts, d.S), Code: code}, nil

	case syntax.XToLower, syntax.XToUpper:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		s, ok := v.(*Str)
		if !ok {
			return nil, &EvalError{Msg: "!tolower/!toupper expect a string", Span: b.Span()}
		}
		if b.Op == syntax.XToLower {
			return &Str{S: strings.ToLower(s.S), Code: s.Code}, nil
		}
		return &Str{S: strings.ToUpper(s.S), Code: s.Code}, nil

	case syntax.XRepr:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		if d, ok := v.(*DefVal); ok {
			txt, err := d.Rec.DumpText()
			if err != nil {
				return nil, err
			}
			return &Str{S: txt}, nil
		}
		return &Str{S: v.String()}, nil

	case syntax.XSubstr:
		sv, err := eval(0)
		if err != nil {
			return nil, err
		}
		start, err := k.evalInt(b.Args[1], env, rec)
		if err != nil {
			return nil, err
		}
		s, ok := sv.(*Str)
		if !ok {
			return nil, &EvalError{Msg: "!substr expects a string", Span: b.Span()}
		}
		size := int64(len(s.S))
		if start < 0 || start > size {
			k.softErrorAt("!substr start position is out of range 0..."+strconv.FormatInt(size, 10)+": "+strconv.FormatInt(start, 10), b.Span(), rec)
		}
		end := size
		if len(b.Args) > 2 {
			n, err := k.evalInt(b.Args[2], env, rec)
			if err != nil {
				return nil, err
			}
			if n < 0 {
				k.softError("!substr length must be nonnegative", b.Span())
			} else if n < size-start || start < 0 {
				end = start + n
			}
		}
		// std::string::substr clamps instead of failing; llvm-tblgen prints its
		// diagnostics and keeps the truncated value.
		if start < 0 {
			start = 0
		}
		if start > size {
			start = size
		}
		if end < start {
			end = start
		}
		if end > size {
			end = size
		}
		return &Str{S: s.S[start:end], Code: s.Code}, nil

	case syntax.XFind:
		sv, err := eval(0)
		if err != nil {
			return nil, err
		}
		sub, err := eval(1)
		if err != nil {
			return nil, err
		}
		s, ok1 := sv.(*Str)
		q, ok2 := sub.(*Str)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "!find expects strings", Span: b.Span()}
		}
		start := int64(0)
		if len(b.Args) > 2 {
			start, err = k.evalInt(b.Args[2], env, rec)
			if err != nil {
				return nil, err
			}
		}
		if start < 0 || start > int64(len(s.S)) {
			// Record.cpp:1894 uses PrintError here: the message is emitted but
			// the fold still yields -1. The location is CurRec->getLoc(), the
			// record's name token, not the operator.
			k.softErrorAt("!find start position is out of range 0..."+
				strconv.Itoa(len(s.S))+": "+strconv.FormatInt(start, 10), b.Span(), rec)
			return &Int{N: -1}, nil
		}
		idx := strings.Index(s.S[start:], q.S)
		if idx < 0 {
			return &Int{N: -1}, nil
		}
		return &Int{N: start + int64(idx)}, nil

	case syntax.XMatch:
		sv, err := eval(0)
		if err != nil {
			return nil, err
		}
		rv, err := eval(1)
		if err != nil {
			return nil, err
		}
		s, ok1 := sv.(*Str)
		r, ok2 := rv.(*Str)
		if !ok1 || !ok2 {
			// TGParser::ParseOperation type-checks !match's arguments against
			// string and reports the usual "expected value of type" message.
			bad, arg := sv, b.Args[0]
			if ok1 {
				bad, arg = rv, b.Args[1]
			}
			return nil, &EvalError{Msg: "expected value of type 'string', got '" + bad.Type().String() + "'", Span: arg.Span()}
		}
		re, err := regexp.Compile(r.S)
		if err != nil {
			// Record.cpp:2201 is PrintFatalError(Twine), which has no location.
			return nil, &EvalError{Msg: "invalid regex '" + r.S + "'", NoLoc: true}
		}
		return &Bit{B: re.MatchString(s.S)}, nil

	case syntax.XCast:
		return k.evalCast(b, env, rec)

	case syntax.XIsA, syntax.XExists:
		return k.evalTypeTest(b, env, rec)

	case syntax.XForEach, syntax.XFilter, syntax.XSort:
		return k.evalListComprehension(b, env, rec)

	case syntax.XFoldl:
		return k.evalFoldl(b, env, rec)

	case syntax.XDag:
		return k.evalDagCtor(b, env, rec)

	case syntax.XGetDagOp:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		d, ok := v.(*Dag)
		if !ok {
			return nil, &EvalError{Msg: "!getdagop expects a dag", Span: b.Span()}
		}
		if b.TypeArg.Kind != syntax.InvalidType {
			// UnOpInit::Fold(GETDAGOP): the operator must be an instance of
			// the requested class. Module-level, at CurRec->getLoc().
			target := k.tyFromSyntax(b.TypeArg)
			if opTy := d.Op.Type(); !opTy.IsA(target) {
				return nil, &EvalError{
					Msg: "Expected type '" + target.String() + "', got '" + opTy.String() +
						"' in: !getdagop(" + d.String() + ")\n",
					AtRecord: true,
				}
			}
		}
		return d.Op, nil

	case syntax.XGetDagOpName:
		v, err := eval(0)
		if err != nil {
			return nil, err
		}
		d, ok := v.(*Dag)
		if !ok {
			return nil, &EvalError{Msg: "!getdagopname expects a dag", Span: b.Span()}
		}
		if d.OpName == "" {
			return &Unset{}, nil
		}
		return &Str{S: d.OpName}, nil

	case syntax.XSetDagOp:
		dv, err := eval(0)
		if err != nil {
			return nil, err
		}
		ov, err := eval(1)
		if err != nil {
			return nil, err
		}
		d, ok := dv.(*Dag)
		if !ok {
			return nil, &EvalError{Msg: "!setdagop expects a dag", Span: b.Span()}
		}
		out := *d
		out.Op = ov
		return &out, nil

	case syntax.XSetDagOpName:
		dv, err := eval(0)
		if err != nil {
			return nil, err
		}
		nv, err := eval(1)
		if err != nil {
			return nil, err
		}
		d, ok1 := dv.(*Dag)
		n, ok2 := nv.(*Str)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "!setdagopname expects a dag and a string", Span: b.Span()}
		}
		out := *d
		out.OpName = n.S
		return &out, nil

	case syntax.XGetDagArg:
		return k.evalGetDagArg(b, env, rec)

	case syntax.XGetDagName:
		dv, err := eval(0)
		if err != nil {
			return nil, err
		}
		iv, err := eval(1)
		if err != nil {
			return nil, err
		}
		d, ok1 := dv.(*Dag)
		idx, ok2 := iv.(*Int)
		if !ok1 || !ok2 {
			return nil, &EvalError{Msg: "!getdagname expects a dag and an index", Span: b.Span()}
		}
		if idx.N < 0 || int(idx.N) >= len(d.Args) {
			return nil, &EvalError{Msg: "!getdagname index is out of range", Span: b.Span()}
		}
		if d.Args[idx.N].Name == "" {
			return &Unset{}, nil
		}
		return &Str{S: d.Args[idx.N].Name}, nil

	case syntax.XSetDagArg, syntax.XSetDagName:
		return k.evalSetDagArgName(b, env, rec)

	case syntax.XSubst:
		a, err := eval(0)
		if err != nil {
			return nil, err
		}
		bb, err := eval(1)
		if err != nil {
			return nil, err
		}
		c, err := eval(2)
		if err != nil {
			return nil, err
		}
		if ad, ok := a.(*DefVal); ok {
			cd, ok := c.(*DefVal)
			if !ok {
				return nil, &EvalError{Msg: "!subst expects a def", Span: b.Span()}
			}
			if ad.Rec == cd.Rec {
				if bd, ok := bb.(*DefVal); ok {
					return bd, nil
				}
			}
			return cd, nil
		}
		as, ok1 := a.(*Str)
		bs, ok2 := bb.(*Str)
		cs, ok3 := c.(*Str)
		if !ok1 || !ok2 || !ok3 {
			return nil, &EvalError{Msg: "!subst expects strings", Span: b.Span()}
		}
		return &Str{S: strings.ReplaceAll(cs.S, as.S, bs.S)}, nil

	case syntax.XInstances:
		// InstancesOpInit::Fold: every def (not class) whose type is a
		// subtype of the requested class, optionally name-filtered, sorted by
		// name. Our lazy evaluation happens at the same point as llvm-tblgen's
		// final resolve, i.e. after the defs above have been added.
		target := k.tyFromSyntax(b.TypeArg)
		var re *regexp.Regexp
		if len(b.Args) > 0 {
			rv, err := eval(0)
			if err != nil {
				return nil, err
			}
			rs, ok := rv.(*Str)
			if !ok {
				return nil, &EvalError{Msg: "expected string type argument in !instances operator", Span: b.Span()}
			}
			re, err = regexp.Compile(rs.S)
			if err != nil {
				return nil, &EvalError{Msg: "invalid regex '" + rs.S + "'", Span: b.Span()}
			}
		}
		names := make([]string, 0, len(k.defs))
		for name, d := range k.defs {
			if d.IsClass {
				continue
			}
			dt := d.Type()
			if !dt.IsA(target) {
				continue
			}
			if re != nil && !re.MatchString(name) {
				continue
			}
			names = append(names, name)
		}
		sort.Strings(names)
		out := &List{Elem: target}
		for _, name := range names {
			out.Elems = append(out.Elems, &DefVal{Rec: k.defs[name]})
		}
		return out, nil

	default:
		return nil, unsupported("!"+strings.TrimPrefix(b.Op.String(), "!"), b.Span())
	}
}

func intBinOp(op syntax.Kind, a, b Value, _ diag.Span) (Value, error) {
	ai, err := valueInt(a)
	if err != nil {
		return nil, err
	}
	bi, err := valueInt(b)
	if err != nil {
		return nil, err
	}
	switch op {
	case syntax.XAdd:
		return &Int{N: ai + bi}, nil
	case syntax.XSub:
		return &Int{N: ai - bi}, nil
	case syntax.XMul:
		return &Int{N: ai * bi}, nil
	case syntax.XDiv:
		if bi == 0 {
			return nil, &EvalError{Msg: "Illegal operation: division by zero"}
		}
		// Go returns INT64_MIN here instead of trapping; llvm-tblgen refuses.
		if ai == math.MinInt64 && bi == -1 {
			return nil, &EvalError{Msg: "Illegal operation: INT64_MIN / -1"}
		}
		return &Int{N: ai / bi}, nil
	case syntax.XAnd:
		return &Int{N: ai & bi}, nil
	case syntax.XOr:
		return &Int{N: ai | bi}, nil
	case syntax.XXor:
		return &Int{N: ai ^ bi}, nil
	case syntax.XShl:
		if bi < 0 || bi >= 64 {
			return nil, &EvalError{Msg: "Illegal operation: out of bounds shift"}
		}
		return &Int{N: int64(uint64(ai) << uint64(bi))}, nil
	case syntax.XSra:
		if bi < 0 || bi >= 64 {
			return nil, &EvalError{Msg: "Illegal operation: out of bounds shift"}
		}
		return &Int{N: ai >> uint64(bi)}, nil
	case syntax.XSrl:
		if bi < 0 || bi >= 64 {
			return nil, &EvalError{Msg: "Illegal operation: out of bounds shift"}
		}
		return &Int{N: int64(uint64(ai) >> uint64(bi))}, nil
	}
	return nil, &EvalError{Msg: "internal: unknown integer operator"}
}

func valueInt(v Value) (int64, error) {
	switch x := v.(type) {
	case *Int:
		return x.N, nil
	case *Bit:
		return boolInt(x.B), nil
	case *Bits:
		if n, ok := bitsToInt(*x); ok {
			return n, nil
		}
		return 0, &EvalError{Msg: "bits value has unresolved bits"}
	}
	return 0, &EvalError{Msg: fmt.Sprintf("expected int, got %s", v.Type().String())}
}

func leadingZeros(x uint64) int {
	n := 0
	for i := 63; i >= 0; i-- {
		if x&(1<<uint(i)) != 0 {
			break
		}
		n++
	}
	return n
}

func equalValues(a, b Value) bool {
	if c, ok := compareValues(syntax.XEq, a, b); ok {
		return c
	}
	return a.String() == b.String()
}

func compareValues(op syntax.Kind, a, b Value) (bool, bool) {
	if ai, err := valueInt(a); err == nil {
		if bi, err := valueInt(b); err == nil {
			switch op {
			case syntax.XEq:
				return ai == bi, true
			case syntax.XNe:
				return ai != bi, true
			case syntax.XLe:
				return ai <= bi, true
			case syntax.XLt:
				return ai < bi, true
			case syntax.XGe:
				return ai >= bi, true
			case syntax.XGt:
				return ai > bi, true
			}
		}
	}
	if as, ok := a.(*Str); ok {
		if bs, ok := b.(*Str); ok {
			switch op {
			case syntax.XEq:
				return as.S == bs.S, true
			case syntax.XNe:
				return as.S != bs.S, true
			case syntax.XLe:
				return as.S <= bs.S, true
			case syntax.XLt:
				return as.S < bs.S, true
			case syntax.XGe:
				return as.S >= bs.S, true
			case syntax.XGt:
				return as.S > bs.S, true
			}
		}
	}
	if op == syntax.XEq || op == syntax.XNe {
		ad, ok1 := a.(*DefVal)
		bd, ok2 := b.(*DefVal)
		if ok1 && ok2 {
			eq := ad.Rec == bd.Rec
			if op == syntax.XNe {
				eq = !eq
			}
			return eq, true
		}
	}
	return false, false
}

func rangeList(args []int64, _ diag.Span) (Value, error) {
	out := &List{Elem: &Ty{Kind: IntTy}}
	if len(args) != 3 {
		return nil, &EvalError{Msg: "expected three values of integer"}
	}
	start, end, step := args[0], args[1], args[2]
	if step == 0 {
		return nil, &EvalError{Msg: "Step of !range can't be 0"}
	}
	if start < end && step > 0 {
		for i := start; i < end; i += step {
			out.Elems = append(out.Elems, &Int{N: i})
		}
	} else if start > end && step < 0 {
		for i := start; i > end; i += step {
			out.Elems = append(out.Elems, &Int{N: i})
		}
	}
	return out, nil
}

func (k *Keeper) evalCast(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	v, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	target := k.tyFromSyntax(b.TypeArg)
	switch target.Kind {
	case StringTy:
		switch x := v.(type) {
		case *Str:
			return x, nil
		case *DefVal:
			return &Str{S: x.Rec.Name}, nil
		case *Int:
			return &Str{S: x.String()}, nil
		}
		// IntInit::getAsString also covers bit and bits<N> through
		// convertInitializerTo(IntRecTy).
		if n, err := valueInt(v); err == nil {
			return &Str{S: (&Int{N: n}).String()}, nil
		}
	case RecordTy:
		if s, ok := v.(*Str); ok {
			if d := k.Def(s.S); d != nil {
				// UnOpInit::Fold(CAST) reports a record that is not an
				// instance of the target type, printing the whole cast node
				// (which still spells the string operand) and the node's own
				// type (Record.cpp:843-849).
				dv := &DefVal{Rec: d}
				if !dv.Type().ConvertibleTo(target) {
					return nil, &EvalError{
						Msg: "Expected type '" + target.String() + "', got '" + dv.Type().String() +
							"' in: !cast<" + target.String() + ">(" + v.String() + ")\n",
						Span: b.Span(), AtRecord: true,
					}
				}
				return dv, nil
			}
			// UnOpInit::Fold allows a self-reference by name, but only at the
			// final resolve (IsFinal); earlier passes leave the cast as-is. A
			// non-final self-cast here would recurse into the record's own
			// fields and poison their cached state (getsetdagop.td).
			if rec != nil && rec.Name == s.S && !k.parseFold {
				return &DefVal{Rec: rec}, nil
			}
			// Record.cpp:839 prints this through PrintFatalErrorHelper: at
			// the record when one is being resolved, and with no location at
			// all from a global scope (if-unresolved-cast.td).
			// Record.cpp:839: the message itself ends with "\n" and
			// PrintFatalErrorHelper prints at the record, or with no
			// location at all from global scope (if-unresolved-cast.td).
			ee := &EvalError{Msg: "Undefined reference to record: '" + s.S + "'\n", Span: b.Span()}
			if rec != nil {
				ee.AtRecord = true
			} else {
				ee.NoLoc = true
			}
			return nil, ee
		}
	}
	return k.coerce(v, target)
}

func (k *Keeper) evalTypeTest(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	if b.Op == syntax.XExists {
		v, err := k.evalSyntax(b.Args[0], env, rec)
		if err != nil {
			return nil, err
		}
		s, ok := v.(*Str)
		if !ok {
			return nil, &EvalError{Msg: "!exists expects a string", Span: b.Span()}
		}
		d := k.Def(s.S)
		if d == nil {
			if rec != nil && rec.Name == s.S {
				d = rec
			} else {
				return &Int{N: 0}, nil
			}
		}
		target := k.tyFromSyntax(b.TypeArg)
		return &Int{N: boolInt(d.Type().IsA(target))}, nil
	}
	v, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	target := k.tyFromSyntax(b.TypeArg)
	return &Int{N: boolInt(v.Type().IsA(target))}, nil
}

func (k *Keeper) evalListComprehension(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	list, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	expr := b.Args[1]
	varName := ""
	if len(b.VarNames) > 0 {
		varName = b.VarNames[0]
	}
	if b.Op == syntax.XSort {
		ls, ok := list.(*List)
		if !ok {
			return nil, &EvalError{Msg: "!sort expects a list", Span: b.Span()}
		}
		type kv struct {
			key Value
			val Value
		}
		items := make([]kv, 0, len(ls.Elems))
		for _, e := range ls.Elems {
			key, err := k.evalSyntax(expr, env.WithBinding(varName, Binding{Val: e}), rec)
			if err != nil {
				return nil, err
			}
			items = append(items, kv{key: key, val: e})
		}
		useInt := false
		if len(items) > 0 {
			_, err := valueInt(items[0].key)
			useInt = err == nil
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
		out := &List{Elem: ls.Elem}
		for _, it := range items {
			out.Elems = append(out.Elems, it.val)
		}
		return out, nil
	}
	if d, ok := list.(*Dag); ok {
		if b.Op != syntax.XForEach {
			return nil, &EvalError{Msg: "!filter/!sort expects a list", Span: b.Span()}
		}
		return k.foreachDag(d, varName, expr, env, rec)
	}
	ls, ok := list.(*List)
	if !ok {
		return nil, &EvalError{Msg: "expected a list", Span: b.Span()}
	}
	out := &List{Elem: ls.Elem, Elems: make([]Value, 0, len(ls.Elems))}
	for _, e := range ls.Elems {
		sub := env.WithBinding(varName, Binding{Val: e})
		v, err := k.evalSyntax(expr, sub, rec)
		if err != nil {
			return nil, err
		}
		if b.Op == syntax.XFilter {
			n, err := k.asInt(v)
			if err != nil {
				return nil, err
			}
			if n != 0 {
				out.Elems = append(out.Elems, e)
			}
			out.Elem = ls.Elem
			continue
		}
		out.Elems = append(out.Elems, v)
		if out.Elem == nil {
			out.Elem = v.Type()
		}
	}
	return out, nil
}

// foreachDag mirrors ForeachDagApply: the operator and every argument are
// visited, nested dags recursively, and argument names are preserved.
func (k *Keeper) foreachDag(d *Dag, varName string, expr syntax.Value, env *Env, rec *Record) (Value, error) {
	apply := func(v Value) (Value, error) {
		return k.evalSyntax(expr, env.WithBinding(varName, Binding{Val: v}), rec)
	}
	op, err := apply(d.Op)
	if err != nil {
		return nil, err
	}
	out := &Dag{Op: op, OpName: d.OpName}
	for _, a := range d.Args {
		var av Value
		if ad, ok := a.Value.(*Dag); ok {
			av, err = k.foreachDag(ad, varName, expr, env, rec)
		} else {
			av, err = apply(a.Value)
		}
		if err != nil {
			return nil, err
		}
		out.Args = append(out.Args, DagArg{Value: av, Name: a.Name})
	}
	return out, nil
}

func (k *Keeper) evalFoldl(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	acc, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	list, err := k.evalSyntax(b.Args[1], env, rec)
	if err != nil {
		return nil, err
	}
	ls, ok := list.(*List)
	if !ok {
		return nil, &EvalError{Msg: "!foldl expects a list", Span: b.Span()}
	}
	aName, bName := "a", "b"
	if len(b.VarNames) > 0 {
		aName = b.VarNames[0]
	}
	if len(b.VarNames) > 1 {
		bName = b.VarNames[1]
	}
	for _, e := range ls.Elems {
		sub := env.With(map[string]Binding{aName: {Val: acc}, bName: {Val: e}})
		acc, err = k.evalSyntax(b.Args[2], sub, rec)
		if err != nil {
			return nil, err
		}
	}
	return acc, nil
}

func (k *Keeper) evalDagCtor(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	op, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	children, err := k.evalSyntax(b.Args[1], env, rec)
	if err != nil {
		return nil, err
	}
	names, err := k.evalSyntax(b.Args[2], env, rec)
	if err != nil {
		return nil, err
	}
	d := &Dag{Op: op}
	cl, childrenList := children.(*List)
	if !childrenList {
		if _, unset := children.(*Unset); !unset {
			return nil, &EvalError{Msg: "!dag children must be a list", Span: b.Span()}
		}
	}
	nl, namesList := names.(*List)
	if !namesList {
		if _, unset := names.(*Unset); !unset {
			return nil, &EvalError{Msg: "!dag names must be a list", Span: b.Span()}
		}
	}
	if childrenList && namesList && len(cl.Elems) != len(nl.Elems) {
		return nil, &EvalError{Msg: "!dag names list is longer than children", Span: b.Span()}
	}
	// TernOpInit::Fold(DAG) builds one child per position, using UnsetInit for
	// whichever side is `?`.
	size := 0
	if childrenList {
		size = len(cl.Elems)
	} else if namesList {
		size = len(nl.Elems)
	}
	for i := 0; i < size; i++ {
		arg := DagArg{Value: &Unset{}}
		if childrenList {
			arg.Value = cl.Elems[i]
		}
		if namesList {
			if s, ok := nl.Elems[i].(*Str); ok {
				arg.Name = s.S
			} else if _, unset := nl.Elems[i].(*Unset); !unset {
				return nil, &EvalError{Msg: "!dag names must be strings", Span: b.Span()}
			}
		}
		d.Args = append(d.Args, arg)
	}
	return d, nil
}

// dagArgIndex mirrors getDagArgNoByKey (Record.cpp:1187-1220) together with
// the "!getdagarg "/"!setdagarg "/"!setdagname " prefix each caller adds
// (Record.cpp:1410, 1914, 1932).
func dagArgIndex(d *Dag, key Value, op string) (int, error) {
	switch x := key.(type) {
	case *Int:
		if x.N < 0 {
			return 0, &EvalError{Msg: op + " index " + itoa(int(x.N)) + " is negative"}
		}
		if int(x.N) >= len(d.Args) {
			return 0, &EvalError{Msg: op + " index " + itoa(int(x.N)) + " is out of range (dag has " +
				itoa(len(d.Args)) + " arguments)"}
		}
		return int(x.N), nil
	case *Str:
		for i, a := range d.Args {
			if a.Name == x.S {
				return i, nil
			}
		}
		return 0, &EvalError{Msg: op + " key '" + x.S + "' is not found"}
	}
	// TGParser rejects a non int/string key while parsing (TGParser.cpp:2195).
	return 0, &EvalError{Msg: "expected integer index or string name, got type '" + key.Type().String() + "'"}
}

// softError records a diagnostic that llvm-tblgen prints without failing the
// run (PrintError inside a folding operator).
func (k *Keeper) softError(msg string, span diag.Span) {
	if k.diags != nil {
		k.diags.SoftError("E_SEMANTIC", msg, span)
	}
}

// softErrorAt anchors a PrintError to Record::getLoc() the way the folding
// operators do (Record.cpp:1877, 1896): the record's name token when one is
// being resolved, and the operator otherwise.
func (k *Keeper) softErrorAt(msg string, span diag.Span, rec *Record) {
	if rec != nil && len(rec.Locs) > 0 {
		span = diag.Span{Start: rec.Locs[0], End: rec.Locs[0]}
	}
	k.softError(msg, span)
}

func (k *Keeper) evalGetDagArg(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	dv, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	key, err := k.evalSyntax(b.Args[1], env, rec)
	if err != nil {
		return nil, err
	}
	d, ok := dv.(*Dag)
	if !ok {
		return nil, &EvalError{Msg: "!getdagarg expects a dag", Span: b.Span()}
	}
	idx, err := dagArgIndex(d, key, "!getdagarg")
	if err != nil {
		return nil, err
	}
	arg := d.Args[idx].Value
	// GETDAGARG folds to UnsetInit when the argument's static type is not
	// convertible to the requested type.
	if target := k.tyFromSyntax(b.TypeArg); target.Kind != AnyTy {
		if !arg.Type().ConvertibleTo(target) {
			return &Unset{}, nil
		}
	}
	return arg, nil
}

func (k *Keeper) evalSetDagArgName(b *syntax.BangValue, env *Env, rec *Record) (Value, error) {
	dv, err := k.evalSyntax(b.Args[0], env, rec)
	if err != nil {
		return nil, err
	}
	key, err := k.evalSyntax(b.Args[1], env, rec)
	if err != nil {
		return nil, err
	}
	val, err := k.evalSyntax(b.Args[2], env, rec)
	if err != nil {
		return nil, err
	}
	d, ok := dv.(*Dag)
	if !ok {
		return nil, &EvalError{Msg: "operator expects a dag", Span: b.Span()}
	}
	op := "!setdagarg"
	if b.Op == syntax.XSetDagName {
		op = "!setdagname"
	}
	idx, err := dagArgIndex(d, key, op)
	if err != nil {
		return nil, err
	}
	out := &Dag{Op: d.Op, OpName: d.OpName, Args: append([]DagArg(nil), d.Args...)}
	if b.Op == syntax.XSetDagArg {
		out.Args[idx].Value = val
	} else if _, unset := val.(*Unset); unset {
		// LLVM stores dyn_cast<StringInit>(RHS), so `?` clears the name.
		out.Args[idx].Name = ""
	} else {
		s, ok := val.(*Str)
		if !ok {
			return nil, &EvalError{Msg: "!setdagname expects a string", Span: b.Span()}
		}
		out.Args[idx].Name = s.S
	}
	return out, nil
}
