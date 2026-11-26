package main

import (
	"reflect"
	"testing"
)

func TestNormalizeArgs(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{[]string{"-DDIAG1", "x.td"}, []string{"-D", "DIAG1", "x.td"}},
		{[]string{"-D", "A", "-DB", "x.td"}, []string{"-D", "A", "-D", "B", "x.td"}},
		{[]string{"-D=A", "x.td"}, []string{"-D=A", "x.td"}},
		{[]string{"--dump-tokens", "-Ix", "x.td"}, []string{"--dump-tokens", "-I", "x", "x.td"}},
	}
	for _, c := range cases {
		if got := normalizeArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("normalizeArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReorderArgs(t *testing.T) {
	cases := []struct {
		in      []string
		opts    []string
		files   []string
		missing string
	}{
		// A value-taking option steals the next argument whatever it looks
		// like, and keeps doing so after a positional (cl parses argv in order).
		{[]string{"x.td", "-D", "A"}, []string{"-D", "A"}, []string{"x.td"}, ""},
		{[]string{"x.td", "-D", "-I", "."}, []string{"-D", "-I"}, []string{"x.td", "."}, ""},
		// `file -D` has no value: cl errors instead of reaching back to the
		// already-seen positional (CommandLine.cpp:719-726).
		{[]string{"x.td", "-D"}, []string{"-D"}, []string{"x.td"}, "-D"},
		{[]string{"-I"}, []string{"-I"}, nil, "-I"},
	}
	for _, c := range cases {
		opts, files, missing := reorderArgs(c.in)
		if !reflect.DeepEqual(opts, c.opts) || !reflect.DeepEqual(files, c.files) || missing != c.missing {
			t.Errorf("reorderArgs(%q) = %q, %q, %q; want %q, %q, %q",
				c.in, opts, files, missing, c.opts, c.files, c.missing)
		}
	}
}
