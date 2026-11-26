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
