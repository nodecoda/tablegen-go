package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Some TableGen tests are containers: a `split-file %s %t` RUN line cuts the
// file into parts, writes them under %t, and the tblgen invocation reads
// %t/<part>.td. Without materializing %t the harness either drops the -I flags
// or feeds the container itself to the binary, which turns a real test into a
// fake "include not found" disagreement. This file implements just the part of
// llvm/utils/split-file/split-file.cpp the corpus actually uses.

type splitPart struct {
	Name string
	Body string
}

// splitMarker reports whether the line starts a part and returns its name.
// split-file.cpp uses the regexp `^(.|//)--- `, i.e. either `//--- name` or a
// single arbitrary character followed by `--- name` (so `;--- name` works too).
func splitMarker(line string) (string, bool) {
	s := strings.TrimRight(line, "\r\n")
	var name string
	switch {
	case strings.HasPrefix(s, "//--- "):
		name = s[len("//--- "):]
	case len(s) >= 5 && s[1:5] == "--- ":
		name = s[5:]
	default:
		return "", false
	}
	if name == "" || strings.TrimSpace(name) != name {
		return "", false
	}
	return name, true
}

// parseSplitFile returns the parts in source order. The body of a part is every
// line after the marker up to the next marker, matching split-file.cpp (which
// does not strip the trailing newline of the last part either).
func parseSplitFile(text string) ([]splitPart, error) {
	var parts []splitPart
	var cur *splitPart
	for _, line := range strings.SplitAfter(text, "\n") {
		name, ok := splitMarker(line)
		if !ok {
			if cur != nil {
				cur.Body += line
			}
			continue
		}
		if cur != nil {
			parts = append(parts, *cur)
		}
		cur = &splitPart{Name: name}
	}
	if cur != nil {
		parts = append(parts, *cur)
	}
	if len(parts) == 0 {
		return nil, fmt.Errorf("no part separator was found")
	}
	return parts, nil
}

// materializeSplit writes every part under a fresh temporary directory and
// returns it. The caller owns the directory and must remove it.
func materializeSplit(file string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	parts, err := parseSplitFile(string(b))
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "tblgen-split-")
	if err != nil {
		return "", err
	}
	for _, p := range parts {
		rel := filepath.FromSlash(p.Name)
		if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "..") {
			os.RemoveAll(dir)
			return "", fmt.Errorf("part name escapes the output directory: %q", p.Name)
		}
		dst := filepath.Join(dir, rel)
		if !strings.HasPrefix(dst, dir+string(os.PathSeparator)) {
			os.RemoveAll(dir)
			return "", fmt.Errorf("part name escapes the output directory: %q", p.Name)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
		if err := os.WriteFile(dst, []byte(p.Body), 0o644); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}
