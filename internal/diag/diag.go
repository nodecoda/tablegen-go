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
// matching the shape of llvm-tblgen's output: the message line, the source
// line, a single caret, then each note with its own source line and caret.
// src is the main input; load resolves include files (nil means "src only").
func (d Diagnostic) Render(src string) string { return d.RenderWith(src, nil) }

func (d Diagnostic) RenderWith(src string, load func(string) (string, bool)) string {
	var b strings.Builder
	b.WriteString(d.String())
	b.WriteByte('\n')
	if line, ok := renderAt(src, d.Span.Start, load); ok {
		b.WriteString(line)
	}
	for _, n := range d.Notes {
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
