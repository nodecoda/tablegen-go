// Package syntax holds the surface layer: tokens, lexer, AST, parser.
//
// The token inventory is transcribed from LLVM's TGLexer.h (TokKind) rather
// than reconstructed from memory. Table is the single source for both the
// lexer and the parser's expectations.
package syntax

import (
	"fmt"

	"github.com/nodecoda/tablegen-go/internal/diag"
)

// Pos and Span live in diag so that diagnostics do not import the syntax layer.
type Pos = diag.Pos
type Span = diag.Span

// Kind is a TableGen token kind. The ordering follows TGLexer.h so that the
// two can be diffed by eye.
type Kind int

const (
	Eof Kind = iota
	ErrorTok

	// Punctuation with no payload.
	Minus
	Plus
	LSquare
	RSquare
	LBrace
	RBrace
	LParen
	RParen
	Less
	Greater
	Colon
	Semi
	Comma
	Dot
	Equal
	Question
	Paste // '#'
	DotDotDot

	// Literals.
	TrueVal
	FalseVal
	IntVal
	BinaryIntVal

	// Preprocessor-only tokens (never produced by Lex; kept for completeness).
	Ifdef
	Ifndef
	Else
	Endif
	Define

	// Reserved keywords.
	Bit
	Bits
	Code
	Dag
	ElseKW
	Field
	In
	Include
	Int
	List
	String
	Then

	// Object starts. Contiguous, so range checks need no sentinel constants:
	// a sentinel alias in the middle of an iota block makes every following
	// bare line repeat *that expression* instead of iota. Cost us a build.
	AssertTok
	Class
	Def
	Defm
	Defset
	Deftype
	Defvar
	Dump
	Foreach
	If
	Let
	MultiClass

	// Bang operators.
	XConcat
	XAdd
	XSub
	XMul
	XDiv
	XNot
	XLog2
	XAnd
	XOr
	XXor
	XSra
	XSrl
	XShl
	XListConcat
	XListFlatten
	XListSplat
	XStrConcat
	XInterleave
	XSubstr
	XFind
	XMatch
	XCast
	XSubst
	XForEach
	XFilter
	XFoldl
	XHead
	XTail
	XSize
	XEmpty
	XInitialized
	XInstances
	XIf
	XCond
	XSwitch
	XEq
	XIsA
	XDag
	XNe
	XLe
	XLt
	XGe
	XGt
	XSetDagOp
	XGetDagOp
	XSetDagOpName
	XGetDagOpName
	XExists
	XListRemove
	XToLower
	XToUpper
	XRange
	XSort
	XGetDagArg
	XGetDagName
	XSetDagArg
	XSetDagName
	XRepr

	// String-valued tokens.
	Id
	StrVal
	VarName
	CodeFragment
)

// Token is one lexed token. Text is the payload for Id/StrVal/VarName/
// CodeFragment and for IntVal (raw spelling, so diagnostics can echo it).
type Token struct {
	Kind Kind
	Text string
	// Num is the value for IntVal/BinaryIntVal. A negative decimal is stored as
	// its two's-complement bit pattern, matching LLVM's int64_t CurIntVal.
	Num  uint64
	Span Span
}

func (k Kind) IsBangOp() bool { return XConcat <= k && k <= XRepr }

func (k Kind) IsObjectStart() bool { return AssertTok <= k && k <= MultiClass }

func (k Kind) IsStringValued() bool { return Id <= k && k <= CodeFragment }

// keywords mirrors the StringSwitch in TGLexer.cpp:422-447, plus the object
// starts in TGLexer.cpp:430-441. `true`/`false` are literals, not keywords.
var keywords = map[string]Kind{
	"int": Int, "bit": Bit, "bits": Bits, "string": String, "list": List,
	"code": Code, "dag": Dag, "class": Class, "def": Def, "defm": Defm,
	"defset": Defset, "deftype": Deftype, "defvar": Defvar, "multiclass": MultiClass,
	"field": Field, "let": Let, "in": In, "include": Include, "if": If,
	"then": Then, "else": ElseKW, "assert": AssertTok, "dump": Dump,
	"foreach": Foreach,
}

// bangOps mirrors the StringSwitch in TGLexer.cpp:631-673 verbatim.
var bangOps = map[string]Kind{
	"eq": XEq, "ne": XNe, "le": XLe, "lt": XLt, "ge": XGe, "gt": XGt,
	"if": XIf, "cond": XCond, "switch": XSwitch, "isa": XIsA,
	"head": XHead, "tail": XTail, "size": XSize, "empty": XEmpty,
	"con": XConcat, "dag": XDag,
	"add": XAdd, "sub": XSub, "mul": XMul, "div": XDiv,
	"not": XNot, "logtwo": XLog2, "and": XAnd, "or": XOr, "xor": XXor,
	"shl": XShl, "sra": XSra, "srl": XSrl,
	"cast": XCast, "subst": XSubst, "foldl": XFoldl, "foreach": XForEach,
	"filter": XFilter, "listconcat": XListConcat, "listflatten": XListFlatten,
	"listsplat": XListSplat, "listremove": XListRemove,
	"range": XRange, "sort": XSort, "strconcat": XStrConcat,
	"initialized": XInitialized, "interleave": XInterleave,
	"instances": XInstances, "substr": XSubstr, "find": XFind,
	"tolower": XToLower, "toupper": XToUpper, "repr": XRepr, "match": XMatch,
	"exists":   XExists,
	"setdagop": XSetDagOp, "getdagop": XGetDagOp,
	"setdagopname": XSetDagOpName, "getdagopname": XGetDagOpName,
	"getdagarg": XGetDagArg, "getdagname": XGetDagName,
	"setdagarg": XSetDagArg, "setdagname": XSetDagName,
}

// LookupKeyword reports whether s is a keyword.
func LookupKeyword(s string) (Kind, bool) { k, ok := keywords[s]; return k, ok }

// LookupBangOp reports whether s (without the leading '!') is a bang operator.
func LookupBangOp(s string) (Kind, bool) { k, ok := bangOps[s]; return k, ok }

// Counts are exported for the tests that pin them to the C++ inventory.
func KeywordCount() int { return len(keywords) }
func BangOpCount() int  { return len(bangOps) }

var kindNames = map[Kind]string{
	Eof: "eof", ErrorTok: "error",
	Minus: "minus", Plus: "plus", LSquare: "lsquare", RSquare: "rsquare",
	LBrace: "lbrace", RBrace: "rbrace", LParen: "lparen", RParen: "rparen",
	Less: "less", Greater: "greater", Colon: "colon", Semi: "semi", Comma: "comma",
	Dot: "dot", Equal: "equal", Question: "question", Paste: "paste", DotDotDot: "dotdotdot",
	TrueVal: "true", FalseVal: "false", IntVal: "intval", BinaryIntVal: "binaryintval",
	Ifdef: "ifdef", Ifndef: "ifndef", Else: "else-pp", Endif: "endif", Define: "define",
	Bit: "bit", Bits: "bits", Code: "code", Dag: "dag", ElseKW: "else", Field: "field",
	In: "in", Include: "include", Int: "int", List: "list", String: "string", Then: "then",
	AssertTok: "assert", Class: "class", Def: "def", Defm: "defm", Defset: "defset",
	Deftype: "deftype", Defvar: "defvar", Dump: "dump", Foreach: "foreach", If: "if",
	Let: "let", MultiClass: "multiclass",
	Id: "id", StrVal: "string-literal", VarName: "varname", CodeFragment: "code-fragment",
}

// String returns a stable name for goldens and diagnostics.
func (k Kind) String() string {
	if s, ok := kindNames[k]; ok {
		return s
	}
	if k.IsBangOp() {
		for s, kk := range bangOps {
			if kk == k {
				return "!" + s
			}
		}
	}
	return fmt.Sprintf("kind(%d)", int(k))
}
