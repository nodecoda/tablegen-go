package main

import "testing"

func TestIncludeArgs(t *testing.T) {
	file := "/corpus/sub/case.td"
	cases := []struct {
		run  string
		want []string
	}{
		{"llvm-tblgen %s | FileCheck %s", nil},
		{"llvm-tblgen -I %S/Inputs %s | FileCheck %s", []string{"-I", "/corpus/sub/Inputs"}},
		{"llvm-tblgen -I%S/Inputs %s", []string{"-I", "/corpus/sub/Inputs"}},
		{"llvm-tblgen -I %p/../../include %s", []string{"-I", "/include"}},
		{"llvm-tblgen %s ; llvm-tblgen -I %S/Inputs -I %S/Inputs %s", []string{"-I", "/corpus/sub/Inputs"}},
		// %t is a split-file temp dir and cannot be reconstructed; dropping it
		// keeps both binaries on the same (failing) include.
		{"llvm-tblgen -I %t -I %S %s", []string{"-I", "/corpus/sub"}},
		{"", nil},
	}
	for _, c := range cases {
		got := includeArgs(file, c.run)
		if len(got) != len(c.want) {
			t.Errorf("includeArgs(%q) = %v, want %v", c.run, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("includeArgs(%q) = %v, want %v", c.run, got, c.want)
			}
		}
	}
}

func TestClassify(t *testing.T) {
	valid := runResult{Class: "valid", OutSHA: "aa"}
	validOther := runResult{Class: "valid", OutSHA: "bb"}
	invalid := func(line int) runResult { return runResult{Class: "invalid", Exit: 1, ErrLine: line} }
	unsigned := func(head string) runResult {
		return runResult{Class: "invalid", Exit: 1, ErrHead: head}
	}
	cases := []struct {
		name         string
		oracle, ours runResult
		want         string
	}{
		{"same json", valid, valid, "match"},
		{"different json", valid, validOther, "diff"},
		{"unsupported", valid, runResult{Exit: 2}, "unsupported"},
		{"timeout", valid, runResult{Class: "timeout", Exit: -1}, "timeout"},
		{"exec failure", valid, runResult{Exit: -1}, "crash"},
		{"same error line", invalid(9), invalid(9), "reject-match"},
		{"different error line", invalid(9), invalid(10), "diff"},
		{"loc-less errors compare their message", unsigned("error: no loc"), unsigned("error: no loc"), "reject-match"},
		{"loc-less errors with different messages", unsigned("error: one"), unsigned("error: two"), "diff"},
		// reportError prefixes argv0, which differs between the two binaries.
		{"program prefix is stripped", unsigned("/a/llvm-tblgen: Could not open input file 'x'"),
			unsigned("/b/tblgen: Could not open input file 'x'"), "reject-match"},
		{"oracle valid, ours invalid", valid, invalid(3), "diff"},
	}
	for _, c := range cases {
		if got := classify(c.oracle, c.ours); got != c.want {
			t.Errorf("%s: classify = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestErrorLine(t *testing.T) {
	stderr := "t.td:12:5: error: def already exists: ok\ndef ok;\n^\nnote: previous\n"
	if got := errorLine(stderr); got != 12 {
		t.Errorf("errorLine = %d, want 12", got)
	}
	if got := errorLine("warning: only\nnote: t.td:3:1\n"); got != 0 {
		t.Errorf("errorLine without an error = %d, want 0", got)
	}
}
