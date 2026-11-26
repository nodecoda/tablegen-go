package record

import (
	"sort"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// Expr is an unevaluated initializer. Class fields stay as expressions until a
// concrete def inherits them, which is what lets one class body be reused with
// different template arguments without a closure or a second AST.
type Expr interface{ isExpr() }

// AstExpr is a parser expression plus the lexical frame that binds template
// arguments visible at that point.
type AstExpr struct {
	V   syntax.Value
	Env *Env
}

// AppendExpr is `let append|prepend field = value`.
type AppendExpr struct {
	Base    Expr
	Add     Expr
	Prepend bool
	Kind    TyKind
}

// BitsExpr is `let field{hi...lo} = value`.
type BitsExpr struct {
	Base  Expr
	Add   Expr
	Bits  []int32 // ascending, as C++ stores them after std::reverse
	Width int     // declared width of the field being patched
}

// BindExpr evaluates a captured variable binding. AddParentClass exposes a
// class's template arguments to every expression in its body through a flat
// MapResolver, which also reaches references that were captured inside another
// binding's environment; a plain Env frame cannot. The pre-fold stand-in
// record uses this to reproduce that behaviour.
type BindExpr struct {
	B   Binding
	Env *Env
	Rec *Record
}

func (*BindExpr) isExpr()   {}
func (*AstExpr) isExpr()    {}
func (*AppendExpr) isExpr() {}
func (*BitsExpr) isExpr()   {}

// Binding is one name in an environment. Dynamic bindings (class template-arg
// defaults) are evaluated in the environment of the concrete instantiation;
// lexical bindings (caller-provided arguments, loop variables) are evaluated
// in the environment where they were written.
type Binding struct {
	V       syntax.Value
	Val     Value // already-evaluated binding (loop variables)
	Env     *Env
	Rec     *Record // lexical record for caller-provided expressions
	Owner   *Record // class the expression was written in (for qualification)
	Sep     string
	Dynamic bool
	Ty      *Ty    // declared type of a template argument, coerced on use
	Self    string // qualified name to hide while evaluating this default
	// ParseTime marks bindings LLVM would already know while parsing (loop
	// variables substituted by a parse-time-unrolled foreach). Anything that
	// depends on a multiclass template argument is not parse-time known.
	ParseTime bool
	// Mapped marks a complete default that TGParser::resolveArguments installed
	// for an omitted template argument (MapResolver). Unlike a class's own
	// default frame, it replaces the VarInit in a child's copied fields.
	Mapped bool
}

// Env is a stack of template-argument frames. Values are parser expressions,
// not evaluated Values: a frame installed on a class field must see the
// concrete arguments of whichever def finally inherits it. Frames are searched
// from the innermost (last) outward.
type Env struct {
	frames []Frame
	// kv is the single binding WithBinding pushed, kept out of frames: it is
	// called once per Class* or loop iteration, and a one-entry map plus a
	// copied frame slice was the largest allocation in --dump-json. It is a
	// pointer so that the Env copies made for every expression stay small;
	// kvBinding is immutable once built. Lookup consults it first, because
	// WithBinding pushes the newest binding on top.
	kv       *kvBinding
	owner    *Record // class or multiclass whose template arguments live here
	ownerSep string  // ":" for classes, "::" for multiclasses
	hidden   map[string]bool
	// keeper lets a printer resolve `Def.Field` against the records the
	// parser already built. TGParser folds such a reference while parsing
	// because the def's field is already concrete then (FieldInit::Fold,
	// Record.cpp:2498); a Go printer needs the record graph to do the same.
	keeper *Keeper
	// implicitTo overrides the record name used for the implicit NAME when a
	// class body is printed. TGParser::AddParentClass rewrites a parent's
	// VarInit("Parent:NAME") to the inheriting record via MapResolver
	// (TGParser.cpp:382), so the copied field prints `Child:NAME`.
	implicitTo string
	// printing is the record whose Init tree the printer is rendering. A
	// class body is printed unresolved, so a class application inside it must
	// not fold using that class's own argument bindings (classApplyName).
	printing *Record
}

// kvBinding is one binding stored outside Env.frames. Successive WithBinding
// calls chain instead of folding each older binding into a one-entry map: a
// map with one Binding costs a whole bucket, which showed up as ~500MB of
// --dump-json allocation. A chain keeps the newest binding first (Lookup walks
// it before frames) and is immutable, so Env copies share it.
type kvBinding struct {
	name string
	b    Binding
	next *kvBinding
}

// fold builds one frame holding the whole chain, oldest first, so a later
// entry shadows an earlier one inside the frame exactly as the chain does.
func (kv *kvBinding) fold() []Frame {
	if kv == nil {
		return nil
	}
	var m map[string]Binding
	// Walk newest first and keep the first hit: that is the binding Lookup
	// would return from the chain.
	for n := kv; n != nil; n = n.next {
		if _, ok := m[n.name]; ok {
			continue
		}
		if m == nil {
			m = make(map[string]Binding, 2)
		}
		m[n.name] = n.b
	}
	return []Frame{frameFromMap(m)}
}

// Frame is one immutable environment frame. Up to smallFrame names live in a
// plain slice: Env.WithBinding and class application made maps holding one or
// two Bindings the dominant allocation, and a Go map with one entry costs a
// whole bucket (~1KB here). Larger frames index the same entries with a map.
type Frame struct {
	entries []frameEntry
	m       map[string]Binding
}

type frameEntry struct {
	name string
	b    Binding
}

// smallFrame is the cutoff where a map beats a linear scan. 82% of the class
// applications in dag-isel-subregs.td have one or two template arguments.
const smallFrame = 8

// frameFromMap copies a map-backed frame. It splits at smallFrame so a caller
// that built a map for convenience (class bodies, !foldl) still gets the
// compact form.
func frameFromMap(vars map[string]Binding) Frame {
	if len(vars) == 0 {
		return Frame{}
	}
	if len(vars) > smallFrame {
		return Frame{m: vars}
	}
	e := make([]frameEntry, 0, len(vars))
	for k, v := range vars {
		e = append(e, frameEntry{name: k, b: v})
	}
	return Frame{entries: e}
}

// get looks a name up in the frame. A frame may be nil (rebind passes nil when
// a class has no defaults).
func (f *Frame) get(name string) (Binding, bool) {
	if f == nil {
		return Binding{}, false
	}
	if f.m != nil {
		b, ok := f.m[name]
		return b, ok
	}
	for i := range f.entries {
		if f.entries[i].name == name {
			return f.entries[i].b, true
		}
	}
	return Binding{}, false
}

// set adds or replaces a name while a frame is being built. Once a frame is
// installed in an Env it is never written again.
func (f *Frame) set(name string, b Binding) {
	if f.m != nil {
		f.m[name] = b
		return
	}
	for i := range f.entries {
		if f.entries[i].name == name {
			f.entries[i].b = b
			return
		}
	}
	f.entries = append(f.entries, frameEntry{name: name, b: b})
	if len(f.entries) > smallFrame {
		m := make(map[string]Binding, len(f.entries))
		for _, e := range f.entries {
			m[e.name] = e.b
		}
		f.m, f.entries = m, nil
	}
}

// each visits every name once; order is unspecified for larger frames.
func (f *Frame) each(fn func(name string, b Binding)) {
	if f == nil {
		return
	}
	if f.m != nil {
		for k, v := range f.m {
			fn(k, v)
		}
		return
	}
	for _, e := range f.entries {
		fn(e.name, e.b)
	}
}

func (f *Frame) len() int {
	if f == nil {
		return 0
	}
	if f.m != nil {
		return len(f.m)
	}
	return len(f.entries)
}

// hasVal reports whether the record declares a field with this name.
func (r *Record) hasVal(name string) bool {
	for _, v := range r.Vals {
		if v.Name == name {
			return true
		}
	}
	return false
}

func (e *Env) Lookup(name string) (Binding, bool) {
	if e == nil || e.hidden[name] {
		return Binding{}, false
	}
	for kv := e.kv; kv != nil; kv = kv.next {
		if kv.name == name {
			return kv.b, true
		}
	}
	for i := len(e.frames) - 1; i >= 0; i-- {
		if v, ok := e.frames[i].get(name); ok {
			return v, true
		}
	}
	return Binding{}, false
}

func (e *Env) With(vars map[string]Binding) *Env {
	if len(vars) == 0 {
		return e.WithFrame(nil)
	}
	f := frameFromMap(vars)
	return e.WithFrame(&f)
}

// WithFrame pushes a prepared frame. The frame must not be written afterwards:
// envs are copied by value and share the slice.
func (e *Env) WithFrame(f *Frame) *Env {
	if e == nil {
		if f == nil {
			return &Env{}
		}
		return &Env{frames: []Frame{*f}}
	}
	out := *e
	// Frames are read-only once built (only this method appends, and it always
	// appends to a fresh slice), so a nil frame can share the slice and the
	// hidden set.
	if f == nil {
		return &out
	}
	// The new frame must shadow the pending single bindings, so fold that
	// chain into one frame below it.
	if out.kv != nil {
		kvs := out.kv.fold()
		frames := make([]Frame, 0, len(e.frames)+1+len(kvs))
		frames = append(frames, e.frames...)
		frames = append(frames, kvs...)
		out.frames = append(frames, *f)
		out.kv = nil
		return &out
	}
	frames := make([]Frame, 0, len(e.frames)+1)
	frames = append(frames, e.frames...)
	out.frames = append(frames, *f)
	return &out
}

// WithBinding is With for the single-variable frame a loop body or
// comprehension binding needs.
func (e *Env) WithBinding(name string, b Binding) *Env {
	if e == nil {
		return &Env{kv: &kvBinding{name: name, b: b}}
	}
	out := *e
	out.kv = &kvBinding{name: name, b: b, next: e.kv}
	return &out
}

// StackOn builds the environment a substituted Init tree must be resolved in:
// the lexical environment it was written in, with the frames of the current
// application site on top. LLVM's MapResolver rewrites a parent's field with
// the caller's Init and then keeps resolving inside that Init, so a qualified
// name from an outer application (BBB:vals reaching through BB) must see the
// newer frame. owner stays the lexical one, which is what QualifyName uses.
func (e *Env) StackOn(base *Env) *Env {
	if base == nil {
		return e
	}
	out := &Env{}
	baseFrames := base.kv.fold()
	out.frames = make([]Frame, 0, len(base.frames)+len(e.frames)+len(baseFrames))
	out.frames = append(out.frames, base.frames...)
	out.frames = append(out.frames, baseFrames...)
	out.frames = append(out.frames, e.frames...)
	out.kv = e.kv
	out.owner = base.owner
	out.ownerSep = base.ownerSep
	out.implicitTo = base.implicitTo
	// The render context belongs to the printer's current environment, not to
	// the lexical one the substituted Init was written in.
	out.printing = e.printing
	if out.printing == nil {
		out.printing = base.printing
	}
	// The record graph travels with the bindings: a printer that stacks a
	// resolved binding on top of the current environment must keep it.
	if e.keeper != nil {
		out.keeper = e.keeper
	} else {
		out.keeper = base.keeper
	}
	switch {
	case len(base.hidden) > 0 && len(e.hidden) > 0:
		out.hidden = make(map[string]bool, len(base.hidden)+len(e.hidden))
		for k := range base.hidden {
			out.hidden[k] = true
		}
		for k := range e.hidden {
			out.hidden[k] = true
		}
	case len(base.hidden) > 0:
		out.hidden = base.hidden
	case len(e.hidden) > 0:
		out.hidden = e.hidden
	}
	return out
}

// LexicalOnly drops the frames that bind class and multiclass template
// arguments (their keys are qualified as "Owner:name" / "Owner::name") and
// clears the implicit-name owner. What is left is what TGParser had already
// substituted while parsing: loop variables, defvar inlines, and nothing else.
// LLVM's parse-time VarDefInit::Fold uses an empty resolver, so a template
// argument or a record field is still a VarInit there; it must not make a
// `Class<...>` application look concrete and allocate an anonymous name early.
func (e *Env) LexicalOnly() *Env {
	if e == nil {
		return nil
	}
	out := &Env{}
	lexical := func(k string, b Binding) bool {
		return b.Owner == nil && b.Self == "" && !strings.Contains(k, ":")
	}
	for i := range e.frames {
		var keep Frame
		e.frames[i].each(func(k string, b Binding) {
			if lexical(k, b) {
				keep.set(k, b)
			}
		})
		if keep.len() > 0 {
			out.frames = append(out.frames, keep)
		}
	}
	// Rebuild the chain in place order, keeping only lexical bindings.
	var kept []*kvBinding
	for kv := e.kv; kv != nil; kv = kv.next {
		if lexical(kv.name, kv.b) {
			kept = append(kept, kv)
		}
	}
	for i := len(kept) - 1; i >= 0; i-- {
		out.kv = &kvBinding{name: kept[i].name, b: kept[i].b, next: out.kv}
	}
	return out
}

// Qualify prefixes a template-argument name the way TGParser's QualifyName
// does. Without it, a class argument named `vals` would be shadowed by an
// inherited field named `vals` (see ListArgs.td).
func (e *Env) Qualify(name string) (string, bool) {
	if e == nil || e.owner == nil {
		return "", false
	}
	sep := e.ownerSep
	if sep == "" {
		sep = ":"
	}
	return e.owner.Name + sep + name, true
}

// rebindAssert prepends a template-argument frame to a deferred assertion.
func rebindAssert(a Assertion, vars *Frame) Assertion {
	return rebindAssertFrames(a, nil, vars)
}

func rebindAssertFrames(a Assertion, defaults, vars *Frame) Assertion {
	env := a.Env
	if defaults.len() > 0 {
		env = env.WithFrame(defaults)
	}
	return Assertion{Cond: a.Cond, Message: a.Message, Env: env.WithFrame(vars)}
}

func rebindDump(d Dump, vars *Frame) Dump {
	return rebindDumpFrames(d, nil, vars)
}

func rebindDumpFrames(d Dump, defaults, vars *Frame) Dump {
	env := d.Env
	if defaults.len() > 0 {
		env = env.WithFrame(defaults)
	}
	return Dump{Loc: d.Loc, Msg: d.Msg, Env: env.WithFrame(vars)}
}

// rebind prepends a template-argument frame to every expression in e. It is
// how a class body is instantiated for a concrete def: the class's own
// defaults stay in the chain, the caller's arguments win.
func rebind(e Expr, vars *Frame) Expr {
	return rebindFrames(e, nil, vars)
}

// rebindFrames is rebind with an extra frame below the caller's arguments.
// Instantiation needs it for the class's defaulted template arguments: a field
// inherited from a parent class is resolved against the parent's expression
// environment, which never saw the child's own template frame. Without the
// defaults frame a reference like Versioned's `min` has no binding at all and
// only the parser's inlined literal keeps it alive (which the strict pre-fold
// path rejects, allocating an anonymous name for an instance that then fails).
func rebindFrames(e Expr, defaults, vars *Frame) Expr {
	if defaults.len() == 0 && vars.len() == 0 {
		return e
	}
	with := func(env *Env) *Env {
		if defaults.len() > 0 {
			env = env.WithFrame(defaults)
		}
		return env.WithFrame(vars)
	}
	switch x := e.(type) {
	case *AstExpr:
		return &AstExpr{V: x.V, Env: with(x.Env)}
	case *AppendExpr:
		return &AppendExpr{Base: rebindFrames(x.Base, defaults, vars), Add: rebindFrames(x.Add, defaults, vars), Prepend: x.Prepend, Kind: x.Kind}
	case *BitsExpr:
		return &BitsExpr{Base: rebindFrames(x.Base, defaults, vars), Add: rebindFrames(x.Add, defaults, vars), Bits: x.Bits, Width: x.Width}
	default:
		return e
	}
}

type valState uint8

const (
	unresolved valState = iota
	resolving
	resolved
	failed
)

// RecordVal is one field (or class template argument) of a record.
type RecordVal struct {
	Name string
	// Qual is the qualified name class template arguments are stored under,
	// "Class:Arg". bindArgs rebuilds it for every instantiation otherwise.
	Qual        string
	Ty          *Ty
	Nonconcrete bool
	Expr        Expr
	Loc         diag.Pos

	state    valState
	val      Value
	err      error
	selfBase Value // value of the old expression while a BitsExpr is resolving
}

// Record is a class, a def, or an anonymous instance of a class. Classes keep
// unresolved field expressions; defs are resolved when they are added.
type Record struct {
	Name        string
	IsClass     bool
	IsAnonymous bool
	// VarDefInit marks the anonymous record a class application such as
	// `C<...>` creates when its value is needed (VarDefInit::instantiate).
	// Unlike `def : C` it is not an object of the block it appears in, so a
	// defset must not collect it (TGParser.cpp:4005-4030 adds only the defs
	// ParseObjectList created).
	VarDefInit bool
	Locs       []diag.Pos

	TArgs []*RecordVal
	Vals  []*RecordVal
	Super []*Record // direct superclasses, in source order

	// Env binds this class's template arguments to their defaults.
	Env *Env

	// Asserts are class-body assertions. LLVM checks them when a concrete
	// record inherits the class, not when the class is defined, so they are
	// stored with the environment that binds the class arguments.
	Asserts []Assertion

	// Dumps are class- or def-body `dump` statements. Record::emitRecordDumps
	// resolves the message with a final RecordResolver in addDefOne, so they
	// are stored with the environment they were written in and emitted when
	// the def is complete. AddParentClass appends the parent's dumps
	// (TGParser.cpp:363), so inherited dumps run once per concrete def.
	Dumps []Dump

	// AssertsChecked records that Record::checkRecordAssertions already ran
	// for this record. An anonymous instance is often created during a
	// parse-time fold, where there is nothing to check yet, and is then
	// reached again by the concrete evaluation; LLVM checks the assertions
	// exactly once, right after RecordKeeper::addDef (Record.cpp:2408-2419).
	AssertsChecked bool

	keeper *Keeper

	// PendingAdd marks an anonymous instance that was created while a
	// multiclass body was parsed (TGParser::ParseDefm runs there, and the
	// VarDefInit it resolves allocates the name) but has not been added to the
	// keeper yet. LLVM keeps such a record in MultiClass::Entries and adds it
	// only when the enclosing defm is instantiated (resolve() copies it and
	// addDefOne registers the copy under the name it already has), so the name
	// is consumed at parse time and the def appears later.
	PendingAdd bool

	// tyCache memoizes Type(). Super is only ever appended to while a record
	// is being built, so a length check is enough to notice an invalidation.
	tyCache *Ty
	tyLen   int
	tyValid bool
}

// Assertion is a deferred `assert cond, msg;` from a class body.
type Assertion struct {
	Cond    syntax.Value
	Message syntax.Value
	Env     *Env
}

// Dump is a deferred `dump msg;` from a class or def body.
type Dump struct {
	Loc diag.Pos
	Msg syntax.Value
	Env *Env
}

func (r *Record) Val(name string) *RecordVal {
	for _, v := range r.Vals {
		if v.Name == name {
			return v
		}
	}
	return nil
}

func (r *Record) addVal(v *RecordVal) {
	if old := r.Val(v.Name); old != nil {
		old.Ty = v.Ty
		old.Expr = v.Expr
		old.Nonconcrete = v.Nonconcrete
		old.Loc = v.Loc
		old.state, old.val, old.err = unresolved, nil, nil
		return
	}
	v.state, v.val, v.err = unresolved, nil, nil
	r.Vals = append(r.Vals, v)
}

// Field resolves one field on demand. Recursion through fields is legal in
// TableGen (`int a = b; int b = 1;`), so resolution is lazy and cycle-checked.
func (r *Record) Field(name string) (Value, error) {
	v := r.Val(name)
	if v == nil {
		return nil, &EvalError{Msg: "Record '" + r.Name + "' does not have a field named '" + name + "'"}
	}
	return r.evalVal(v)
}

func (r *Record) evalVal(v *RecordVal) (Value, error) {
	switch v.state {
	case resolved:
		return v.val, nil
	case failed:
		return nil, v.err
	case resolving:
		if v.selfBase != nil {
			// `let x{1} = ... x{0} ...` reads the previous value of x.
			return v.selfBase, nil
		}
		return nil, &EvalError{Msg: "Recursion / self-assignment forbidden: '" + v.Name + "'", Span: v.locSpan()}
	}
	v.state = resolving
	if bx, ok := v.Expr.(*BitsExpr); ok {
		if base, err := r.keeper.eval(bx.Base, r, v.Ty); err == nil {
			v.selfBase = base
			defer func() { v.selfBase = nil }()
		}
	}
	val, err := r.keeper.eval(v.Expr, r, v.Ty)
	if err == nil {
		val, err = r.keeper.coerce(val, v.Ty)
	}
	if err != nil {
		if ee, ok := err.(*EvalError); ok && ee.AtRecord && !r.IsClass && len(r.Locs) > 0 {
			ee.Span = diag.Span{Start: r.Locs[0], End: r.Locs[0]}
			ee.AtRecord = false
		}
		v.state, v.err = failed, err
		return nil, err
	}
	v.state, v.val = resolved, val
	return val, nil
}

func (v *RecordVal) locSpan() diag.Span { return diag.Span{Start: v.Loc, End: v.Loc} }

// SuperClasses returns all superclasses in post-order, with duplicates kept.
// LLVM's JSON backend exposes exactly this list, duplicates included.
// Type is Record::getType(): the canonical set of direct superclasses, sorted
// by name (RecordRecTy::get sorts). A def without superclasses has the "any
// record" type. Classes themselves are referenced by name, not by this set.
func (r *Record) Type() *Ty {
	if r.tyValid && r.tyLen == len(r.Super) {
		return r.tyCache
	}
	t := r.computeType()
	r.tyCache, r.tyLen, r.tyValid = t, len(r.Super), true
	return t
}

func (r *Record) computeType() *Ty {
	if r.IsClass {
		return &Ty{Kind: RecordTy, Recs: []*Record{r}, Names: []string{r.Name}}
	}
	if len(r.Super) == 0 {
		return anyRecTy
	}
	recs := append([]*Record(nil), r.Super...)
	sort.Slice(recs, func(i, j int) bool { return recs[i].Name < recs[j].Name })
	out := recs[:0]
	for _, sc := range recs {
		if len(out) == 0 || out[len(out)-1].Name != sc.Name {
			out = append(out, sc)
		}
	}
	names := make([]string, len(out))
	for i, sc := range out {
		names[i] = sc.Name
	}
	return &Ty{Kind: RecordTy, Recs: out, Names: names}
}

func (r *Record) SuperClasses() []*Record {
	var out []*Record
	var walk func(*Record)
	walk = func(x *Record) {
		for _, sc := range x.Super {
			walk(sc)
			out = append(out, sc)
		}
	}
	walk(r)
	return out
}

// DumpText renders a record the way llvm-tblgen's operator<< does, which is
// what !repr(record) returns: name, template args, superclass comment, and one
// line per value (non-concrete fields first).
func (r *Record) DumpText() (string, error) {
	var b strings.Builder
	b.WriteString(r.Name)
	if len(r.TArgs) > 0 {
		b.WriteString("<")
		for i, ta := range r.TArgs {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(r.TemplateArgText(ta))
		}
		b.WriteString(">")
	}
	b.WriteString(" {")
	if scs := r.SuperClasses(); len(scs) > 0 {
		b.WriteString("\t//")
		for _, sc := range scs {
			b.WriteString(" ")
			b.WriteString(sc.Name)
		}
	}
	b.WriteString("\n")
	writeVal := func(v *RecordVal) error {
		t, err := r.ValueText(v)
		if err != nil {
			return err
		}
		b.WriteString("  ")
		b.WriteString(t)
		b.WriteString(";\n")
		return nil
	}
	for _, v := range r.Vals {
		if v.Nonconcrete {
			if err := writeVal(v); err != nil {
				return "", err
			}
		}
	}
	for _, v := range r.Vals {
		if !v.Nonconcrete {
			if err := writeVal(v); err != nil {
				return "", err
			}
		}
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// printTypeText is RecordVal::getPrintType (Record.cpp:2746): a value of the
// string type prints as `code` when its current value is a code-formatted
// StringInit. Classes have no resolved value yet, so the parsed initialiser
// decides; defs use the evaluated value, which `val` may already carry.
func (r *Record) printTypeText(v *RecordVal, val Value) string {
	if v.Ty.Kind != StringTy {
		return v.Ty.String()
	}
	code := false
	if r.IsClass {
		// A class field's Value is the Init stored by setValue, which is the
		// initialiser as printed: only a code literal keeps the code format.
		code = strings.HasPrefix(r.exprText(v), "[{")
	} else {
		if val == nil {
			if evaluated, err := r.evalVal(v); err == nil {
				val = evaluated
			}
		}
		if sv, ok := val.(*Str); ok && sv.Code {
			code = true
		}
	}
	if code {
		return "code"
	}
	return "string"
}

// TemplateArgText renders one template argument the way RecordVal::print
// does: TGParser stores the qualified name, so `class P<int i>` prints
// `int P:i = ?`.
func (r *Record) TemplateArgText(ta *RecordVal) string {
	var b strings.Builder
	b.WriteString(r.printTypeText(ta, nil))
	b.WriteString(" ")
	if r.IsClass {
		b.WriteString(r.Name)
		b.WriteString(":")
	}
	b.WriteString(ta.Name)
	b.WriteString(" = ")
	b.WriteString(r.exprText(ta))
	return b.String()
}

// ValueText renders one field as RecordVal::print(os, false) does: the
// optional `field` marker, the print type, the name, and `= value`, without a
// terminator. Classes print their Init tree; defs print the resolved value.
func (r *Record) ValueText(v *RecordVal) (string, error) {
	var b strings.Builder
	if v.Nonconcrete {
		b.WriteString("field ")
	}
	var val Value
	if !r.IsClass {
		var err error
		val, err = r.evalVal(v)
		if err != nil {
			return "", err
		}
	}
	b.WriteString(r.printTypeText(v, val))
	b.WriteString(" ")
	b.WriteString(v.Name)
	b.WriteString(" = ")
	if r.IsClass {
		// Class bodies are never resolved; the dump shows the Init tree.
		b.WriteString(r.exprText(v))
	} else {
		b.WriteString(val.String())
	}
	return b.String(), nil
}

// HasDirectSuperClass reports whether sc is one of the direct superclasses.
func (r *Record) HasDirectSuperClass(sc *Record) bool {
	for _, s := range r.Super {
		if s == sc {
			return true
		}
	}
	return false
}

func (r *Record) IsSubClassOf(name string) bool {
	for _, sc := range r.Super {
		if sc.Name == name || sc.IsSubClassOf(name) {
			return true
		}
	}
	return false
}

// resolveAll forces every field in insertion order. The order matters: it is
// the order in which anonymous class applications get their names, and the
// JSON backend must see the same anonymous_0, anonymous_1, ... as llvm-tblgen.
func (r *Record) resolveAll() error {
	for _, v := range r.Vals {
		if _, err := r.evalVal(v); err != nil {
			return err
		}
	}
	return nil
}

// Keeper owns the class/def tables and the global variables of one file.
type Keeper struct {
	classes map[string]*Record
	defs    map[string]*Record
	globals map[string]*Global

	anon      int
	anonCache map[string]*Record
	// defOrder keeps defs in creation order; defset lists are built from it.
	defOrder []*Record
	diags    *diag.Collector
	file     string
	depth    int
	// parseFold is set while a value is being evaluated at the point where
	// TGParser would have folded it. A record field is still VarInit there,
	// even though the parser inlines its initializer for type checking.
	parseFold bool
	// readOnly forbids instantiating an anonymous class instance while a
	// printer probes the record graph: output must be reproducible, and
	// VarDefInit::instantiate assigns the next anonymous number.
	readOnly bool
	anonLoc  diag.Pos
	// preInstantiate is set while a multiclass body is parsed the way
	// TGParser::ParseObject does it: class applications that are already
	// concrete get their anonymous name there, but the records stay pending
	// until the enclosing defm is expanded. See Record.PendingAdd.
	preInstantiate bool

	// loopDefs records the defs created while expanding a statement-level
	// foreach, keyed by syntax.ForeachStmt.Seq. The parser uses them to make
	// dynamically named records visible to later statements.
	loopDefs map[int][]LoopDef
	// defmDefs does the same for a statement-level defm, keyed by
	// syntax.DefmStmt.Seq: a multiclass body can name its records from a
	// resolved argument (`def Ext.Suffix`), which the parser cannot know.
	defmDefs map[int][]LoopDef

	// shapeLoc maps a class application's parsed shape to the location of its
	// first parse; resolvedSite maps a resolved argument fingerprint to the
	// location stamped on the anonymous def it instantiates. See sites.go.
	shapeLoc     map[string]diag.Pos
	resolvedSite map[string]diag.Pos
}

// LoopDef is a record created by a statement-level foreach.
type LoopDef struct {
	Name    string
	Parents []string
}

type Global struct {
	Name     string
	Expr     Expr
	state    valState
	val      Value
	err      error
	selfBase Value // value of the old expression while a BitsExpr is resolving
}

func NewKeeper(file string, diags *diag.Collector) *Keeper {
	return &Keeper{
		classes:   map[string]*Record{},
		defs:      map[string]*Record{},
		globals:   map[string]*Global{},
		anonCache: map[string]*Record{},
		loopDefs:  map[int][]LoopDef{},
		defmDefs:  map[int][]LoopDef{},
		diags:     diags,
		file:      file,
	}
}

func (k *Keeper) Class(name string) *Record { return k.classes[name] }
func (k *Keeper) Def(name string) *Record   { return k.defs[name] }

// LoopDefs returns the defs each statement-level foreach created, by Seq.
func (k *Keeper) LoopDefs() map[int][]LoopDef { return k.loopDefs }

// DefmDefs returns the defs each statement-level defm created, by Seq.
func (k *Keeper) DefmDefs() map[int][]LoopDef { return k.defmDefs }

func (k *Keeper) Classes() []*Record { return sortedRecords(k.classes) }
func (k *Keeper) Defs() []*Record    { return sortedRecords(k.defs) }

// Globals returns every top-level defvar, sorted by name: RecordKeeper keeps
// its Globals vector sorted and DetailedRecordsBackend prints it in that order.
func (k *Keeper) Globals() []*Global {
	names := make([]string, 0, len(k.globals))
	for n := range k.globals {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*Global, len(names))
	for i, n := range names {
		out[i] = k.globals[n]
	}
	return out
}

// InputFile is the path the record keeper was built from, which the detailed
// records report prints in its heading.
func (k *Keeper) InputFile() string { return k.file }

func sortedRecords(m map[string]*Record) []*Record {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]*Record, len(names))
	for i, n := range names {
		out[i] = m[n]
	}
	return out
}

func (k *Keeper) newAnonymous(loc diag.Pos) *Record {
	name := "anonymous_" + itoa(k.anon)
	k.anon++
	return &Record{Name: name, IsAnonymous: true, Locs: []diag.Pos{loc}, keeper: k}
}

func (k *Keeper) addDef(r *Record) error {
	if old := k.defs[r.Name]; old != nil {
		return &EvalError{Msg: "def already exists: " + r.Name, Span: diag.Span{Start: r.Locs[0], End: r.Locs[0]}, Note: old.Locs[0], Abort: true}
	}
	k.defs[r.Name] = r
	k.defOrder = append(k.defOrder, r)
	return nil
}

// Global resolves a top-level defvar on demand.
func (k *Keeper) Global(name string) (Value, bool, error) {
	g := k.globals[name]
	if g == nil {
		return nil, false, nil
	}
	switch g.state {
	case resolved:
		return g.val, true, nil
	case failed:
		return nil, true, g.err
	case resolving:
		return nil, true, &EvalError{Msg: "Recursion / self-assignment forbidden: '" + name + "'"}
	}
	g.state = resolving
	v, err := k.eval(g.Expr, nil, nil)
	if err != nil {
		g.state, g.err = failed, err
		return nil, true, err
	}
	g.state, g.val = resolved, v
	return v, true, nil
}

func (k *Keeper) defineGlobal(name string, e Expr) {
	k.globals[name] = &Global{Name: name, Expr: e}
}

// EvalError carries the source position of the failing expression so the
// driver can render a normal diagnostic.
type EvalError struct {
	Msg  string
	Span diag.Span
	Note diag.Pos
	// Notes are the additional source locations a diagnostic prints after the
	// message, e.g. "instantiated from multiclass" for every extra loc a defm
	// generated record carries (Error.cpp:34-41).
	Notes []diag.Note
	// AtRecord marks errors llvm-tblgen prints with CurRec->getLoc(): failures
	// that only surface while a record's field is resolved are reported at the
	// record, not at the expression.
	AtRecord bool
	// NoLoc marks PrintFatalError(Twine) diagnostics, which carry no source
	// location at all ("error: invalid regex '...'").
	NoLoc bool
	// Fatal marks PrintFatalError paths. LLVM exits immediately, so no
	// multiclass note chain is appended even when the record came from a
	// defm (Error.cpp:132-145).
	Fatal bool
	// Abort marks errors whose parse-time counterpart propagates out of
	// ParseObjectList (for example AddDef's duplicate-name error), so no
	// further records are built.
	Abort bool
}

func (e *EvalError) Error() string { return e.Msg }

// WithImplicitName returns e with the printable implicit NAME owner replaced.
func (e *Env) WithKeeper(k *Keeper) *Env {
	if e == nil {
		return &Env{keeper: k}
	}
	out := *e
	out.keeper = k
	return &out
}

func (e *Env) WithImplicitName(name string) *Env {
	if e == nil {
		return &Env{implicitTo: name}
	}
	out := *e
	out.implicitTo = name
	return &out
}

// HideOwnArgs hides this record's own template-argument bindings. LLVM never
// resolves a class body: AddParentClass copies its field unresolved into the
// inheriting record and runs resolveReferences there with the caller's
// arguments (TGParser.cpp:346-374). An application such as `Foo<T>` therefore
// keeps printing as written inside Foo's own body even when T has a default;
// only the copy made for a child can fold to a DefInit.
func (e *Env) HideOwnArgs(r *Record) *Env {
	var hidden map[string]bool
	hide := func(k string) {
		if hidden == nil {
			hidden = make(map[string]bool, len(e.hidden)+4)
			for hk, hv := range e.hidden {
				hidden[hk] = hv
			}
		}
		hidden[k] = true
	}
	for i := range e.frames {
		e.frames[i].each(func(k string, b Binding) {
			if b.Owner == r && b.Self != "" {
				hide(k)
			}
		})
	}
	for kv := e.kv; kv != nil; kv = kv.next {
		if kv.b.Owner == r && kv.b.Self != "" {
			hide(kv.name)
		}
	}
	if hidden == nil {
		return e
	}
	out := *e
	out.hidden = hidden
	return &out
}
