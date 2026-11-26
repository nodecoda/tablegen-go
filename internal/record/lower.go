package record

import (
	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// Lower turns a parsed file into a record graph. It is the semantic half of
// TableGen that the parser deliberately leaves out: inheritance, template
// argument substitution, let stacking, foreach/if expansion, and defm.
//
// Errors are collected, never thrown; a broken object is skipped so later
// objects still produce diagnostics.
func Lower(file *syntax.File, filename string) (*Keeper, []diag.Diagnostic) {
	return lowerWith(file, filename, false)
}

// LowerNames is Lower without field resolution: it still instantiates every
// def and loop so the keeper knows the record names and parents, but skips
// record field evaluation, concreteness checks, and assertions. The driver
// uses it only to learn the names a statement-level loop creates, then parses
// the file again with those names installed; nothing it computes survives.
func LowerNames(file *syntax.File, filename string) (*Keeper, []diag.Diagnostic) {
	return lowerWith(file, filename, true)
}

func lowerWith(file *syntax.File, filename string, namesOnly bool) (*Keeper, []diag.Diagnostic) {
	col := &diag.Collector{}
	l := &Lowerer{
		k:            NewKeeper(filename, col),
		diags:        col,
		file:         filename,
		mcs:          map[string]*syntax.MultiClassStmt{},
		mcLets:       map[string][]*syntax.LetBind{},
		anonName:     map[*syntax.DefStmt]string{},
		anonDefmName: map[*syntax.DefmStmt]string{},
		loopSeq:      -1,
		defmSeq:      -1,
		namesOnly:    namesOnly,
	}
	l.objects(file.Objects, nil, nil)
	return l.k, col.Diags
}

type Lowerer struct {
	// stop halts lowering after an error that LLVM's parser would have
	// propagated out of ParseObjectList (see EvalError.Abort).
	stop  bool
	k     *Keeper
	diags *diag.Collector
	file  string
	mcs   map[string]*syntax.MultiClassStmt
	// mcLets keeps the `let` bindings a multiclass definition sits inside.
	// TGParser applies the active LetStack to every def prototype while
	// parsing the multiclass body (ParseObjectBody calls ApplyLetStack before
	// ParseBody), so each generated def starts with those values already set.
	mcLets       map[string][]*syntax.LetBind
	anonName     map[*syntax.DefStmt]string
	anonDefmName map[*syntax.DefmStmt]string
	loopSeq      int // Seq of the statement-level loop being expanded, or -1
	// defmSeq is the Seq of the statement-level defm whose expansion is in
	// progress, or -1. Every record finished under it is published so the
	// driver can re-parse with those names installed.
	defmSeq int
	// namesOnly skips the parts of addDefOne that only matter for the record
	// graph (field resolution, concreteness, assertions); see LowerNames.
	namesOnly bool
	// skipLoopFold makes preFold ignore values that mention a foreach
	// iterator. TGParser parses an `if` body with the iterator still a
	// VarInit, so such values are not concrete at parse time and fold only
	// when the surviving branch executes.
	skipLoopFold bool
	// preResolve is set while modelling the resolve() pass that runs before
	// addDefOne for a loop entry. A class application instantiated there is
	// not folded into the value: the class body it came from still prints as
	// `Class<args>`, so tryInstantiate must not record FoldName.
	preResolve bool
	// pending, when non-nil, is the entry list of the defm currently being
	// expanded. TGParser resolves a defm's records into NewEntries and only
	// afterwards runs addDefOne on each, so buildDef queues its record here
	// instead of finalising it. nil outside a defm expansion (plain defs are
	// added immediately).
	pending *[]*deferredDef
	// scope is the number of enclosing braced blocks (let/foreach/if/defset).
	// A defvar only reaches the record-layer global table at top level; in a
	// block, TGParser's TGVarScope inlines it into later values and drops it
	// when the block ends (defvar.td). Defining it globally would leak.
	scope int
	// eager is set while a defm's multiclass body is expanded. LLVM applies
	// the multiclass's arguments while parsing the defm, so values that refer
	// to them are already concrete there; a def/class body parsed at its
	// definition site instead keeps them as VarInit.
	eager bool
	// preDepth guards preExpandMC against a multiclass that reaches itself.
	preDepth int
	// loopVars is the stack of iterator names of the enclosing foreach loops.
	// TGParser parses a loop body once with these as VarInit and substitutes
	// them only into record entries; a defvar value is never re-resolved, so a
	// class application that mentions one must stay a VarDefInit forever.
	loopVars []string
}

func (l *Lowerer) errorAt(err error, fallback diag.Span) {
	var span diag.Span
	switch e := err.(type) {
	case *UnsupportedError:
		if e.Span.Start.File != "" {
			span = e.Span
		} else {
			span = fallback
		}
		l.diags.Error("E_UNSUPPORTED", e.Feature, span)
		return
	case *EvalError:
		if e.Abort {
			l.stop = true
		}
		if e.NoLoc {
			// PrintFatalError(Twine): no location, no source line, no caret.
			l.diags.Error("E_SEMANTIC", e.Msg, diag.Span{})
			return
		}
		span = e.Span
		if span.Start.File == "" {
			span = fallback
		}
		if e.Note.File != "" {
			l.diags.Diags = append(l.diags.Diags, diag.Diagnostic{
				Severity: diag.Error, Code: "E_SEMANTIC", Message: e.Msg, Span: span,
				Notes: append([]diag.Note{{Message: "location of previous definition", Pos: e.Note}}, e.Notes...),
			})
			return
		}
		if len(e.Notes) > 0 {
			l.diags.Diags = append(l.diags.Diags, diag.Diagnostic{
				Severity: diag.Error, Code: "E_SEMANTIC", Message: e.Msg, Span: span, Notes: e.Notes,
			})
			return
		}
		l.diags.Error("E_SEMANTIC", e.Msg, span)
	default:
		l.diags.Error("E_SEMANTIC", err.Error(), fallback)
	}
}

// objects processes a statement list. lets are the top-level `let ... in`
// bindings currently in scope; they are applied when a def is built.
func (l *Lowerer) objects(objs []syntax.Object, env *Env, lets []*syntax.LetBind) {
	for _, o := range objs {
		if o == nil || l.stop {
			continue
		}
		l.object(o, env, lets)
	}
}

func (l *Lowerer) object(o syntax.Object, env *Env, lets []*syntax.LetBind) {
	switch t := o.(type) {
	case *syntax.ClassStmt:
		if err := l.class(t, lets); err != nil {
			l.errorAt(err, t.Span())
		}
	case *syntax.DefStmt:
		if _, err := l.buildDef(t, env, scopedLets(lets, env), nil, nil, nil, ""); err != nil {
			l.errorAt(err, t.Span())
		}
	case *syntax.DefmStmt:
		if err := l.defm(t, env, lets); err != nil {
			l.errorAt(err, t.Span())
		}
	case *syntax.MultiClassStmt:
		l.mcs[t.Name] = t
		l.mcLets[t.Name] = lets
		// A multiclass definition resolves its parents before its body is
		// parsed (AddParentMultiClass -> resolve, TGParser.cpp:388-404). That
		// substitution unrolls every foreach whose list becomes concrete, so a
		// class application inside the inherited entries - SchedSEWSet<mx,
		// isF, isWidening> after mx comes from a concrete MxList - is
		// instantiated, and takes its anonymous number, when the inheriting
		// multiclass is defined rather than when a defm expands it
		// (VarDefInit::get, Record.cpp:2374).
		for _, p := range t.Parents {
			if pm := l.mcs[p.Name]; pm != nil {
				l.preExpandMC(pm, p.Args)
			}
		}
		// TGParser parses a multiclass body statement by statement: a `def`
		// takes its anonymous name when ParseDef runs, and a foreach whose
		// list is already concrete is unrolled the moment its closing brace is
		// reached (addEntry -> resolve, TGParser.cpp:426-430), resolving the
		// copied records with the loop substitutions and instantiating the
		// class applications they contain. Doing both in source order keeps
		// those instantiations ahead of the names the later statements take.
		for _, o := range t.Body {
			l.reserveAnonNames([]syntax.Object{o})
			l.preExpandBody([]syntax.Object{o}, nil)
		}
	case *syntax.DefvarStmt:
		if l.scope == 0 {
			l.k.defineGlobal(t.Name, &AstExpr{V: t.Value, Env: env})
		}
		l.preFoldDefvar(t.Value, env)
		// TGParser folds operators while parsing the value
		// (ParseOperation -> Fold), so a bad operator is diagnosed even when
		// the defvar is never read. We keep values lazy for record building,
		// so re-run the evaluator here purely to surface those diagnostics.
		//
		// A value that mentions a foreach iterator must not be evaluated
		// here: TGParser keeps it unresolved until the loop body executes,
		// and folding it now would instantiate anonymous class applications
		// (and report errors) for iterations whose use site never runs.
		if b, isBang := t.Value.(*syntax.BangValue); isBang && b.Op != syntax.XCast && !l.usesLoopVar(t.Value) {
			if _, err := l.k.evalSyntax(t.Value, env, nil); err != nil {
				l.errorAt(err, t.Span())
			}
		}
	case *syntax.DeftypeStmt:
		// Type aliases are resolved while parsing; they emit no records.
	case *syntax.DefsetStmt:
		if err := l.defset(t, env, lets); err != nil {
			l.errorAt(err, t.Span())
		}
	case *syntax.ForeachStmt:
		l.foreach(t, env, lets)
	case *syntax.IfStmt:
		l.ifStmt(t, env, lets)
	case *syntax.LetStmt:
		nl := append(append([]*syntax.LetBind(nil), lets...), t.Binds...)
		l.scope++
		l.objects(t.Body, env, nl)
		l.scope--
	case *syntax.AssertStmt:
		l.assert(t, env, nil)
	case *syntax.DumpStmt:
		// With no current record ParseDump resolves the message against an
		// empty final resolver and prints it right away (TGParser.cpp:4838-4843).
		l.emitDump(t.Span().Start, t.Value, env, nil)
	case *syntax.IncludeStmt:
		l.errorAt(unsupported("include (resolved in M5)", t.Span()), t.Span())
	}
}

// defset parses a block of objects and publishes every def it created (in
// creation order, nested defsets included) as a list-typed global. The elements
// must be instances of the declared element type.
func (l *Lowerer) defset(st *syntax.DefsetStmt, env *Env, lets []*syntax.LetBind) error {
	elem := l.k.tyFromSyntax(st.ElemType)
	before := len(l.k.defOrder)
	errs := len(l.diags.Diags)
	l.scope++
	l.objects(st.Objects, env, lets)
	l.scope--
	if len(l.diags.Diags) != errs {
		// A broken element already reported; do not publish a partial set.
		return nil
	}
	out := &List{Elem: elem}
	for _, d := range l.k.defOrder[before:] {
		if d.VarDefInit {
			// VarDefInit records (anonymous Class<...> instances) are not
			// defs and are never members of a defset.
			continue
		}
		dt := d.Type()
		if !dt.IsA(elem) {
			// addDefOne prints the note at the defset's element type
			// ("defset list<B> Bs" -> the `list<B>` token, TGParser.cpp:4018
			// sets Defset.Loc right after the `defset` keyword).
			note := diag.Note{Message: "location of defset declaration", Pos: st.TypeSpan.Start}
			return &EvalError{
				Msg:   "adding record of incompatible type '" + dt.String() + "' to defset",
				Span:  diag.Span{Start: d.Locs[0], End: d.Locs[0]},
				Notes: []diag.Note{note},
			}
		}
		out.Elems = append(out.Elems, &DefVal{Rec: d})
	}
	l.k.globals[st.Name] = &Global{Name: st.Name, state: resolved, val: out}
	return nil
}

func (l *Lowerer) foreach(st *syntax.ForeachStmt, env *Env, lets []*syntax.LetBind) {
	var elems []Value
	if st.Range != nil {
		for _, n := range st.Range {
			elems = append(elems, &Int{N: int64(n)})
		}
	} else {
		l.preFold(st.List, env)
		v, err := l.k.evalSyntax(st.List, env, nil)
		if err != nil {
			l.errorAt(err, st.Span())
			return
		}
		ls, ok := v.(*List)
		if !ok {
			l.errorAt(&EvalError{Msg: "attempting to loop over a non-list", Span: st.Span()}, st.Span())
			return
		}
		elems = ls.Elems
	}
	prev := l.loopSeq
	if st.Seq >= 0 {
		l.loopSeq = st.Seq
	}
	l.scope++
	// TGParser parses the loop body exactly once, before any iteration is
	// resolved: an anonymous `def` takes its name when ParseDef runs, and every
	// concrete class application in a parent argument or a field initializer
	// instantiates there, in source order. Only then does resolve() add each
	// iteration's records (each copy keeps the parse-time name, and addDefOne
	// renames the duplicates). Lowering element by element would interleave
	// those two phases and shift the anonymous numbering, so the body is
	// parsed here first.
	for _, o := range st.Body {
		l.reserveAnonNames([]syntax.Object{o})
	}
	l.preFoldParseItems(st.Body)
	l.loopVars = append(l.loopVars, st.Var)
	for _, e := range elems {
		sub := env.WithBinding(st.Var, Binding{Val: e})
		l.objects(st.Body, sub, lets)
	}
	l.loopVars = l.loopVars[:len(l.loopVars)-1]
	l.scope--
	l.loopSeq = prev
}

func (l *Lowerer) ifStmt(st *syntax.IfStmt, env *Env, lets []*syntax.LetBind) {
	l.preFold(st.Cond, env)
	v, err := l.k.evalSyntax(st.Cond, env, nil)
	if err != nil {
		l.errorAt(err, st.Span())
		return
	}
	n, err := l.k.asInt(v)
	if err != nil {
		l.errorAt(err, st.Span())
		return
	}
	l.scope++
	// TGParser turns each arm into its own foreach over a 0-or-1-element list
	// and parses *both* bodies before either list is resolved. Their literal
	// class applications therefore instantiate in source order even though only
	// one arm survives. Values that mention a foreach iterator are not
	// concrete at parse time (resolve() runs the surviving arm later), so
	// folding them here would create anonymous records LLVM never makes.
	prevSkip := l.skipLoopFold
	l.skipLoopFold = true
	for _, o := range st.Then {
		l.reserveAnonNames([]syntax.Object{o})
	}
	for _, o := range st.Else {
		l.reserveAnonNames([]syntax.Object{o})
	}
	l.preFoldParseItems(st.Then)
	l.preFoldParseItems(st.Else)
	l.skipLoopFold = prevSkip
	if n != 0 {
		l.objects(st.Then, env, lets)
	} else {
		l.objects(st.Else, env, lets)
	}
	l.scope--
}

func (l *Lowerer) assert(st *syntax.AssertStmt, env *Env, rec *Record) {
	cond, err := l.k.evalSyntax(st.Cond, env, rec)
	if err != nil {
		l.errorAt(err, st.Span())
		return
	}
	n, err := l.k.asInt(cond)
	if err != nil {
		l.errorAt(err, st.Span())
		return
	}
	if n != 0 {
		return
	}
	msg := "(assert message is not a string)"
	if mv, err := l.k.evalSyntax(st.Message, env, rec); err == nil {
		if s, ok := mv.(*Str); ok {
			msg = s.S
		}
	}
	// CheckAssert is called with the condition's location, so the caret lands
	// on the expression rather than on the `assert` keyword
	// (TGParser::ParseAssert passes Assert->Loc, which ParseValue set to the
	// start of the condition).
	l.diags.Error("E_ASSERT", "assertion failed: "+msg, st.Cond.Span())
}

// class builds a class record. Classes keep unresolved field expressions; a
// class body is instantiated every time a def inherits it with different
// template arguments.
func (l *Lowerer) class(st *syntax.ClassStmt, lets []*syntax.LetBind) error {
	rec := l.k.Class(st.Name)
	if rec != nil {
		if len(rec.Vals) > 0 || len(rec.TArgs) > 0 || len(rec.Super) > 0 {
			if !st.DeclOnly {
				return &EvalError{Msg: "Class '" + st.Name + "' already defined", Span: st.Span()}
			}
			return nil
		}
		rec.Locs = []diag.Pos{st.Span().Start}
	} else {
		rec = &Record{Name: st.Name, IsClass: true, Locs: []diag.Pos{st.Span().Start}, keeper: l.k}
		l.k.classes[st.Name] = rec
	}
	env := &Env{}
	frame := map[string]Binding{}
	for _, d := range st.TemplateArgs {
		rv := &RecordVal{Name: d.Name, Qual: st.Name + ":" + d.Name, Ty: l.k.tyFromSyntax(d.Type), Loc: d.Span().Start}
		if d.Value != nil {
			rv.Expr = &AstExpr{V: d.Value, Env: env}
			l.preFold(d.Value, env)
		}
		rec.TArgs = append(rec.TArgs, rv)
	}
	// Fill the frame before With: a Frame is immutable once installed, and
	// Env copies share it.
	for i, d := range st.TemplateArgs {
		if d.Value != nil {
			frame[st.Name+":"+d.Name] = Binding{
				V: d.Value, Dynamic: true, Ty: rec.TArgs[i].Ty,
				Owner: rec, Sep: ":", Self: st.Name + ":" + d.Name,
			}
		}
	}
	env = env.With(frame)
	env.owner = rec
	env.ownerSep = ":"
	rec.Env = env
	// Rebind defaults now that the frame exists.
	for _, rv := range rec.TArgs {
		if rv.Expr != nil {
			rv.Expr = &AstExpr{V: rv.Expr.(*AstExpr).V, Env: env}
		}
	}

	for _, p := range st.Parents {
		if err := l.applyParent(rec, p, env); err != nil {
			return err
		}
	}
	// ParseObjectBody runs ApplyLetStack between the parent list and the body,
	// so `let TargetPrefix = "aarch64" in { class C : Intrinsic<...> }` stores
	// the let on the class and every def that inherits it sees the value.
	for _, b := range lets {
		if err := l.applyLet(rec, b, env); err != nil {
			return err
		}
	}
	if st.DeclOnly {
		return nil
	}
	for _, item := range st.Body {
		// llvm-tblgen keeps parsing the rest of a body after a bad member and
		// reports every offending field (BitsInitErrors.td has eight in one
		// def), so an error here must not end the body.
		if err := l.bodyItem(rec, item, env); err != nil {
			l.errorAt(err, item.Span())
		}
	}
	return nil
}

func (l *Lowerer) applyParent(target *Record, p *syntax.ParentRef, env *Env) error {
	// Argument values are folded where they were parsed, never with the
	// record's frame: `Whatev<Or4<BaseOpc>.V>` stays a VarDefInit until a
	// field actually reads it (pr8330.td: the unused argument must not
	// instantiate Or4).
	cl := l.k.Class(p.Name)
	if cl == nil {
		return &EvalError{Msg: "Couldn't find class '" + p.Name + "'", Span: p.Span()}
	}
	if l.namesOnly {
		// LowerNames only needs the direct parent list: skip folding the
		// arguments and instantiating the class body.
		target.Super = append(target.Super, cl)
		return nil
	}
	for _, a := range p.Args {
		// The arguments are folded without the record's frame, but the parser
		// did parse them in its scope, which qualifies template-argument
		// names for VarDefInit pooling.
		l.k.noteShapeSites(a.Value, env)
		l.preFold(a.Value, nil)
	}
	return l.k.applyClass(target, cl, p.Args, env, target, p.Span().Start)
}

func (l *Lowerer) bodyItem(rec *Record, item syntax.BodyItem, env *Env) error {
	switch t := item.(type) {
	case *syntax.Decl:
		// A redeclaration sets the value of the existing field; the type and
		// the field-ness come from the first declaration (AddValue keeps the
		// existing RecordVal and only calls SetValue on it).
		ty := l.k.tyFromSyntax(t.Type)
		nonconcrete := t.IsField
		if old := rec.Val(t.Name); old != nil {
			ty = old.Ty
			nonconcrete = old.Nonconcrete
		}
		if t.Value != nil {
			if err := l.k.checkFieldValue(ty, t.Value, rec, env, t.Name); err != nil {
				// TGParser::AddValue resets a redeclared field to `?` before
				// the initializer is set, and RecordVal::setValue leaves it
				// there when the value does not fit (TGParser.cpp:218-231,
				// Record.cpp:2766-2792). The error is reported and parsing
				// continues, so later expressions see the unset value.
				rec.addVal(&RecordVal{Name: t.Name, Ty: ty, Nonconcrete: nonconcrete, Loc: t.Span().Start})
				l.errorAt(err, t.Span())
				return nil
			}
			l.preFold(t.Value, env)
		}
		rv := &RecordVal{Name: t.Name, Ty: ty, Nonconcrete: nonconcrete, Loc: t.Span().Start}
		if t.Value != nil {
			rv.Expr = &AstExpr{V: l.k.implicitCast(ty, t.Value, rec), Env: env}
		}
		rec.addVal(rv)
		return nil
	case *syntax.LetItem:
		l.preFold(t.Bind.Value, env)
		return l.applyLet(rec, t.Bind, env)
	case *syntax.DefvarStmt:
		// Parsed expressions already inline local defvar values; nothing to do.
		return nil
	case *syntax.AssertStmt:
		l.preFold(t.Cond, env)
		l.preFold(t.Message, env)
		// TGParser::ParseAssert hands a body assertion to the current record
		// (CurRec->addAssertion, TGParser.cpp:4302) and Record::
		// checkRecordAssertions evaluates it in addDefOne - for a class only
		// when a def inherits it, with the class arguments bound.
		rec.Asserts = append(rec.Asserts, Assertion{Cond: t.Cond, Message: t.Message, Env: env})
		return nil
	case *syntax.DumpStmt:
		// ParseDump with a current record stores the message on it
		// (CurRec->addDump, TGParser.cpp:4835); addDefOne emits it once the
		// def is complete.
		rec.Dumps = append(rec.Dumps, Dump{Loc: t.Span().Start, Msg: t.Value, Env: env})
		return nil
	default:
		return unsupported("body item", item.Span())
	}
}

func (l *Lowerer) applyLet(rec *Record, b *syntax.LetBind, env *Env) error {
	rv := rec.Val(b.Field)
	if rv == nil {
		return &EvalError{Msg: "Value '" + b.Field + "' unknown!", Span: b.FieldSpan}
	}
	// SetValue(..., OverrideDefLoc=true) stamps the let's identifier location
	// on the field (TGParser.cpp:326, 3851): the detailed records report then
	// points at the `let` line, not at the declaration.
	if b.FieldSpan.Start.File != "" {
		rv.Loc = b.FieldSpan.Start
	}
	// `let X = X` is a self-assignment only when X is the field. A template
	// argument or loop variable of the same name is a different binding
	// (multiclass CompareAndSwap does `let Acq = Acq`).
	if vv, ok := b.Value.(*syntax.VarValue); ok && vv.Name == b.Field &&
		!vv.ArgRef && !vv.Lexical && !vv.GlobalRef {
		return &EvalError{Msg: "Recursion / self-assignment forbidden", Span: b.Value.Span()}
	}
	add := &AstExpr{V: b.Value, Env: env}
	if len(b.Bits) > 0 {
		if b.Mode == syntax.LetAppend || b.Mode == syntax.LetPrepend {
			return &EvalError{Msg: "Cannot use append/prepend with bit range", Span: b.FieldSpan}
		}
		if rv.Ty == nil || rv.Ty.Kind != BitsTy {
			return &EvalError{Msg: "Value '" + b.Field + "' is not a bits type", Span: b.FieldSpan}
		}
		bits := make([]int32, len(b.Bits))
		for i := range b.Bits {
			bits[i] = b.Bits[len(b.Bits)-1-i]
		}
		rv.Expr = &BitsExpr{Base: rv.Expr, Add: add, Bits: bits, Width: rv.Ty.Bits}
		rv.state = unresolved
		return nil
	}
	switch b.Mode {
	case syntax.LetAppend, syntax.LetPrepend:
		kind := StringTy
		if rv.Ty != nil {
			switch rv.Ty.Kind {
			case ListTy:
				kind = ListTy
			case DagTy:
				kind = DagTy
			case CodeTy:
				kind = CodeTy
			case StringTy:
				kind = StringTy
			default:
				verb := "append to"
				if b.Mode == syntax.LetPrepend {
					verb = "prepend to"
				}
				return &EvalError{Msg: "Cannot " + verb + " field '" + b.Field + "' of type '" + rv.Ty.String() +
					"' (expected list, string, code, or dag)", Span: b.FieldSpan}
			}
		}
		rv.Expr = &AppendExpr{Base: rv.Expr, Add: add, Prepend: b.Mode == syntax.LetPrepend, Kind: kind}
	default:
		if b.Value != nil {
			if err := l.k.checkFieldValue(rv.Ty, b.Value, rec, env, rv.Name); err != nil {
				return err
			}
			add = &AstExpr{V: l.k.implicitCast(rv.Ty, b.Value, rec), Env: env}
		}
		rv.Expr = add
	}
	rv.state = unresolved
	return nil
}

func scopedLets(bs []*syntax.LetBind, env *Env) []scopedLet {
	out := make([]scopedLet, len(bs))
	for i, b := range bs {
		out[i] = scopedLet{b: b, env: env}
	}
	return out
}

// scopedLet is a let binding together with the environment it was written in.
// A let inside a multiclass body is substituted with that body's template
// arguments, even when it is applied after the def has been built.
type scopedLet struct {
	b   *syntax.LetBind
	env *Env
}

// deferredDef is a defm entry that has been through TGParser's resolve phase
// but not yet through addDefOne. See expandDefm.
type deferredDef struct {
	rec       *Record
	anonymous bool
	env       *Env
	loopSeq   int
}

// buildDef builds one def. Order is the part that TableGen users trip over:
// parents, then top-level lets, then the body; for defm-generated records the
// trailing classes and the defm's own let stack are applied after the body,
// and every entry of the defm is finalised only once all of them are built.
func (l *Lowerer) buildDef(st *syntax.DefStmt, env *Env, preLets, postLets []scopedLet, extraParents []*syntax.ParentRef, extraLocs []diag.Pos, fixedName string) (*Record, error) {
	rec, anonymous, err := l.makeDef(st, env, preLets, extraLocs, fixedName)
	if err != nil {
		return nil, err
	}
	if l.pending != nil && l.loopSeq < 0 {
		// TGParser::ParseDefm resolves every entry of the multiclass body into
		// NewEntries first and only then calls addDefOne for each. Both phases
		// instantiate anonymous class applications - parents while resolving,
		// field initialisers such as `TypeInfo` while finalising - so finishing
		// here would interleave the two and shift anonymous_N. Inside a
		// statement-level foreach the defm's records become entries of that
		// loop (addEntry pushes to Loops.back()->Entries), and the loop then
		// resolves and adds each one before touching the next, so there the
		// per-def order is already correct.
		*l.pending = append(*l.pending, &deferredDef{
			rec: rec, anonymous: anonymous, env: env, loopSeq: l.loopSeq,
		})
		return rec, nil
	}
	if err := l.applyParents(rec, extraParents, env); err != nil {
		return nil, err
	}
	if !l.namesOnly {
		for _, lb := range postLets {
			if err := l.applyLet(rec, lb.b, lb.env); err != nil {
				return nil, err
			}
		}
		if l.loopSeq >= 0 {
			// A record built under a statement-level foreach is finished the
			// way addEntry finishes a loop: resolve() clones the entry and
			// runs Record::resolveReferences with the iterator substitution
			// (TGParser.cpp:525-552), and only then calls addDefOne.
			// VarDefInit::resolveReferences instantiates every anonymous
			// class application whose arguments just became concrete
			// (Record.cpp:2423-2452), so those records take their
			// anonymous_N before addDefOne can rename a colliding def
			// (TGParser.cpp:585-591). The folded values are deliberately
			// dropped: addDefOne resolves the fields again with the final
			// record name, which is what a self-reference has to see.
			l.preResolveFields(rec)
		}
	}
	if err := l.finishDef(rec, anonymous); err != nil {
		return nil, err
	}
	return rec, nil
}

// applyParents is the AddParentClass loop: TGParser applies a defm's trailing
// regular classes to every generated entry before any addDefOne runs.
func (l *Lowerer) applyParents(rec *Record, parents []*syntax.ParentRef, env *Env) error {
	for _, p := range parents {
		if err := l.applyParent(rec, p, env); err != nil {
			return err
		}
	}
	return nil
}

// finishDef is addDefOne: rename a colliding anonymous record, resolve the
// remaining field references (RecordResolver), validate, and register.
func (l *Lowerer) finishDef(rec *Record, anonymous bool) error {
	if anonymous {
		if _, exists := l.k.defs[rec.Name]; exists {
			// addDefOne renames before resolving so that NAME/!cast(NAME)
			// already see the final name.
			rec.Name = l.k.newAnonymous(rec.Locs[0]).Name
		}
	}
	if !l.namesOnly {
		if err := rec.resolveAll(); err != nil {
			return withDefmLocs(rec, err)
		}
	}
	if l.namesOnly {
		// Skip checkConcrete and the assertions: the caller only needs the
		// record's name and parents.
		if err := l.k.addDef(rec); err != nil {
			return err
		}
		return l.recordLoopDef(rec)
	}
	// addDefOne runs checkConcrete between resolveReferences and the
	// assertions: a value that is still a reference is only legal behind
	// `field`. Anonymous class applications skip this (VarDefInit::instantiate
	// does not call checkConcrete), so it lives here, not in instantiate.
	if err := l.k.checkConcrete(rec); err != nil {
		return withDefmLocs(rec, err)
	}
	// Record::checkRecordAssertions runs in addDefOne, after every field and
	// let of the def has been applied.
	l.k.checkAssertions(rec)
	// Record::emitRecordDumps runs next, before the record joins the keeper
	// (TGParser.cpp:603-606).
	l.emitRecordDumps(rec)
	if err := l.k.addDef(rec); err != nil {
		return err
	}
	return l.recordLoopDef(rec)
}

// withDefmLocs appends the "instantiated from multiclass" notes LLVM prints
// for every location a defm generated record carries beyond its own
// (Record::getLoc returns the whole chain; Error.cpp:34-41 prints the first
// and a note for each of the rest).
func withDefmLocs(rec *Record, err error) error {
	if err == nil || len(rec.Locs) < 2 {
		return err
	}
	ee, ok := err.(*EvalError)
	if !ok || ee.Fatal {
		return err
	}
	cp := *ee
	for _, p := range rec.Locs[1:] {
		cp.Notes = append(cp.Notes, diag.Note{Message: "instantiated from multiclass", Pos: p})
	}
	return &cp
}

// recordLoopDef publishes a def created by a statement-level loop so the
// driver can re-parse with its name known.
func (l *Lowerer) recordLoopDef(rec *Record) error {
	seqs := []struct {
		seq   int
		table map[int][]LoopDef
	}{
		{l.loopSeq, l.k.loopDefs},
		{l.defmSeq, l.k.defmDefs},
	}
	for _, s := range seqs {
		if s.seq < 0 {
			continue
		}
		kd := LoopDef{Name: rec.Name}
		for _, sc := range rec.Super {
			kd.Parents = append(kd.Parents, sc.Name)
		}
		s.table[s.seq] = append(s.table[s.seq], kd)
	}
	return nil
}

// makeDef parses one def up to (but excluding) the trailing classes, the defm
// let stack, and addDefOne.
func (l *Lowerer) makeDef(st *syntax.DefStmt, env *Env, preLets []scopedLet, extraLocs []diag.Pos, fixedName string) (*Record, bool, error) {
	loc := st.Span().Start
	name := fixedName
	anonymous := false
	if name == "" {
		if st.Name == nil || isUnset(st.Name) {
			anonymous = true
			// A `def` inside a loop body was parsed once, so every iteration
			// reuses the name ParseDef gave that statement; addDefOne renames
			// the copies after the first (finishDef below).
			name = l.anonName[st]
		} else {
			// TGParser registers a VarDefInit for every class application
			// while parsing, including the ones inside a def's name
			// (`def !foldl(..., Idx<N>.slice, ...) : C`). The node keeps the
			// parse site's location, so the anonymous instance it later
			// creates reports the name's line even when the first *use* is
			// further down the body. Folding the name here mirrors
			// ParseObjectName -> ParseValue -> VarDefInit::Fold.
			l.preFold(st.Name, env)
			n, err := l.evalNameAt(st.Name, env, diag.Span{Start: loc, End: loc})
			if err != nil {
				return nil, false, err
			}
			name = n
		}
	} else if st.Name == nil || isUnset(st.Name) {
		anonymous = true
	}
	var rec *Record
	switch {
	case name == "" && anonymous:
		rec = l.k.newAnonymous(loc)
	case anonymous:
		rec = &Record{Name: name, IsAnonymous: true, Locs: []diag.Pos{loc}, keeper: l.k}
	default:
		rec = &Record{Name: name, Locs: []diag.Pos{loc}, keeper: l.k}
	}
	for _, x := range extraLocs {
		rec.Locs = append(rec.Locs, x)
	}
	// TGParser applies a def's parent classes while it parses the object
	// body, so AddParentClass sees NAME as the name Init written in the
	// source (TGParser.cpp:356-362). Inside a loop or a multiclass body that
	// Init still contains VarInits (`"D" # f`, `name # "_a"`), so a class
	// application that mentions NAME stays unresolved there; only a literal
	// name is a StringInit at that point.
	prevUnresolved := l.k.nameUnresolved
	l.k.nameUnresolved = st.Name != nil && !l.parseTimeKnown(st.Name, env)
	for _, p := range st.Parents {
		err := l.applyParent(rec, p, env)
		if err != nil {
			l.k.nameUnresolved = prevUnresolved
			return nil, false, err
		}
	}
	l.k.nameUnresolved = prevUnresolved
	if !l.namesOnly {
		// LowerNames never instantiates the parents, so their fields are
		// missing and a `let Field` would report "Value unknown". Names and
		// the direct parent list are all it needs, so skip the let stack.
		for _, lb := range preLets {
			if err := l.applyLet(rec, lb.b, lb.env); err != nil {
				return nil, false, err
			}
		}
	}
	if !l.namesOnly {
		// A def body only carries fields, lets, and assertions; the name and
		// the parent list are already fixed above, so LowerNames skips it.
		for _, item := range st.Body {
			if err := l.bodyItem(rec, item, env); err != nil {
				l.errorAt(err, item.Span())
			}
		}
	}
	// LLVM's Record location is the object's name token; a multi-line `def`
	// header therefore reports the name's line, not the `def` keyword's.
	if !anonymous && st.Name != nil {
		if p := st.Name.Span().Start; p.File != "" {
			rec.Locs[0] = p
		}
	}
	return rec, anonymous, nil
}

func (l *Lowerer) evalName(v syntax.Value, env *Env) (string, error) {
	return l.evalNameAt(v, env, v.Span())
}

// evalNameAt is evalName with the record's own location for the failure
// diagnostic: Record::checkName runs in the Record constructor and reports
// there (Record.cpp:2813-2821), so a `def true` points at the `def` keyword
// rather than at the name token.
func (l *Lowerer) evalNameAt(v syntax.Value, env *Env, at diag.Span) (string, error) {
	val, err := l.k.evalSyntax(v, env, nil)
	if err != nil {
		return "", err
	}
	s, ok := val.(*Str)
	if !ok {
		return "", &EvalError{Msg: "Record name '" + initText(v, env, nil) + "' is not a string!", Span: at}
	}
	return s.S, nil
}

func spanAt(loc *diag.Pos) diag.Span {
	if loc == nil {
		return diag.Span{}
	}
	return diag.Span{Start: *loc, End: *loc}
}

func isUnset(v syntax.Value) bool {
	_, ok := v.(*syntax.UnsetValue)
	return ok
}

// defm expands each multiclass reference into concrete defs.
// reserveAnonNames allocates parse-time anonymous names for unnamed defs in a
// multiclass body, mirroring TGParser::ParseDef. LLVM allocates these when the
// multiclass is parsed; instantiation keeps the name unless it collides.
func (l *Lowerer) reserveAnonNames(items []syntax.Object) {
	for _, o := range items {
		switch t := o.(type) {
		case *syntax.DefStmt:
			if t.Name == nil || isUnset(t.Name) {
				if _, ok := l.anonName[t]; !ok {
					l.anonName[t] = l.k.newAnonymous(t.Span().Start).Name
				}
			}
			l.preFold(t.Name, nil)
			l.preFoldParents(t.Parents)
			l.preFoldItems(t.Body)
		case *syntax.DefmStmt:
			if t.Name == nil || isUnset(t.Name) {
				if _, ok := l.anonDefmName[t]; !ok {
					l.anonDefmName[t] = l.k.newAnonymous(t.Span().Start).Name
				}
			}
			l.preFoldParents(t.Parents)
			l.preExpandInnerDefm(t)
		case *syntax.LetStmt:
			for _, b := range t.Binds {
				l.preFold(b.Value, nil)
			}
			l.reserveAnonNames(t.Body)
		case *syntax.IfStmt:
			l.preFold(t.Cond, nil)
			l.reserveAnonNames(t.Then)
			l.reserveAnonNames(t.Else)
		case *syntax.ForeachStmt:
			l.preFold(t.List, nil)
			l.reserveAnonNames(t.Body)
		case *syntax.DefvarStmt:
			l.preFoldDefvar(t.Value, nil)
		case *syntax.AssertStmt:
			l.preFold(t.Cond, nil)
			l.preFold(t.Message, nil)
		case *syntax.ClassStmt:
			// A class defined inside a multiclass body is parsed here too.
			for _, d := range t.TemplateArgs {
				l.preFold(d.Value, nil)
			}
			l.preFoldParents(t.Parents)
			l.preFoldItems(t.Body)
		}
	}
}

// preExpandInnerDefm mirrors what TGParser does with a `defm` written inside a
// multiclass body: ParseObject calls ParseDefm there, so the referenced
// multiclass is expanded while the body is parsed and its entries become
// entries of the enclosing multiclass (TGParser.cpp:4565, addEntry at :411).
// The records themselves stay unresolved, but every class application that is
// already concrete - notably the elements of a foreach list such as
// `VTPair<AccumType, v4f32>` - is instantiated then and keeps the anonymous
// number it was given (VarDefInit::resolveReferences, Record.cpp:2423-2452).
func (l *Lowerer) preExpandInnerDefm(st *syntax.DefmStmt) {
	if l.preDepth > 16 {
		return
	}
	l.preDepth++
	defer func() { l.preDepth-- }()
	prevEager := l.eager
	l.eager = true
	defer func() { l.eager = prevEager }()
	prevPre := l.k.preInstantiate
	l.k.preInstantiate = true
	defer func() { l.k.preInstantiate = prevPre }()
	for _, ref := range st.Parents[:st.ClassStart] {
		if mc := l.mcs[ref.Name]; mc != nil {
			l.preExpandMC(mc, ref.Args)
		}
	}
}

// preExpandMC walks one multiclass the parser would instantiate at this point,
// binding its template arguments to the call site's values.
func (l *Lowerer) preExpandMC(mc *syntax.MultiClassStmt, args []*syntax.Arg) {
	if l.preDepth > 16 {
		return
	}
	l.preDepth++
	defer func() { l.preDepth-- }()
	binds := map[string]Binding{}
	for i, a := range args {
		if a == nil || a.Value == nil {
			continue
		}
		name := a.Name
		if name == "" {
			if i >= len(mc.TemplateArgs) {
				continue
			}
			name = mc.TemplateArgs[i].Name
		}
		binds[mc.Name+"::"+name] = Binding{V: a.Value, ParseTime: l.parseTimeKnown(a.Value, nil)}
	}
	for _, d := range mc.TemplateArgs {
		key := mc.Name + "::" + d.Name
		if _, ok := binds[key]; ok || d.Value == nil {
			continue
		}
		binds[key] = Binding{V: d.Value, ParseTime: l.parseTimeKnown(d.Value, nil)}
	}
	env := (&Env{}).With(binds)
	for _, p := range mc.Parents {
		if pm := l.mcs[p.Name]; pm != nil {
			l.preExpandMC(pm, p.Args)
		}
	}
	l.preExpandBody(mc.Body, env)
}

// preExpandBody walks a multiclass body the way it is parsed, unrolling every
// loop whose list is already a list of concrete values and descending into
// nested defms. It only creates what the parser would create; a body that
// still needs template arguments is left alone, so the instantiation that
// follows sees the same VarDefInits LLVM would.
func (l *Lowerer) preExpandBody(objs []syntax.Object, env *Env) {
	prevPre := l.k.preInstantiate
	l.k.preInstantiate = true
	defer func() { l.k.preInstantiate = prevPre }()
	prevEager := l.eager
	l.eager = true
	defer func() { l.eager = prevEager }()
	for _, o := range objs {
		switch t := o.(type) {
		case *syntax.ForeachStmt:
			if t.Range != nil {
				// `foreach i = 1...4` has no list value; TGParser unrolls it
				// while parsing, so the body sees a concrete iterator.
				for _, n := range t.Range {
					l.preExpandBody(t.Body, env.WithBinding(t.Var, Binding{Val: &Int{N: int64(n)}, ParseTime: true}))
				}
				continue
			}
			if t.List == nil {
				continue
			}
			l.preFold(t.List, env)
			v, err := l.k.evalSyntax(t.List, env, nil)
			if err != nil {
				continue
			}
			ls, ok := v.(*List)
			if !ok {
				continue
			}
			known := l.parseTimeList(t.List, env)
			for i, e := range ls.Elems {
				kt := known
				if lit, ok := t.List.(*syntax.ListValue); ok {
					kt = kt && i < len(lit.Elems) && l.parseTimeKnown(lit.Elems[i], env)
				}
				l.preExpandBody(t.Body, env.WithBinding(t.Var, Binding{Val: e, ParseTime: kt}))
			}
		case *syntax.IfStmt:
			l.preFold(t.Cond, env)
			v, err := l.k.evalSyntax(t.Cond, env, nil)
			if err != nil {
				continue
			}
			n, ok := v.(*Int)
			if !ok {
				continue
			}
			if n.N != 0 {
				l.preExpandBody(t.Then, env)
			} else {
				l.preExpandBody(t.Else, env)
			}
		case *syntax.LetStmt:
			l.preExpandBody(t.Body, env)
		case *syntax.DefmStmt:
			l.preExpandInnerDefm(t)
		case *syntax.DefStmt:
			l.preFold(t.Name, env)
			// TGParser::resolve copies a multiclass-body def and resolves
			// the copy with the loop substitutions (TGParser.cpp:556-564), so
			// a class application written in a parent's argument list -
			// `def : Pat<(VT (f (SVEType<VT>.Packed ...)))>` - instantiates
			// its anonymous record here, not when the enclosing defm's record
			// is finished (VarDefInit::resolveReferences, Record.cpp:2423).
			l.preFoldParentArgs(t.Parents, env)
			l.preFoldBodyItems(t.Body, env)
		}
	}
}

// preFoldBodyItems is preFoldItems with the environment the enclosing entry is
// resolved in, so a class application that mentions a parse-time-unrolled loop
// variable instantiates the way VarDefInit::resolveReferences does.
func (l *Lowerer) preFoldBodyItems(items []syntax.BodyItem, env *Env) {
	for _, it := range items {
		switch t := it.(type) {
		case *syntax.Decl:
			l.preFold(t.Value, env)
		case *syntax.LetItem:
			l.preFold(t.Bind.Value, env)
		case *syntax.AssertStmt:
			l.preFold(t.Cond, env)
			l.preFold(t.Message, env)
		case *syntax.DumpStmt:
			l.preFold(t.Value, env)
		}
	}
}

func (l *Lowerer) preFoldParents(parents []*syntax.ParentRef) {
	l.preFoldParentArgs(parents, nil)
}

// preFoldParentArgs folds the argument list of every superclass application.
// A multiclass body is resolved with the loop substitutions applied, so the
// arguments see the iterator bindings (unlike a class body, which is resolved
// when a def inherits it).
func (l *Lowerer) preFoldParentArgs(parents []*syntax.ParentRef, env *Env) {
	for _, p := range parents {
		for _, a := range p.Args {
			l.preFold(a.Value, env)
		}
	}
}

func (l *Lowerer) preFoldItems(items []syntax.BodyItem) {
	for _, it := range items {
		switch t := it.(type) {
		case *syntax.Decl:
			l.preFold(t.Value, nil)
		case *syntax.LetItem:
			l.preFold(t.Bind.Value, nil)
		case *syntax.AssertStmt:
			l.preFold(t.Cond, nil)
			l.preFold(t.Message, nil)
		case *syntax.DumpStmt:
			l.preFold(t.Value, nil)
		}
	}
}

// preFoldParseItems runs the folds TGParser performs while it parses a block of
// statements that will be resolved later (the body of a foreach or if). It
// only sees the parse-time environment: `nil` stands for TGParser's empty
// resolver, so template arguments, record fields and loop variables are still
// VarInit and block instantiation. defvar inlines are already substituted in
// the AST, exactly like TGParser's scope lookup.
func (l *Lowerer) preFoldParseItems(items []syntax.Object) {
	for _, o := range items {
		switch t := o.(type) {
		case *syntax.DefStmt:
			l.preFold(t.Name, nil)
			l.preFoldParents(t.Parents)
			l.preFoldItems(t.Body)
		case *syntax.ClassStmt:
			for _, d := range t.TemplateArgs {
				l.preFold(d.Value, nil)
			}
			l.preFoldParents(t.Parents)
			l.preFoldItems(t.Body)
		case *syntax.MultiClassStmt:
			l.preFoldParseItems(t.Body)
		case *syntax.DefmStmt:
			l.preFoldParents(t.Parents)
		case *syntax.ForeachStmt:
			l.preFold(t.List, nil)
			l.preFoldParseItems(t.Body)
		case *syntax.IfStmt:
			l.preFold(t.Cond, nil)
			l.preFoldParseItems(t.Then)
			l.preFoldParseItems(t.Else)
		case *syntax.LetStmt:
			for _, b := range t.Binds {
				l.preFold(b.Value, nil)
			}
			l.preFoldParseItems(t.Body)
		case *syntax.DefvarStmt:
			l.preFoldDefvar(t.Value, nil)
		case *syntax.AssertStmt:
			l.preFold(t.Cond, nil)
			l.preFold(t.Message, nil)
		case *syntax.DumpStmt:
			l.preFold(t.Value, nil)
		case *syntax.DefsetStmt:
			l.preFoldParseItems(t.Objects)
		}
	}
}

func (l *Lowerer) defm(st *syntax.DefmStmt, env *Env, lets []*syntax.LetBind) error {
	defmName := ""
	if st.Name == nil || isUnset(st.Name) {
		// The anonymous name is only a prefix; it is not itself a def. A defm
		// inside a loop body was parsed once, so every iteration keeps the
		// name ParseDefm gave it (TGParser.cpp:4573) and only the records it
		// generates collide.
		defmName = l.anonDefmName[st]
		if defmName == "" {
			defmName = l.k.newAnonymous(st.Span().Start).Name
		}
	} else {
		n, err := l.evalNameAt(st.Name, env, st.Span())
		if err != nil {
			return err
		}
		defmName = n
	}
	prevSeq := l.defmSeq
	if st.Seq >= 0 {
		l.defmSeq = st.Seq
	}
	err := l.expandDefm(st, defmName, env, scopedLets(lets, env), nil, nil, nil)
	l.defmSeq = prevSeq
	return err
}

// expandDefm expands every multiclass in st's parent list. defmName is the
// final prefix for the generated defs; outerLocs are the defm locations of
// enclosing multiclass instantiations, innermost first.
func (l *Lowerer) expandDefm(st *syntax.DefmStmt, defmName string, env *Env, siteLets, outerLets []scopedLet, outerLocs []diag.Pos, extraParents []*syntax.ParentRef) error {
	// siteLets are the lets lexically around this defm statement; outerLets
	// come from the enclosing defm levels and are only needed on the path
	// where a statement-level loop finishes a record before phase 3.
	// ParseDefm at each level applies first its trailing classes and then its
	// own let stack, so a nested defm's lets land before the enclosing
	// level's trailing classes (TGParser.cpp:4626-4659).
	mergedLets := append(append([]scopedLet(nil), siteLets...), outerLets...)
	// Each ParseDefm applies its own trailing classes to every entry it
	// resolves; the enclosing levels' classes arrive later, when those levels
	// finalise this defm's records. A record finished by a statement-level
	// loop inside phase 1 never reaches phase 2, so it takes the merged list
	// here (inner list first, enclosing classes appended after it).
	ownTrailing := st.Parents[st.ClassStart:]
	trailing := append(append([]*syntax.ParentRef(nil), ownTrailing...), extraParents...)
	// Phase 1: resolve every generated entry (this is where a class
	// application used as a parent argument instantiates its anonymous def).
	var pool []*deferredDef
	prevPending := l.pending
	l.pending = &pool
	var err error
	for _, ref := range st.Parents[:st.ClassStart] {
		mc := l.mcs[ref.Name]
		if mc == nil {
			err = &EvalError{Msg: "Couldn't find multiclass '" + ref.Name + "'", Span: ref.Span()}
			break
		}
		locs := append([]diag.Pos{ref.Span().Start}, outerLocs...)
		if err = l.expandMC(mc, ref.Args, defmName, locs, env, mergedLets, trailing); err != nil {
			break
		}
	}
	l.pending = prevPending
	if err != nil {
		return err
	}
	// Phase 2: this level's trailing regular classes, applied to every entry
	// up front (TGParser::ParseDefm's AddParentClass loop).
	for _, d := range pool {
		if err := l.applyParents(d.rec, ownTrailing, d.env); err != nil {
			return err
		}
		// This defm sits inside a multiclass body, so its own ParseDefm ran
		// while that body was parsed: LLVM applies siteLets there, before the
		// enclosing defm adds its trailing classes. The outermost defm instead
		// applies them in phase 3, interleaved with addDefOne, exactly like
		// ParseDefm's final ApplyLetStack/addEntry loop.
		if prevPending != nil && !l.namesOnly {
			for _, lb := range siteLets {
				if err := l.applyLet(d.rec, lb.b, lb.env); err != nil {
					return err
				}
			}
		}
	}
	// addEntry appends the entries of a defm that sits inside a multiclass body
	// to that multiclass, so the enclosing defm owns their addDefOne. Only a
	// top-level defm finalises here; this is also what keeps a sibling
	// `def NAME#_#vt` visible to `!cast<...>(NAME)` in a nested defm.
	if prevPending != nil {
		*prevPending = append(*prevPending, pool...)
		return nil
	}
	// Phase 3: the defm let stack, then addDefOne per entry (this is where a
	// class application in a field initialiser instantiates its anonymous def).
	for _, d := range pool {
		for _, lb := range siteLets {
			if err := l.applyLet(d.rec, lb.b, lb.env); err != nil {
				return err
			}
		}
		loopSeq := l.loopSeq
		l.loopSeq = d.loopSeq
		err := l.finishDef(d.rec, d.anonymous)
		l.loopSeq = loopSeq
		if err != nil {
			return err
		}
	}
	return nil
}

func (l *Lowerer) expandMC(mc *syntax.MultiClassStmt, args []*syntax.Arg, defmName string, locs []diag.Pos, callEnv *Env, postLets []scopedLet, extraParents []*syntax.ParentRef) error {
	prevEager := l.eager
	l.eager = true
	defer func() { l.eager = prevEager }()
	mcRec := &Record{Name: mc.Name, IsClass: true}
	frame := map[string]Binding{
		mc.Name + "::NAME": {Val: &Str{S: defmName}},
	}
	for _, d := range mc.TemplateArgs {
		if d.Value != nil {
			frame[mc.Name+"::"+d.Name] = Binding{
				V: d.Value, Dynamic: true, Ty: l.k.tyFromSyntax(d.Type),
				Owner: mcRec, Sep: "::", Self: mc.Name + "::" + d.Name,
			}
		}
	}
	env := callEnv.With(frame)
	env.owner = mcRec
	env.ownerSep = "::"
	// Caller-supplied arguments are lexical in the defm's scope.
	provided, err := l.bindMCArgs(mc, args, callEnv, firstLoc(locs))
	if err != nil {
		return err
	}
	if len(provided) > 0 {
		env = env.With(provided)
	}
	// Parent multiclasses contribute their entries first.
	for _, p := range mc.Parents {
		pm := l.mcs[p.Name]
		if pm == nil {
			return &EvalError{Msg: "Couldn't find multiclass '" + p.Name + "'", Span: p.Span()}
		}
		if err := l.expandMC(pm, p.Args, defmName, locs, env, postLets, extraParents); err != nil {
			return err
		}
	}
	preLets := scopedLets(l.mcLets[mc.Name], env)
	return l.mcObjects(mc.Body, env, preLets, defmName, locs, postLets, extraParents)
}

func firstLoc(locs []diag.Pos) *diag.Pos {
	if len(locs) == 0 {
		return nil
	}
	return &locs[0]
}

func (l *Lowerer) bindMCArgs(mc *syntax.MultiClassStmt, args []*syntax.Arg, callEnv *Env, loc *diag.Pos) (map[string]Binding, error) {
	if len(args) > len(mc.TemplateArgs) {
		return nil, &EvalError{Msg: "Too many template arguments for multiclass '" + mc.Name + "'", Span: spanAt(loc)}
	}
	out := map[string]Binding{}
	for i, a := range args {
		name := a.Name
		if name == "" {
			if i >= len(mc.TemplateArgs) {
				return nil, &EvalError{Msg: "Too many template arguments for multiclass '" + mc.Name + "'", Span: a.Span()}
			}
			name = mc.TemplateArgs[i].Name
		}
		ty := (*Ty)(nil)
		for _, d := range mc.TemplateArgs {
			if d.Name == name {
				ty = l.k.tyFromSyntax(d.Type)
				break
			}
		}
		if a.Value != nil {
			if err := l.k.checkValueType(ty, a.Value, nil); err != nil {
				return nil, err
			}
		}
		out[mc.Name+"::"+name] = Binding{V: a.Value, Env: callEnv, Ty: ty}
	}
	for _, d := range mc.TemplateArgs {
		if _, ok := out[mc.Name+"::"+d.Name]; ok {
			continue
		}
		if d.Value == nil || isUnset(d.Value) {
			// resolveArguments reports the qualified argument name, which
			// QualifyName builds with "::" for a multiclass (Record.cpp:118-127).
			return nil, &EvalError{Msg: "value not specified for template argument '" + mc.Name + "::" + d.Name + "'", Span: spanAt(loc)}
		}
	}
	return out, nil
}

func (l *Lowerer) mcObjects(objs []syntax.Object, env *Env, preLets []scopedLet, defmName string, outerLocs []diag.Pos, postLets []scopedLet, extraParents []*syntax.ParentRef) error {
	for _, o := range objs {
		switch t := o.(type) {
		case *syntax.DefStmt:
			name := l.anonName[t]
			if t.Name != nil && !isUnset(t.Name) {
				n, err := l.evalName(t.Name, env)
				if err != nil {
					return err
				}
				if usesNAME(t.Name) {
					name = n
				} else {
					name = defmName + n
				}
			}
			if _, err := l.buildDef(t, env, preLets, postLets, extraParents, outerLocs, name); err != nil {
				return err
			}
		case *syntax.LetStmt:
			nl := append(append([]scopedLet(nil), preLets...), scopedLets(t.Binds, env)...)
			if err := l.mcObjects(t.Body, env, nl, defmName, outerLocs, postLets, extraParents); err != nil {
				return err
			}
		case *syntax.IfStmt:
			v, err := l.k.evalSyntax(t.Cond, env, nil)
			if err != nil {
				return err
			}
			n, err := l.k.asInt(v)
			if err != nil {
				return err
			}
			body := t.Else
			if n != 0 {
				body = t.Then
			}
			// LLVM lowers `if` to a foreach over !if(cond, [0], []), so it
			// expands a statement loop exactly like the foreach case.
			locs := loopLocs(l, t.Cond, env, outerLocs, false)
			if err := l.mcObjects(body, env, preLets, defmName, locs, postLets, extraParents); err != nil {
				return err
			}
		case *syntax.ForeachStmt:
			// A loop whose list is already a list literal at parse time is
			// unrolled by LLVM's parser, so its records sit directly in the
			// multiclass body and inherit the defm's location. A loop whose
			// list only resolves at instantiation time becomes a ForeachLoop
			// entry that resolve() expands without the current defm's Loc
			// (TGParser.cpp:511); the records then join the enclosing level
			// as entries, which appends that level's location there.
			locs := outerLocs
			if t.Range == nil {
				locs = loopLocs(l, t.List, env, outerLocs, true)
			}
			if err := l.mcForeach(t, env, preLets, defmName, locs, postLets, extraParents); err != nil {
				return err
			}
		case *syntax.DefvarStmt:
			// Inlined by the parser.
		case *syntax.AssertStmt:
			l.assert(t, env, nil)
		case *syntax.DumpStmt:
			// resolve() applies the defm substitutions and calls dumpMessage
			// immediately for a dump that is not inside a def
			// (TGParser.cpp:544-556).
			l.emitDump(t.Span().Start, t.Value, env, nil)
		case *syntax.DefmStmt:
			var inner string
			switch {
			case t.Name == nil || isUnset(t.Name):
				inner = defmName + l.anonDefmName[t]
			case usesNAME(t.Name):
				n, err := l.evalName(t.Name, env)
				if err != nil {
					return err
				}
				inner = n
			default:
				n, err := l.evalName(t.Name, env)
				if err != nil {
					return err
				}
				inner = defmName + n
			}
			// A let around a defm is applied after the generated records.
			// The lets written here in the multiclass body belong to this
			// nested defm's own ParseDefm (applied before the enclosing
			// level's trailing classes); the call-site stack rides along as
			// outerLets for the immediate loop path only.
			if err := l.expandDefm(t, inner, env, preLets, postLets, outerLocs, extraParents); err != nil {
				return err
			}
		default:
			return unsupported("multiclass body item", o.Span())
		}
	}
	return nil
}

// outerLocsAbove drops the innermost defm location. A loop that resolve()
// unrolls creates its records without the current site; enclosing multiclass
// levels still append theirs once the records become their entries
// (TGParser.cpp:561).
func outerLocsAbove(locs []diag.Pos) []diag.Pos {
	if len(locs) <= 1 {
		return nil
	}
	return locs[1:]
}

// loopLocs mirrors the three ways TGParser expands a foreach/if body:
//
//   - the list or condition is already a literal while the multiclass is
//     parsed (addEntry -> resolve, TGParser.cpp:426-430): the records are
//     plain body entries and keep the whole location chain;
//   - it becomes a literal only after substituting this defm's arguments
//     (TGParser.cpp:511): resolve() expands the loop without the current
//     site, and each enclosing level appends its own loc later;
//   - it is still unresolved here (a name that only the enclosing multiclass
//     will bind): the loop survives into an outer resolve(), which drops
//     every remaining site, so the records carry none of them.
func loopLocs(l *Lowerer, v syntax.Value, env *Env, outerLocs []diag.Pos, list bool) []diag.Pos {
	known := l.parseTimeKnown
	if list {
		// A foreach unrolls for any ListInit, even when the elements are
		// still unresolved class applications.
		known = l.parseTimeList
	}
	switch {
	case known(v, env):
		return outerLocs
	case l.levelKnown(v, env):
		return outerLocsAbove(outerLocs)
	}
	return nil
}

// levelKnown reports whether v is foldable once this defm call's arguments are
// substituted: literals, and bindings whose argument expression is itself a
// literal. A binding that still names another template argument stays a
// VarInit and does not count.
func (l *Lowerer) levelKnown(v syntax.Value, env *Env) bool {
	switch t := v.(type) {
	case nil, *syntax.UnsetValue, *syntax.IntValue, *syntax.BitsValue,
		*syntax.StringValue, *syntax.ListValue:
		return true
	case *syntax.VarValue:
		if t.Subst && t.Lit != nil {
			return l.levelKnown(t.Lit, env)
		}
		if t.GlobalRef {
			return true
		}
		if q, ok := env.Qualify(t.Name); ok {
			if b, ok := env.Lookup(q); ok {
				return bindingLevelKnown(b)
			}
		}
		if b, ok := env.Lookup(t.Name); ok {
			return bindingLevelKnown(b)
		}
		if t.Lit != nil {
			return false
		}
		// No binding at all: a global def or defvar, known while parsing.
		return true
	case *syntax.FieldValue:
		return l.levelKnown(t.Base, env)
	case *syntax.SliceValue:
		if !l.levelKnown(t.Base, env) {
			return false
		}
		for _, e := range t.Index {
			if e.Lo != nil && !l.levelKnown(e.Lo, env) {
				return false
			}
			if e.Hi != nil && !l.levelKnown(e.Hi, env) {
				return false
			}
		}
		return true
	case *syntax.BitRangeValue:
		return l.levelKnown(t.Base, env)
	case *syntax.PasteValue:
		return l.levelKnown(t.LHS, env) && l.levelKnown(t.RHS, env)
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			if a.Value != nil && !l.levelKnown(a.Value, env) {
				return false
			}
		}
		return true
	case *syntax.BangValue:
		if t.Op == syntax.XInstances {
			return false
		}
		for _, a := range t.Args {
			if a != nil && !l.levelKnown(a, env) {
				return false
			}
		}
		for _, c := range t.Cases {
			if c.Key != nil && !l.levelKnown(c.Key, env) {
				return false
			}
			if c.Value != nil && !l.levelKnown(c.Value, env) {
				return false
			}
		}
		return true
	}
	return false
}

// bindingLevelKnown is levelKnown for an env binding: an evaluated value or a
// parse-time binding is known; otherwise only a literal expression counts. A
// bound VarInit is deliberately opaque - resolve() does not chase it through
// the enclosing multiclass's substitutions (TGParser.cpp:511).
func bindingLevelKnown(b Binding) bool {
	if b.ParseTime || b.Val != nil {
		return true
	}
	if b.V != nil {
		return levelValueKnown(b.V)
	}
	return false
}

func levelValueKnown(v syntax.Value) bool {
	switch t := v.(type) {
	case nil, *syntax.UnsetValue, *syntax.IntValue, *syntax.BitsValue,
		*syntax.StringValue, *syntax.ListValue:
		return true
	case *syntax.VarValue:
		return t.Lit != nil && levelValueKnown(t.Lit)
	}
	return false
}

func (l *Lowerer) mcForeach(st *syntax.ForeachStmt, env *Env, preLets []scopedLet, defmName string, outerLocs []diag.Pos, postLets []scopedLet, extraParents []*syntax.ParentRef) error {
	var elems []Value
	if st.Range != nil {
		for _, n := range st.Range {
			elems = append(elems, &Int{N: int64(n)})
		}
	} else {
		l.preFold(st.List, env)
		v, err := l.k.evalSyntax(st.List, env, nil)
		if err != nil {
			return err
		}
		ls, ok := v.(*List)
		if !ok {
			return &EvalError{Msg: "attempting to loop over a non-list", Span: st.Span()}
		}
		elems = ls.Elems
	}
	l.loopVars = append(l.loopVars, st.Var)
	defer func() { l.loopVars = l.loopVars[:len(l.loopVars)-1] }()
	for i, e := range elems {
		// A loop variable is known at parse time only when the loop was
		// unrolled then *and* this element was a literal at that point.
		known := outerLocs != nil && (st.Range != nil || l.parseTimeList(st.List, env))
		if lit, ok := st.List.(*syntax.ListValue); ok {
			known = known && i < len(lit.Elems) && l.parseTimeKnown(lit.Elems[i], env)
		}
		sub := env.WithBinding(st.Var, Binding{Val: e, ParseTime: known})
		if err := l.mcObjects(st.Body, sub, preLets, defmName, outerLocs, postLets, extraParents); err != nil {
			return err
		}
	}
	return nil
}

// parseTimeList reports whether an expression is structurally a list at parse
// time, i.e. resolves to a ListInit even while template arguments are still
// unresolved. LLVM only unrolls such foreach loops during parsing; the records
// then live directly in the multiclass body.
func (l *Lowerer) parseTimeList(v syntax.Value, env *Env) bool {
	switch t := v.(type) {
	case *syntax.ListValue:
		return true
	case *syntax.VarValue:
		// A defvar is substituted textually by the parser.
		if t.Lit != nil {
			return l.parseTimeList(t.Lit, env)
		}
	case *syntax.FieldValue:
		// A field of a top-level def is complete while the multiclass is
		// parsed, so `foreach d = AMDGPUDims.All` unrolls there and its
		// records inherit the defm location.
		if b, ok := t.Base.(*syntax.VarValue); ok && b.GlobalRef {
			if d := l.k.defs[b.Name]; d != nil {
				if rv := d.Val(t.Field); rv != nil && rv.Expr != nil {
					if e, ok := rv.Expr.(*AstExpr); ok {
						return l.parseTimeList(e.V, env)
					}
				}
			}
		}
	case *syntax.BangValue:
		if t.Op == syntax.XListConcat && len(t.Args) == 2 {
			return l.parseTimeList(t.Args[0], env) && l.parseTimeList(t.Args[1], env)
		}
		if t.Op == syntax.XRange && len(t.Args) == 3 {
			// TGParser fills the bounds while parsing, so a one-argument
			// `!range(x)` is already (0, x, 1) here — or (0, !size(x), 1) for
			// a list (TGParser.cpp:2008-2044). The range's size is known as
			// soon as the list (or the bound) is.
			if !l.parseTimeKnown(t.Args[0], env) || !l.parseTimeKnown(t.Args[2], env) {
				return false
			}
			if size, ok := t.Args[1].(*syntax.BangValue); ok &&
				size.Op == syntax.XSize && len(size.Args) == 1 {
				return l.parseTimeKnown(size.Args[0], env)
			}
			return l.parseTimeKnown(t.Args[1], env)
		}
	}
	return false
}

// parseTimeKnown reports whether LLVM's parser could already resolve v while
// parsing a multiclass definition: its free variables must be literals,
// defvars, or loop variables substituted by a parse-time-unrolled loop.
// Multiclass template arguments are not parse-time known.
func (l *Lowerer) parseTimeKnown(v syntax.Value, env *Env) bool {
	switch t := v.(type) {
	case nil:
		return true
	case *syntax.UnsetValue, *syntax.IntValue, *syntax.BitsValue, *syntax.StringValue:
		return true
	case *syntax.ListValue:
		for _, e := range t.Elems {
			if !l.parseTimeKnown(e, env) {
				return false
			}
		}
		return true
	case *syntax.DagValue:
		if t.Op != nil && !l.parseTimeKnown(t.Op, env) {
			return false
		}
		for _, a := range t.Args {
			if a.Value != nil && !l.parseTimeKnown(a.Value, env) {
				return false
			}
		}
		return true
	case *syntax.VarValue:
		if t.Subst && t.Lit != nil {
			// A defvar is substituted textually, but its value can still
			// mention template arguments (VectorMathLibcall's
			// `defvar VT = !cast<...>(suffix);`), so look inside.
			return l.parseTimeKnown(t.Lit, env)
		}
		if t.GlobalRef {
			// Defs are complete when the multiclass is parsed.
			return true
		}
		if q, ok := env.Qualify(t.Name); ok {
			// A multiclass's caller-supplied arguments are bound without an
			// owner record, so any hit under the qualified key is a template
			// argument.
			if b, ok := env.Lookup(q); ok {
				return b.ParseTime
			}
		}
		if b, ok := env.Lookup(t.Name); ok {
			return b.ParseTime
		}
		if t.Lit != nil {
			// TGParser sees VarInit(field) here, never the inlined
			// initializer, so a field reference is not parse-time known.
			return false
		}
		// A name with no binding at all is a global defvar or record.
		return true
	case *syntax.FieldValue:
		return l.parseTimeKnown(t.Base, env)
	case *syntax.SliceValue:
		if !l.parseTimeKnown(t.Base, env) {
			return false
		}
		for _, e := range t.Index {
			if e.Lo != nil && !l.parseTimeKnown(e.Lo, env) {
				return false
			}
			if e.Hi != nil && !l.parseTimeKnown(e.Hi, env) {
				return false
			}
		}
		return true
	case *syntax.BitRangeValue:
		return l.parseTimeKnown(t.Base, env)
	case *syntax.PasteValue:
		return l.parseTimeKnown(t.LHS, env) && l.parseTimeKnown(t.RHS, env)
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			if a.Value != nil && !l.parseTimeKnown(a.Value, env) {
				return false
			}
		}
		return true
	case *syntax.BangValue:
		if t.Op == syntax.XInstances {
			return false
		}
		for _, a := range t.Args {
			if a != nil && !l.parseTimeKnown(a, env) {
				return false
			}
		}
		for _, c := range t.Cases {
			if c.Key != nil && !l.parseTimeKnown(c.Key, env) {
				return false
			}
			if c.Value != nil && !l.parseTimeKnown(c.Value, env) {
				return false
			}
		}
		return true
	}
	return false
}

func usesNAME(v syntax.Value) bool {
	switch t := v.(type) {
	case *syntax.VarValue:
		return t.Name == "NAME"
	case *syntax.PasteValue:
		return usesNAME(t.LHS) || usesNAME(t.RHS)
	case *syntax.BangValue:
		for _, a := range t.Args {
			if usesNAME(a) {
				return true
			}
		}
		for _, c := range t.Cases {
			if usesNAME(c.Key) || usesNAME(c.Value) {
				return true
			}
		}
	}
	return false
}

// preFold evaluates a value at the point where TGParser's ParseValue would
// have folded it. The value itself is thrown away: what matters is that a
// `Class<concrete args>` application instantiates its anonymous record right
// there, because llvm-tblgen allocates anonymous_N names during parsing, not
// when a record is first used (VarDefInit::Fold). Errors are ignored; the same
// expression is evaluated for real when its record resolves, and that is where
// diagnostics come from.
//
// Only lexical bindings are visible unless the caller is expanding a defm:
// at parse time a class template argument or a record field is still a
// VarInit, so an application over it is not concrete yet.
func (l *Lowerer) preFold(v syntax.Value, env *Env) {
	if v == nil || !hasClassApply(v) {
		return
	}
	if l.skipLoopFold && l.usesLoopVar(v) {
		return
	}
	// VarDefInit::get runs while the value is parsed, with the record's scope
	// visible, so sites are noted before the parse-time resolver is stripped.
	l.k.noteShapeSites(v, env)
	if !l.eager {
		env = env.LexicalOnly()
	}
	prev := l.k.parseFold
	l.k.parseFold = true
	l.preFoldValue(v, env, nil)
	l.k.parseFold = prev
}

// preFoldDefvar folds a defvar initializer the way ParseDefvar does. TGParser
// parses the value once, with a foreach iterator still a VarInit, and resolve()
// never revisits defvar values (they are not RecordsEntry nodes). So an
// initializer that mentions a loop variable is never concrete and never
// instantiates; only a purely literal one folds at parse time.
func (l *Lowerer) preFoldDefvar(v syntax.Value, env *Env) {
	if l.usesLoopVar(v) {
		return
	}
	l.preFold(v, env)
}

// usesLoopVar reports whether a value mentions any enclosing foreach iterator,
// looking through defvar inlines (a defvar's value is substituted textually by
// the parser, so `defvar b = a` copies a's expression).
func (l *Lowerer) usesLoopVar(v syntax.Value) bool {
	switch t := v.(type) {
	case nil, *syntax.UnsetValue, *syntax.IntValue, *syntax.BitsValue, *syntax.StringValue:
		return false
	case *syntax.VarValue:
		for _, name := range l.loopVars {
			if t.Name == name {
				return true
			}
		}
		return t.Lit != nil && l.usesLoopVar(t.Lit)
	case *syntax.ListValue:
		for _, e := range t.Elems {
			if l.usesLoopVar(e) {
				return true
			}
		}
	case *syntax.DagValue:
		if l.usesLoopVar(t.Op) {
			return true
		}
		for _, a := range t.Args {
			if l.usesLoopVar(a.Value) {
				return true
			}
		}
	case *syntax.FieldValue:
		return l.usesLoopVar(t.Base)
	case *syntax.SliceValue:
		if l.usesLoopVar(t.Base) {
			return true
		}
		for _, e := range t.Index {
			if l.usesLoopVar(e.Lo) || l.usesLoopVar(e.Hi) {
				return true
			}
		}
	case *syntax.BitRangeValue:
		return l.usesLoopVar(t.Base)
	case *syntax.PasteValue:
		return l.usesLoopVar(t.LHS) || l.usesLoopVar(t.RHS)
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			if l.usesLoopVar(a.Value) {
				return true
			}
		}
	case *syntax.BangValue:
		for _, a := range t.Args {
			if l.usesLoopVar(a) {
				return true
			}
		}
		for _, c := range t.Cases {
			if l.usesLoopVar(c.Key) || l.usesLoopVar(c.Value) {
				return true
			}
		}
	}
	return false
}

// preFoldValue walks a value in parse order and instantiates each class
// application whose arguments are concrete at that point. The walk is
// structural, not one eval of the whole expression: a sibling that references
// a template argument must not hide a fully concrete `ImmArgPrinter<"...">`
// next to it, which is exactly what TGParser::ParseValue does (each argument
// is folded on its own while the value list is parsed).
func (l *Lowerer) preFoldValue(v syntax.Value, env *Env, rec *Record) {
	switch t := v.(type) {
	case nil:
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			l.preFoldValue(a.Value, env, rec)
		}
		l.tryInstantiate(t, env, rec)
	case *syntax.BitsValue:
		for _, e := range t.Elems {
			l.preFoldValue(e, env, rec)
		}
	case *syntax.ListValue:
		for _, e := range t.Elems {
			l.preFoldValue(e, env, rec)
		}
	case *syntax.DagValue:
		l.preFoldValue(t.Op, env, rec)
		for _, a := range t.Args {
			l.preFoldValue(a.Value, env, rec)
		}
	case *syntax.VarValue:
		l.preFoldValue(t.Lit, env, rec)
	case *syntax.FieldValue:
		l.preFoldValue(t.Base, env, rec)
	case *syntax.SliceValue:
		l.preFoldValue(t.Base, env, rec)
		for _, e := range t.Index {
			l.preFoldValue(e.Lo, env, rec)
			l.preFoldValue(e.Hi, env, rec)
		}
	case *syntax.BitRangeValue:
		l.preFoldValue(t.Base, env, rec)
	case *syntax.PasteValue:
		l.preFoldValue(t.LHS, env, rec)
		l.preFoldValue(t.RHS, env, rec)
	case *syntax.BangValue:
		for _, a := range t.Args {
			l.preFoldValue(a, env, rec)
		}
		for _, c := range t.Cases {
			l.preFoldValue(c.Key, env, rec)
			l.preFoldValue(c.Value, env, rec)
		}
		// A comprehension over a concrete list folds at parse time
		// (`!foreach(v, [1, 2], X<v>)`), instantiating with each element.
		_, _ = l.k.evalSyntax(t, env, rec)
	}
}

// tryInstantiate is VarDefInit::Fold: instantiate only when every argument is
// already concrete. A VarInit/VarBitInit argument leaves the application as a
// value that resolves later, with no anonymous name yet.
func (l *Lowerer) tryInstantiate(ca *syntax.ClassApplyValue, env *Env, rec *Record) {
	if l.k.Class(ca.Class) == nil {
		return
	}
	for _, a := range ca.Args {
		if a.Value == nil {
			return
		}
		val, err := l.k.evalSyntax(a.Value, env, rec)
		if err != nil || hasReference(val) {
			return
		}
	}
	v, err := l.k.evalSyntax(ca, env, rec)
	if err != nil {
		return
	}
	if dv, ok := v.(*DefVal); ok && !l.namesOnly && !l.preResolve {
		// VarDefInit::Fold returns the DefInit; later prints and evaluations
		// see the anonymous record, not `Class<args>`.
		ca.FoldName = dv.Rec.Name
	}
}

// hasReference reports whether an evaluated value still contains a VarInit or
// VarBitInit, i.e. !Init::isConcrete is false for VarDefInit::Fold purposes.
func hasReference(v Value) bool {
	switch x := v.(type) {
	case *Unresolved, *BitRef:
		return true
	case *Bits:
		for _, b := range x.B {
			if hasReference(b) {
				return true
			}
		}
	case *List:
		for _, e := range x.Elems {
			if hasReference(e) {
				return true
			}
		}
	case *Dag:
		if hasReference(x.Op) {
			return true
		}
		for _, a := range x.Args {
			if hasReference(a.Value) {
				return true
			}
		}
	}
	return false
}

// hasClassApply reports whether the tree contains a `Class<...>` value node.
// Walking is cheap and lets preFold skip the evaluator for the common case.
func hasClassApply(v syntax.Value) bool {
	switch t := v.(type) {
	case nil:
		return false
	case *syntax.ClassApplyValue:
		return true
	case *syntax.BitsValue:
		for _, e := range t.Elems {
			if hasClassApply(e) {
				return true
			}
		}
	case *syntax.ListValue:
		for _, e := range t.Elems {
			if hasClassApply(e) {
				return true
			}
		}
	case *syntax.DagValue:
		if hasClassApply(t.Op) {
			return true
		}
		for _, a := range t.Args {
			if hasClassApply(a.Value) {
				return true
			}
		}
	case *syntax.VarValue:
		return hasClassApply(t.Lit)
	case *syntax.FieldValue:
		return hasClassApply(t.Base) || hasClassApply(t.Lit)
	case *syntax.SliceValue:
		if hasClassApply(t.Base) {
			return true
		}
		for _, e := range t.Index {
			if hasClassApply(e.Lo) || hasClassApply(e.Hi) {
				return true
			}
		}
	case *syntax.BitRangeValue:
		return hasClassApply(t.Base)
	case *syntax.PasteValue:
		return hasClassApply(t.LHS) || hasClassApply(t.RHS)
	case *syntax.BangValue:
		for _, a := range t.Args {
			if hasClassApply(a) {
				return true
			}
		}
		for _, c := range t.Cases {
			if hasClassApply(c.Key) || hasClassApply(c.Value) {
				return true
			}
		}
	}
	return false
}

// implicitCast reproduces the conversion TGParser::SetValue performs before
// storing a value: TGParser.cpp:326 calls RecordVal::setValue, which calls
// V->getCastTo(FieldTy) (Record.cpp:2767). TypedInit::getCastTo
// (Record.cpp:2262) keeps identical types, folds what convertInitializerTo can
// handle (bit -> bits<1> included), and otherwise stores `!cast<Ty>(V)` as an
// UnOpInit. That node is what --print-records shows inside a class body, so
// the cast has to exist in our expression tree too.
func (k *Keeper) implicitCast(want *Ty, v syntax.Value, self *Record) syntax.Value {
	if want == nil || v == nil || want.Kind == AnyTy {
		return v
	}
	// TGParser resolves a name while parsing, so a reference to a defvar (or
	// any other binding it inlined) is that Init when the value is stored:
	// getCastTo then converts the literal itself (TGParser.cpp:1180-1216).
	if lit := inlinedLit(v, 0); lit != nil && lit != v {
		return k.implicitCast(want, lit, self)
	}
	st := v.Type()
	if st.Kind == syntax.InvalidType || st.Kind == syntax.UnsetType {
		return v
	}
	got := k.staticTy(st, self)
	if got == nil || got.Kind == AnyTy || sameRecTy(got, want) {
		return v
	}
	// TypedInit::getCastTo returns `this` when the value's type already is a
	// subtype of the destination (Record.cpp:2262), so a subclass instance
	// assigned to a parent-typed field keeps its Init with no cast node.
	if got.IsA(want) {
		return v
	}
	// TypedInit::convertInitializerTo covers bit -> bits<1> and bits<1> -> bit
	// without a cast (Record.cpp:447-451 returns the single bit).
	if got.Kind == BitTy && want.Kind == BitsTy && want.Bits == 1 {
		return v
	}
	if got.Kind == BitsTy && got.Bits == 1 && want.Kind == BitTy {
		return v
	}
	if !got.ConvertibleTo(want) || concreteInit(v) {
		return v
	}
	return &syntax.BangValue{
		ValueBase: syntax.ValueBase{Base: syntax.Base{At: v.Span()}, Ty: syntaxTypeOf(want)},
		Op:        syntax.XCast,
		OpSpan:    v.Span(),
		TypeArg:   syntaxTypeOf(want),
		Args:      []syntax.Value{v},
	}
}

// inlinedLit returns the Init TGParser would have substituted for a name at
// parse time. Only bindings the parser resolved eagerly carry one: a defvar
// (GlobalRef), a substituted template argument, or a lexical binding. A
// class's own template argument stays a VarInit and has no literal.
func inlinedLit(v syntax.Value, depth int) syntax.Value {
	if depth > 16 {
		return nil
	}
	x, ok := v.(*syntax.VarValue)
	if !ok || x.Lit == nil {
		return nil
	}
	if !x.Subst && !x.Lexical && !x.GlobalRef {
		return nil
	}
	if next := inlinedLit(x.Lit, depth+1); next != nil {
		return next
	}
	return x.Lit
}

// concreteInit reports whether a parsed value is already a concrete Init, the
// state in which getCastTo folds the conversion to a literal instead of
// building a cast operator.
func concreteInit(v syntax.Value) bool {
	switch x := v.(type) {
	case *syntax.IntValue, *syntax.StringValue:
		return true
	case *syntax.UnsetValue:
		// UnsetInit::isConcrete() is false, so FieldInit::Fold keeps the
		// reference (`B:impl.value`) instead of folding `?`.
		return false
	case *syntax.BitsValue:
		for _, e := range x.Elems {
			if !concreteInit(e) {
				return false
			}
		}
		return true
	case *syntax.ListValue:
		for _, e := range x.Elems {
			if !concreteInit(e) {
				return false
			}
		}
		return true
	case *syntax.DagValue:
		if x.Op != nil && !concreteInit(x.Op) {
			return false
		}
		for _, a := range x.Args {
			if a.Value != nil && !concreteInit(a.Value) {
				return false
			}
		}
		return true
	}
	return false
}

// preResolveFields is the resolve() half of a foreach iteration: resolve()
// clones a loop entry and runs Record::resolveReferences with the iterator
// substitution before addDefOne renames and finalises it
// (TGParser.cpp:525-552). VarDefInit::resolveReferences instantiates an
// anonymous class application only when that resolve changed one of its
// arguments (Record.cpp:2444-2449), and instantiation is what hands out the
// anonymous_N (Record.cpp:2448). The folded values are not kept: addDefOne
// resolves the fields again with the final record name, which is what a
// self-reference has to see (TGParser.cpp:592).
func (l *Lowerer) preResolveFields(rec *Record) {
	prev := l.preResolve
	l.preResolve = true
	defer func() { l.preResolve = prev }()
	for _, v := range rec.Vals {
		if v.state != unresolved || v.Expr == nil {
			continue
		}
		l.preResolveExpr(v.Expr, rec)
	}
}

func (l *Lowerer) preResolveExpr(e Expr, rec *Record) {
	switch x := e.(type) {
	case *AstExpr:
		l.preResolveValue(x.V, x.Env, rec)
	case *AppendExpr:
		l.preResolveExpr(x.Base, rec)
		l.preResolveExpr(x.Add, rec)
	case *BitsExpr:
		l.preResolveExpr(x.Base, rec)
		l.preResolveExpr(x.Add, rec)
	}
}

// preResolveValue walks the value the way *Init::resolveReferences descends
// it: every operand is resolved, but a selector that resolves short-circuits
// the alternatives behind it.
func (l *Lowerer) preResolveValue(v syntax.Value, env *Env, rec *Record) {
	switch t := v.(type) {
	case nil:
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			l.preResolveValue(a.Value, env, rec)
		}
		if l.loopChanged(t, env) {
			l.tryInstantiate(t, env, rec)
		}
	case *syntax.BitsValue:
		for _, e := range t.Elems {
			l.preResolveValue(e, env, rec)
		}
	case *syntax.ListValue:
		for _, e := range t.Elems {
			l.preResolveValue(e, env, rec)
		}
	case *syntax.DagValue:
		l.preResolveValue(t.Op, env, rec)
		for _, a := range t.Args {
			l.preResolveValue(a.Value, env, rec)
		}
	case *syntax.VarValue:
		l.preResolveValue(t.Lit, env, rec)
	case *syntax.FieldValue:
		l.preResolveValue(t.Base, env, rec)
	case *syntax.SliceValue:
		l.preResolveValue(t.Base, env, rec)
		for _, e := range t.Index {
			l.preResolveValue(e.Lo, env, rec)
			l.preResolveValue(e.Hi, env, rec)
		}
	case *syntax.BitRangeValue:
		l.preResolveValue(t.Base, env, rec)
	case *syntax.PasteValue:
		l.preResolveValue(t.LHS, env, rec)
		l.preResolveValue(t.RHS, env, rec)
	case *syntax.BangValue:
		l.preResolveBang(t, env, rec)
	}
}

func (l *Lowerer) preResolveBang(b *syntax.BangValue, env *Env, rec *Record) {
	switch b.Op {
	case syntax.XIf:
		l.preResolveValue(b.Args[0], env, rec)
		if n, ok := l.constInt(b.Args[0], env, rec); ok && len(b.Args) == 3 {
			// TernOpInit::resolveReferences short-circuits once the condition
			// resolves (Record.cpp:1949-1958); a dead arm never instantiates.
			if n != 0 {
				l.preResolveValue(b.Args[1], env, rec)
			} else {
				l.preResolveValue(b.Args[2], env, rec)
			}
			return
		}
		for _, a := range b.Args[1:] {
			l.preResolveValue(a, env, rec)
		}
	case syntax.XCond:
		// CondOpInit::resolveReferences resolves the value before it looks at
		// the condition, and stops after the first true one
		// (Record.cpp:2548-2571).
		for _, c := range b.Cases {
			l.preResolveValue(c.Key, env, rec)
			l.preResolveValue(c.Value, env, rec)
			if n, ok := l.constInt(c.Key, env, rec); ok && n != 0 {
				return
			}
		}
	case syntax.XForEach, syntax.XFilter, syntax.XSort:
		if len(b.Args) < 2 {
			for _, a := range b.Args {
				l.preResolveValue(a, env, rec)
			}
			return
		}
		l.preResolveValue(b.Args[0], env, rec)
		lis, err := l.k.evalSyntax(b.Args[0], env, rec)
		ls, ok := lis.(*List)
		if err != nil || !ok {
			l.preResolveValue(b.Args[1], env, rec)
			return
		}
		name := "x"
		if len(b.VarNames) > 0 && b.VarNames[0] != "" {
			name = b.VarNames[0]
		}
		for _, e := range ls.Elems {
			l.preResolveValue(b.Args[1], env.WithBinding(name, Binding{Val: e}), rec)
		}
	default:
		for _, a := range b.Args {
			l.preResolveValue(a, env, rec)
		}
		for _, c := range b.Cases {
			l.preResolveValue(c.Key, env, rec)
			l.preResolveValue(c.Value, env, rec)
		}
	}
}

// constInt folds one selector. A selector that still depends on something the
// loop substitution cannot supply stays unresolved, and LLVM then walks every
// alternative (TernOpInit::resolveReferences, Record.cpp:1972-1975).
func (l *Lowerer) constInt(v syntax.Value, env *Env, rec *Record) (int64, bool) {
	val, err := l.k.evalSyntax(v, env, rec)
	if err != nil {
		return 0, false
	}
	n, err := l.k.asInt(val)
	if err != nil {
		return 0, false
	}
	return n, true
}

// loopChanged reports whether resolving this application in the current
// environment changes its arguments. LLVM only instantiates a VarDefInit when
// the resolve changed them (Record.cpp:2444-2449); an application whose
// arguments were already concrete when the enclosing object was parsed was
// folded then, and must not take a second anonymous number now. At this point
// the loop substitution is the only resolver the parser did not already have.
func (l *Lowerer) loopChanged(ca *syntax.ClassApplyValue, env *Env) bool {
	for _, a := range ca.Args {
		if a.Value != nil && !l.parseTimeKnown(a.Value, env) {
			return true
		}
	}
	return false
}
