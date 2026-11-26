package diag

import "testing"

// Two inclusions of the same file are two SourceMgr buffers with their own
// IncludeLoc chain (SourceMgr.cpp:58-67). The chain must follow the instance
// id in Pos, not the file name.
func TestRenderAllIncludeInstances(t *testing.T) {
	sites := IncludeSites{Parents: []Pos{
		{},
		{File: "main.td", Line: 1, Col: 1},      // 1: a.td
		{File: "a.td", Line: 2, Col: 1, Inc: 1}, // 2: b.td inside it
		{File: "main.td", Line: 7, Col: 1},      // 3: a.td again
		{File: "a.td", Line: 2, Col: 1, Inc: 3}, // 4: b.td inside it
	}}
	d := Diagnostic{
		Severity: Error, Message: "boom",
		Span: Span{Start: Pos{File: "b.td", Line: 5, Col: 5, Inc: 4}},
	}
	got := d.RenderAll("", nil, sites.Parent)
	want := "Included from main.td:7:\nIncluded from a.td:2:\nb.td:5:5: error: boom\n"
	if got != want {
		t.Fatalf("RenderAll = %q, want %q", got, want)
	}
}
