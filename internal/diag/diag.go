// Package diag is the lowest layer: source positions and diagnostics.
// Everything above (tokens, AST, records) speaks in these types, which is what
// makes `explain` and drift checks possible at every stage.
package diag

import (
	"fmt"
	"strings"
)

// Pos is a 1-based source position.
type Pos struct {
	File string
	Line int
	Col  int
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
)

func (s Severity) String() string {
	if s == Warning {
		return "warning"
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
// matching the shape of llvm-tblgen's output closely enough to read side by side.
func (d Diagnostic) Render(src string) string {
	var b strings.Builder
	b.WriteString(d.String())
	b.WriteByte('\n')
	lines := strings.Split(src, "\n")
	if d.Span.Start.File != "" && d.Span.Start.Line >= 1 && d.Span.Start.Line <= len(lines) {
		line := lines[d.Span.Start.Line-1]
		b.WriteString(line)
		b.WriteByte('\n')
		col := d.Span.Start.Col
		if col < 1 {
			col = 1
		}
		b.WriteString(strings.Repeat(" ", col-1))
		// TGParser reports at an SMLoc, not an SMRange, so llvm-tblgen always
		// prints exactly one caret - every caret in the whole test corpus is a
		// single '^'. Underlining the token span (as we used to) made -stderr
		// differ on every diagnostic that pointed at a named token.
		b.WriteByte('^')
		b.WriteByte('\n')
	}
	for _, n := range d.Notes {
		b.WriteString(fmt.Sprintf("%s: note: %s\n", n.Pos, n.Message))
	}
	return b.String()
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
