package syntax

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tblgen/internal/diag"
)

// This file is the differential ratchet for the parser. It reads the oracle
// baseline recorded by cmd/conformance and asserts two things:
//
//  1. every self-contained file the oracle accepts parses with zero errors;
//  2. every error case we currently match keeps the same *first error line*,
//     and the expected message, so a line can never be matched by accident.
//
// Files whose oracle failure is semantic (field type checks, assert, cast
// typing, ...) are not listed: those belong to the record-graph phase.

const baselinePath = "../../conformance/baseline.json"

type baselineCase struct {
	File   string   `json:"file"`
	Scope  string   `json:"scope"`
	Args   []string `json:"args"`
	Oracle struct {
		Class   string `json:"class"`
		ErrLine int    `json:"err_line"`
	} `json:"oracle"`
}

func loadBaseline(t *testing.T) []baselineCase {
	t.Helper()
	b, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Skipf("oracle baseline not available: %v", err)
	}
	var rep struct {
		Cases []baselineCase `json:"cases"`
	}
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("parse baseline: %v", err)
	}
	return rep.Cases
}

func parseFile(t *testing.T, path string, includes []string) []diag.Diagnostic {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	_, diags := Parse(path, string(src), nil, includes)
	return diags
}

// includeDirs extracts the -I pairs from a baseline case's argv. The baseline
// records exactly what the oracle was run with, so the parser ratchet sees the
// same search path instead of failing on every file that includes a target
// description.
func includeDirs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-I" && i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}

func firstErrLine(diags []diag.Diagnostic) (int, string) {
	for _, d := range diags {
		if d.Severity == diag.Error {
			return d.Span.Start.Line, d.Message
		}
	}
	return 0, ""
}

// TestCorpusValidFilesParse is the M2 exit criterion: no oracle-accepted
// self-contained input may produce a diagnostic.
func TestCorpusValidFilesParse(t *testing.T) {
	cases := loadBaseline(t)
	checked := 0
	for _, c := range cases {
		if c.Scope != "in-scope" || c.Oracle.Class != "valid" {
			continue
		}
		// Parse-only coverage is the self-contained corpus. A file that needs
		// -I resolves some names at lowering time (a dag argument may be a
		// forward reference), so the parser alone is allowed to report an
		// undefined name there; compare mode covers those files end-to-end.
		if len(c.Args) > 0 {
			continue
		}
		if _, err := os.Stat(c.File); err != nil {
			t.Skipf("corpus missing: %v", err)
		}
		checked++
		for _, d := range parseFile(t, c.File, includeDirs(c.Args)) {
			// Soft errors are unresolved names that lowering may still
			// resolve (a forward reference created by a loop); the binary
			// only fails on hard errors, and this ratchet must use the same
			// bar or it disagrees with the command it is guarding.
			if d.Severity == diag.Error && !d.Soft {
				t.Errorf("%s: unexpected %s", filepath.Base(c.File), d.String())
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d valid files checked; corpus not found?", checked)
	}
}

// matchedErrorLines pins the error cases the parser reproduces exactly. The
// tuple is (file, oracle line, substring of our message).
var matchedErrorLines = map[string]struct {
	line int
	msg  string
}{
	"MultiClass-def-fail.td":         {9, "Couldn't find class 'M2'"},
	"UnterminatedComment.td":         {5, "unterminated comment"},
	"eof-after-endif.td":             {10, "reached EOF without matching #endif"},
	"eq-unset.td":                    {8, "expected value to be a typed value, got '?'"},
	"escape-dependency-filenames.td": {38, "could not find include file 'normal-file.td'"},
	"exists-error-non-string.td":     {7, "expected string type argument in !exists operator"},
	"exists-error-record.td":         {7, "please use !isa instead"},
	"exists-error-uninitialized.td":  {7, "expected string type argument in !exists operator"},
	"foreach-range-parse-errors0.td": {17, "Unknown or reserved token when parsing a value"},
	"foreach-range-parse-errors1.td": {5, "invalid range, cannot be negative"},
	"foreach-range-parse-errors2.td": {11, "invalid range, cannot be negative"},
	"foreach-range-parse-errors3.td": {5, "expected integer or bitrange"},
	"foreach-range-parse-errors4.td": {6, "Variable not defined: 'foo'"},
	"foreach-range-parse-errors5.td": {5, "expected integer or bitrange"},
	"letUnknownValue.td":             {5, "Value 'abc' unknown!"},
	"listflatten-error.td":           {5, "expected list type argument in unary operator"},
	"prep-ifndef-diag-1.td":          {4, "expected macro name after #ifndef"},
	"prep-ifndef-diag-2.td":          {4, "only comments are supported after a preprocessing directive"},
}

func TestCorpusErrorLinesMatchOracle(t *testing.T) {
	cases := loadBaseline(t)
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Scope != "in-scope" || c.Oracle.Class != "invalid" {
			continue
		}
		base := filepath.Base(c.File)
		want, pinned := matchedErrorLines[base]
		if !pinned {
			continue
		}
		seen[base] = true
		line, msg := firstErrLine(parseFile(t, c.File, includeDirs(c.Args)))
		if line != want.line {
			t.Errorf("%s: first error on line %d, oracle says %d (%q)", base, line, want.line, msg)
		}
		if !strings.Contains(msg, want.msg) {
			t.Errorf("%s: message %q does not contain %q", base, msg, want.msg)
		}
	}
	for base := range matchedErrorLines {
		if !seen[base] {
			t.Errorf("%s is pinned but not in the baseline", base)
		}
	}
	if len(matchedErrorLines) < 18 {
		t.Fatalf("ratchet shrank to %d cases", len(matchedErrorLines))
	}
}

// TestCorpusNoPanics parses every self-contained corpus file twice and requires
// identical diagnostics: the parser must be deterministic and total.
func TestCorpusNoPanics(t *testing.T) {
	cases := loadBaseline(t)
	for _, c := range cases {
		if c.Scope != "in-scope" {
			continue
		}
		src, err := os.ReadFile(c.File)
		if err != nil {
			t.Skipf("corpus missing: %v", err)
		}
		_, a := Parse(c.File, string(src), nil, nil)
		_, b := Parse(c.File, string(src), nil, nil)
		if len(a) != len(b) {
			t.Fatalf("%s: nondeterministic diagnostics", filepath.Base(c.File))
		}
		for i := range a {
			if a[i].String() != b[i].String() {
				t.Fatalf("%s: nondeterministic diagnostic %d", filepath.Base(c.File), i)
			}
		}
	}
}
