package syntax

import (
	"sort"

	"tblgen/internal/diag"
)

// idMode mirrors IDParseMode: a name being introduced (`def Foo`) is not looked
// up, a name being used is.
type idMode uint8

const (
	parseValueMode idMode = iota
	parseNameMode
)

// Shared singleton types. Types are values, so sharing is just convenience.
var (
	InvalidTy = Type{Kind: InvalidType}
	UnsetTy   = Type{Kind: UnsetType}
	IntTy     = Type{Kind: IntType}
	BitTy     = Type{Kind: BitType}
	StringTy  = Type{Kind: StringType}
	CodeTy    = Type{Kind: CodeType}
	DagTy     = Type{Kind: DagType}
)

func recordType(name string) Type { return Type{Kind: RecordType, Recs: []string{name}} }

// recordTypeSet builds a RecordRecTy-style set: sorted by name and deduped,
// because LLVM canonicalizes record types that way (RecordRecTy::get sorts).
// The empty set is the "any record" type.
func recordTypeSet(names []string) Type {
	if len(names) == 0 {
		return Type{Kind: RecordType}
	}
	cp := append([]string(nil), names...)
	sort.Strings(cp)
	out := cp[:0]
	for _, n := range cp {
		if len(out) == 0 || out[len(out)-1] != n {
			out = append(out, n)
		}
	}
	return Type{Kind: RecordType, Recs: out}
}

// recordInfoType is the static type of a def: the set of its direct
// superclasses (Record::getType), not the def itself.
func recordInfoType(r *recordInfo) Type { return recordTypeSet(r.parents) }

func listOf(elem Type) Type { return Type{Kind: ListType, Elem: &elem} }

func bitsOf(n int) Type { return Type{Kind: BitsType, Bits: n} }

// ---------------------------------------------------------------------------
// Values

// parseValue parses `SimpleValue ValueSuffix*`. item is the expected type, when
// the context knows one; it is needed to give empty list literals a type and to
// type-check nested literals the way C++ does.
func (p *Parser) parseValue(rec *recordInfo, item *Type, mode idMode) Value {
	start := p.i
	lhs := p.parseSimpleValue(rec, item, mode)
	if lhs == nil {
		return nil
	}
	for {
		switch p.cur().Kind {
		case LBrace:
			if mode == parseNameMode {
				// This '{' starts the object body, not a bit range.
				return lhs
			}
			brace := p.cur().Span
			p.advance()
			bits, ok := p.parseRangeList()
			if !ok || len(bits) == 0 {
				return nil
			}
			if !p.eat(RBrace) {
				p.errTok("E_PARSE_BITRANGE_RBRACE", "expected '}' at end of bit range list")
				p.errAt(brace, "E_NOTE_BRACE", "to match this '{'")
				return nil
			}
			base := lhs
			// TypedInit::convertInitializerBitRange wraps every selected bit
			// in a VarBitInit and returns a BitsInit even for one index
			// (Record.cpp:2246-2258), so `op{5}` has type bits<1>.
			lhs = &BitRangeValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: bitsOf(len(bits))}, Base: base, Bits: bits}
			// Init::convertInitializerBitRange runs right here in the parser
			// (TGParser.cpp:3257): slicing a literal BitsInit selects the
			// bits and yields another concrete BitsInit.
			if bv, ok := base.(*BitsValue); ok {
				if folded, ok := foldBitRangeLiteral(bv, bits); ok {
					lhs = folded
				}
			}
		case LSquare:
			next, ok := p.parseSlice(lhs, start)
			if !ok {
				return nil
			}
			lhs = next
		case Dot:
			dotStart := p.i
			_ = dotStart
			p.advance()
			nt, ok := p.expect(Id, "E_PARSE_FIELD_ID", "expected field identifier after '.'")
			if !ok {
				return nil
			}
			ty, ok := p.fieldTypeOf(lhs, nt.Text)
			if !ok {
				p.errAt(nt.Span, "E_PARSE_FIELD",
					"Cannot access field '"+nt.Text+"' of value '"+valueString(lhs)+"'")
				return nil
			}
			var lit Value
			if rt := lhs.Type(); rt.Kind == RecordType {
				for _, rn := range rt.Recs {
					if rr := p.recordInfoFor(rn); rr != nil {
						if b, ok := rr.field(nt.Text, map[string]bool{}); ok {
							lit = b.lit
						}
					}
				}
			}
			lhs = &FieldValue{
				ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: ty},
				Base:      lhs, Field: nt.Text, FieldSpan: nt.Span, Lit: lit,
			}
		case Paste:
			next, ok := p.parsePaste(lhs, item, mode, start)
			if !ok {
				return nil
			}
			lhs = next
		default:
			return lhs
		}
	}
}

func (p *Parser) parseSimpleValue(rec *recordInfo, item *Type, mode idMode) Value {
	start := p.i
	if p.cur().Kind.IsBangOp() {
		return p.parseBang(rec, item)
	}
	switch p.cur().Kind {
	case TrueVal:
		t := p.advance()
		return &IntValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: IntTy}, Num: 1}
	case FalseVal:
		t := p.advance()
		return &IntValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: IntTy}, Num: 0}
	case IntVal:
		t := p.advance()
		return &IntValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: IntTy}, Num: t.Num}
	case BinaryIntVal:
		t := p.advance()
		width := len(t.Text) - 2
		if width < 0 {
			width = 0
		}
		v := &BitsValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: bitsOf(width)}, Num: t.Num}
		for i := width - 1; i >= 0; i-- {
			bit := &IntValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: BitTy}, Num: (t.Num >> uint(i)) & 1}
			v.Elems = append(v.Elems, bit)
		}
		return v
	case StrVal:
		t := p.advance()
		text := t.Text
		for p.at(StrVal) {
			text += p.advance().Text
		}
		return &StringValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: StringTy}, Text: text}
	case CodeFragment:
		t := p.advance()
		return &StringValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: StringTy}, Text: t.Text, Code: true}
	case Question:
		t := p.advance()
		return &UnsetValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: UnsetTy}}
	case Id:
		return p.parseIdentValue(rec, mode, start)
	case LBrace:
		return p.parseBitsLiteral(rec, start)
	case LSquare:
		return p.parseListLiteral(rec, item, start)
	case LParen:
		return p.parseDagLiteral(rec, start)
	default:
		p.errTok("E_PARSE_VALUE", "Unknown or reserved token when parsing a value")
		return nil
	}
}

func (p *Parser) parseIdentValue(rec *recordInfo, mode idMode, start int) Value {
	nt := p.advance()
	if p.at(Less) {
		p.advance()
		args, ok := p.parseTemplateArgValues(rec, p.classArgs[nt.Text], nt.Text, nt.Span, ":")
		if !ok {
			return nil
		}
		ty := InvalidTy
		if _, ok := p.classes[nt.Text]; ok {
			ty = recordType(nt.Text)
			ty.IsClass = true
		} else {
			p.errAt(nt.Span, "E_PARSE_CLASS_APPLY",
				"Expected a class name, got '"+nt.Text+"'")
			return nil
		}
		return &ClassApplyValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: ty}, Class: nt.Text, ClassSpan: nt.Span, Args: args}
	}
	if b, ok := p.lookupLocal(nt.Text); ok {
		return &VarValue{ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: b.ty}, Name: nt.Text, Lit: b.lit, Subst: b.isDefvar && b.lit != nil, Lexical: b.lexical, ArgRef: b.arg, ArgQual: b.qual}
	}
	if mode == parseNameMode {
		return &StringValue{ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: StringTy}, Text: nt.Text}
	}
	if b, g, ok := p.lookupGlobal(nt.Text); ok {
		ty := b.ty
		if g != nil && g.kind == gClass {
			ty = recordType(g.rec.name)
		}
		globalVar := g != nil && (g.kind == gDefvar || g.kind == gDefset)
		return &VarValue{ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: ty}, Name: nt.Text, Lit: b.lit, GlobalRef: true, GlobalVar: globalVar}
	}
	if rec != nil && rec.isClass && nt.Text == "NAME" {
		// TGVarScope::getVar returns VarInit(QualifyName(Rec, "NAME"))
		// (TGParser.cpp:151-163): the printable name carries its owner, with
		// "::" inside a multiclass.
		sep := ":"
		if p.curMulti != nil {
			sep = "::"
		}
		return &VarValue{
			ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: StringTy},
			Name:      nt.Text,
			ArgRef:    true,
			ArgQual:   rec.name + sep + "NAME",
		}
	}
	if rec != nil && !rec.isClass && rec.name == nt.Text {
		// ParseIDValue turns a direct self-reference into
		// `!cast<CurRec->getType()>("Name")` (TGParser.cpp:1206-1211). The
		// type is the set of direct superclasses attached so far, so
		// `def F0 : Fa, Fb<F0>` type-checks while `def R : Cl<R>` leaves the
		// cast at `{}` and prints `!cast<{}>("R")`.
		ty := recordTypeSet(rec.parents)
		return &BangValue{
			ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: ty},
			Op:        XCast,
			OpSpan:    nt.Span,
			TypeArg:   ty,
			Args: []Value{&StringValue{
				ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: StringTy},
				Text:      nt.Text,
			}},
		}
	}
	p.errAt(nt.Span, "E_PARSE_VAR_UNDEFINED", "Variable not defined: '"+nt.Text+"'")
	if p.opts.LenientUndefinedNames {
		// Keep the statement so the driver's lowering pass still sees the
		// loop/defm that will create this name; see Options.
		return &UnsetValue{ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: Type{Kind: UnsetType}}}
	}
	return nil
}

func (p *Parser) parseBitsLiteral(rec *recordInfo, start int) Value {
	p.advance() // '{'
	var elems []Value
	if !p.at(RBrace) {
		var ok bool
		elems, ok = p.parseValueList(rec, nil, true)
		if !ok {
			return nil
		}
	}
	if !p.eat(RBrace) {
		p.errTok("E_PARSE_BITSLIT_RBRACE", "expected '}' at end of bit list value")
		return nil
	}
	width := 0
	for _, e := range elems {
		w, ok := bitWidth(e)
		if !ok {
			p.errAt(e.Span(), "E_PARSE_BITSLIT_ELEM", "bit list element is not convertible to a bit")
			return nil
		}
		width += w
	}
	return &BitsValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: bitsOf(width)}, Elems: elems}
}

func bitWidth(v Value) (int, bool) {
	switch t := v.(type) {
	case *BitsValue:
		return t.Ty.Bits, true
	}
	if v.Type().Kind == BitsType {
		return v.Type().Bits, true
	}
	if v.Type().Kind == InvalidType {
		return 1, false
	}
	return 1, true
}

func (p *Parser) parseListLiteral(rec *recordInfo, item *Type, start int) Value {
	p.advance() // '['
	var hint *Type
	if item != nil {
		if item.Kind == ListType {
			hint = item.Elem
		} else if item.Kind != InvalidType {
			p.errAt(p.toks[start].Span, "E_PARSE_LIST_CONTEXT",
				"Encountered a list when expecting a "+item.String())
			return nil
		}
	}
	var elems []Value
	if !p.at(RSquare) {
		var ok bool
		elems, ok = p.parseValueList(rec, hint, true)
		if !ok {
			return nil
		}
	}
	if !p.eat(RSquare) {
		p.errTok("E_PARSE_LISTLIT_RSQUARE", "expected ']' at end of list value")
		return nil
	}
	var explicit *Type
	if p.at(Less) {
		p.advance()
		ty, ok := p.parseType()
		if !ok {
			return nil
		}
		if !p.eat(Greater) {
			p.errTok("E_PARSE_LISTLIT_GT", "expected '>' at end of list element type")
			return nil
		}
		explicit = &ty
	}
	elemTy := InvalidTy
	if explicit != nil {
		elemTy = *explicit
	}
	for _, e := range elems {
		t := e.Type()
		if t.Kind == InvalidType || t.Kind == UnsetType {
			continue
		}
		if elemTy.Kind == InvalidType {
			elemTy = t
			continue
		}
		if !p.typeConvertible(t, elemTy) {
			if u, ok := p.unifyRecordTypes(elemTy, t); ok {
				elemTy = u
				continue
			}
			p.errTok("E_PARSE_LIST_TYPES", "Incompatible types in list elements")
			return nil
		}
	}
	if explicit != nil && elemTy.Kind != InvalidType && !p.typeConvertible(elemTy, *explicit) {
		p.errTok("E_PARSE_LIST_TYPES", "Incompatible types in list elements")
		return nil
	}
	if explicit != nil {
		elemTy = *explicit
	} else if elemTy.Kind == InvalidType {
		if hint != nil {
			elemTy = *hint
		} else if item != nil && item.Kind == ListType && item.Elem != nil {
			elemTy = *item.Elem
		} else {
			p.errTok("E_PARSE_LIST_NOTYPE", "No type for list")
			return nil
		}
	}
	return &ListValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: listOf(elemTy)}, Elems: elems, ElemType: explicit}
}

// unifyRecordTypes is RecordRecTy::resolveRecordTypes (Record.cpp:300-325):
// walk a's ancestors and keep every class that b is a subclass of. Keeping the
// whole set matters for heterogeneous lists: [LB, SB] must stay a
// list<Instruction>, not collapse to the one immediate superclass both share
// (`Sched`), which is not itself an Instruction.
func (p *Parser) unifyRecordTypes(a, b Type) (Type, bool) {
	if a.Kind != RecordType || b.Kind != RecordType {
		return InvalidTy, false
	}
	var common []string
	seen := map[string]bool{}
	stack := append([]string(nil), a.Recs...)
	for len(stack) > 0 {
		r := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[r] {
			continue
		}
		seen[r] = true
		// RecordRecTy::isSubClassOf: any of b's classes is r or derives from it.
		match := false
		for _, mine := range b.Recs {
			if p.nameIsSubclass(mine, r) {
				match = true
				break
			}
		}
		if match {
			common = append(common, r)
			continue
		}
		if ri := p.recordInfoFor(r); ri != nil {
			stack = append(stack, ri.parents...)
		}
	}
	if len(common) == 0 {
		// RecordRecTy with no classes: the "any record" type.
		return Type{Kind: RecordType}, true
	}
	return recordTypeSet(common), true
}

func (p *Parser) ancestors(t Type) map[string]bool {
	out := map[string]bool{}
	for _, n := range p.ancestorsOrdered(t) {
		out[n] = true
	}
	return out
}

func (p *Parser) ancestorsOrdered(t Type) []string {
	var out []string
	var walk func(name string, depth int)
	seen := map[string]bool{}
	walk = func(name string, depth int) {
		if seen[name] || depth > 64 {
			return
		}
		seen[name] = true
		out = append(out, name)
		rec := p.recordInfoFor(name)
		if rec != nil {
			for _, pn := range rec.parents {
				walk(pn, depth+1)
			}
		}
	}
	for _, n := range t.Recs {
		walk(n, 0)
	}
	return out
}

func (p *Parser) parseDagLiteral(rec *recordInfo, start int) Value {
	p.advance() // '('
	switch p.cur().Kind {
	case Id, XCast, Question, XGetDagOp, LSquare:
	default:
		p.errTok("E_PARSE_DAG_OP", "expected identifier or list of value types in dag init")
		return nil
	}
	op := p.parseValue(rec, nil, parseValueMode)
	if op == nil {
		return nil
	}
	var opName string
	if p.eat(Colon) {
		if !p.at(VarName) {
			p.errTok("E_PARSE_DAG_NAME", "expected variable name in dag operator")
			return nil
		}
		opName = p.advance().Text
	}
	var args []*DagArg
	if !p.at(RParen) {
		for {
			argStart := p.i
			arg := &DagArg{}
			if p.at(VarName) {
				nt := p.advance()
				arg.Name, arg.HasName = nt.Text, true
				arg.Value = &UnsetValue{ValueBase: ValueBase{Base: Base{At: nt.Span}, Ty: UnsetTy}}
			} else {
				v := p.parseValue(rec, nil, parseValueMode)
				if v == nil {
					return nil
				}
				arg.Value = v
				if p.eat(Colon) {
					if !p.at(VarName) {
						p.errTok("E_PARSE_DAG_ARGNAME", "expected variable name in dag literal")
						return nil
					}
					arg.Name, arg.HasName = p.advance().Text, true
				}
			}
			arg.Base.At = p.span(argStart)
			args = append(args, arg)
			if !p.eat(Comma) {
				break
			}
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_DAG_RPAREN", "expected ')' in dag init")
		return nil
	}
	return &DagValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: DagTy}, Op: op, OpName: opName, Args: args}
}

func (p *Parser) parseValueList(rec *recordInfo, item *Type, allowTrailing bool) ([]Value, bool) {
	var out []Value
	for {
		v := p.parseValue(rec, item, parseValueMode)
		if v == nil {
			return nil, false
		}
		out = append(out, v)
		if !p.eat(Comma) {
			return out, true
		}
		if allowTrailing && (p.at(RSquare) || p.at(RBrace) || p.at(Greater)) {
			return out, true
		}
	}
}

// foldBitRangeLiteral is BitsInit's convertInitializerBitRange for a concrete
// literal: bits lists the selected indices high-to-low, and a BitsInit stores
// its bits high-to-low as well.
func foldBitRangeLiteral(b *BitsValue, bits []int32) (Value, bool) {
	n := len(b.Elems)
	if n == 0 {
		return nil, false
	}
	elems := make([]Value, len(bits))
	for i, idx := range bits {
		if idx < 0 || int(idx) >= n {
			return nil, false
		}
		elems[i] = b.Elems[n-1-int(idx)]
	}
	return &BitsValue{ValueBase: ValueBase{Base: b.Base, Ty: bitsOf(len(bits))}, Elems: elems}, true
}

func (p *Parser) parseSlice(base Value, start int) (Value, bool) {
	lhsLoc := base.Span()
	bt := base.Type()
	// TGParser.cpp:3276-3286: a value with no type (an unset `?`) is not a
	// TypedInit, while a typed non-list names its type.
	if bt.Kind == UnsetType {
		p.errAt(lhsLoc, "E_PARSE_SLICE_VALUE", "Invalid value, list expected")
		return nil, false
	}
	if bt.Kind != InvalidType && bt.Kind != ListType {
		p.errAt(lhsLoc, "E_PARSE_SLICE_TYPE", "Type '"+bt.String()+"' is invalid, list expected")
		return nil, false
	}
	p.advance() // '['
	var elems []*SliceElem
	listResult := false
	for {
		elStart := p.i
		lo := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
		if lo == nil {
			return nil, false
		}
		el := &SliceElem{Lo: lo}
		if p.at(DotDotDot) || p.at(Minus) {
			p.advance()
			hi := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
			if hi == nil {
				return nil, false
			}
			// TGParser::ParseSliceElement: RHS then LHS must both be int.
			if ty := hi.Type(); ty.Kind != InvalidType && ty.Kind != IntType {
				p.errAt(hi.Span(), "E_PARSE_SLICE_RANGE", "expected int...int, got "+ty.String())
				return nil, false
			}
			if ty := lo.Type(); ty.Kind != InvalidType && ty.Kind != IntType {
				p.errAt(lo.Span(), "E_PARSE_SLICE_RANGE", "expected int...int, got "+ty.String())
				return nil, false
			}
			el.Hi = hi
		} else if p.at(IntVal) {
			hiVal := -int64(p.cur().Num)
			if hiVal < 0 {
				p.errTok("E_PARSE_RANGE_NEG", "invalid range, cannot be negative")
				return nil, false
			}
			t := p.advance()
			el.Hi = &IntValue{ValueBase: ValueBase{Base: Base{At: t.Span}, Ty: IntTy}, Num: uint64(hiVal)}
		}
		el.Base.At = p.span(elStart)
		// ParseSliceElements: a list subscript must be list<int>; anything
		// else that is not int is not a legal set of indices.
		if el.Hi == nil {
			if ty := lo.Type(); ty.Kind == ListType {
				if ty.Elem != nil && ty.Elem.Kind != InvalidType && ty.Elem.Kind != IntType {
					p.errAt(lo.Span(), "E_PARSE_SLICE_LIST", "expected list<int>, got "+ty.String())
					return nil, false
				}
			} else if ty.Kind != InvalidType && ty.Kind != IntType {
				p.errAt(lo.Span(), "E_PARSE_SLICE_TYPE", "unhandled type "+ty.String()+" in range")
				return nil, false
			}
		}
		elems = append(elems, el)
		if lo.Type().Kind == ListType {
			listResult = true
		}
		if !p.eat(Comma) {
			break
		}
		listResult = true
		if p.at(RSquare) {
			break
		}
	}
	if !p.eat(RSquare) {
		p.errTok("E_PARSE_SLICE_RSQUARE", "expected ']' at end of list slice")
		return nil, false
	}
	elemTy := InvalidTy
	if bt.Kind == ListType && bt.Elem != nil {
		elemTy = *bt.Elem
	}
	single := len(elems) == 1 && elems[0].Hi == nil
	ty := listOf(elemTy)
	if single && !listResult && elemTy.Kind != InvalidType {
		ty = elemTy
	} else if len(elems) == 1 && elems[0].Hi != nil {
		listResult = true
	}
	return &SliceValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: ty}, Base: base, Index: elems, ListResult: listResult}, true
}

func (p *Parser) parsePaste(lhs Value, item *Type, mode idMode, start int) (Value, bool) {
	lt := lhs.Type()
	if lt.Kind == ListType {
		p.advance() // '#'
		switch p.cur().Kind {
		case Colon, Semi, LBrace:
			return lhs, true // trailing paste
		}
		rhs := p.parseValue(p.curRecOrNil(), item, parseValueMode)
		if rhs == nil {
			return nil, false
		}
		ty := lt
		if rhs.Type().Kind == ListType && rhs.Type().Elem != nil {
			ty = rhs.Type()
		}
		return &PasteValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: ty}, LHS: lhs, RHS: rhs}, true
	}
	p.advance() // '#'
	var rhs Value
	switch p.cur().Kind {
	case Colon, Semi, LBrace:
		rhs = &StringValue{ValueBase: ValueBase{Base: Base{At: p.cur().Span}, Ty: StringTy}}
	default:
		rhs = p.parseValue(p.curRecOrNil(), nil, parseNameMode)
		if rhs == nil {
			return nil, false
		}
	}
	return &PasteValue{ValueBase: ValueBase{Base: Base{At: p.span(start)}, Ty: StringTy}, LHS: lhs, RHS: rhs}, true
}

// ---------------------------------------------------------------------------
// Field types

func (p *Parser) fieldTypeOf(base Value, field string) (Type, bool) {
	// A def's static type is only its superclass set, so a def with no
	// superclass looks like "any record". The def itself still knows its
	// fields (DefInit::getFieldType), so consult the named record first.
	if v, ok := base.(*VarValue); ok {
		if rr := p.recordInfoFor(v.Name); rr != nil {
			if b, ok := rr.field(field, map[string]bool{}); ok {
				return b.ty, true
			}
		}
	}
	t := base.Type()
	switch t.Kind {
	case RecordType:
		if len(t.Recs) == 0 {
			// "any record": we genuinely do not know, so do not reject.
			return InvalidTy, true
		}
		for _, name := range t.Recs {
			rec := p.recordInfoFor(name)
			if rec != nil {
				if b, ok := rec.field(field, map[string]bool{}); ok {
					return b.ty, true
				}
			}
		}
		return InvalidTy, false
	}
	return InvalidTy, false
}

// ---------------------------------------------------------------------------
// Bang operators

func (p *Parser) parseOperatorType() (Type, bool) {
	if !p.eat(Less) {
		p.errTok("E_PARSE_OPTYPE_LT", "expected type name for operator")
		return InvalidTy, false
	}
	if p.at(Code) {
		// TGParser::ParseOperatorType reports but still returns the type
		// (TGParser.cpp:2369-2390), so the operator is built and the rest of
		// the value parses normally.
		p.errTok("E_PARSE_OPTYPE_CODE",
			"the 'code' type is not allowed in bang operators; use 'string'")
	}
	ty, ok := p.parseType()
	if !ok {
		// TGParser.cpp:2383-2386 reports "expected type name for operator"
		// after ParseType has already reported the unknown token.
		p.errTok("E_PARSE_OPTYPE_GT", "expected type name for operator")
		return InvalidTy, false
	}
	if !p.eat(Greater) {
		p.errTok("E_PARSE_OPTYPE_GT", "expected type name for operator")
		return InvalidTy, false
	}
	return ty, true
}

func (p *Parser) parseBang(rec *recordInfo, item *Type) Value {
	start := p.i
	v := p.parseBangBody(rec, item)
	if b, ok := v.(*BangValue); ok {
		b.Base.At = p.span(start)
	}
	return v
}

func (p *Parser) parseBangBody(rec *recordInfo, item *Type) Value {
	opTok := p.advance()
	v := &BangValue{Op: opTok.Kind, OpSpan: opTok.Span, TypeArg: InvalidTy}

	switch v.Op {
	case XCast, XIsA, XExists, XInstances, XGetDagArg:
		ty, ok := p.parseOperatorType()
		if !ok {
			return nil
		}
		v.TypeArg = ty
	case XGetDagOp:
		if p.at(Less) {
			ty, ok := p.parseOperatorType()
			if !ok {
				return nil
			}
			// TGParser.cpp:1310-1325 reports a non-record type but keeps
			// parsing to consume the operand.
			if ty.Kind != InvalidType && ty.Kind != RecordType {
				p.errTok("E_PARSE_GETDAGOP_TYPE", "type for !getdagop must be a record type")
			}
			v.TypeArg = ty
		}
	}

	switch v.Op {
	case XNot, XToLower, XToUpper, XListFlatten, XLog2, XHead, XTail, XSize,
		XEmpty, XCast, XRepr, XGetDagOp, XGetDagOpName, XInitialized:
		return p.finishUnary(v, rec)
	case XConcat, XMatch, XAdd, XSub, XMul, XDiv, XAnd, XOr, XXor,
		XSra, XSrl, XShl, XEq, XNe, XLe, XLt, XGe, XGt,
		XListConcat, XListSplat, XListRemove, XStrConcat, XInterleave,
		XGetDagArg, XGetDagName, XSetDagOp, XSetDagOpName:
		return p.finishBinary(v, rec, item)
	case XSetDagArg, XSetDagName, XDag, XIf, XSubst:
		return p.finishTernary(v, rec, item)
	case XIsA:
		if v.TypeArg.Kind == InvalidType {
			return nil
		}
		arg, ok := p.parseOneArg(rec, "!isa")
		if !ok {
			return nil
		}
		v.Args = []Value{arg}
		// IsAOpInit's type is int in LLVM 24 (the value is 0/1).
		return v.withType(IntTy)
	case XExists:
		if v.TypeArg.Kind == InvalidType {
			return nil
		}
		// TGParser records ExprLoc = Lex.getLoc() after consuming the '('
		// (TGParser.cpp:1463), so the diagnostic points at the first token of
		// the argument rather than at the parenthesis.
		if !p.eat(LParen) {
			p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !exists operator")
			return nil
		}
		loc := p.cur().Span
		arg := p.parseValue(rec, nil, parseValueMode)
		if arg == nil {
			return nil
		}
		if !p.eat(RParen) {
			p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !exists")
			return nil
		}
		at := arg.Type()
		if at.Kind == UnsetType || at.Kind == InvalidType {
			p.errAt(loc, "E_PARSE_EXISTS_TYPE", "expected string type argument in !exists operator")
			return nil
		}
		if at.Kind == RecordType {
			p.errAt(loc, "E_PARSE_EXISTS_REC",
				"expected string type argument in !exists operator, please use !isa instead")
			return nil
		}
		if at.Kind != StringType {
			p.errAt(loc, "E_PARSE_EXISTS_TYPE", "expected string type argument in !exists operator")
			return nil
		}
		v.Args = []Value{arg}
		// ExistsOpInit is int-typed as well.
		return v.withType(IntTy)
	case XInstances:
		if v.TypeArg.Kind == InvalidType {
			return nil
		}
		if !p.eat(LParen) {
			p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after type of !instances")
			return nil
		}
		loc := p.cur().Span
		if !p.at(RParen) {
			arg := p.parseValue(rec, &StringTy, parseValueMode)
			if arg == nil {
				return nil
			}
			at := arg.Type()
			if at.Kind != StringType {
				p.errAt(loc, "E_PARSE_INSTANCES_TYPE", "expected string type argument in !instances operator")
				return nil
			}
			v.Args = []Value{arg}
		} else {
			// TGParser.cpp:1526: an omitted regex becomes the wildcard
			// StringInit(".*"), so the Init tree always carries an argument.
			v.Args = []Value{&StringValue{ValueBase: ValueBase{Base: Base{At: loc}, Ty: StringTy}, Text: ".*"}}
		}
		if !p.eat(RParen) {
			p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !instances")
			return nil
		}
		return v.withType(listOf(v.TypeArg))
	case XRange:
		return p.finishRange(v, rec)
	case XSubstr:
		return p.finishSubstr(v, rec)
	case XFind:
		return p.finishFind(v, rec)
	case XCond:
		return p.finishCond(v, rec, item)
	case XSwitch:
		return p.finishSwitch(v, rec, item)
	case XForEach, XFilter, XSort:
		return p.finishListComprehension(v, rec, item)
	case XFoldl:
		return p.finishFoldl(v, rec)
	default:
		p.errAt(opTok.Span, "E_PARSE_BANG_UNKNOWN", "unknown bang operator")
		return nil
	}
}

func (v *BangValue) withType(t Type) Value {
	v.Ty = t
	return v
}

func (p *Parser) bangOpen(desc string) (Span, bool) {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after "+desc)
		return Span{}, false
	}
	return p.cur().Span, true
}

func (p *Parser) parseOneArg(rec *recordInfo, desc string) (Value, bool) {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after "+desc+" operator")
		return nil, false
	}
	arg := p.parseValue(rec, nil, parseValueMode)
	if arg == nil {
		return nil, false
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in "+desc)
		return nil, false
	}
	return arg, true
}

func (p *Parser) finishUnary(v *BangValue, rec *recordInfo) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after unary operator")
		return nil
	}
	argLoc := p.cur().Span
	arg := p.parseValue(rec, nil, parseValueMode)
	if arg == nil {
		return nil
	}
	at := arg.Type()
	switch v.Op {
	case XEmpty, XSize:
		if at.Kind != InvalidType && at.Kind != ListType && at.Kind != StringType && at.Kind != DagType && at.Kind != UnsetType {
			p.errTok("E_PARSE_UNARY_ARG", "expected string, list, or dag type argument in unary operator")
			return nil
		}
	case XHead, XTail, XListFlatten:
		if at.Kind != InvalidType && at.Kind != UnsetType && at.Kind != ListType {
			p.errTok("E_PARSE_UNARY_LIST", "expected list type argument in unary operator")
			return nil
		}
		if at.Kind == ListType && at.Elem != nil && argIsEmptyList(arg) {
			p.errTok("E_PARSE_UNARY_EMPTY", "empty list argument in unary operator")
			return nil
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in unary operator")
		return nil
	}
	_ = argLoc

	if v.Op == XCast && p.castIsIdentity(at, v.TypeArg) {
		// TGParser folds a unary operator right after building it
		// (`UnOpInit::get(...)->Fold(CurRec)`, TGParser.cpp:1419). The CAST
		// case ends with LHS->convertInitializerTo(getType()), and every
		// convertInitializerTo returns the operand itself when it already has
		// the target type (Record.cpp:681 ListInit, 803-856 UnOpInit CAST),
		// so an identity cast leaves no node in the Init tree.
		return arg
	}
	if v.Op == XCast {
		if folded := foldCastLiteral(arg, v.TypeArg); folded != nil {
			return folded
		}
	}

	var ret Type
	switch v.Op {
	case XNot, XLog2, XSize, XEmpty, XInitialized:
		ret = IntTy
	case XRepr, XToLower, XToUpper, XGetDagOpName:
		ret = StringTy
	case XCast:
		ret = v.TypeArg
	case XGetDagOp:
		if v.TypeArg.Kind != InvalidType {
			ret = v.TypeArg
		} else {
			ret = Type{Kind: RecordType}
		}
	case XHead:
		ret = elemOf(at)
	case XTail:
		if at.Kind == ListType {
			ret = at
		} else {
			ret = InvalidTy
		}
	case XListFlatten:
		if at.Kind == ListType && at.Elem != nil {
			if at.Elem.Kind == ListType && at.Elem.Elem != nil {
				ret = listOf(*at.Elem.Elem)
			} else {
				ret = at
			}
		} else {
			ret = InvalidTy
		}
	}
	v.Args = []Value{arg}
	return v.withType(ret)
}

// foldCastLiteral applies the literal conversions of
// IntInit::convertInitializerTo (Record.cpp:577-600) and
// BitsInit::convertInitializerTo (Record.cpp:447-451): an int cast to
// bits<N> becomes a concrete bit list, and a 1-bit list cast to bit becomes
// that bit. Values that do not convert are left as a cast node.
func foldCastLiteral(arg Value, to Type) Value {
	if to.Kind == BitType {
		// A bit range is a BitsInit of VarBitInits; a one-bit range converts
		// to that bit (BitsInit::convertInitializerTo(BitRecTy) returns
		// getBit(0), Record.cpp:447-451).
		if b, ok := arg.(*BitRangeValue); ok && len(b.Bits) == 1 && b.Ty.Kind == BitsType && b.Ty.Bits == 1 {
			nb := *b
			nb.Ty = BitTy
			return &nb
		}
		if b, ok := arg.(*BitsValue); ok && b.Ty.Kind == BitsType && b.Ty.Bits == 1 && len(b.Elems) == 1 {
			// BitsInit::convertInitializerTo(BitRecTy) returns getBit(0)
			// (Record.cpp:447-451): the bit itself, which keeps a reference
			// such as `Inst{22}` in the tree.
			el := b.Elems[0]
			if iv, ok := el.(*IntValue); ok {
				return &IntValue{ValueBase: ValueBase{Base: Base{At: arg.Span()}, Ty: BitTy}, Num: iv.Num}
			}
			return el
		}
	}
	if to.Kind != BitType && to.Kind != BitsType && to.Kind != IntType {
		return nil
	}
	n, ok := constInt(arg)
	if !ok {
		return nil
	}
	at := Base{At: arg.Span()}
	switch to.Kind {
	case IntType:
		return &IntValue{ValueBase: ValueBase{Base: at, Ty: IntTy}, Num: uint64(n)}
	case BitType:
		if n != 0 && n != 1 {
			return nil
		}
		return &IntValue{ValueBase: ValueBase{Base: at, Ty: BitTy}, Num: uint64(n)}
	default:
		if !canFitInBits(n, to.Bits) {
			return nil
		}
		elems := make([]Value, 0, to.Bits)
		for i := to.Bits - 1; i >= 0; i-- {
			var bit uint64
			if i < 64 {
				bit = uint64(n) >> uint(i) & 1
			}
			elems = append(elems, &IntValue{ValueBase: ValueBase{Base: at, Ty: BitTy}, Num: bit})
		}
		return &BitsValue{ValueBase: ValueBase{Base: at, Ty: bitsOf(to.Bits)}, Elems: elems}
	}
}

// canFitInBits mirrors canFitInBitfield (Record.cpp:571): with NumBits == 4,
// values from -7 to 15 fit.
func canFitInBits(n int64, bits int) bool {
	return bits >= 64 || n>>uint(bits) == 0 || n>>uint(bits-1) == -1
}

// castIsIdentity reports whether `!cast<to>(v)` of type from is the operand
// itself. convertInitializerTo returns `this` when the value already converts
// without changing anything: the same string type, a record that already is a
// subclass of the target, or a list whose elements all convert unchanged
// (Record.cpp:681-705). Other conversions still fold, but into a different
// literal, which the printer renders from the declared type instead.
func (p *Parser) castIsIdentity(from, to Type) bool {
	if to.Kind == InvalidType || from.Kind == InvalidType || from.Kind == UnsetType {
		return false
	}
	switch to.Kind {
	case StringType:
		return from.Kind == StringType || from.Kind == CodeType
	case ListType:
		if from.Kind != ListType || from.Elem == nil || to.Elem == nil {
			return false
		}
		return p.castIsIdentity(*from.Elem, *to.Elem)
	case IntType:
		return from.Kind == IntType
	case RecordType:
		return from.Kind == RecordType && p.typeConvertible(from, to)
	case BitType:
		return from.Kind == BitType
	case BitsType:
		return from.Kind == BitsType && from.Bits == to.Bits
	case DagType:
		return from.Kind == DagType
	}
	return false
}

func argIsEmptyList(v Value) bool {
	if l, ok := v.(*ListValue); ok {
		return len(l.Elems) == 0
	}
	return false
}

func elemOf(t Type) Type {
	if t.Kind == ListType && t.Elem != nil {
		return *t.Elem
	}
	return InvalidTy
}

// finishBinary parses the 2..N argument form. N > 2 is allowed only for
// associative operators, exactly as in TGParser.
func (p *Parser) finishBinary(v *BangValue, rec *recordInfo, item *Type) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after binary operator")
		return nil
	}
	var args []Value
	var hint *Type
	for {
		argItem := p.binaryArgType(v.Op, item)
		if hint != nil {
			argItem = hint
		}
		arg := p.parseValue(rec, argItem, parseValueMode)
		if arg == nil {
			return nil
		}
		if arg.Type().Kind == UnsetType {
			p.errAt(v.OpSpan, "E_PARSE_BANG_UNTYPED",
				"expected value to be a typed value, got '"+valueString(arg)+"'")
			return nil
		}
		// The comparison operators type-check the first operand right after
		// it is parsed; a bad one aborts the whole value and leaves the parser
		// at the comma (TGParser.cpp:1802-1826).
		if len(args) == 0 && !p.checkCompareArg(v.Op, arg) {
			return nil
		}
		// !interleave checks each argument as it is parsed (TGParser.cpp:
		// 1827-1845).
		if v.Op == XInterleave && !p.checkInterleaveArg(len(args), arg) {
			return nil
		}
		if v.Op == XListConcat || v.Op == XListRemove {
			if len(args) == 0 && arg.Type().Kind != InvalidType && arg.Type().Kind != ListType {
				p.errAt(arg.Span(), "E_PARSE_BANG_LIST",
					"expected a list, got value of type '"+arg.Type().String()+"'")
				return nil
			}
		}
		args = append(args, arg)
		// !listconcat/!listremove determine the element type from their first
		// argument and pass it to the rest (TGParser: ArgType = ListType), so
		// an untyped `[]` in a later operand still gets a type.
		if hint == nil && len(args) == 1 && (v.Op == XListConcat || v.Op == XListRemove) && arg.Type().Kind == ListType {
			t := arg.Type()
			hint = &t
		}
		if !p.eat(Comma) {
			break
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in operator")
		return nil
	}
	if len(args) != 2 {
		assoc := v.Op == XStrConcat || v.Op == XListConcat || v.Op == XConcat ||
			v.Op == XAdd || v.Op == XAnd || v.Op == XOr || v.Op == XXor || v.Op == XMul
		if !assoc || len(args) < 2 {
			p.errAt(v.OpSpan, "E_PARSE_BANG_ARITY", "expected two operands to operator")
			return nil
		}
	}
	v.Args = args
	return v.withType(p.binaryResultType(v.Op, args, item))
}

// checkCompareArg applies TGParser's first-operand check for the six
// comparison operators (TGParser.cpp:1802-1826). It reports at the operand and
// returns false when the type cannot be compared.
func (p *Parser) checkCompareArg(op Kind, arg Value) bool {
	t := arg.Type()
	if t.Kind == InvalidType {
		return true
	}
	switch op {
	case XEq, XNe:
		switch t.Kind {
		case IntType, BitType, BitsType, StringType, RecordType:
			return true
		}
		p.errAt(arg.Span(), "E_PARSE_CMP",
			"expected bit, bits, int, string, or record; got value of type '"+t.String()+"'")
		return false
	case XLe, XLt, XGe, XGt:
		switch t.Kind {
		case IntType, BitType, BitsType, StringType:
			return true
		}
		p.errAt(arg.Span(), "E_PARSE_CMP",
			"expected bit, bits, int, or string; got value of type '"+t.String()+"'")
		return false
	}
	return true
}

// checkInterleaveArg mirrors the INTERLEAVE cases in ParseOperation's
// argument loop: the first argument must be a list of string/int/bits/bit and
// the second a string (TGParser.cpp:1827-1845).
func (p *Parser) checkInterleaveArg(index int, arg Value) bool {
	t := arg.Type()
	if t.Kind == InvalidType {
		return true
	}
	switch index {
	case 0:
		ok := t.Kind == ListType && t.Elem != nil &&
			(t.Elem.Kind == StringType || t.Elem.Kind == IntType ||
				t.Elem.Kind == BitsType || t.Elem.Kind == BitType)
		if !ok {
			p.errAt(arg.Span(), "E_PARSE_INTERLEAVE",
				"expected list of string, int, bits, or bit; got value of type '"+t.String()+"'")
		}
		return ok
	case 1:
		if t.Kind != StringType {
			p.errAt(arg.Span(), "E_PARSE_INTERLEAVE",
				"expected second argument to be a string, got value of type '"+t.String()+"'")
			return false
		}
	}
	return true
}

func (p *Parser) binaryArgType(op Kind, item *Type) *Type {
	switch op {
	case XListConcat, XListRemove:
		return item
	case XStrConcat:
		return &StringTy
	case XMatch:
		return &StringTy
	case XAdd, XSub, XMul, XDiv, XAnd, XOr, XXor, XSra, XSrl, XShl:
		return &IntTy
	case XConcat:
		return &DagTy
	}
	return nil
}

func (p *Parser) binaryResultType(op Kind, args []Value, item *Type) Type {
	switch op {
	case XAdd, XSub, XMul, XDiv, XAnd, XOr, XXor, XSra, XSrl, XShl:
		return IntTy
	case XEq, XNe, XLe, XLt, XGe, XGt, XMatch:
		return BitTy
	case XConcat, XSetDagOp, XSetDagOpName:
		return DagTy
	case XStrConcat, XInterleave:
		return StringTy
	case XGetDagName:
		return StringTy
	case XGetDagArg:
		return InvalidTy
	case XListConcat, XListRemove:
		if len(args) > 0 {
			if args[0].Type().Kind == ListType {
				return args[0].Type()
			}
		}
	case XListSplat:
		if len(args) > 0 {
			return listOf(args[0].Type())
		}
	}
	return InvalidTy
}

func (p *Parser) finishTernary(v *BangValue, rec *recordInfo, item *Type) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after ternary operator")
		return nil
	}
	var args []Value
	for i := 0; i < 3; i++ {
		var hint *Type
		if v.Op == XIf || v.Op == XSubst {
			hint = item
		}
		arg := p.parseValue(rec, hint, parseValueMode)
		if arg == nil {
			return nil
		}
		args = append(args, arg)
		if i < 2 {
			if !p.eat(Comma) {
				p.errTok("E_PARSE_BANG_COMMA", "expected ',' in ternary operator")
				return nil
			}
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in binary operator")
		return nil
	}
	v.Args = args
	switch v.Op {
	case XDag, XSetDagArg, XSetDagName:
		return v.withType(DagTy)
	case XSubst:
		return v.withType(args[2].Type())
	case XIf:
		a, b := args[1].Type(), args[2].Type()
		if a.Kind == UnsetType {
			return v.withType(b)
		}
		if b.Kind == UnsetType {
			return v.withType(a)
		}
		if a.Kind == InvalidType {
			return v.withType(b)
		}
		if b.Kind == InvalidType {
			return v.withType(a)
		}
		// TernOpInit::get resolves the arms with resolveTypes(MHS, RHS); the
		// order matters because the *target* of a conversion wins.
		ty, ok := p.unifyTypes([]Type{a, b}, "for !if")
		if !ok {
			return nil
		}
		return v.withType(ty)
	}
	return v.withType(InvalidTy)
}

func (p *Parser) finishRange(v *BangValue, rec *recordInfo) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !range operator")
		return nil
	}
	// TGParser.cpp:1946-2000 checks the arguments of !range while parsing:
	// the first is a list or an int, and every later one must be an int.
	var args []Value
	firstIsList := false
	for {
		if len(args) >= 3 {
			p.errTok("E_PARSE_RANGE_ARITY", "expected at most three values of integer")
			return nil
		}
		argLoc := p.cur().Span
		arg := p.parseValue(rec, &IntTy, parseValueMode)
		if arg == nil {
			return nil
		}
		at := arg.Type()
		if !firstIsList || len(args) == 0 {
			switch {
			case len(args) == 0 && at.Kind == ListType:
				firstIsList = true
			case at.Kind == IntType:
			default:
				if len(args) != 0 {
					p.errAt(argLoc, "E_PARSE_RANGE_ARG", "expected value of type 'int', got '"+at.String()+"'")
				} else {
					p.errAt(argLoc, "E_PARSE_RANGE_ARG", "expected list or int, got value of type '"+at.String()+"'")
				}
				return nil
			}
		} else {
			p.errAt(argLoc, "E_PARSE_RANGE_LIST", "expected one list, got extra value of type '"+at.String()+"'")
			return nil
		}
		args = append(args, arg)
		if !p.eat(Comma) {
			break
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in operator")
		return nil
	}
	// TGParser fills the missing bounds before building the operator
	// (TGParser.cpp:2008-2044): `!range(x)` is (0, x, 1) — or (0, !size(x), 1)
	// for a list — and `!range(lo, hi)` is (lo, hi, 1).
	at := v.OpSpan
	switch len(args) {
	case 1:
		if args[0].Type().Kind == ListType {
			args = []Value{
				&IntValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Num: 0},
				&BangValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Op: XSize, Args: []Value{args[0]}},
				&IntValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Num: 1},
			}
		} else {
			args = []Value{
				&IntValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Num: 0},
				args[0],
				&IntValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Num: 1},
			}
		}
	case 2:
		args = []Value{
			args[0],
			args[1],
			&IntValue{ValueBase: ValueBase{Base: Base{At: at}, Ty: IntTy}, Num: 1},
		}
	}
	v.Args = args
	return v.withType(listOf(IntTy))
}

func (p *Parser) finishSubstr(v *BangValue, rec *recordInfo) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !substr operator")
		return nil
	}
	lhs := p.parseValue(rec, &StringTy, parseValueMode)
	if lhs == nil {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !substr operator")
		return nil
	}
	startLoc := p.cur().Span
	mhs := p.parseValue(rec, &IntTy, parseValueMode)
	if mhs == nil {
		return nil
	}
	args := []Value{lhs, mhs}
	if p.eat(Comma) {
		rhs := p.parseValue(rec, &IntTy, parseValueMode)
		if rhs == nil {
			return nil
		}
		args = append(args, rhs)
	} else {
		// TGParser supplies INT64_MAX as the default length
		// (TGParser.cpp:2424-2433), so the stored operator always has three
		// arguments.
		args = append(args, &IntValue{
			ValueBase: ValueBase{Base: Base{At: startLoc}, Ty: IntTy},
			Num:       9223372036854775807,
		})
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !substr operator")
		return nil
	}
	// TGParser::ParseOperationSubstr checks the operands after the ')':
	// the string operand reports through TokError (the token after the call),
	// the start position at its own location, and the length through TokError
	// again (TGParser.cpp:2438-2489).
	if lt := lhs.Type(); lt.Kind != InvalidType && lt.Kind != UnsetType && lt.Kind != StringType {
		p.errTok("E_PARSE_SUBSTR", "expected string, got type '"+lt.String()+"'")
		return nil
	}
	if mt := mhs.Type(); mt.Kind != InvalidType && mt.Kind != UnsetType && mt.Kind != IntType {
		p.errAt(startLoc, "E_PARSE_SUBSTR", "expected int, got type '"+mt.String()+"'")
		return nil
	}
	if rt := args[2].Type(); rt.Kind != InvalidType && rt.Kind != UnsetType && rt.Kind != IntType {
		p.errTok("E_PARSE_SUBSTR", "expected int, got type '"+rt.String()+"'")
		return nil
	}
	v.Args = args
	return v.withType(StringTy)
}

func (p *Parser) finishFind(v *BangValue, rec *recordInfo) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !find operator")
		return nil
	}
	lhs := p.parseValue(rec, &StringTy, parseValueMode)
	if lhs == nil {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !find operator")
		return nil
	}
	mhs := p.parseValue(rec, &StringTy, parseValueMode)
	if mhs == nil {
		return nil
	}
	args := []Value{lhs, mhs}
	if p.eat(Comma) {
		rhs := p.parseValue(rec, &IntTy, parseValueMode)
		if rhs == nil {
			return nil
		}
		args = append(args, rhs)
	} else {
		// The Init tree always carries the start position; `!find(a, b)` is
		// the two-argument spelling of `!find(a, b, 0)`.
		args = append(args, &IntValue{ValueBase: ValueBase{Base: Base{At: p.span(p.i - 1)}, Ty: IntTy}, Num: 0})
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !find operator")
		return nil
	}
	v.Args = args
	return v.withType(IntTy)
}

func (p *Parser) finishCond(v *BangValue, rec *recordInfo, item *Type) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !cond operator")
		return nil
	}
	var cases []*BangCase
	for !p.at(RParen) && !p.at(Eof) {
		cs := p.i
		key := p.parseValue(rec, nil, parseValueMode)
		if key == nil {
			return nil
		}
		if !p.eat(Colon) {
			p.errTok("E_PARSE_COND_COLON", "expected ':'  following a condition in !cond operator")
			return nil
		}
		val := p.parseValue(rec, item, parseValueMode)
		if val == nil {
			return nil
		}
		cases = append(cases, &BangCase{Base: Base{At: p.span(cs)}, Key: key, Value: val})
		if p.at(RParen) {
			break
		}
		if !p.eat(Comma) {
			p.errTok("E_PARSE_COND_COMMA", "expected ',' or ')' following a value in !cond operator")
			return nil
		}
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !cond")
		return nil
	}
	if len(cases) < 1 {
		p.errTok("E_PARSE_COND_EMPTY", "there should be at least 1 'condition : value' in the !cond operator")
		return nil
	}
	var tys []Type
	for _, c := range cases {
		if c.Value.Type().Kind != UnsetType {
			tys = append(tys, c.Value.Type())
		}
	}
	ty, ok := p.unifyTypes(tys, "for !cond")
	if !ok {
		return nil
	}
	if ty.Kind == InvalidType {
		p.errTok("E_PARSE_COND_TYPE", "could not determine type for !cond from its arguments")
		return nil
	}
	v.Cases = cases
	return v.withType(ty)
}

// unifyTypes is resolveTypes: record types are unified by hierarchy first,
// then convertibility decides - and the preferred type is the *target* of a
// conversion, so `!cond(1, x{0})` resolves to bits<1>, not int.
func (p *Parser) unifyTypes(tys []Type, ctx string) (Type, bool) {
	var cur Type
	for _, t := range tys {
		if t.Kind == UnsetType {
			continue
		}
		if cur.Kind == InvalidType {
			cur = t
			continue
		}
		if cur.Equal(t) {
			continue
		}
		if cur.Kind == RecordType && t.Kind == RecordType {
			if u, ok := p.unifyRecordTypes(cur, t); ok {
				cur = u
				continue
			}
		}
		// Record.cpp:326-353 resolveTypes: after the record-type case it asks
		// typeIsConvertibleTo in both directions - and that call is hierarchic,
		// so list<Sub> converts to list<Base>. Type.ConvertibleTo alone only
		// compares names and would reject the list case we see in llvm's own
		// headers. The list branch comes last, as in LLVM.
		if p.typeConvertible(cur, t) {
			cur = t
			continue
		}
		if p.typeConvertible(t, cur) {
			continue
		}
		if cur.Kind == ListType && t.Kind == ListType && cur.Elem != nil && t.Elem != nil {
			if el, ok := p.unifyTypes([]Type{*cur.Elem, *t.Elem}, ctx); ok && el.Kind != InvalidType {
				cur = listOf(el)
				continue
			}
		}
		p.errTok("E_PARSE_BANG_TYPES", "inconsistent types '"+cur.String()+"' and '"+t.String()+"' "+ctx)
		return InvalidTy, false
	}
	return cur, true
}

func (p *Parser) finishSwitch(v *BangValue, rec *recordInfo, item *Type) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !switch operator")
		return nil
	}
	key := p.parseValue(rec, nil, parseValueMode)
	if key == nil {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_SWITCH_COMMA", "expected ',' after key in !switch operator")
		return nil
	}
	var cases []*BangCase
	for {
		cs := p.i
		val := p.parseValue(rec, nil, parseValueMode)
		if val == nil {
			return nil
		}
		if p.eat(RParen) {
			cases = append(cases, &BangCase{Base: Base{At: p.span(cs)}, Value: val})
			break
		}
		if !p.eat(Colon) {
			p.errTok("E_PARSE_SWITCH_COLON", "expected ':' after case key, or ')' to close !switch operator")
			return nil
		}
		body := p.parseValue(rec, item, parseValueMode)
		if body == nil {
			return nil
		}
		cases = append(cases, &BangCase{Base: Base{At: p.span(cs)}, Key: val, Value: body})
		if !p.eat(Comma) {
			p.errTok("E_PARSE_SWITCH_COMMA2", "expected ',' after case value in !switch operator")
			return nil
		}
	}
	if len(cases) < 2 {
		// TGParser's TokError fires after the closing ')' was consumed, so the
		// caret sits on the token that follows it (TGParser.cpp:2889-2891).
		p.errTok("E_PARSE_SWITCH_EMPTY",
			"there should be at least 1 'case: value' in the !switch operator")
		return nil
	}
	var tys []Type
	for _, c := range cases {
		if c.Value.Type().Kind != UnsetType {
			tys = append(tys, c.Value.Type())
		}
	}
	// TGParser.cpp:2908 also checks the key against every case key; the
	// diagnostic is emitted at the token after the closing paren.
	kt := key.Type()
	for _, c := range cases {
		if c.Key == nil {
			continue
		}
		ct := c.Key.Type()
		if kt.Kind == InvalidType || ct.Kind == InvalidType || kt.Kind == UnsetType || ct.Kind == UnsetType {
			continue
		}
		if !p.typeConvertible(kt, ct) && !p.typeConvertible(ct, kt) {
			p.errTok("E_PARSE_SWITCH_KEY", "inconsistent types '"+kt.String()+"' and '"+ct.String()+
				"' between !switch key and case keys")
			return nil
		}
	}
	ty, ok := p.unifyTypes(tys, "for !switch values")
	if !ok {
		return nil
	}
	if ty.Kind == InvalidType {
		p.errTok("E_PARSE_SWITCH_TYPE", "could not determine type for !switch from its arguments")
		return nil
	}
	// TGParser.cpp:2900-2923 reduces !switch to !cond while parsing: each
	// case becomes !eq(Key, caseKey) and the mandatory default arm is guarded
	// by the literal 1. The Init tree downstream (and therefore
	// -print-records output) only ever carries the !cond form.
	cond := &BangValue{ValueBase: ValueBase{Base: v.Base}, Op: XCond, OpSpan: v.OpSpan}
	for _, c := range cases {
		eqKey := Value(&IntValue{ValueBase: ValueBase{Base: Base{At: c.Base.At}, Ty: IntTy}, Num: 1})
		if c.Key != nil {
			eq := &BangValue{
				ValueBase: ValueBase{Base: Base{At: c.Key.Span()}, Ty: BitTy},
				Op:        XEq,
				OpSpan:    c.Key.Span(),
				Args:      []Value{key, c.Key},
			}
			eqKey = eq
		}
		cond.Cases = append(cond.Cases, &BangCase{Base: c.Base, Key: eqKey, Value: c.Value})
	}
	cond.Ty = ty
	return cond
}

// listOpName names the list-comprehension operator for diagnostics.
func listOpName(op Kind) string {
	switch op {
	case XForEach:
		return "foreach"
	case XSort:
		return "sort"
	default:
		return "filter"
	}
}

func (p *Parser) finishListComprehension(v *BangValue, rec *recordInfo, item *Type) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !foreach/!filter")
		return nil
	}
	nt, ok := p.expect(Id, "E_PARSE_BANG_ID", "first argument of !foreach/!filter must be an identifier")
	if !ok {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !foreach/!filter")
		return nil
	}
	list := p.parseValue(rec, nil, parseValueMode)
	if list == nil {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !foreach/!filter")
		return nil
	}
	lt := list.Type()
	var inElem Type = InvalidTy
	isDag := false
	switch lt.Kind {
	case ListType:
		if lt.Elem != nil {
			inElem = *lt.Elem
		}
	case DagType:
		// Only !foreach accepts a dag; the others report from the token
		// after the comma, which has already been consumed
		// (TGParser.cpp:2649-2657, :2668-2682).
		if v.Op == XFilter || v.Op == XSort {
			p.errTok("E_PARSE_"+listOpName(v.Op)+"_ARG", "!"+listOpName(v.Op)+" must have a list argument")
			return nil
		}
		isDag = true
		inElem = DagTy
	case InvalidType, UnsetType:
	default:
		switch v.Op {
		case XForEach:
			p.errTok("E_PARSE_FOREACH_ARG", "!foreach must have a list or dag argument")
		default:
			p.errTok("E_PARSE_"+listOpName(v.Op)+"_ARG", "!"+listOpName(v.Op)+" must have a list argument")
		}
		return nil
	}
	p.pushScope(nil)
	p.defineLocal(nt.Text, binding{ty: inElem, lexical: true})
	var exprHint *Type
	if v.Op == XFilter {
		exprHint = &BitTy
	} else if v.Op == XForEach && item != nil && item.Kind == ListType {
		exprHint = item.Elem
	}
	expr := p.parseValue(rec, exprHint, parseValueMode)
	p.popScope()
	if expr == nil {
		return nil
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in !foreach/!filter")
		return nil
	}
	v.Args = []Value{list, expr}
	v.VarNames = []string{nt.Text}
	switch v.Op {
	case XForEach:
		if isDag {
			return v.withType(DagTy)
		}
		return v.withType(listOf(expr.Type()))
	case XFilter:
		return v.withType(listOf(inElem))
	case XSort:
		return v.withType(listOf(inElem))
	}
	return v.withType(InvalidTy)
}

func (p *Parser) finishFoldl(v *BangValue, rec *recordInfo) Value {
	if !p.eat(LParen) {
		p.errTok("E_PARSE_BANG_LPAREN", "expected '(' after !foldl")
		return nil
	}
	start := p.parseValue(rec, nil, parseValueMode)
	if start == nil {
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !foldl")
		return nil
	}
	list := p.parseValue(rec, nil, parseValueMode)
	if list == nil {
		return nil
	}
	if list.Type().Kind != InvalidType && list.Type().Kind != ListType {
		p.errTok("E_PARSE_FOLDL_LIST", "!foldl list must be a list, but is of type '"+list.Type().String()+"'")
		return nil
	}
	if !p.eat(Comma) {
		p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !foldl")
		return nil
	}
	var names []string
	for i := 0; i < 2; i++ {
		nt, ok := p.expect(Id, "E_PARSE_FOLDL_ID", "third argument of !foldl must be an identifier")
		if !ok {
			return nil
		}
		names = append(names, nt.Text)
		if !p.eat(Comma) {
			p.errTok("E_PARSE_BANG_COMMA", "expected ',' in !foldl")
			return nil
		}
	}
	p.pushScope(nil)
	p.defineLocal(names[0], binding{ty: start.Type(), lexical: true})
	p.defineLocal(names[1], binding{ty: elemOf(list.Type()), lexical: true})
	exprTy := start.Type()
	expr := p.parseValue(rec, &exprTy, parseValueMode)
	p.popScope()
	if expr == nil {
		return nil
	}
	if !p.eat(RParen) {
		p.errTok("E_PARSE_BANG_RPAREN", "expected ')' in fold operator")
		return nil
	}
	v.Args = []Value{start, list, expr}
	v.VarNames = names
	return v.withType(start.Type())
}

// ---------------------------------------------------------------------------
// Debug helpers

func valueString(v Value) string {
	switch t := v.(type) {
	case *UnsetValue:
		return "?"
	case *IntValue:
		return itoa(int(int64(t.Num)))
	case *StringValue:
		return t.Text
	case *VarValue:
		return t.Name
	case *FieldValue:
		return valueString(t.Base) + "." + t.Field
	case *ListValue:
		return "[...]"
	case *BitsValue:
		return "{...}"
	case *DagValue:
		return "(dag)"
	}
	return "<value>"
}

var _ = diag.Pos{}
