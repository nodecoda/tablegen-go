package syntax

import (
	"os"
	"path/filepath"
	"strings"

	"tblgen/internal/diag"
)

// Parser is a single-pass, total recursive-descent parser. It mirrors
// TGParser.cpp's productions and, where the C++ parser computes a type or
// resolves a name *while parsing* (because later decisions depend on it), we do
// the same. Everything else is deferred to the record-graph phase.
//
// Error policy: never panic, never early-return on the first problem, and never
// consume the same token twice. A failed statement is recovered by skipping to
// the next object start at the same brace depth.

// binding is a name visible to expressions: a template arg, a field, a defvar
// or a loop variable. lit is the literal value when one is trivially known; the
// parser uses it only for integer ranges.
type binding struct {
	ty       Type
	lit      Value
	arg      bool   // a class/multiclass template argument
	qual     string // qualified name of a template argument ("C:a", "MC::a")
	isDefvar bool   // defvar is substituted at parse scope, shadowing args
	// lexical marks a binding introduced by a loop or a local variable, which
	// shadows a class/multiclass template argument of the same name. A
	// reference to it must not be qualified with the class name.
	lexical bool
}

type recordInfo struct {
	name    string
	isClass bool
	fields  map[string]binding
	parents []string
	targs   []*Decl
	classes map[string]*recordInfo // shared class table, for parent lookups
}

func (r *recordInfo) field(name string, seen map[string]bool) (binding, bool) {
	if b, ok := r.fields[name]; ok {
		return b, true
	}
	if seen[r.name] {
		return binding{}, false
	}
	seen[r.name] = true
	for _, pn := range r.parents {
		if pr := r.classes[pn]; pr != nil {
			if b, ok := pr.field(name, seen); ok {
				return b, true
			}
		}
	}
	return binding{}, false
}

type globalKind uint8

const (
	gClass globalKind = iota
	gDef
	gDefvar
	gDefset
)

type globalBinding struct {
	kind globalKind
	ty   Type
	rec  *recordInfo
	lit  Value
}

type scope struct {
	vars map[string]binding
	rec  *recordInfo // record whose fields are visible (nil outside a record)
}

// Parser holds all mutable parsing state. One Parser per file; not reusable.
type Parser struct {
	file  string
	src   string
	toks  []Token
	i     int
	last  int
	diags diag.Collector

	globals      map[string]*globalBinding
	typeAliases  map[string]Type
	classes      map[string]*recordInfo
	multiclasses map[string]*MultiClassStmt

	scopes    []*scope
	curMulti  *MultiClassStmt
	loopDepth int

	anonCount   int
	includeDirs []string

	curRec       *recordInfo
	classDepth   int // >0 while parsing a class body (defs there are not global)
	loopSeq      int // next statement-level foreach number
	knownLoops   map[int][]KnownDef
	dynLoopNames bool // a statement-level loop had names we could not materialize
	classArgs    map[string][]*Decl
	classParents map[string][]*ParentRef
	defParents   []defParentsRec

	opts Options
	// usedArgs records, per class/multiclass record, which template arguments
	// were referenced while its body was parsed. LLVM marks them in
	// TGVarScope::getVar and warns from Record::checkUnusedTemplateArgs.
	usedArgs map[*recordInfo]map[string]bool
}

// Options carries the handful of parser switches llvm-tblgen exposes on the
// command line.
type Options struct {
	// NoWarnUnusedTArgs mirrors -no-warn-on-unused-template-args.
	NoWarnUnusedTArgs bool
}

// Parse lexes and parses one file. Lexer errors suppress parsing, matching
// LLVM: the parser's first Lex() returns Error and the parse aborts, so the
// lexer diagnostic is the only one reported.
func Parse(file, src string, macros, includeDirs []string) (*File, []diag.Diagnostic) {
	return ParseWith(file, src, macros, includeDirs, nil, Options{})
}

// ParseWith is Parse with names learned from a previous lowering pass: entries
// are installed into the parser's global table when the corresponding
// statement-level loop ends, which is exactly when LLVM's parser adds them.
func ParseWith(file, src string, macros, includeDirs []string, known map[int][]KnownDef, opts Options) (*File, []diag.Diagnostic) {
	return TokenizeFile(file, src, macros, includeDirs).Parse(file, src, includeDirs, known, opts)
}

// Tokens is one lexed source file. The driver parses a file twice (once to
// learn the names statement-level loops create, once with those names
// installed); tokens are read-only, so the second pass reuses them instead of
// lexing again.
type Tokens struct {
	Toks    []Token
	Diags   []diag.Diagnostic
	Sources []string
	// Includes maps each included file to the site of the include statement
	// that opened it, for the "Included from" diagnostic prefix.
	Includes map[string]diag.Pos
}

// TokenizeFile lexes one file and keeps everything a later Parse needs.
func TokenizeFile(file, src string, macros, includeDirs []string) *Tokens {
	toks, diags, sources, includes := TokenizeWithDeps(file, src, macros, includeDirs)
	return &Tokens{Toks: toks, Diags: diags, Sources: sources, Includes: includes}
}

// Parse runs the parser over t's tokens. Lexer errors suppress parsing,
// matching LLVM: the parser's first Lex() returns Error and the parse aborts,
// so the lexer diagnostic is the only one reported.
func (t *Tokens) Parse(file, src string, includeDirs []string, known map[int][]KnownDef, opts Options) (*File, []diag.Diagnostic) {
	for _, d := range t.Diags {
		if d.Severity == diag.Error {
			return &File{}, t.Diags
		}
	}
	p := &Parser{
		file:         file,
		src:          src,
		toks:         t.Toks,
		globals:      map[string]*globalBinding{},
		typeAliases:  map[string]Type{},
		classes:      map[string]*recordInfo{},
		multiclasses: map[string]*MultiClassStmt{},
		includeDirs:  includeDirs,
		classArgs:    map[string][]*Decl{},
		classParents: map[string][]*ParentRef{},
		knownLoops:   known,
		opts:         opts,
	}
	f := p.parseFile()
	f.DynLoopNames = p.dynLoopNames
	f.Sources = t.Sources
	diags := append(append([]diag.Diagnostic{}, t.Diags...), p.diags.Diags...)
	return f, diags
}

// ---------------------------------------------------------------------------
// Token plumbing

func (p *Parser) cur() Token { return p.toks[p.i] }

func (p *Parser) peekKind(n int) Kind {
	if p.i+n >= len(p.toks) {
		return Eof
	}
	return p.toks[p.i+n].Kind
}

func (p *Parser) at(k Kind) bool { return p.cur().Kind == k }

func (p *Parser) advance() Token {
	t := p.toks[p.i]
	p.last = p.i
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *Parser) eat(k Kind) bool {
	if p.at(k) {
		p.advance()
		return true
	}
	return false
}

// span returns the range from token index start through the last consumed
// token.
func (p *Parser) span(start int) Span {
	end := p.toks[p.last].Span.End
	if p.last < start {
		end = p.toks[start].Span.End
	}
	return Span{Start: p.toks[start].Span.Start, End: end}
}

func (p *Parser) errTok(code, msg string) {
	t := p.cur()
	p.diags.Error(code, msg, t.Span)
}

func (p *Parser) errAt(span Span, code, msg string) {
	p.diags.Error(code, msg, span)
}

func (p *Parser) expect(k Kind, code, msg string) (Token, bool) {
	if p.at(k) {
		return p.advance(), true
	}
	p.errTok(code, msg)
	return p.cur(), false
}

// recoverTop skips to the next object-start keyword at bracket depth zero.
func (p *Parser) recoverTop() {
	depth := 0
	for p.cur().Kind != Eof {
		k := p.cur().Kind
		switch k {
		case LBrace, LParen, LSquare:
			depth++
		case RBrace, RParen, RSquare:
			if depth > 0 {
				depth--
			}
		}
		if depth == 0 && (k.IsObjectStart() || k == Include) {
			return
		}
		p.advance()
	}
}

// recoverBlock consumes tokens through the matching '}' (or EOF). The caller
// has already reported the error.
func (p *Parser) recoverBlock() {
	depth := 0
	for p.cur().Kind != Eof {
		switch p.cur().Kind {
		case LBrace:
			depth++
		case RBrace:
			if depth == 0 {
				p.advance()
				return
			}
			depth--
		}
		p.advance()
	}
}

// ---------------------------------------------------------------------------
// Scopes

func (p *Parser) pushScope(rec *recordInfo) *scope {
	s := &scope{vars: map[string]binding{}, rec: rec}
	p.scopes = append(p.scopes, s)
	return s
}

func (p *Parser) popScope() {
	if n := len(p.scopes); n > 0 {
		p.scopes = p.scopes[:n-1]
	}
}

func (p *Parser) topLevel() bool { return len(p.scopes) == 0 }

// lookupLocal searches scopes and their records, innermost first.
// lookupLocal mirrors TGVarScope::getVar: local variables shadow everything,
// then the record's fields (own first, then inherited), then its template
// arguments, and finally NAME.
func (p *Parser) lookupLocal(name string) (binding, bool) {
	for i := len(p.scopes) - 1; i >= 0; i-- {
		s := p.scopes[i]
		if b, ok := s.vars[name]; ok {
			if b.arg {
				p.markArgUsed(s.rec, name)
			}
			return b, true
		}
		if s.rec != nil {
			if b, ok := s.rec.fields[name]; ok && !b.arg {
				return b, true
			}
			for _, pn := range s.rec.parents {
				if pr := s.rec.classes[pn]; pr != nil {
					if b, ok := pr.field(name, map[string]bool{}); ok && !b.arg {
						return b, true
					}
				}
			}
			if b, ok := s.rec.fields[name]; ok && b.arg {
				p.markArgUsed(s.rec, name)
				return b, true
			}
			if s.rec.isClass && name == "NAME" {
				return binding{ty: StringTy}, true
			}
		}
	}
	return binding{}, false
}

func (p *Parser) markArgUsed(rec *recordInfo, name string) {
	if rec == nil {
		return
	}
	if p.usedArgs == nil {
		p.usedArgs = map[*recordInfo]map[string]bool{}
	}
	m := p.usedArgs[rec]
	if m == nil {
		m = map[string]bool{}
		p.usedArgs[rec] = m
	}
	m[name] = true
}

// warnUnusedArgs mirrors Record::checkUnusedTemplateArgs: every argument of the
// class or multiclass that its body never mentioned is reported once, at the
// argument's own location, after the body has been parsed.
func (p *Parser) warnUnusedArgs(rec *recordInfo, name, sep string, args []*Decl) {
	if p.opts.NoWarnUnusedTArgs {
		return
	}
	used := p.usedArgs[rec]
	for _, a := range args {
		if used[a.Name] {
			continue
		}
		p.diags.Warn("W_PARSE_UNUSED_TARG",
			"unused template argument: "+name+sep+a.Name, a.NameSpan)
	}
}

func (p *Parser) lookupGlobal(name string) (binding, *globalBinding, bool) {
	if g, ok := p.globals[name]; ok {
		return binding{ty: g.ty, lit: g.lit}, g, true
	}
	return binding{}, nil, false
}

func (p *Parser) recordInfoFor(name string) *recordInfo {
	if rec := p.classes[name]; rec != nil {
		return rec
	}
	if g := p.globals[name]; g != nil && g.rec != nil {
		return g.rec
	}
	return nil
}

func (p *Parser) defineLocal(name string, b binding) {
	if n := len(p.scopes); n > 0 {
		p.scopes[n-1].vars[name] = b
	}
}

func (p *Parser) defineGlobal(name string, g *globalBinding) {
	p.globals[name] = g
}

// defineClassGlobal publishes a class in the global table without shadowing a
// def of the same name. TableGen keeps classes and defs in separate namespaces
// for value lookup (a bare name in a value position is a def, never a class),
// and real .td files rely on this: Attributes.td defines `def NoAlias` while
// Intrinsics.td defines `class NoAlias`.
func (p *Parser) defineClassGlobal(name string, rec *recordInfo) {
	if g, ok := p.globals[name]; ok && g.kind == gDef {
		return
	}
	p.globals[name] = &globalBinding{kind: gClass, ty: recordType(name), rec: rec}
}

// ---------------------------------------------------------------------------
// File and objects

func (p *Parser) parseFile() *File {
	f := &File{}
	for p.cur().Kind != Eof {
		if !p.cur().Kind.IsObjectStart() && !p.at(Include) {
			p.errTok("E_PARSE_TOPLEVEL", "Unexpected token at top level")
			p.advance()
			p.recoverTop()
			continue
		}
		obj := p.parseObject()
		if obj == nil {
			p.recoverTop()
			continue
		}
		f.Objects = append(f.Objects, obj)
	}
	return f
}

func (p *Parser) parseObject() Object {
	start := p.i
	switch p.cur().Kind {
	default:
		p.errTok("E_PARSE_OBJECT",
			"Expected assert, class, def, defm, defset, dump, foreach, if, or let")
		return nil
	case AssertTok:
		if o := p.parseAssert(p.curRecOrNil(), start); o != nil {
			return o
		}
		return nil
	case Def:
		return p.parseDef()
	case Defm:
		return p.parseDefm()
	case Deftype:
		return p.parseDeftype()
	case Defvar:
		if o := p.parseDefvar(nil); o != nil {
			return o
		}
		return nil
	case Dump:
		if o := p.parseDump(p.curRecOrNil(), start); o != nil {
			return o
		}
		return nil
	case Foreach:
		return p.parseForeach()
	case If:
		return p.parseIf()
	case Let:
		return p.parseTopLet()
	case Defset:
		if p.curMulti != nil {
			p.errTok("E_PARSE_DEFSET_MC", "defset is not allowed inside multiclass")
			return nil
		}
		return p.parseDefset()
	case Class:
		if p.curMulti != nil {
			p.errTok("E_PARSE_CLASS_MC", "class is not allowed inside multiclass")
			return nil
		}
		if p.loopDepth > 0 {
			p.errTok("E_PARSE_CLASS_LOOP", "class is not allowed inside foreach loop")
			return nil
		}
		return p.parseClass()
	case MultiClass:
		if p.loopDepth > 0 {
			p.errTok("E_PARSE_MC_LOOP", "multiclass is not allowed inside foreach loop")
			return nil
		}
		return p.parseMultiClass()
	case Include:
		return p.parseInclude()
	}
}

func (p *Parser) curRecOrNil() *recordInfo {
	if p.curRec != nil {
		return p.curRec
	}
	for i := len(p.scopes) - 1; i >= 0; i-- {
		if p.scopes[i].rec != nil {
			return p.scopes[i].rec
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// include

func (p *Parser) parseInclude() Object {
	start := p.i
	p.advance() // include
	var file string
	var fileSpan Span
	if p.at(StrVal) {
		t := p.advance()
		file = t.Text
		fileSpan = t.Span
	} else {
		p.errTok("E_PARSE_INCLUDE", "expected filename after include")
		return nil
	}
	if path, ok := p.resolveInclude(file); ok {
		_ = path
		p.errAt(fileSpan, "E_UNSUPPORTED_INCLUDE",
			"include expansion is not implemented until M5")
	} else {
		p.errAt(fileSpan, "E_PARSE_INCLUDE_NOT_FOUND",
			"could not find include file '"+file+"'")
	}
	return &IncludeStmt{Base: Base{At: p.span(start)}, File: file, FileSpan: fileSpan}
}

func (p *Parser) resolveInclude(name string) (string, bool) {
	var dirs []string
	if p.file != "" && p.file != "-" {
		dirs = append(dirs, filepath.Dir(p.file))
	}
	dirs = append(dirs, p.includeDirs...)
	for _, d := range dirs {
		cand := filepath.Join(d, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, true
		}
	}
	if !strings.HasPrefix(name, "/") {
		if st, err := os.Stat(name); err == nil && !st.IsDir() {
			return name, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// class / def / defm / multiclass

func (p *Parser) parseClass() Object {
	start := p.i
	p.advance() // class
	nt, ok := p.expect(Id, "E_PARSE_CLASS_NAME", "expected class name after 'class' keyword")
	if !ok {
		return nil
	}
	name := nt.Text
	if _, exists := p.typeAliases[name]; exists {
		p.errAt(nt.Span, "E_PARSE_CLASS_ALIAS",
			"there is already a defined type alias '"+name+"'")
	}
	if old, exists := p.classes[name]; exists {
		if len(old.fields) > 0 || len(old.parents) > 0 {
			p.errAt(nt.Span, "E_PARSE_CLASS_REDEF", "Class '"+name+"' already defined")
		}
	}
	rec := &recordInfo{name: name, isClass: true, fields: map[string]binding{}, classes: p.classes}
	p.classes[name] = rec
	p.defineClassGlobal(name, rec)

	s := p.pushScope(rec)
	if p.at(Less) {
		args, ok := p.parseTemplateArgList(rec, s)
		if !ok {
			p.popScope()
			p.recoverBlock()
			return nil
		}
		p.classArgs[name] = args
		rec.targs = args
	}
	if p.at(Colon) {
		refs, ok := p.parseParentRefs(rec, false)
		if !ok {
			p.popScope()
			p.recoverBlock()
			return nil
		}
		p.classParents[name] = refs
	}
	p.classDepth++
	items, ok := p.parseBody(rec)
	p.classDepth--
	p.warnUnusedArgs(rec, name, ":", p.classArgs[name])
	p.popScope()
	if !ok {
		p.recoverBlock()
		return nil
	}
	return &ClassStmt{
		Base:         Base{At: p.span(start)},
		Name:         name,
		NameSpan:     nt.Span,
		TemplateArgs: p.classArgs[name],
		Parents:      p.classParents[name],
		Body:         items,
		DeclOnly:     items == nil,
	}
}

func (p *Parser) parseDef() Object {
	start := p.i
	p.advance() // def
	name, ok := p.parseObjectName()
	if !ok {
		return nil
	}
	rec := &recordInfo{name: p.anonName(name), isClass: false, fields: map[string]binding{}, classes: p.classes}
	if name != nil {
		if v, isVar := name.(*VarValue); isVar {
			rec.name = v.Name
		}
		if sv, isStr := name.(*StringValue); isStr {
			rec.name = sv.Text
		}
	}
	p.curRec = rec
	isAnon := name == nil || nameIsUnset(name)
	p.pushScope(rec)
	if p.at(Colon) {
		refs, ok := p.parseParentRefs(rec, false)
		if !ok {
			p.curRec = nil
			return nil
		}
		p.defParents = append(p.defParents, defParentsRec{rec, refs})
	}
	items, ok := p.parseBody(rec)
	p.popScope()
	p.curRec = nil
	if !ok {
		p.recoverBlock()
		return nil
	}
	if !isAnon && p.curMulti == nil && rec.name != "" {
		ty := recordInfoType(rec)
		p.defineGlobal(rec.name, &globalBinding{kind: gDef, ty: ty, rec: rec})
	}
	return &DefStmt{
		Base:    Base{At: p.span(start)},
		Name:    name,
		Parents: p.lastDefParents(rec),
		Body:    items,
		info:    rec,
	}
}

func nameIsUnset(v Value) bool {
	_, ok := v.(*UnsetValue)
	return ok
}

func (p *Parser) anonName(v Value) string {
	if v == nil || nameIsUnset(v) {
		p.anonCount++
		return "anonymous_" + itoa(p.anonCount-1)
	}
	return ""
}

func (p *Parser) parseObjectName() (Value, bool) {
	switch p.cur().Kind {
	case Colon, Semi, LBrace:
		return nil, true
	}
	v := p.parseValue(p.curRecOrNil(), &StringTy, parseNameMode)
	if v == nil {
		return nil, false
	}
	return v, true
}

func (p *Parser) parseDefm() Object {
	start := p.i
	p.advance() // defm
	name, ok := p.parseObjectName()
	if !ok {
		return nil
	}
	if !p.eat(Colon) {
		p.errTok("E_PARSE_DEFM_COLON", "expected ':' after defm identifier")
		return nil
	}
	var parents []*ParentRef
	classStart := -1
	// The first parent must be a multiclass: ParseDefm calls
	// ParseClassReference(isDefm=true) before it ever inspects Records.getClass.
	first, ok := p.parseClassRef(true)
	if !ok {
		return nil
	}
	parents = append(parents, first)
	for p.eat(Comma) {
		if !p.at(Id) {
			p.errTok("E_PARSE_CLASSREF_ID", "expected name for ClassID")
			return nil
		}
		if _, isMC := p.multiclasses[p.cur().Text]; !isMC {
			// Ordinary classes may only appear at the end of the list.
			classStart = len(parents)
			for {
				r, ok := p.parseClassRef(false)
				if !ok {
					return nil
				}
				parents = append(parents, r)
				if !p.eat(Comma) {
					break
				}
			}
			break
		}
		r, ok := p.parseClassRef(true)
		if !ok {
			return nil
		}
		parents = append(parents, r)
	}
	if classStart < 0 {
		classStart = len(parents)
	}
	if !p.eat(Semi) {
		p.errTok("E_PARSE_DEFM_SEMI", "expected ';' at end of defm")
		return nil
	}
	p.registerDefmNames(name, parents[:classStart], nil)
	return &DefmStmt{
		Base:       Base{At: p.span(start)},
		Name:       name,
		Parents:    parents,
		ClassStart: classStart,
	}
}

// registerDefmNames adds the record names a defm *would* create. Expanding the
// defs (template args, let, inheritance) is M3; names are a purely syntactic
// product of the defm name and the multiclass body, and later statements may
// reference them (e.g. `bar_MC` after `defm bar : MC;`).
//
// Names can mention template arguments (`def NAME#lcmod`), so the walk threads
// a small string environment from the defm application down through nested
// defm/multiclass bodies. Literal arguments, NAME, and loop variables bound by
// a statically-known foreach list are tracked; anything else stays
// unregistered and is resolved (or rejected) by the record phase.
func (p *Parser) registerDefmNames(name Value, mcs []*ParentRef, seed map[string]string) {
	prefix, ok := p.materializeName(name, "", seed)
	if !ok {
		return
	}
	p.registerDefmPrefix(prefix, mcs, seed)
}

// registerDefmPrefix is registerDefmNames with the defm name already
// materialized; nested defm bodies reuse the enclosing prefix for NAME.
func (p *Parser) registerDefmPrefix(prefix string, mcs []*ParentRef, seed map[string]string) {
	seen := map[string]bool{}
	var walk func(mc *MultiClassStmt, prefix string, args []*Arg, callerEnv map[string]string)
	walk = func(mc *MultiClassStmt, prefix string, args []*Arg, callerEnv map[string]string) {
		if mc == nil {
			return
		}
		key := prefix + "\x00" + mc.Name
		if seen[key] {
			return
		}
		seen[key] = true
		env := copyNameEnv(callerEnv)
		p.bindLiteralArgs(mc, args, callerEnv, env)
		for _, parent := range mc.Parents {
			walk(p.multiclasses[parent.Name], prefix, parent.Args, env)
		}
		p.registerObjectNames(mc.Body, prefix, env)
	}
	for _, ref := range mcs {
		walk(p.multiclasses[ref.Name], prefix, ref.Args, seed)
	}
}

func copyNameEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+4)
	for k, v := range env {
		out[k] = v
	}
	return out
}

// registerObjectNames pre-registers the global names that the objects of a loop
// or multiclass body create, substituting the literal values bound in env. The
// parser needs these names while parsing later statements: a reference like
// `[sub0, sub1]` in a template argument is type-checked immediately, long
// before the record phase would create sub0.
func (p *Parser) registerObjectNames(objs []Object, defmPrefix string, env map[string]string) int {
	missed := 0
	for _, o := range objs {
		switch t := o.(type) {
		case *DefvarStmt:
			if v, ok := p.materializeName(t.Value, "", env); ok && env != nil {
				env[t.Name] = v
			}
		case *DefStmt:
			n, ok := p.materializeName(t.Name, defmPrefix, env)
			if !ok {
				missed++
				continue
			}
			// In a multiclass body, a name that does not mention NAME is
			// prefixed with the defm name, matching ParseObjectName. `def ""`
			// therefore registers the defm's own name.
			if defmPrefix != "" && !usesNAME(t.Name) {
				n = defmPrefix + n
			}
			if n == "" {
				continue
			}
			if _, exists := p.globals[n]; exists {
				continue
			}
			var rec *recordInfo
			if t.info != nil {
				cp := *t.info
				cp.name = n
				rec = &cp
			} else {
				rec = &recordInfo{name: n, fields: map[string]binding{}, classes: p.classes}
				for _, pr := range t.Parents {
					rec.parents = append(rec.parents, pr.Name)
				}
			}
			p.defineGlobal(n, &globalBinding{kind: gDef, ty: recordInfoType(rec), rec: rec})
		case *DefmStmt:
			inner, ok := p.materializeName(t.Name, defmPrefix, env)
			if !ok {
				missed++
				continue
			}
			if defmPrefix != "" && !usesNAME(t.Name) {
				inner = defmPrefix + inner
			}
			p.registerDefmPrefix(inner, t.Parents[:t.ClassStart], env)
		case *IfStmt:
			// Both arms are registered: a name that the condition rejects is
			// simply never defined in the record phase, which still reports
			// the reference as undefined.
			missed += p.registerObjectNames(t.Then, defmPrefix, env)
			missed += p.registerObjectNames(t.Else, defmPrefix, env)
		case *LetStmt:
			missed += p.registerObjectNames(t.Body, defmPrefix, env)
		case *ForeachStmt:
			vals, ok := p.loopValues(t.List, t.Range, env)
			if !ok {
				missed++
				continue
			}
			for _, v := range vals {
				sub := copyNameEnv(env)
				sub[t.Var] = v
				missed += p.registerObjectNames(t.Body, defmPrefix, sub)
			}
		}
	}
	return missed
}

// registerLoopNames materializes the names a top-level foreach creates. LLVM
// resolves the loop right after parsing it, so subsequent statements see the
// records; the parser mirrors that without re-parsing the body.
func (p *Parser) registerLoopNames(varName string, list Value, rng []int32, body []Object) bool {
	vals, ok := p.loopValues(list, rng, nil)
	if !ok {
		return false
	}
	missed := 0
	for _, v := range vals {
		missed += p.registerObjectNames(body, "", map[string]string{varName: v})
	}
	return missed == 0
}

// loopValues returns the literal element strings of a loop list when they are
// statically known: an integer range, or a list of literals and loop
// variables. An expression list (e.g. a class field) is not knowable while
// parsing and reports false.
func (p *Parser) loopValues(list Value, rng []int32, env map[string]string) ([]string, bool) {
	if rng != nil {
		out := make([]string, len(rng))
		for i, n := range rng {
			out[i] = itoa(int(n))
		}
		return out, true
	}
	lv, ok := list.(*ListValue)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(lv.Elems))
	for _, e := range lv.Elems {
		s, ok := p.materializeName(e, "", env)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// bindLiteralArgs maps a multiclass's template arguments to the literal string
// values supplied at the defm site (positional or named), falling back to the
// declared default. Values that are not statically materializable are skipped.
func (p *Parser) bindLiteralArgs(mc *MultiClassStmt, args []*Arg, callerEnv, dst map[string]string) {
	named := map[string]Value{}
	var positional []Value
	for _, a := range args {
		if a.Name != "" {
			named[a.Name] = a.Value
		} else {
			positional = append(positional, a.Value)
		}
	}
	for i, decl := range mc.TemplateArgs {
		v := named[decl.Name]
		if v == nil && i < len(positional) {
			v = positional[i]
		}
		if v == nil {
			v = decl.Value
		}
		if v == nil {
			continue
		}
		if s, ok := p.materializeName(v, "", callerEnv); ok {
			dst[decl.Name] = s
		}
	}
}

// usesNAME reports whether an object name mentions the implicit NAME.
func usesNAME(v Value) bool {
	switch t := v.(type) {
	case *VarValue:
		return t.Name == "NAME"
	case *PasteValue:
		return usesNAME(t.LHS) || usesNAME(t.RHS)
	}
	return false
}

// materializeName evaluates an object name made only of literals, NAME and
// template arguments bound to literal strings (env).
func (p *Parser) materializeName(v Value, defmName string, env map[string]string) (string, bool) {
	switch t := v.(type) {
	case nil:
		return "", false
	case *StringValue:
		return t.Text, true
	case *IntValue:
		return itoa(int(t.Num)), true
	case *VarValue:
		if t.Name == "NAME" {
			return defmName, true
		}
		if env != nil {
			if s, ok := env[t.Name]; ok {
				return s, true
			}
		}
		// A reference to a top-level def stringifies to the def's name; this
		// is how `def NAME#_#vt` with a loop over value types works.
		if g := p.globals[t.Name]; g != nil && g.kind == gDef && g.rec != nil {
			return g.rec.name, true
		}
		return "", false
	case *PasteValue:
		a, ok := p.materializeName(t.LHS, defmName, env)
		if !ok {
			return "", false
		}
		b, ok := p.materializeName(t.RHS, defmName, env)
		if !ok {
			return "", false
		}
		return a + b, true
	case *UnsetValue:
		return "", false
	}
	return "", false
}

func (p *Parser) parseMultiClass() Object {
	start := p.i
	p.advance() // multiclass
	nt, ok := p.expect(Id, "E_PARSE_MC_NAME", "expected identifier after multiclass for name")
	if !ok {
		return nil
	}
	name := nt.Text
	if _, exists := p.multiclasses[name]; exists {
		p.errAt(nt.Span, "E_PARSE_MC_REDEF", "multiclass '"+name+"' already defined")
	}
	mc := &MultiClassStmt{Base: Base{At: p.span(start)}, Name: name, NameSpan: nt.Span}
	p.multiclasses[name] = mc
	p.curMulti = mc
	rec := &recordInfo{name: name, isClass: true, fields: map[string]binding{}, classes: p.classes}
	s := p.pushScope(rec)
	if p.at(Less) {
		args, ok := p.parseTemplateArgList(rec, s)
		if !ok {
			p.popScope()
			p.curMulti = nil
			return nil
		}
		mc.TemplateArgs = args
	}
	inherits := false
	if p.eat(Colon) {
		inherits = true
		for {
			ref, ok := p.parseClassRef(true)
			if !ok {
				p.popScope()
				p.curMulti = nil
				return nil
			}
			mc.Parents = append(mc.Parents, ref)
			if !p.eat(Comma) {
				break
			}
		}
	}
	if !p.at(LBrace) {
		if !inherits {
			p.errTok("E_PARSE_MC_BRACE", "expected '{' in multiclass definition")
			p.popScope()
			p.curMulti = nil
			return nil
		}
		if !p.eat(Semi) {
			p.errTok("E_PARSE_MC_SEMI", "expected ';' in multiclass definition")
		}
	} else {
		p.advance() // {
		if p.at(RBrace) {
			p.errTok("E_PARSE_MC_EMPTY", "multiclass must contain at least one def")
		}
		for !p.at(RBrace) && !p.at(Eof) {
			obj := p.parseMultiClassObject()
			if obj == nil {
				p.recoverBlock()
				break
			}
			mc.Body = append(mc.Body, obj)
		}
		p.eat(RBrace)
		if p.at(Semi) {
			st := p.cur().Span
			p.advance()
			p.errAt(st, "E_PARSE_MC_SEMI_BODY", "A multiclass body should not end with a semicolon")
		}
	}
	p.warnUnusedArgs(rec, name, "::", mc.TemplateArgs)
	p.popScope()
	p.curMulti = nil
	mc.Base.At = p.span(start)
	return mc
}

// parseMultiClassObject restricts the body grammar, matching ParseMultiClass.
func (p *Parser) parseMultiClassObject() Object {
	switch p.cur().Kind {
	case AssertTok, Def, Defm, Defvar, Dump, Foreach, If, Let:
		return p.parseObject()
	default:
		p.errTok("E_PARSE_MC_BODY",
			"expected 'assert', 'def', 'defm', 'defvar', 'dump', 'foreach', 'if', or 'let' in multiclass body")
		return nil
	}
}

// ---------------------------------------------------------------------------
// defset / deftype / defvar / assert / dump

func (p *Parser) parseDefset() Object {
	start := p.i
	p.advance() // defset
	loc := p.cur().Span
	ty, ok := p.parseType()
	if !ok {
		return nil
	}
	if ty.Kind != ListType {
		p.errAt(loc, "E_PARSE_DEFSET_TYPE", "expected list type")
	}
	nt, ok := p.expect(Id, "E_PARSE_DEFSET_ID", "expected identifier")
	if !ok {
		return nil
	}
	if _, exists := p.globals[nt.Text]; exists {
		p.errAt(nt.Span, "E_PARSE_DEFSET_DUP",
			"def or global variable of this name already exists")
	}
	if !p.eat(Equal) {
		p.errTok("E_PARSE_DEFSET_EQ", "expected '='")
		return nil
	}
	if !p.eat(LBrace) {
		p.errTok("E_PARSE_DEFSET_BRACE", "expected '{'")
		return nil
	}
	var objs []Object
	for !p.at(RBrace) && !p.at(Eof) {
		obj := p.parseObject()
		if obj == nil {
			p.recoverBlock()
			break
		}
		objs = append(objs, obj)
	}
	if !p.eat(RBrace) {
		p.errTok("E_PARSE_DEFSET_RBRACE", "expected '}' at end of defset")
	}
	elem := Type{Kind: IntType}
	if ty.Elem != nil {
		elem = *ty.Elem
	}
	p.defineGlobal(nt.Text, &globalBinding{kind: gDefset, ty: Type{Kind: ListType, Elem: &elem}})
	return &DefsetStmt{
		Base:     Base{At: p.span(start)},
		ElemType: elem,
		TypeSpan: loc,
		Name:     nt.Text,
		NameSpan: nt.Span,
		Objects:  objs,
	}
}

func (p *Parser) parseDeftype() Object {
	start := p.i
	p.advance() // deftype
	nt, ok := p.expect(Id, "E_PARSE_DEFTYPE_ID", "expected identifier")
	if !ok {
		return nil
	}
	if _, exists := p.typeAliases[nt.Text]; exists {
		p.errAt(nt.Span, "E_PARSE_DEFTYPE_DUP",
			"type of this name '"+nt.Text+"' already exists")
	}
	if _, exists := p.classes[nt.Text]; exists {
		p.errAt(nt.Span, "E_PARSE_DEFTYPE_DUP",
			"type of this name '"+nt.Text+"' already exists")
	}
	if !p.eat(Equal) {
		p.errTok("E_PARSE_DEFTYPE_EQ", "expected '='")
		return nil
	}
	loc := p.cur().Span
	ty, ok := p.parseType()
	if !ok {
		return nil
	}
	if ty.Kind == RecordType {
		p.errAt(loc, "E_PARSE_DEFTYPE_REC",
			"cannot define type alias for class type '"+ty.String()+"'")
	}
	p.typeAliases[nt.Text] = ty
	if !p.eat(Semi) {
		p.errTok("E_PARSE_DEFTYPE_SEMI", "expected ';'")
		return nil
	}
	return &DeftypeStmt{Base: Base{At: p.span(start)}, Name: nt.Text, NameSpan: nt.Span, Type: ty}
}

func (p *Parser) parseDefvar(rec *recordInfo) *DefvarStmt {
	start := p.i
	p.advance() // defvar
	nt, ok := p.expect(Id, "E_PARSE_DEFVAR_ID", "expected identifier")
	if !ok {
		return nil
	}
	if !p.eat(Equal) {
		p.errTok("E_PARSE_DEFVAR_EQ", "expected '='")
		return nil
	}
	v := p.parseValue(rec, nil, parseValueMode)
	if v == nil {
		return nil
	}
	if !p.eat(Semi) {
		p.errTok("E_PARSE_DEFVAR_SEMI", "expected ';'")
		return nil
	}
	b := binding{ty: v.Type(), lit: v, isDefvar: true, lexical: true}
	if p.topLevel() {
		// TGParser::AddDefvar: one namespace for defs, classes and globals.
		if _, dup := p.globals[nt.Text]; dup {
			p.errAt(nt.Span, "E_PARSE_DEFVAR_DUP", "def or global variable of this name already exists")
		}
		p.defineGlobal(nt.Text, &globalBinding{kind: gDefvar, ty: v.Type(), lit: v})
	} else {
		// A defvar may shadow a template argument (defvar.td relies on it),
		// so only real fields and real locals collide.
		if rec != nil {
			if b, isField := rec.fields[nt.Text]; isField && !b.arg {
				p.errAt(nt.Span, "E_PARSE_DEFVAR_FIELD", "field of this name already exists")
			}
		}
		if n := len(p.scopes); n > 0 {
			if b, dup := p.scopes[n-1].vars[nt.Text]; dup && !b.arg {
				p.errAt(nt.Span, "E_PARSE_DEFVAR_LOCAL", "local variable of this name already exists")
			}
		}
		p.defineLocal(nt.Text, b)
	}
	return &DefvarStmt{Base: Base{At: p.span(start)}, Name: nt.Text, NameSpan: nt.Span, Value: v}
}

func (p *Parser) parseAssert(rec *recordInfo, start int) *AssertStmt {
	p.advance() // assert
	condLoc := p.cur().Span
	cond := p.parseValue(rec, nil, parseValueMode)
	if cond == nil {
		return nil
	}
	_ = condLoc
	if !p.eat(Comma) {
		p.errTok("E_PARSE_ASSERT_COMMA", "expected ',' in assert statement")
		return nil
	}
	msg := p.parseValue(rec, nil, parseValueMode)
	if msg == nil {
		return nil
	}
	if !p.eat(Semi) {
		p.errTok("E_PARSE_ASSERT_SEMI", "expected ';'")
		return nil
	}
	return &AssertStmt{Base: Base{At: p.span(start)}, Cond: cond, Message: msg}
}

func (p *Parser) parseDump(rec *recordInfo, start int) *DumpStmt {
	p.advance() // dump
	v := p.parseValue(rec, nil, parseValueMode)
	if v == nil {
		return nil
	}
	if !p.eat(Semi) {
		p.errTok("E_PARSE_DUMP_SEMI", "expected ';'")
		return nil
	}
	return &DumpStmt{Base: Base{At: p.span(start)}, Value: v}
}

// ---------------------------------------------------------------------------
// foreach / if / let

func (p *Parser) parseForeach() Object {
	start := p.i
	p.advance() // foreach
	varName, varSpan, list, rng, ok := p.parseForeachDecl()
	if !ok {
		p.errTok("E_PARSE_FOREACH_DECL", "expected declaration in for")
		return nil
	}
	if !p.eat(In) {
		p.errTok("E_PARSE_FOREACH_IN", "Unknown tok")
		return nil
	}
	seq := -1
	statementLevel := p.classDepth == 0 && p.curMulti == nil && p.curRec == nil && p.loopDepth == 0
	if statementLevel {
		seq = p.loopSeq
		p.loopSeq++
	}
	p.pushScope(nil)
	p.defineLocal(varName, binding{ty: foreachElemType(list, rng), lexical: true})
	p.loopDepth++
	body, ok := p.parseLoopBody("foreach command")
	p.loopDepth--
	p.popScope()
	if !ok {
		p.recoverBlock()
		return nil
	}
	// LLVM resolves a top-level loop as soon as the statement ends and adds
	// the resulting records to the keeper, so later statements can reference
	// them. We keep the loop body for the record phase, but the parser's name
	// table needs the same names now: template arguments are type-checked
	// while parsing, and `RegisterTuples<[sub0, sub1], ...>` must know that
	// sub0 is a SubRegIndex.
	if statementLevel {
		if !p.registerLoopNames(varName, list, rng, body) {
			p.dynLoopNames = true
		}
		// A second parse (after lowering once) knows the names this loop
		// really created, including ones built by bang expressions.
		for _, kd := range p.knownLoops[seq] {
			if _, exists := p.globals[kd.Name]; exists {
				continue
			}
			rec := &recordInfo{name: kd.Name, fields: map[string]binding{}, classes: p.classes}
			rec.parents = append(rec.parents, kd.Parents...)
			p.defineGlobal(kd.Name, &globalBinding{kind: gDef, ty: recordInfoType(rec), rec: rec})
		}
	}
	return &ForeachStmt{
		Base:    Base{At: p.span(start)},
		Var:     varName,
		VarSpan: varSpan,
		List:    list,
		Range:   rng,
		Body:    body,
		Seq:     seq,
	}
}

func foreachElemType(list Value, rng []int32) Type {
	if list != nil {
		if t := list.Type(); t.Kind == ListType && t.Elem != nil {
			return *t.Elem
		}
	}
	return Type{Kind: IntType}
}

// parseForeachDecl parses `ID = ('{' RangeList '}' | Value | RangePiece)`.
func (p *Parser) parseForeachDecl() (string, Span, Value, []int32, bool) {
	if !p.at(Id) {
		p.errTok("E_PARSE_FOREACH_ID", "Expected identifier in foreach declaration")
		return "", Span{}, nil, nil, false
	}
	id := p.advance()
	if !p.eat(Equal) {
		p.errTok("E_PARSE_FOREACH_EQ", "Expected '=' in foreach declaration")
		return "", Span{}, nil, nil, false
	}
	if p.at(LBrace) {
		brace := p.cur().Span
		p.advance()
		rng, ok := p.parseRangeList()
		if !ok {
			return "", Span{}, nil, nil, false
		}
		if len(rng) == 0 {
			return "", Span{}, nil, nil, false
		}
		if !p.eat(RBrace) {
			p.errTok("E_PARSE_FOREACH_RBRACE", "expected '}' at end of bit range list")
			p.errAt(brace, "E_NOTE_BRACE", "to match this '{'")
			return "", Span{}, nil, nil, false
		}
		return id.Text, id.Span, nil, rng, true
	}
	val := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
	if val == nil {
		return "", Span{}, nil, nil, false
	}
	if val.Type().Kind == ListType {
		return id.Text, id.Span, val, nil, true
	}
	if _, isUnset := val.(*UnsetValue); isUnset {
		p.errAt(val.Span(), "E_PARSE_FOREACH_LIST", "expected a list, got '?'")
		return "", Span{}, nil, nil, false
	}
	rng, ok := p.parseRangePiece(val)
	if !ok {
		return "", Span{}, nil, nil, false
	}
	if len(rng) == 0 {
		return "", Span{}, nil, nil, false
	}
	// ParseRangePiece with a trailing comma inside braces is not supported
	// here; `foreach i = {0,1}` goes through the brace branch above.
	if p.at(Comma) {
		p.errTok("E_PARSE_FOREACH_RANGE", "expected integer or bitrange")
		return "", Span{}, nil, nil, false
	}
	if !p.at(In) {
		p.errTok("E_PARSE_FOREACH_IN", "Unknown tok")
		return "", Span{}, nil, nil, false
	}
	return id.Text, id.Span, nil, rng, true
}

func (p *Parser) parseIf() Object {
	start := p.i
	p.advance() // if
	cond := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
	if cond == nil {
		return nil
	}
	if !p.eat(Then) {
		p.errTok("E_PARSE_IF_THEN", "Unknown tok")
		return nil
	}
	p.pushScope(nil)
	then, ok := p.parseLoopBody("'then' clause")
	p.popScope()
	if !ok {
		p.recoverBlock()
		return nil
	}
	obj := &IfStmt{Base: Base{At: p.span(start)}, Cond: cond, Then: then}
	if p.eat(ElseKW) {
		p.pushScope(nil)
		els, ok := p.parseLoopBody("'else' clause")
		p.popScope()
		if !ok {
			p.recoverBlock()
			return nil
		}
		obj.Else = els
	}
	obj.Base.At = p.span(start)
	return obj
}

// parseLoopBody parses `'{' ObjectList '}'` or a single Object.
func (p *Parser) parseLoopBody(kind string) ([]Object, bool) {
	if !p.at(LBrace) {
		obj := p.parseObject()
		if obj == nil {
			return nil, false
		}
		return []Object{obj}, true
	}
	brace := p.cur().Span
	p.advance()
	var objs []Object
	for !p.at(RBrace) && !p.at(Eof) {
		obj := p.parseObject()
		if obj == nil {
			return nil, false
		}
		objs = append(objs, obj)
	}
	if !p.eat(RBrace) {
		p.errTok("E_PARSE_BLOCK_RBRACE", "expected '}' at end of "+kind)
		p.errAt(brace, "E_NOTE_BRACE", "to match this '{'")
		return nil, false
	}
	return objs, true
}

func (p *Parser) parseTopLet() Object {
	start := p.i
	p.advance() // let
	binds, ok := p.parseLetList()
	if !ok || len(binds) == 0 {
		return nil
	}
	if !p.eat(In) {
		p.errTok("E_PARSE_LET_IN", "expected 'in' at end of top-level 'let'")
		return nil
	}
	p.pushScope(nil)
	body, ok := p.parseLoopBody("top level let command")
	p.popScope()
	if !ok {
		p.recoverBlock()
		return nil
	}
	return &LetStmt{Base: Base{At: p.span(start)}, Binds: binds, Body: body}
}

func (p *Parser) parseLetList() ([]*LetBind, bool) {
	var out []*LetBind
	for {
		if !p.at(Id) {
			p.errTok("E_PARSE_LET_ID", "expected identifier in let definition")
			return nil, false
		}
		b, ok := p.parseTopLetBind()
		if !ok {
			return nil, false
		}
		out = append(out, b)
		if !p.eat(Comma) {
			break
		}
	}
	return out, true
}

func (p *Parser) parseLetBindHead() (*LetBind, bool) {
	start := p.i
	mode, nt := p.parseLetModeAndName()
	b := &LetBind{Base: Base{At: Span{}}, Mode: mode, Field: nt.Text, FieldSpan: nt.Span}
	for {
		if p.at(LBrace) {
			// A bit range belongs to the field; ranges are written high..low.
			if !p.at(LBrace) {
				break
			}
			brace := p.cur().Span
			p.advance()
			rng, ok := p.parseRangeList()
			if !ok {
				return nil, false
			}
			b.Bits = append(b.Bits, rng...)
			if !p.eat(RBrace) {
				p.errTok("E_PARSE_LET_RBRACE", "expected '}' at end of bit list")
				p.errAt(brace, "E_NOTE_BRACE", "to match this '{'")
				return nil, false
			}
			continue
		}
		break
	}
	b.Base.At = p.span(start)
	return b, true
}

// parseLetBind parses a complete let binding. Top-level lets have no record,
// so the field check is skipped there (matching ParseLetList).
func (p *Parser) parseLetBind() (*LetBind, bool) {
	b, ok := p.parseLetBindHead()
	if !ok {
		return nil, false
	}
	return p.parseLetValue(b, p.curRecOrNil())
}

// parseTopLetBind is the same production without the field check. A top-level
// let is applied to whatever record it wraps, and that record is not known yet
// (it may inherit the field from a class), so ParseLetList does not check it.
func (p *Parser) parseTopLetBind() (*LetBind, bool) {
	b, ok := p.parseLetBindHead()
	if !ok {
		return nil, false
	}
	if !p.eat(Equal) {
		p.errTok("E_PARSE_LET_EQ", "expected '=' in let expression")
		return nil, false
	}
	v := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
	if v == nil {
		return nil, false
	}
	b.Value = v
	return b, true
}

// parseLetValue checks the field (ParseBodyItem does this before parsing the
// value) and parses `= Value`.
func (p *Parser) parseLetValue(b *LetBind, rec *recordInfo) (*LetBind, bool) {
	var item *Type
	if rec != nil {
		fb, ok := rec.field(b.Field, map[string]bool{})
		if !ok {
			p.errAt(b.FieldSpan, "E_PARSE_LET_UNKNOWN", "Value '"+b.Field+"' unknown!")
			return nil, false
		}
		ty := fb.ty
		if len(b.Bits) > 0 && ty.Kind == BitsType {
			ty = bitsOf(len(b.Bits))
		}
		item = &ty
	}
	if !p.eat(Equal) {
		p.errTok("E_PARSE_LET_EQ", "expected '=' in let expression")
		return nil, false
	}
	v := p.parseValue(rec, item, parseValueMode)
	if v == nil {
		return nil, false
	}
	b.Value = v
	return b, true
}

// parseLetModeAndName implements the contextual append/prepend keywords.
func (p *Parser) parseLetModeAndName() (LetMode, Token) {
	nt := p.advance()
	mode := LetReplace
	switch nt.Text {
	case "append":
		mode = LetAppend
	case "prepend":
		mode = LetPrepend
	}
	if mode != LetReplace && p.at(Id) {
		return mode, p.advance()
	}
	return LetReplace, nt
}

// ---------------------------------------------------------------------------
// Bodies and declarations

func (p *Parser) parseBody(rec *recordInfo) ([]BodyItem, bool) {
	if p.eat(Semi) {
		return nil, true
	}
	if !p.eat(LBrace) {
		p.errTok("E_PARSE_BODY_START",
			"Expected '{' to start body or ';' for declaration only")
		return nil, false
	}
	var items []BodyItem
	for !p.at(RBrace) && !p.at(Eof) {
		it := p.parseBodyItem(rec)
		if it == nil {
			return nil, false
		}
		items = append(items, it)
	}
	if !p.eat(RBrace) {
		p.errTok("E_PARSE_BODY_RBRACE", "expected '}' at end of body")
		return nil, false
	}
	if p.at(Semi) {
		st := p.cur().Span
		p.advance()
		p.errAt(st, "E_PARSE_BODY_SEMI", "A class or def body should not end with a semicolon")
	}
	return items, true
}

func (p *Parser) parseBodyItem(rec *recordInfo) BodyItem {
	start := p.i
	switch p.cur().Kind {
	case AssertTok:
		if o := p.parseAssert(rec, start); o != nil {
			return o
		}
		return nil
	case Dump:
		if o := p.parseDump(rec, start); o != nil {
			return o
		}
		return nil
	case Defvar:
		if o := p.parseDefvar(rec); o != nil {
			return o
		}
		return nil
	case Let:
		p.advance()
		if !p.at(Id) {
			p.errTok("E_PARSE_LET_FIELD", "expected field identifier after let")
			return nil
		}
		b, ok := p.parseLetBindHead()
		if !ok {
			return nil
		}
		b, ok = p.parseLetValue(b, rec)
		if !ok {
			return nil
		}
		if !p.eat(Semi) {
			p.errTok("E_PARSE_LET_SEMI", "expected ';' after let expression")
			return nil
		}
		return &LetItem{Base: Base{At: p.span(start)}, Bind: b}
	}
	d, ok := p.parseDeclaration(rec, false)
	if !ok {
		return nil
	}
	if !p.eat(Semi) {
		p.errTok("E_PARSE_DECL_SEMI", "expected ';' after declaration")
		return nil
	}
	return d
}

// parseDeclaration parses `['field'] Type Id ('=' Value)?`.
func (p *Parser) parseDeclaration(rec *recordInfo, isArg bool) (*Decl, bool) {
	start := p.i
	isField := false
	if !isArg {
		isField = p.eat(Field)
	}
	ty, ok := p.parseType()
	if !ok {
		return nil, false
	}
	nt, ok := p.expect(Id, "E_PARSE_DECL_ID", "Expected identifier in declaration")
	if !ok {
		return nil, false
	}
	if nt.Text == "NAME" {
		p.errAt(nt.Span, "E_PARSE_DECL_NAME", "'NAME' is a reserved variable name")
	}
	d := &Decl{Base: Base{}, IsField: isField, IsArg: isArg, Type: ty, Name: nt.Text, NameSpan: nt.Span}
	// Fields become visible before their initializer is parsed, matching
	// ParseDeclaration; template args become visible after.
	if !isArg && rec != nil {
		// A field may not take the name of a defvar already visible here;
		// nested scopes may still shadow outer ones.
		if n := len(p.scopes); n > 0 {
			if b, dup := p.scopes[n-1].vars[nt.Text]; dup && b.isDefvar {
				p.errAt(nt.Span, "E_PARSE_FIELD_DEFVAR", "local variable of this name already exists")
			}
		}
		rec.fields[nt.Text] = binding{ty: ty}
		if n := len(p.scopes); n > 0 && p.scopes[n-1].rec == rec {
			// Already reachable through rec.fields.
		}
	}
	if p.eat(Equal) {
		v := p.parseValue(rec, &ty, parseValueMode)
		// "Errors in the value do not stop parsing, so that we can continue to
		// make some progress." (TGParser.cpp:3649-3656) The declaration is
		// kept without a value, so the caller still expects the ';' and
		// reports it when the failed value left the lexer elsewhere.
		if v != nil {
			d.Value = v
			if !isArg && rec != nil {
				if b, ok := rec.fields[nt.Text]; ok {
					b.lit = v
					rec.fields[nt.Text] = b
				}
				if isLiteral(v) {
					p.defineLocal(nt.Text, binding{ty: ty, lit: v})
				}
			}
		}
	}
	if isArg {
		// TGParser::QualifyName stores the owning record in the VarInit, so a
		// reference keeps pointing at *this* class even when the surrounding
		// record changes (an instance inheriting the class, a defm body).
		sep := ":"
		if p.curMulti != nil {
			sep = "::"
		}
		b := binding{ty: ty, lit: d.Value, arg: true}
		if rec != nil {
			b.qual = rec.name + sep + nt.Text
		}
		p.defineLocal(nt.Text, b)
		if rec != nil {
			rec.fields[nt.Text] = b
		}
	}
	d.Base.At = p.span(start)
	return d, true
}

func isLiteral(v Value) bool {
	switch v.(type) {
	case *IntValue, *StringValue, *BitsValue:
		return true
	}
	return false
}

func (p *Parser) parseTemplateArgList(rec *recordInfo, s *scope) ([]*Decl, bool) {
	start := p.i
	p.advance() // '<'
	var args []*Decl
	seen := map[string]bool{}
	for {
		d, ok := p.parseDeclaration(rec, true)
		if !ok {
			return nil, false
		}
		// TGParser::ParseTemplateArgList rejects a repeated name at the start
		// of the second declaration, which is where the diagnostic points.
		if seen[d.Name] {
			p.errAt(Span{Start: d.Base.At.Start, End: d.Base.At.Start}, "E_PARSE_TARG_DUP",
				"template argument with the same name has already been defined")
		}
		seen[d.Name] = true
		args = append(args, d)
		if !p.eat(Comma) {
			break
		}
	}
	if !p.eat(Greater) {
		p.errTok("E_PARSE_TARGS_GT", "expected '>' at end of template argument list")
		return nil, false
	}
	_ = start
	return args, true
}

// parseType parses the Type production.
func (p *Parser) parseType() (Type, bool) {
	start := p.i
	switch p.cur().Kind {
	case String, Code:
		p.advance()
		return StringTy, true
	case Bit:
		p.advance()
		return BitTy, true
	case Int:
		p.advance()
		return IntTy, true
	case Dag:
		p.advance()
		return DagTy, true
	case Id:
		nt := p.advance()
		if ty, ok := p.typeAliases[nt.Text]; ok {
			return ty, true
		}
		if _, ok := p.classes[nt.Text]; ok {
			t := recordType(nt.Text)
			t.IsClass = true
			return t, true
		}
		p.errAt(nt.Span, "E_PARSE_TYPE_CLASS", "Couldn't find class '"+nt.Text+"'")
		p.errAt(nt.Span, "E_PARSE_TYPE_UNKNOWN", "unknown class name")
		return InvalidTy, false
	case Bits:
		p.advance()
		if !p.eat(Less) {
			p.errTok("E_PARSE_TYPE_BITS_LT", "expected '<' after bits type")
			return InvalidTy, false
		}
		nt, ok := p.expect(IntVal, "E_PARSE_TYPE_BITS_INT", "expected integer in bits<n> type")
		if !ok {
			return InvalidTy, false
		}
		if !p.eat(Greater) {
			p.errTok("E_PARSE_TYPE_BITS_GT", "expected '>' at end of bits<n> type")
			return InvalidTy, false
		}
		return Type{Kind: BitsType, Bits: int(nt.Num)}, true
	case List:
		p.advance()
		if !p.eat(Less) {
			p.errTok("E_PARSE_TYPE_LIST_LT", "expected '<' after list type")
			return InvalidTy, false
		}
		sub, ok := p.parseType()
		if !ok {
			return InvalidTy, false
		}
		if !p.eat(Greater) {
			p.errTok("E_PARSE_TYPE_LIST_GT", "expected '>' at end of list<ty> type")
			return InvalidTy, false
		}
		return Type{Kind: ListType, Elem: &sub}, true
	default:
		p.errTok("E_PARSE_TYPE", "Unknown token when expecting a type")
		_ = start
		return InvalidTy, false
	}
}

// ---------------------------------------------------------------------------
// Parents and class references

type defParentsRec struct {
	rec  *recordInfo
	refs []*ParentRef
}

func (p *Parser) lastDefParents(rec *recordInfo) []*ParentRef {
	for i := len(p.defParents) - 1; i >= 0; i-- {
		if p.defParents[i].rec == rec {
			refs := p.defParents[i].refs
			p.defParents = p.defParents[:i]
			return refs
		}
	}
	return nil
}

func (p *Parser) parseParentRefs(rec *recordInfo, isDefm bool) ([]*ParentRef, bool) {
	p.advance() // ':'
	var out []*ParentRef
	for {
		ref, ok := p.parseClassRef(isDefm)
		if !ok {
			return nil, false
		}
		out = append(out, ref)
		// AddParentClass runs as each parent is parsed, so a later reference
		// to the record itself inside a template argument already sees the
		// earlier parents (self-reference.td's `def F0 : Fa, Fb<F0>`).
		if rec != nil {
			rec.parents = append(rec.parents, ref.Name)
		}
		if !p.eat(Comma) {
			break
		}
	}
	return out, true
}

func (p *Parser) parseClassRef(isMulticlass bool) (*ParentRef, bool) {
	start := p.i
	nt, ok := p.expect(Id, "E_PARSE_CLASSREF_ID", "expected name for ClassID")
	if !ok {
		return nil, false
	}
	ref := &ParentRef{Name: nt.Text, NameSpan: nt.Span, MultiClass: isMulticlass}
	if isMulticlass {
		if _, ok := p.multiclasses[nt.Text]; !ok {
			p.errAt(nt.Span, "E_PARSE_CLASSREF_MC", "Couldn't find multiclass '"+nt.Text+"'")
			return nil, false
		}
	} else if _, ok := p.classes[nt.Text]; !ok {
		msg := "Couldn't find class '" + nt.Text + "'"
		if _, isMC := p.multiclasses[nt.Text]; isMC {
			msg += ". Use 'defm' if you meant to use multiclass '" + nt.Text + "'"
		}
		p.errAt(nt.Span, "E_PARSE_CLASSREF_CLASS", msg)
		return nil, false
	}
	if p.eat(Less) {
		var formals []*Decl
		if isMulticlass {
			if mc := p.multiclasses[nt.Text]; mc != nil {
				formals = mc.TemplateArgs
			}
		} else {
			formals = p.classArgs[nt.Text]
		}
		sep := ":"
		if isMulticlass {
			sep = "::"
		}
		args, ok := p.parseTemplateArgValues(p.curRecOrNil(), formals, nt.Text, nt.Span, sep)
		if !ok {
			return nil, false
		}
		ref.Args = args
	}
	ref.Base.At = p.span(start)
	return ref, true
}

// parseTemplateArgValues parses an argument list after '<' has been consumed,
// through '>'. An empty list is allowed.
func (p *Parser) parseTemplateArgValues(rec *recordInfo, formals []*Decl, owner string, nameSpan Span, sep string) ([]*Arg, bool) {
	var args []*Arg
	if p.eat(Greater) {
		return args, true
	}
	named := map[string]bool{}
	positional := 0
	sawNamed := false
	for {
		start := p.i
		if p.at(Id) && p.peekKind(1) == Equal {
			nameTok := p.advance()
			p.advance() // '='
			sawNamed = true
			slot := -1
			var hint *Type
			for i, f := range formals {
				if f != nil && f.Name == nameTok.Text {
					slot = i
					ty := f.Type
					hint = &ty
					break
				}
			}
			v := p.parseValue(rec, hint, parseValueMode)
			if v == nil {
				return nil, false
			}
			// TGParser::ParseTemplateArgValue: the name must exist, may not
			// repeat (neither itself nor the slot a positional already took),
			// and must carry a value. All three report at the class name.
			switch {
			case slot < 0:
				p.errAt(nameTok.Span, "E_PARSE_ARG_UNKNOWN", "Argument \""+nameTok.Text+"\" doesn't exist")
			case named[nameTok.Text] || slot < positional:
				p.errAt(nameSpan, "E_PARSE_ARG_DUP",
					"We can only specify the template argument '"+owner+sep+nameTok.Text+"' once")
			}
			if _, unset := v.(*UnsetValue); unset {
				p.errAt(v.Span(), "E_PARSE_ARG_UNSET",
					"The value of named argument should be initialized, but we got '?'")
			}
			named[nameTok.Text] = true
			p.checkTemplateArgValue(v, hint, owner, nameTok.Text, sep)
			args = append(args, &Arg{Base: Base{At: p.span(start)}, Name: nameTok.Text, NameSpan: nameTok.Span, Value: v})
		} else {
			var hint *Type
			if n := len(args); n < len(formals) && formals[n] != nil {
				ty := formals[n].Type
				hint = &ty
			}
			v := p.parseValue(rec, hint, parseValueMode)
			if v == nil {
				return nil, false
			}
			argName := ""
			if n := len(args); n < len(formals) && formals[n] != nil {
				argName = formals[n].Name
			}
			if sawNamed {
				p.errAt(v.Span(), "E_PARSE_ARG_POS", "Positional argument should be put before named argument")
			}
			// `3=0`: the value parsed, and the '=' shows the author meant a
			// named argument with an unusable name.
			if p.at(Equal) {
				p.errAt(v.Span(), "E_PARSE_ARG_NAME", "The name of named argument should be a valid identifier")
			}
			positional++
			p.checkTemplateArgValue(v, hint, owner, argName, sep)
			args = append(args, &Arg{Base: Base{At: p.span(start)}, Value: v})
		}
		if !p.eat(Comma) {
			break
		}
	}
	if !p.eat(Greater) {
		p.errTok("E_PARSE_TARGS_GT", "expected '>' at end of template argument list")
		return nil, false
	}
	return args, true
}

// checkTemplateArgValue mirrors TGParser::CheckTemplateArgValues: a template
// argument must be convertible to the declared parameter type. LLVM also
// inserts casts for convertible values; expression typing already produces the
// right type for literals, so only the diagnostic matters here.
func (p *Parser) checkTemplateArgValue(v Value, formal *Type, owner, name, sep string) {
	if v == nil || formal == nil || name == "" || owner == "" {
		return
	}
	vt := v.Type()
	if vt.Kind == InvalidType || vt.Kind == UnsetType {
		return
	}
	if p.typeConvertible(vt, *formal) {
		return
	}
	// TGParser stores the qualified name in the argument RecordVal
	// (QualifyName uses "::" inside a multiclass, ":" otherwise) and appends
	// the offending value (TGParser.cpp:4778).
	p.errAt(v.Span(), "E_PARSE_ARG_TYPE",
		"Value specified for template argument '"+owner+sep+name+"' is of type "+
			vt.String()+"; expected type "+formal.String()+": "+InitText(v))
}

// typeConvertible resolves record types through the class hierarchy, which
// Type.ConvertibleTo cannot do (it only knows names). RecordRecTy converts to
// another record type when every target class is one of its classes or a
// superclass of one.
func (p *Parser) typeConvertible(t, u Type) bool {
	if t.Kind == InvalidType || u.Kind == InvalidType || u.Kind == UnsetType {
		return true
	}
	if t.Kind == ListType && u.Kind == ListType {
		if t.Elem == nil || u.Elem == nil {
			return true
		}
		return p.typeConvertible(*t.Elem, *u.Elem)
	}
	if t.Kind == RecordType && u.Kind == RecordType {
		if len(u.Recs) == 0 {
			return true
		}
		if len(t.Recs) == 0 {
			return false
		}
		for _, target := range u.Recs {
			ok := false
			for _, mine := range t.Recs {
				if p.nameIsSubclass(mine, target) {
					ok = true
					break
				}
			}
			if !ok {
				return false
			}
		}
		return true
	}
	return t.ConvertibleTo(u)
}

func (p *Parser) nameIsSubclass(name, target string) bool {
	if name == "" {
		return false
	}
	if name == target {
		return true
	}
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == "" || seen[n] {
			return false
		}
		seen[n] = true
		ri := p.recordInfoFor(n)
		if ri == nil {
			return false
		}
		for _, pn := range ri.parents {
			if pn == target || walk(pn) {
				return true
			}
		}
		return false
	}
	return walk(name)
}

// ---------------------------------------------------------------------------
// Ranges

func (p *Parser) parseRangeList() ([]int32, bool) {
	var out []int32
	r, ok := p.parseRangePiece(nil)
	if !ok {
		return nil, false
	}
	out = append(out, r...)
	for p.eat(Comma) {
		r, ok := p.parseRangePiece(nil)
		if !ok {
			return nil, false
		}
		out = append(out, r...)
	}
	return out, true
}

// parseRangePiece parses `N`, `N...M`, `N-M` or `N M`. first is the already
// parsed start value, or nil when it has not been read yet.
func (p *Parser) parseRangePiece(first Value) ([]int32, bool) {
	var cur Value
	if first != nil {
		cur = first
	} else {
		cur = p.parseValue(p.curRecOrNil(), nil, parseValueMode)
		if cur == nil {
			return nil, false
		}
	}
	start, ok := constInt(cur)
	if !ok {
		p.errTok("E_PARSE_RANGE_INT", "expected integer or bitrange")
		return nil, false
	}
	if start < 0 {
		p.errTok("E_PARSE_RANGE_NEG", "invalid range, cannot be negative")
		return nil, false
	}
	var end int64
	switch p.cur().Kind {
	case DotDotDot, Minus:
		p.advance()
		v := p.parseValue(p.curRecOrNil(), nil, parseValueMode)
		if v == nil {
			return nil, false
		}
		e, ok := constInt(v)
		if !ok {
			p.errTok("E_PARSE_RANGE_END", "expected integer value as end of range")
			return nil, false
		}
		end = e
	case IntVal:
		// Deprecated `0 5` spelling: a bare positive literal is a negative end.
		end = -int64(p.cur().Num)
		p.advance()
	default:
		return []int32{int32(start)}, true
	}
	if end < 0 {
		p.errTok("E_PARSE_RANGE_NEG", "invalid range, cannot be negative")
		return nil, false
	}
	var out []int32
	if start < end {
		for i := start; i <= end; i++ {
			out = append(out, int32(i))
		}
	} else {
		for i := start; i >= end; i-- {
			out = append(out, int32(i))
		}
	}
	return out, true
}

// constInt folds the cases the parser needs: literals, and names/fields whose
// literal value was recorded while parsing.
func constInt(v Value) (int64, bool) {
	switch t := v.(type) {
	case *IntValue:
		return int64(t.Num), true
	case *VarValue:
		if t.Ty.Kind == IntType || t.Ty.Kind == BitType || t.Ty.Kind == BitsType {
			if t.Lit != nil {
				return constInt(t.Lit)
			}
			return 0, false
		}
	case *FieldValue:
		if t.Ty.Kind == IntType || t.Ty.Kind == BitType || t.Ty.Kind == BitsType {
			if t.Lit != nil {
				return constInt(t.Lit)
			}
			return 0, false
		}
	}
	return 0, false
}
