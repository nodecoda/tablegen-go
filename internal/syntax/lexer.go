package syntax

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"

	"tblgen/internal/diag"
)

// Lexer is a hand-written, total lexer: it never panics, every token carries a
// span, and a malformed token produces a diagnostic plus line-level recovery
// instead of aborting the file.
//
// It follows LLVM's TGLexer.cpp closely, including three behaviours that are
// easy to get wrong:
//   - a token starting with a digit is an identifier if the next char is not a
//     digit (so `foo#8i` lexes as Id, Paste, Id);
//   - `#` at the start of a line is a preprocessor directive, elsewhere it is
//     the paste operator;
//   - `/* */` comments nest.
type Lexer struct {
	file      string
	src       string
	i         int
	line      int
	col       int
	toks      []Token
	diags     diag.Collector
	macros    map[string]bool
	prep      []prepEntry
	start     diag.Pos
	startOff  int
	lineStart bool
	done      bool

	// includeDirs are the -I directories, searched after the filename itself
	// (matching SourceMgr::OpenIncludeFile: the literal path first, then each
	// include directory in order). The including file's directory is *not*
	// searched, because llvm-tblgen does not add it either.
	includeDirs []string
	included    []includeFrame
	// deps are the resolved include paths in encounter order, for the
	// make-style dependency file (-d). SourceMgr keeps the same list.
	deps []string
}

// includeFrame is the lexer state of an including file. LLVM pushes a new
// source buffer plus a fresh preprocessor control stack per include; macros
// stay global. Restoring this frame on EOF does the same.
type includeFrame struct {
	file      string
	src       string
	i         int
	line      int
	col       int
	prep      []prepEntry
	lineStart bool
}

// maxIncludeDepth is a safety valve: LLVM has no cycle check and would recurse
// forever on a self-including file. We stop loudly instead of hanging.
const maxIncludeDepth = 64

type prepEntry struct {
	kind    Kind // Ifdef or Else
	defined bool
	at      diag.Pos
}

// NewLexer prepares a lexer. macros are the -D names.
func NewLexer(file, src string, macros []string) *Lexer {
	return NewLexerWithDirs(file, src, macros, nil)
}

// NewLexerWithDirs additionally resolves `include "..."` against includeDirs.
func NewLexerWithDirs(file, src string, macros, includeDirs []string) *Lexer {
	m := make(map[string]bool, len(macros))
	for _, name := range macros {
		m[name] = true
	}
	// TableGen averages roughly one token per 8 source bytes; presizing keeps
	// the token slice from doubling its way to the final size.
	return &Lexer{file: file, src: src, line: 1, col: 1, macros: m, lineStart: true,
		includeDirs: includeDirs, toks: make([]Token, 0, len(src)/8+8)}
}

// Tokenize is the one-shot form used by tests and the CLI.
func Tokenize(file, src string, macros []string) ([]Token, []diag.Diagnostic) {
	return NewLexer(file, src, macros).Lex()
}

// TokenizeWithDirs is Tokenize with -I directories for include resolution.
func TokenizeWithDirs(file, src string, macros, includeDirs []string) ([]Token, []diag.Diagnostic) {
	toks, diags, _ := TokenizeWithDeps(file, src, macros, includeDirs)
	return toks, diags
}

// TokenizeWithDeps also returns the resolved include paths, which -d writes.
func TokenizeWithDeps(file, src string, macros, includeDirs []string) ([]Token, []diag.Diagnostic, []string) {
	l := NewLexerWithDirs(file, src, macros, includeDirs)
	toks, diags := l.Lex()
	return toks, diags, l.deps
}

func (l *Lexer) pos() diag.Pos { return diag.Pos{File: l.file, Line: l.line, Col: l.col} }

func (l *Lexer) spanFrom(from diag.Pos) diag.Span { return diag.Span{Start: from, End: l.pos()} }

func (l *Lexer) atEnd() bool { return l.i >= len(l.src) }

// peek looks k bytes ahead without consuming. It returns 0 past the end, which
// every caller treats as "not a valid character".
func (l *Lexer) peek(k int) byte {
	if l.i+k >= len(l.src) {
		return 0
	}
	return l.src[l.i+k]
}

// advance consumes one character and folds CRLF/LFCR into a single newline,
// matching getNextChar(). The bool is false only at end of input; a NUL inside
// the file returns (0, true).
func (l *Lexer) advance() (byte, bool) {
	if l.i >= len(l.src) {
		return 0, false
	}
	c := l.src[l.i]
	l.i++
	if c == '\n' || c == '\r' {
		if l.i < len(l.src) && (l.src[l.i] == '\n' || l.src[l.i] == '\r') && l.src[l.i] != c {
			l.i++
		}
		l.line++
		l.col = 1
		return '\n', true
	}
	l.col++
	return c, true
}

func (l *Lexer) emit(k Kind, text string, from diag.Pos, num uint64) {
	l.toks = append(l.toks, Token{Kind: k, Text: text, Num: num, Span: diag.Span{Start: from, End: l.pos()}})
	l.lineStart = false
}

func (l *Lexer) errorf(code, msg string, from diag.Pos) {
	l.diags.Error(code, msg, l.spanFrom(from))
}

// recoverLine makes progress after a malformed construct: drop everything to
// the end of the current line. Valid files never take this path, so it cannot
// disturb a differential comparison on well-formed input.
func (l *Lexer) recoverLine() {
	for !l.atEnd() {
		c, _ := l.advance()
		if c == '\n' {
			l.lineStart = true
			return
		}
	}
}

// Lex returns the token stream (always ending in Eof) and every diagnostic.
func (l *Lexer) Lex() ([]Token, []diag.Diagnostic) {
	for !l.done {
		l.next()
	}
	l.toks = append(l.toks, Token{Kind: Eof, Span: diag.Span{Start: l.pos(), End: l.pos()}})
	l.reportUnclosed()
	return l.toks, l.diags.Diags
}

// reportUnclosed mirrors prepReportPreprocessorStackError: the EOF position is
// reported first, then the *latest* open control, both as errors.
func (l *Lexer) reportUnclosed() {
	if len(l.prep) == 0 {
		return
	}
	at := l.pos()
	l.diags.Error("E_LEX_PREP_UNCLOSED", "reached EOF without matching #endif", diag.Span{Start: at, End: at})
	top := l.prep[len(l.prep)-1]
	l.diags.Error("E_LEX_PREP_CONTROL", "the latest preprocessor control is here", diag.Span{Start: top.at, End: top.at})
	l.prep = nil
	// An unbalanced #ifdef in an included file is fatal for the whole lex, not
	// just for that buffer: LLVM's processEOF/prepSkipRegion returns Error and
	// the parser aborts. Do not resume the including file.
	if len(l.included) > 0 {
		l.done = true
	}
}

func (l *Lexer) processing() bool {
	for _, e := range l.prep {
		if !e.defined {
			return false
		}
	}
	return true
}

func (l *Lexer) next() {
	for {
		if l.done {
			return
		}
		if !l.processing() {
			if !l.skipRegion() {
				l.done = true
				return
			}
			continue
		}

		l.start = l.pos()
		l.startOff = l.i
		c, ok := l.advance()
		if !ok {
			// End of the current buffer: resume the including file if there is
			// one (LLVM's processEOF). Otherwise this is the real end.
			if l.popInclude() {
				continue
			}
			l.done = true
			return
		}

		switch c {
		case 0:
			l.diags.Error("E_LEX_NUL", "NUL character is invalid in source; treated as space", l.spanFrom(l.start))
		case ' ', '\t':
			// whitespace; lineStart is unchanged
		case '\n':
			l.lineStart = true
		case ':':
			l.emit(Colon, "", l.start, 0)
		case ';':
			l.emit(Semi, "", l.start, 0)
		case ',':
			l.emit(Comma, "", l.start, 0)
		case '<':
			l.emit(Less, "", l.start, 0)
		case '>':
			l.emit(Greater, "", l.start, 0)
		case ']':
			l.emit(RSquare, "", l.start, 0)
		case '{':
			l.emit(LBrace, "", l.start, 0)
		case '}':
			l.emit(RBrace, "", l.start, 0)
		case '(':
			l.emit(LParen, "", l.start, 0)
		case ')':
			l.emit(RParen, "", l.start, 0)
		case '=':
			l.emit(Equal, "", l.start, 0)
		case '?':
			l.emit(Question, "", l.start, 0)
		case '#':
			if l.lineStart {
				if k, yes := l.prepIsDirective(); yes {
					if !l.handleDirective(k, true) {
						l.recoverLine()
					}
					continue
				}
			}
			l.emit(Paste, "", l.start, 0)
		case '.':
			if l.peek(0) == '.' {
				l.advance()
				if l.peek(0) == '.' {
					l.advance()
					l.emit(DotDotDot, "...", l.start, 0)
				} else {
					l.errorf("E_LEX_DOT", "invalid '..' punctuation", l.start)
				}
			} else {
				l.emit(Dot, "", l.start, 0)
			}
		case '/':
			switch l.peek(0) {
			case '/':
				l.advance()
				l.skipBCPLComment()
			case '*':
				l.advance()
				l.skipCComment(l.start)
			default:
				l.errorf("E_LEX_CHAR", "unexpected character", l.start)
			}
		case '"':
			l.lexString(l.start)
		case '$':
			l.lexVarName(l.start)
		case '[':
			l.lexBracket(l.start)
		case '!':
			l.lexExclaim(l.start)
		case '-', '+', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
			l.lexNumberOrIdentifier(c)
		default:
			if isIDStart(c) {
				l.lexIdentifier(l.start)
			} else {
				l.errorf("E_LEX_CHAR", "unexpected character", l.start)
			}
		}
		return
	}
}

// lexNumberOrIdentifier reproduces the sign/digit heuristic in LexToken: a
// run that starts with a digit may still be an identifier (`foo#8i`).
func (l *Lexer) lexNumberOrIdentifier(first byte) {
	if isDigit(first) {
		i := 0
		var n byte
		for {
			n = l.peek(i)
			if !isDigit(n) {
				break
			}
			i++
		}
		if n == 'x' || n == 'b' {
			nn := l.peek(i + 1)
			if n == 'b' && (nn == '0' || nn == '1') {
				l.lexNumber(first)
				return
			}
			if n == 'x' && isHex(nn) {
				l.lexNumber(first)
				return
			}
		}
		if isIDStart(n) {
			l.lexIdentifier(l.start)
			return
		}
	}
	l.lexNumber(first)
}

func (l *Lexer) lexIdentifier(from diag.Pos) {
	for isIDChar(l.peek(0)) {
		l.advance()
	}
	text := l.src[l.startOff:l.i]
	if text == "include" {
		// LLVM consumes `include` together with its string in the lexer, so the
		// parser never sees either token. Included tokens are spliced into the
		// stream with their own file:line spans.
		l.lexInclude(from)
		return
	}
	switch text {
	case "true":
		l.emit(TrueVal, text, from, 0)
		return
	case "false":
		l.emit(FalseVal, text, from, 0)
		return
	}
	if k, ok := LookupKeyword(text); ok {
		l.emit(k, text, from, 0)
		return
	}
	l.emit(Id, text, from, 0)
}

func (l *Lexer) lexNumber(first byte) {
	from := l.start
	startOff := l.startOff
	text := func() string { return l.src[startOff:l.i] }

	if first == '0' && (l.peek(0) == 'x' || l.peek(0) == 'b') {
		base, kind := 10, IntVal
		if l.peek(0) == 'x' {
			base = 16
		} else {
			base, kind = 2, BinaryIntVal
		}
		l.advance() // x or b
		digits := 0
		for {
			c := l.peek(0)
			if (base == 16 && isHex(c)) || (base == 2 && (c == '0' || c == '1')) {
				l.advance()
				digits++
				continue
			}
			break
		}
		if digits == 0 {
			l.errorf("E_LEX_NUMBER", "invalid number", from)
			l.recoverLine()
			return
		}
		v, err := strconv.ParseUint(text()[2:], base, 64)
		if err != nil {
			l.errorf("E_LEX_NUMBER_RANGE", "number out of range", from)
			l.recoverLine()
			return
		}
		l.emit(kind, text(), from, v)
		return
	}

	if first == '-' || first == '+' {
		if !isDigit(l.peek(0)) {
			if first == '-' {
				l.emit(Minus, "", from, 0)
			} else {
				l.emit(Plus, "", from, 0)
			}
			return
		}
	}
	for isDigit(l.peek(0)) {
		l.advance()
	}
	raw := text()
	var v uint64
	var err error
	if first == '-' {
		var n int64
		n, err = strconv.ParseInt(raw, 10, 64)
		v = uint64(n)
	} else {
		s := raw
		if first == '+' {
			s = s[1:]
		}
		v, err = strconv.ParseUint(s, 10, 64)
	}
	if err != nil {
		if errors.Is(err, strconv.ErrRange) {
			l.errorf("E_LEX_NUMBER_RANGE", "number out of range", from)
		} else {
			l.errorf("E_LEX_NUMBER", "invalid number", from)
		}
		l.recoverLine()
		return
	}
	l.emit(IntVal, raw, from, v)
}

// lexInclude handles `include "..."` after the keyword itself has been
// consumed. It mirrors TGLexer::LexInclude: the filename is a string token
// read immediately after the keyword, the search order is the literal path
// followed by each -I directory, and the included buffer is pushed with a
// fresh preprocessor control stack (macros remain global).
func (l *Lexer) lexInclude(from diag.Pos) {
	_ = from
	if !l.skipSpaceAndComments() {
		return
	}
	if l.atEnd() || l.peek(0) != '"' {
		l.errorf("E_LEX_INCLUDE", "expected filename after include", l.pos())
		return
	}
	nameStart := l.pos()
	l.advance() // opening quote
	name, ok := l.readString(nameStart)
	if !ok {
		return
	}
	path, src, ok := l.resolveInclude(name)
	if !ok {
		l.diags.Error("E_LEX_INCLUDE_NOT_FOUND",
			"could not find include file '"+name+"'",
			diag.Span{Start: nameStart, End: l.pos()})
		return
	}
	if len(l.included) >= maxIncludeDepth {
		l.diags.Error("E_LEX_INCLUDE_DEPTH",
			"include nesting exceeds "+strconv.Itoa(maxIncludeDepth)+" levels",
			diag.Span{Start: nameStart, End: l.pos()})
		return
	}
	l.included = append(l.included, includeFrame{
		file: l.file, src: l.src, i: l.i, line: l.line, col: l.col,
		prep: l.prep, lineStart: l.lineStart,
	})
	l.deps = append(l.deps, path)
	l.file, l.src, l.i, l.line, l.col = path, src, 0, 1, 1
	l.prep = nil
	l.lineStart = true
}

// skipSpaceAndComments advances past whitespace and comments, which is what
// LexToken does while looking for the string after `include`. It is not used
// for ordinary token lexing, which inlines the same logic in next().
func (l *Lexer) skipSpaceAndComments() bool {
	for {
		if l.atEnd() {
			return true
		}
		switch l.peek(0) {
		case ' ', '\t':
			l.advance()
		case '\n', '\r':
			l.advance()
			l.lineStart = true
		case 0:
			l.advance()
			l.diags.Error("E_LEX_NUL", "NUL character is invalid in source; treated as space", l.spanFrom(l.pos()))
		case '/':
			switch l.peek(1) {
			case '/':
				l.advance()
				l.advance()
				l.skipBCPLComment()
			case '*':
				start := l.pos()
				l.advance()
				l.advance()
				if !l.skipCComment(start) {
					return false
				}
			default:
				return true
			}
		default:
			return true
		}
	}
}

// resolveInclude implements SourceMgr::OpenIncludeFile: try the name as
// given, then <dir>/<name> for each -I directory in order.
func (l *Lexer) resolveInclude(name string) (path, src string, ok bool) {
	candidates := []string{name}
	if !filepath.IsAbs(name) {
		for _, d := range l.includeDirs {
			candidates = append(candidates, filepath.Join(d, name))
		}
	}
	for _, c := range candidates {
		b, err := os.ReadFile(c)
		if err == nil {
			return c, string(b), true
		}
	}
	return "", "", false
}

// popInclude resumes the including file at EOF of an included buffer. It
// returns false when there is nothing to pop (or when the include's
// preprocessor control stack is unbalanced, which stops lexing after the
// diagnostic, matching prepExitInclude).
func (l *Lexer) popInclude() bool {
	if len(l.included) == 0 {
		return false
	}
	if len(l.prep) != 0 {
		l.reportUnclosed()
		l.done = true
		return false
	}
	top := l.included[len(l.included)-1]
	l.included = l.included[:len(l.included)-1]
	l.file, l.src, l.i, l.line, l.col = top.file, top.src, top.i, top.line, top.col
	l.prep = top.prep
	// LLVM: "Leaving an include file means that the next symbol is located at
	// the end of the 'include "..."' construct" and is not at line start.
	l.lineStart = false
	return true
}

func (l *Lexer) lexString(from diag.Pos) {
	if text, ok := l.readString(from); ok {
		l.emit(StrVal, text, from, 0)
	}
}

// readString consumes a string literal (the opening quote is already eaten)
// and returns its value without emitting a token. Include filenames need the
// value only.
func (l *Lexer) readString(from diag.Pos) (string, bool) {
	var buf []byte
	for {
		if l.atEnd() {
			l.diags.Error("E_LEX_STRING_EOF", "end of file in string literal", l.spanFrom(from))
			return "", false
		}
		c := l.peek(0)
		if c == '"' {
			l.advance()
			return string(buf), true
		}
		if c == '\n' || c == '\r' {
			l.diags.Error("E_LEX_STRING_EOL", "end of line in string literal", l.spanFrom(from))
			return "", false
		}
		l.advance()
		if c != '\\' {
			buf = append(buf, c)
			continue
		}
		if l.atEnd() {
			l.diags.Error("E_LEX_STRING_EOF", "end of file in string literal", l.spanFrom(from))
			return "", false
		}
		e := l.peek(0)
		switch e {
		case '\\', '\'', '"':
			buf = append(buf, e)
			l.advance()
		case 't':
			buf = append(buf, '\t')
			l.advance()
		case 'n':
			buf = append(buf, '\n')
			l.advance()
		case '\n', '\r':
			l.diags.Error("E_LEX_ESCAPED_NEWLINE", "escaped newlines not supported in tblgen", l.spanFrom(from))
			l.skipStringRest()
			return "", false
		default:
			l.errorf("E_LEX_ESCAPE", "invalid escape in string literal", l.pos())
			l.skipStringRest()
			return "", false
		}
	}
}

// skipStringRest consumes the remainder of a malformed literal so the lexer can
// resume on the next token, and does not emit a token for it.
func (l *Lexer) skipStringRest() {
	for !l.atEnd() {
		c := l.peek(0)
		if c == '"' {
			l.advance()
			return
		}
		if c == '\n' || c == '\r' {
			return
		}
		l.advance()
	}
}

func (l *Lexer) lexVarName(from diag.Pos) {
	if !isIDStart(l.peek(0)) {
		l.errorf("E_LEX_VARNAME", "invalid variable name", from)
		return
	}
	for isIDChar(l.peek(0)) {
		l.advance()
	}
	l.emit(VarName, l.src[l.startOff+1:l.i], from, 0)
}

// lexBracket handles '[' — either a code fragment `[{ ... }]` or a plain '['.
func (l *Lexer) lexBracket(from diag.Pos) {
	if l.peek(0) != '{' {
		l.emit(LSquare, "", from, 0)
		return
	}
	l.advance() // '{'
	contentStart := l.i
	for {
		if l.atEnd() {
			l.diags.Error("E_LEX_CODE_EOF", "unterminated code block", l.spanFrom(from))
			return
		}
		c, _ := l.advance()
		if c == '}' && l.peek(0) == ']' {
			l.advance()
			contentEnd := l.i - 2
			if contentEnd < contentStart {
				contentEnd = contentStart
			}
			l.emit(CodeFragment, l.src[contentStart:contentEnd], from, 0)
			return
		}
	}
}

func (l *Lexer) lexExclaim(from diag.Pos) {
	if !isAlpha(l.peek(0)) {
		l.errorf("E_LEX_BANG", `invalid "!operator"`, from)
		return
	}
	for isAlpha(l.peek(0)) {
		l.advance()
	}
	name := l.src[l.startOff+1 : l.i]
	k, ok := LookupBangOp(name)
	if !ok {
		l.errorf("E_LEX_BANG", "unknown operator !"+name, from)
		return
	}
	l.emit(k, name, from, 0)
}

func (l *Lexer) skipBCPLComment() {
	for !l.atEnd() {
		c := l.peek(0)
		if c == '\n' || c == '\r' {
			return
		}
		l.advance()
	}
}

// skipCComment consumes a nested /* ... */ comment. The opening "/*" has
// already been consumed.
func (l *Lexer) skipCComment(from diag.Pos) bool {
	depth := 1
	for {
		if l.atEnd() {
			l.diags.Error("E_LEX_COMMENT_EOF", "unterminated comment", l.spanFrom(from))
			return false
		}
		c, _ := l.advance()
		switch c {
		case '*':
			if l.peek(0) == '/' {
				l.advance()
				depth--
				if depth == 0 {
					return true
				}
			}
		case '/':
			if l.peek(0) == '*' {
				l.advance()
				depth++
			}
		}
	}
}

// ---- preprocessor ---------------------------------------------------------

type prepDir struct {
	kind Kind
	word string
}

var prepDirs = []prepDir{
	{Ifdef, "ifdef"},
	{Ifndef, "ifndef"},
	{Else, "else"},
	{Endif, "endif"},
	{Define, "define"},
}

// prepIsDirective is called just past '#'. It mirrors prepIsDirective() in
// TGLexer.cpp, including the rule that a directive word must be followed by
// whitespace, end of line, or a comment.
func (l *Lexer) prepIsDirective() (Kind, bool) {
	for _, d := range prepDirs {
		if l.i+len(d.word) > len(l.src) || l.src[l.i:l.i+len(d.word)] != d.word {
			continue
		}
		next := l.peek(len(d.word))
		if next == ' ' || next == '\t' || next == '\n' || next == '\r' {
			return d.kind, true
		}
		if next == '/' {
			n2 := l.peek(len(d.word) + 1)
			if n2 == '*' || n2 == '/' {
				return d.kind, true
			}
		}
	}
	return 0, false
}

func (l *Lexer) prepLexMacroName() string {
	for l.peek(0) == ' ' || l.peek(0) == '\t' {
		l.advance()
	}
	if !isIDStart(l.peek(0)) {
		return ""
	}
	off := l.i
	for isIDChar(l.peek(0)) {
		l.advance()
	}
	return l.src[off:l.i]
}

// prepSkipDirectiveEnd allows only whitespace and comments after a directive.
// A newline is not consumed: callers rely on that to stay line-aligned.
func (l *Lexer) prepSkipDirectiveEnd() bool {
	for !l.atEnd() {
		switch l.peek(0) {
		case ' ', '\t':
			l.advance()
		case '\n', '\r':
			return true
		case '/':
			switch l.peek(1) {
			case '/':
				l.advance()
				l.advance()
				l.skipBCPLComment()
			case '*':
				l.advance()
				l.advance()
				if !l.skipCComment(l.start) {
					return false
				}
			default:
				l.errorf("E_LEX_PREP_TRAILING", "unexpected character after a preprocessing directive", l.pos())
				return false
			}
		default:
			l.errorf("E_LEX_PREP_TRAILING", "only comments are supported after a preprocessing directive", l.pos())
			return false
		}
	}
	return true
}

// prepSkipLineBegin advances to the first non-whitespace, non-comment byte.
func (l *Lexer) prepSkipLineBegin() bool {
	for !l.atEnd() {
		switch l.peek(0) {
		case ' ':
			l.advance()
		case '\t':
			l.advance()
		case '\n':
			l.advance()
			l.lineStart = true
		case '\r':
			l.advance()
			l.lineStart = true
		case '/':
			if l.peek(1) != '*' {
				return true
			}
			l.advance()
			l.advance()
			if !l.skipCComment(l.start) {
				return false
			}
		default:
			return true
		}
	}
	return true
}

// handleDirective consumes a directive word and updates the control stack.
// live means tokens were being emitted before this directive.
func (l *Lexer) handleDirective(kind Kind, live bool) bool {
	// prepIsDirective() only matched the word; consume it before reading the
	// macro name (LLVM's prepEatPreprocessorDirective).
	for n := 0; n < len(word(kind)); n++ {
		l.advance()
	}
	ctrlAt := l.pos()
	switch kind {
	case Ifdef, Ifndef:
		name := l.prepLexMacroName()
		if name == "" {
			// LLVM points at the macro-name position, not at the '#'.
			l.errorf("E_LEX_PREP_MACRO", "expected macro name after #"+word(kind), l.pos())
			return false
		}
		defined := l.macros[name]
		if kind == Ifndef {
			defined = !defined
		}
		l.prep = append(l.prep, prepEntry{kind: Ifdef, defined: defined, at: ctrlAt})
		if !l.prepSkipDirectiveEnd() {
			return false
		}
		if live && !l.processing() {
			return l.skipRegion()
		}
		return true

	case Else:
		if len(l.prep) == 0 {
			l.errorf("E_LEX_PREP_ELSE", "#else without #ifdef or #ifndef", l.start)
			return false
		}
		top := &l.prep[len(l.prep)-1]
		if top.kind != Ifdef {
			l.errorf("E_LEX_PREP_ELSE", "double #else", l.start)
			return false
		}
		top.kind = Else
		top.defined = !top.defined
		if !l.prepSkipDirectiveEnd() {
			return false
		}
		if live {
			return l.skipRegion()
		}
		return true

	case Endif:
		if len(l.prep) == 0 {
			l.errorf("E_LEX_PREP_ENDIF", "#endif without #ifdef or #ifndef", l.start)
			return false
		}
		if !l.prepSkipDirectiveEnd() {
			return false
		}
		l.prep = l.prep[:len(l.prep)-1]
		return true

	case Define:
		name := l.prepLexMacroName()
		if name == "" {
			l.errorf("E_LEX_PREP_MACRO", "expected macro name after #define", l.pos())
			return false
		}
		if l.macros[name] {
			l.diags.Warn("W_LEX_MACRO_DUP", "duplicate definition of macro: "+name, l.spanFrom(l.start))
		}
		l.macros[name] = true
		return l.prepSkipDirectiveEnd()
	}
	l.errorf("E_LEX_PREP_MACRO", "unknown preprocessing directive", l.start)
	return false
}

func (l *Lexer) skipRegion() bool {
	for {
		for !l.atEnd() && l.peek(0) != '\n' && l.peek(0) != '\r' {
			l.advance()
		}
		if !l.prepSkipLineBegin() {
			return false
		}
		if l.atEnd() {
			break
		}
		if l.peek(0) != '#' {
			continue
		}
		l.start = l.pos()
		l.advance() // '#'
		kind, ok := l.prepIsDirective()
		if !ok || kind == Define {
			continue
		}
		if !l.handleDirective(kind, false) {
			l.prep = nil
			return false
		}
		if l.processing() {
			return true
		}
	}
	l.reportUnclosed()
	return false
}

func word(k Kind) string {
	switch k {
	case Ifdef:
		return "ifdef"
	case Ifndef:
		return "ifndef"
	case Else:
		return "else"
	case Endif:
		return "endif"
	case Define:
		return "define"
	}
	return "?"
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHex(c byte) bool {
	return isDigit(c) || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func isIDStart(c byte) bool { return c == '_' || isAlpha(c) }

// isIDChar matches isValidIDChar(): letters, underscore, and digits after the
// first character. Note that '#' is NOT part of an identifier in current LLVM.
func isIDChar(c byte) bool { return isIDStart(c) || isDigit(c) }
