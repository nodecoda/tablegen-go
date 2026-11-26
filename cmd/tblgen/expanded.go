package main

import (
	"bytes"
	"io"
	"os"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// printExpanded is llvm-tblgen -E: print the token stream after include
// expansion and preprocessing, with no parsing.
//
// The layout is the one lit's preprocess tests pin down:
//   - tokens of one source line share an output line, separated by one space;
//   - the first token of a line is indented to its original column;
//   - entering or leaving an included file starts a new output line, even if
//     the token happens to sit on the same line number;
//   - a multi-line token (a [{ ... }] code block) is emitted verbatim and the
//     next token stays on the line where the token ended.
//
// The token text is sliced out of the source it came from, not reconstructed
// from the token kind: `-E` must reproduce spelling (`!add`, `[{...}]`,
// escapes) rather than a canonical form.
type sourceCache struct {
	mainFile string
	mainText []byte
	files    map[string][]byte
}

func newSourceCache(mainFile string, mainText []byte) *sourceCache {
	return &sourceCache{mainFile: mainFile, mainText: mainText, files: map[string][]byte{}}
}

func (c *sourceCache) text(file string) []byte {
	if file == c.mainFile || file == "" {
		return c.mainText
	}
	if t, ok := c.files[file]; ok {
		return t
	}
	t, err := os.ReadFile(file)
	if err != nil {
		t = nil
	}
	c.files[file] = t
	return t
}

// slice returns the exact source text a token spans. Cols are 1-based bytes.
func (c *sourceCache) slice(sp diag.Span) string {
	src := c.text(sp.Start.File)
	if len(src) == 0 {
		return ""
	}
	lines := bytes.Split(src, []byte("\n"))
	if sp.Start.Line < 1 || sp.End.Line > len(lines) {
		return ""
	}
	start := lines[sp.Start.Line-1]
	if sp.Start.Col-1 > len(start) {
		return ""
	}
	if sp.Start.Line == sp.End.Line {
		end := sp.End.Col - 1
		if end > len(start) {
			end = len(start)
		}
		return string(start[sp.Start.Col-1 : end])
	}
	var b bytes.Buffer
	b.Write(start[sp.Start.Col-1:])
	for l := sp.Start.Line; l < sp.End.Line-1; l++ {
		b.WriteByte('\n')
		b.Write(lines[l])
	}
	b.WriteByte('\n')
	last := lines[sp.End.Line-1]
	end := sp.End.Col - 1
	if end > len(last) {
		end = len(last)
	}
	b.Write(last[:end])
	return b.String()
}

func printExpanded(w io.Writer, toks []syntax.Token, src *sourceCache) error {
	var b strings.Builder
	lastFile := ""
	lastEnd := -1
	first := true
	for _, t := range toks {
		if t.Kind == syntax.Eof {
			break
		}
		text := src.slice(t.Span)
		if text == "" {
			text = t.Text
		}
		if text == "" {
			text = t.Kind.String()
		}
		start := t.Span.Start
		if first {
			for i := 1; i < start.Col; i++ {
				b.WriteByte(' ')
			}
			first = false
		} else if start.File != lastFile || start.Line != lastEnd {
			b.WriteByte('\n')
			for i := 1; i < start.Col; i++ {
				b.WriteByte(' ')
			}
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(text)
		lastFile = start.File
		lastEnd = t.Span.End.Line
		if lastEnd < start.Line {
			lastEnd = start.Line
		}
	}
	if !first {
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}
