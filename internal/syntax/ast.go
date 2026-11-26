package syntax

import "strings"

// The AST is deliberately dull: plain structs, no closures, no evaluation.
// Every node carries a source span, so later phases (resolution, evaluation,
// diagnostics) can point back at the text without re-lexing.
//
// Grammar the shapes come from: llvm/lib/TableGen/TGParser.cpp (LLVM 24.0.0git).
// Comments name the C++ production each node mirrors.

// Node is anything with a source span.
type Node interface{ Span() Span }

// Base is embedded by every node to supply the span.
type Base struct{ At Span }

func (b Base) Span() Span { return b.At }

// ValueBase adds the type computed while parsing. `Ty` is what C++ calls the
// RecTy of a TypedInit; `Invalid` means "not statically known here".
type ValueBase struct {
	Base
	Ty Type
}

func (v *ValueBase) Type() Type { return v.Ty }

// ---------------------------------------------------------------------------
// Types

type TypeKind uint8

const (
	InvalidType TypeKind = iota // unknown / not deduced
	UnsetType                   // `?` has no type
	IntType
	BitType
	BitsType
	StringType
	CodeType
	DagType
	ListType
	RecordType
)

// Type is a cheap, comparable description of a TableGen type. Record types keep
// class *names*, not pointers, so the AST stays acyclic and dumpable.
type Type struct {
	Kind TypeKind
	Bits int      // BitsType
	Elem *Type    // ListType
	Recs []string // RecordType (intersection of classes; nil entry means "any record")
	// IsClass marks a RecordType that denotes a class (a type name or a
	// `Class<...>` application) rather than a reference to a def. TableGen
	// keeps classes and defs in separate namespaces and they may share a name
	// (Attributes.td's `def Returned` vs Intrinsics.td's `class Returned`), so
	// the record phase must know which one the syntax meant.
	IsClass bool
}

func (t Type) String() string {
	switch t.Kind {
	case IntType:
		return "int"
	case BitType:
		return "bit"
	case BitsType:
		return "bits<" + itoa(t.Bits) + ">"
	case StringType:
		return "string"
	case CodeType:
		return "code"
	case DagType:
		return "dag"
	case ListType:
		if t.Elem == nil {
			return "list<?>"
		}
		return "list<" + t.Elem.String() + ">"
	case RecordType:
		// RecordRecTy::getAsString: one class prints its name, the empty set
		// prints "{}", and a wider set prints "{A, B}" (Record.cpp:272-284).
		if len(t.Recs) == 0 {
			return "{}"
		}
		if len(t.Recs) == 1 {
			return t.Recs[0]
		}
		return "{" + strings.Join(t.Recs, ", ") + "}"
	case UnsetType:
		return "?"
	default:
		return "<invalid>"
	}
}

// Equal is structural equality for the scalar kinds and by-name equality for
// records, which is enough for every check the parser performs.
func (t Type) Equal(u Type) bool {
	if t.Kind != u.Kind {
		return false
	}
	switch t.Kind {
	case BitsType:
		return t.Bits == u.Bits
	case ListType:
		if t.Elem == nil || u.Elem == nil {
			return t.Elem == u.Elem
		}
		return t.Elem.Equal(*u.Elem)
	case RecordType:
		if len(t.Recs) != len(u.Recs) {
			return false
		}
		for i := range t.Recs {
			if t.Recs[i] != u.Recs[i] {
				return false
			}
		}
		return true
	}
	return true
}

// Convertible reports whether a value of type t may be used where u is
// expected. It is a conservative subset of RecTy::typeIsConvertibleTo: ints,
// bits and bit all convert to each other; record types convert to "any record"
// and to themselves.
// ConvertibleTo is RecTy::typeIsConvertibleTo: whether a value of type t may
// be converted to u without an explicit !cast. The int-like rules are exact
// (bits<2> does not convert to bits<3>, a bit converts only to bits<1>);
// resolveTypes depends on the asymmetry, because it prefers the target type.
func (t Type) ConvertibleTo(u Type) bool {
	if t.Kind == InvalidType || u.Kind == InvalidType || u.Kind == UnsetType {
		return true
	}
	if t.Equal(u) {
		return true
	}
	switch t.Kind {
	case IntType:
		return u.Kind == BitType || u.Kind == BitsType
	case BitType:
		return u.Kind == IntType || (u.Kind == BitsType && u.Bits == 1)
	case BitsType:
		return u.Kind == IntType || (u.Kind == BitType && t.Bits == 1)
	case ListType:
		if u.Kind != ListType {
			return false
		}
		if t.Elem == nil || u.Elem == nil {
			return true
		}
		return t.Elem.ConvertibleTo(*u.Elem)
	case RecordType:
		// By-name approximation; the parser resolves the class hierarchy.
		return u.Kind == RecordType && (len(u.Recs) == 0 || t.Equal(u))
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ---------------------------------------------------------------------------
// File and statements

// File is ObjectList.
type File struct {
	Objects []Object
	// Sources lists every include the lexer opened, in encounter order. The
	// dependency file (-d) reports them together with the main input.
	Sources []string
	// DynLoopNames reports that some statement-level foreach creates record
	// names the parser could not materialize (they depend on a bang
	// expression or a class field). The driver then lowers once to learn the
	// names and parses a second time with Known.Loops filled in.
	DynLoopNames bool
	// DynDefmNames is the same signal for a top-level defm whose records get
	// names from a resolved multiclass expression, e.g. `def Ext.Suffix` or
	// `def ""#x#VTI.Suffix` (RISCV and ARM). The driver fills in
	// Known.Defms from the first lowering pass.
	DynDefmNames bool
}

// KnownDef is a record created by a statement-level foreach or defm, fed back
// into a later parse so that later statements can refer to it with its real
// type.
type KnownDef struct {
	Name    string
	Parents []string
}

// Known carries the names a first lowering pass learned. Loops is keyed by
// ForeachStmt.Seq and Defms by DefmStmt.Seq; the parser installs each entry
// when it finishes parsing that statement, which is when LLVM's parser would
// have added the records to the keeper.
type Known struct {
	Loops map[int][]KnownDef
	Defms map[int][]KnownDef
}

// Object is any top-level (or loop/block) statement.
type Object interface {
	Node
	objectNode()
}

// BodyItem is anything allowed inside a class or def body.
type BodyItem interface {
	Node
	bodyItemNode()
}

// ClassStmt ::= CLASS ID TemplateArgList? ObjectBody
type ClassStmt struct {
	Base
	Name         string
	NameSpan     Span
	TemplateArgs []*Decl
	Parents      []*ParentRef
	Body         []BodyItem
	DeclOnly     bool // `class X;` (no body), legal in TableGen
}

// DefStmt ::= DEF ObjectName ObjectBody
type DefStmt struct {
	Base
	Name    Value // nil == anonymous (`def { ... }`); Value ::= UnsetInit
	Parents []*ParentRef
	Body    []BodyItem
	// info is the parser's view of this def (fields, parents). It is reused
	// when a loop body's def names are materialized ahead of the record phase.
	info *recordInfo
}

// DefmStmt ::= DEFM ObjectName ':' ParentClassListNE ';'
type DefmStmt struct {
	Base
	Name    Value
	Parents []*ParentRef
	// ClassStart is the index of the first non-multiclass parent. A defm may
	// end its list with ordinary classes; everything before is a multiclass.
	ClassStart int
	// Seq numbers statement-level defms (nested ones are -1). A record name
	// built from a resolved multiclass expression is unknown while parsing;
	// Seq is the key the driver uses to feed those names back for a second
	// parse. See File.DynDefmNames.
	Seq int
}

// MultiClassStmt ::= MULTICLASS ID TemplateArgList? (':' MultiClassList)? '{' MultiClassObject+ '}'
type MultiClassStmt struct {
	Base
	Name         string
	NameSpan     Span
	TemplateArgs []*Decl
	Parents      []*ParentRef
	Body         []Object
}

// DefvarStmt ::= DEFVAR Id '=' Value ';'
type DefvarStmt struct {
	Base
	Name     string
	NameSpan Span
	Value    Value
}

// DefsetStmt ::= DEFSET Type Id '=' '{' ObjectList '}'
type DefsetStmt struct {
	Base
	ElemType Type
	// TypeSpan is the element type's own location, which is what
	// DefsetRecord::Loc holds (TGParser.cpp:4018) and where addDefOne points
	// its "location of defset declaration" note.
	TypeSpan Span
	Name     string
	NameSpan Span
	Objects  []Object
}

// DeftypeStmt ::= DEFTYPE Id '=' Type ';'
type DeftypeStmt struct {
	Base
	Name     string
	NameSpan Span
	Type     Type
}

// ForeachStmt ::= FOREACH Declaration IN ('{' ObjectList '}' | Object)
type ForeachStmt struct {
	Base
	Var     string
	VarSpan Span
	// Exactly one of List / Range is set. Bare ranges (`0-4`, `{0,2-4}`)
	// are resolved to integers while parsing, as in ParseRangeList.
	List  Value
	Range []int32
	Body  []Object
	// Seq numbers statement-level loops (nested loops are -1). LLVM resolves a
	// loop as soon as its statement ends, so records it creates are visible to
	// the following statements; Seq is the key the driver uses to feed those
	// names back for a second parse.
	Seq int
}

// IfStmt ::= IF Value THEN IfBody (ELSE IfBody)?
type IfStmt struct {
	Base
	Cond Value
	Then []Object
	Else []Object // nil when absent
}

// LetStmt ::= LET LetList IN ('{' ObjectList '}' | Object)
type LetStmt struct {
	Base
	Binds []*LetBind
	Body  []Object
}

// LetBind ::= [append|prepend] ID OptionalRangeList '=' Value
type LetBind struct {
	Base
	Mode      LetMode
	Field     string
	FieldSpan Span
	Bits      []int32
	Value     Value
}

type LetMode uint8

const (
	LetReplace LetMode = iota
	LetAppend
	LetPrepend
)

func (m LetMode) String() string {
	switch m {
	case LetAppend:
		return "append"
	case LetPrepend:
		return "prepend"
	}
	return "replace"
}

// AssertStmt ::= ASSERT Value ',' Value ';'
type AssertStmt struct {
	Base
	Cond    Value
	Message Value
}

// DumpStmt ::= DUMP Value ';'
type DumpStmt struct {
	Base
	Value Value
}

// IncludeStmt ::= INCLUDE STRVAL
//
// LLVM resolves includes inside the lexer, so a parser-level Include node is a
// deviation. It exists until M5 moves include expansion into the lexer; keeping
// it as data makes that move mechanical.
type IncludeStmt struct {
	Base
	File     string
	FileSpan Span
}

// Decl ::= ['field'] Type Id ('=' Value)?   (field declaration / template arg)
type Decl struct {
	Base
	IsField  bool
	IsArg    bool // parsed inside <...>
	Type     Type
	Name     string
	NameSpan Span
	Value    Value // template-arg default, or field initializer
}

// LetItem ::= LET [append|prepend] ID OptionalRangeList '=' Value ';'
type LetItem struct {
	Base
	Bind *LetBind
}

func (*ClassStmt) objectNode()      {}
func (*DefStmt) objectNode()        {}
func (*DefmStmt) objectNode()       {}
func (*MultiClassStmt) objectNode() {}
func (*DefvarStmt) objectNode()     {}
func (*DefsetStmt) objectNode()     {}
func (*DeftypeStmt) objectNode()    {}
func (*ForeachStmt) objectNode()    {}
func (*IfStmt) objectNode()         {}
func (*LetStmt) objectNode()        {}
func (*AssertStmt) objectNode()     {}
func (*DumpStmt) objectNode()       {}
func (*IncludeStmt) objectNode()    {}

func (*Decl) bodyItemNode()       {}
func (*LetItem) bodyItemNode()    {}
func (*DefvarStmt) bodyItemNode() {}
func (*AssertStmt) bodyItemNode() {}
func (*DumpStmt) bodyItemNode()   {}

// ParentRef ::= ClassID ('<' ArgValueList '>')?
type ParentRef struct {
	Base
	Name     string
	NameSpan Span
	Args     []*Arg
	// MultiClass records whether the name resolved to a multiclass when this
	// was parsed. Defm needs the distinction; ordinary parents never set it.
	MultiClass bool
}

// ArgValueList ::= Positional* (Named '=' Value)*
type Arg struct {
	Base
	Name     string // empty for positional
	NameSpan Span
	Value    Value
}

// ---------------------------------------------------------------------------
// Values

// Value is any expression. All spans and types are data, never computed by a
// method that needs the record graph.
type Value interface {
	Node
	Type() Type
	valueNode()
}

// UnsetValue is `?`.
type UnsetValue struct{ ValueBase }

// IntValue is a decimal literal (including negative, which the lexer folds) or
// true/false.
type IntValue struct {
	ValueBase
	Num uint64
}

// BitsValue is `{a, b, ...}` or a 0b literal. Elems are stored in source order
// (most significant first), matching how they are written.
type BitsValue struct {
	ValueBase
	Elems []Value
	Num   uint64 // raw value for 0b literals
}

// StringValue is a string literal, a run of concatenated literals, or a code
// fragment (`[{ ... }]`).
type StringValue struct {
	ValueBase
	Text string
	Code bool
}

// ListValue is `[a, b]` plus an optional element type `[a, b]<int>`.
type ListValue struct {
	ValueBase
	Elems    []Value
	ElemType *Type // explicit <T> after the closing bracket, if present
}

// DagValue is `(op arg, name: arg, $x)`.
type DagValue struct {
	ValueBase
	Op     Value
	OpName string
	Args   []*DagArg
}

type DagArg struct {
	Base
	Name  string // empty when anonymous
	Value Value  // UnsetValue for the `$x` shorthand
}

// VarValue is a reference to a variable, template arg, class or def. Name is
// the identifier text as written.
type VarValue struct {
	ValueBase
	Name  string
	Lit   Value // literal value when statically known (for integer ranges)
	Subst bool  // a defvar: substitute Lit before record/environment lookup
	// ArgRef marks a name the parser resolved to a class or multiclass
	// template argument. TGParser qualifies such a reference immediately, so a
	// field added later (or inherited by a child) must not steal the name.
	ArgRef bool
	// ArgQual is that qualified name ("Extension:Implies", "MC::NAME"). It is
	// fixed at the parse site: resolving it against the instantiating record
	// would find the instance's own field of the same name.
	ArgQual string
	// GlobalRef marks a name that resolved to a top-level definition while
	// parsing. LLVM turns it into a DefInit immediately, so later record
	// fields may not shadow it (RegisterOperand<RegClass> passes the global
	// `RegClass` while defining a field of the same name).
	GlobalRef bool
	// GlobalVar marks a GlobalRef that resolved to a top-level defvar or
	// defset rather than a def. RecordKeeper::getGlobal checks Defs before
	// ExtraGlobals, so a def of the same name defined later still wins.
	GlobalVar bool
	// Lexical marks a name bound by a loop or a local variable at parse time.
	// It shadows a class template argument of the same name, so the record
	// phase must not qualify it with the class name.
	Lexical bool
}

// FieldValue is `base.field`.
type FieldValue struct {
	ValueBase
	Base      Value
	Field     string
	FieldSpan Span
	Lit       Value // literal field value when statically known
}

// SliceValue is `base[i]` / `base[i, j]` / `base[a...b]`; Index keeps the
// written elements and ranges in source order.
type SliceValue struct {
	ValueBase
	Base       Value
	Index      []*SliceElem
	ListResult bool // written as a slice (trailing comma, range, or list index)
}

type SliceElem struct {
	Base
	Lo, Hi Value // Hi nil for a single index
}

// BitRangeValue is `base{7,4}`.
type BitRangeValue struct {
	ValueBase
	Base Value
	Bits []int32
}

// PasteValue is `lhs # rhs` (string paste, or list concat when LHS is a list).
type PasteValue struct {
	ValueBase
	LHS, RHS Value
}

// ClassApplyValue is `Class<args>`: an anonymous def inheriting from Class.
type ClassApplyValue struct {
	ValueBase
	Class     string
	ClassSpan Span
	Args      []*Arg
	// FoldName is the anonymous record the application was folded to while
	// parsing (VarDefInit::Fold in TGParser.cpp:3041): once every argument is
	// concrete the Init tree holds a DefInit and prints that record's name.
	// Empty means the application is still unresolved and prints as written.
	FoldName string
}

// BangValue is every `!operator` form. The header (`!op` + optional `<T>`) is
// shared; the shapes below cover `(a, b)`, `(c: v, ...)`, `(id, list, expr)`
// and `(init, list, x, y, expr)` without inventing a node per operator.
type BangValue struct {
	ValueBase
	Op      Kind
	OpSpan  Span
	TypeArg Type // from `!op<T>`, if written
	// Args holds positional arguments in order. For !cond/!switch the values
	// live in Cases instead; VarNames holds the identifiers bound by
	// !foreach/!filter/!sort/!foldl.
	Args     []Value
	Cases    []*BangCase
	VarNames []string
}

type BangCase struct {
	Base
	Key   Value // nil for the default arm of !switch
	Value Value
}

func (*UnsetValue) valueNode()      {}
func (*IntValue) valueNode()        {}
func (*BitsValue) valueNode()       {}
func (*StringValue) valueNode()     {}
func (*ListValue) valueNode()       {}
func (*DagValue) valueNode()        {}
func (*VarValue) valueNode()        {}
func (*FieldValue) valueNode()      {}
func (*SliceValue) valueNode()      {}
func (*BitRangeValue) valueNode()   {}
func (*PasteValue) valueNode()      {}
func (*ClassApplyValue) valueNode() {}
func (*BangValue) valueNode()       {}
