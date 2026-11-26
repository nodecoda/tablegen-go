// Package diag is the lowest layer: source positions and diagnostics.
// Everything above (tokens, AST, records) speaks in these types, which is what
// makes `explain` and drift checks possible at every stage.
package diag

import (
	"fmt"
	"strconv"
	"strings"
)

// Pos is a 1-based source position.
type Pos struct {
	File string
	Line int
	Col  int
	// Inc identifies the include instance the position came from: 0 for the
	// main input, and a positive buffer id for a file opened by `include`.
	// The same file included twice yields identical File:Line:Col from two
	// different instances; SourceMgr keeps one buffer (and therefore one
	// IncludeLoc chain) per instance (SourceMgr.cpp:58-67).
	Inc int
}

func (p Pos) String() string { return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col) }

// Span is a half-open source range.
type Span struct{ Start, End Pos }

func (s Span) String() string { return s.Start.String() }

// Severity separates "this input is wrong" from "this input is suspicious".
type Severity int

const (
	Error Severity = iota
	Warning
	// NoteSev is a standalone note (LLVM's PrintNote), used by `dump`. It
	// never fails the run and reads like the note half of a diagnostic. (The
	// type Note above is a secondary location attached to an error.)
	NoteSev
)

func (s Severity) String() string {
	switch s {
	case Warning:
		return "warning"
	case NoteSev:
		return "note"
	}
	return "error"
}

// Note is a secondary location attached to a diagnostic ("previous definition
// is here").
type Note struct {
	Message string
	Pos     Pos
}

// Diagnostic is one reported problem. Code is stable and greppable; Message is
// allowed to change.
type Diagnostic struct {
	Severity Severity
	Code     string
	Message  string
	Span     Span
	Notes    []Note
	// Soft marks a diagnostic that llvm-tblgen prints as an error but keeps
	// running from (PrintError inside a folding operator). It renders like an
	// error and does not fail the run.
	Soft bool
}

func (d Diagnostic) String() string {
	// A zero span means the message has no source location at all: LLVM's
	// PrintFatalError(Twine) path prints "error: <msg>" and nothing else.
	if d.Span.Start.File == "" && d.Span.Start.Line == 0 {
		return fmt.Sprintf("%s: %s", d.Severity, d.Message)
	}
	return fmt.Sprintf("%s: %s: %s", d.Span.Start, d.Severity, d.Message)
}

// Render prints the diagnostic with the offending source line and a caret,
// matching the shape of llvm-tblgen's output: the message line, the source
// line, a single caret, then each note with its own source line and caret.
// src is the main input; load resolves include files (nil means "src only").
func (d Diagnostic) Render(src string) string { return d.RenderWith(src, nil) }

func (d Diagnostic) RenderWith(src string, load func(string) (string, bool)) string {
	return d.RenderAll(src, load, nil)
}

// IncludeChain reports where an include instance was opened from, mirroring
// SourceMgr::BufferInfo::IncludeLoc. RenderAll prints one
// "Included from <file>:<line>:" line per level above the location, outermost
// first, exactly like SourceMgr::PrintIncludeStack (SourceMgr.cpp:288-299).
type IncludeChain func(pos Pos) (Pos, bool)

func (d Diagnostic) RenderAll(src string, load func(string) (string, bool), parent IncludeChain) string {
	var b strings.Builder
	writeIncludeStack(&b, d.Span.Start, parent)
	b.WriteString(d.String())
	b.WriteByte('\n')
	if line, ok := renderAt(src, d.Span.Start, load); ok {
		b.WriteString(line)
	}
	for _, n := range d.Notes {
		writeIncludeStack(&b, n.Pos, parent)
		b.WriteString(n.Pos.String())
		b.WriteString(": note: ")
		b.WriteString(n.Message)
		b.WriteByte('\n')
		if line, ok := renderAt(src, n.Pos, load); ok {
			b.WriteString(line)
		}
	}
	return b.String()
}

// IncludeSites is the include table: Parents[i] is the location that opened
// include instance i. Zero is the main input and has no parent.
type IncludeSites struct{ Parents []Pos }

// Parent implements IncludeChain.
func (s IncludeSites) Parent(pos Pos) (Pos, bool) {
	if pos.Inc <= 0 || pos.Inc >= len(s.Parents) {
		return Pos{}, false
	}
	return s.Parents[pos.Inc], true
}

// writeIncludeStack emits the include chain that leads to pos, if any.
func writeIncludeStack(b *strings.Builder, pos Pos, parent IncludeChain) {
	if parent == nil || pos.File == "" {
		return
	}
	site, ok := parent(pos)
	if !ok {
		return
	}
	writeIncludeStack(b, site, parent)
	b.WriteString("Included from ")
	b.WriteString(site.File)
	b.WriteString(":")
	b.WriteString(strconv.Itoa(site.Line))
	b.WriteString(":\n")
}

// renderAt prints one source line plus a caret at pos. llvm-tblgen reports at an
// SMLoc, so the caret is always a single '^' (never a ~-range).
func renderAt(src string, pos Pos, load func(string) (string, bool)) (string, bool) {
	if pos.File == "" || pos.Line < 1 {
		return "", false
	}
	text := src
	if pos.File != "-" && load != nil {
		if t, ok := load(pos.File); ok {
			text = t
		}
	}
	lines := strings.Split(text, "\n")
	if pos.Line > len(lines) {
		return "", false
	}
	line := lines[pos.Line-1]
	col := pos.Col
	if col < 1 {
		col = 1
	}
	if col > len(line)+1 {
		col = len(line) + 1
	}
	return line + "\n" + strings.Repeat(" ", col-1) + "^\n", true
}

// Collector accumulates diagnostics in order and never throws.
type Collector struct {
	Diags []Diagnostic
}

func (c *Collector) Error(code, msg string, span Span) {
	c.Diags = append(c.Diags, Diagnostic{Severity: Error, Code: code, Message: msg, Span: span})
}

// SoftError records an error that llvm-tblgen prints without failing.
func (c *Collector) SoftError(code, msg string, span Span) {
	c.Diags = append(c.Diags, Diagnostic{Severity: Error, Code: code, Message: msg, Span: span, Soft: true})
}

// Note records a standalone note (llvm::PrintNote with a location). It is
// rendered like a diagnostic but never affects the exit code.
func (c *Collector) Note(code, msg string, span Span) {
	c.Diags = append(c.Diags, Diagnostic{Severity: NoteSev, Code: code, Message: msg, Span: span})
}

func (c *Collector) Warn(code, msg string, span Span) {
	c.Diags = append(c.Diags, Diagnostic{Severity: Warning, Code: code, Message: msg, Span: span})
}

// HasErrors reports whether any Error-severity diagnostic was collected.
func (c *Collector) HasErrors() bool {
	for _, d := range c.Diags {
		if d.Severity == Error && !d.Soft {
			return true
		}
	}
	return false
}
