package record_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"tblgen/internal/diag"
)

// let append/prepend, bit-range lets, list indexing and slicing, bit ranges,
// !con, string paste and assertions, all in one def. The expected object is
// llvm-tblgen 24.0.0git's --dump-json output for D.
func TestRecordExpressionForms(t *testing.T) {
	src := `def op;
def op3;
def op2;
class Base {
  list<int> items = [1, 2];
  string text = "hi";
  dag d = (op);
  dag de = !foreach(x, d, x);
  int i = 1;
  bits<4> b = {1, 0, 0, 0};
}
def D : Base {
  let append items = [3];
  let prepend text = "x";
  let b{1} = 1;
  list<int> xs = [10, 20, 30];
  int one = xs[1];
  list<int> slice = xs[0, 2];
  bits<2> br = b{2...1};
  bit bf = b{3};
  dag cd = !con((op 1), (op 2));
  string sc = "a" # 1;
  int sum = 0;
  assert !eq(1, 1), "items";
}
`
	want := `{"b":[0,1,0,1],"bf":1,"br":[1,0],"cd":{"args":[[1,null],[2,null]],"kind":"dag","operator":{"def":"op","kind":"def","printable":"op"},"printable":"(op 1, 2)"},"d":{"args":[],"kind":"dag","operator":{"def":"op","kind":"def","printable":"op"},"printable":"(op)"},"de":{"args":[],"kind":"dag","operator":{"def":"op","kind":"def","printable":"op"},"printable":"(op)"},"i":1,"items":[1,2,3],"one":20,"sc":"a1","slice":[10,30],"sum":0,"text":"xhi","xs":[10,20,30]}`
	var got map[string]any
	if err := json.Unmarshal([]byte(lowerJSON(t, src)), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	def, ok := got["D"].(map[string]any)
	if !ok {
		t.Fatalf("no D object")
	}
	for k := range def {
		if strings.HasPrefix(k, "!") {
			delete(def, k)
		}
	}
	var wantObj map[string]any
	if err := json.Unmarshal([]byte(want), &wantObj); err != nil {
		t.Fatalf("want json: %v", err)
	}
	if !reflect.DeepEqual(def, wantObj) {
		t.Errorf("D = %#v\nwant %#v", def, wantObj)
	}
}

// The deferred forms and the assertion note order must match the oracle word
// for word, including the record-level "assertion failed in this record".
func TestDeferredOperatorsAndAssert(t *testing.T) {
	src := `def op;
def D {
  dag d = (op 1, 2);
  dag e = !foreach(x, d, x);
}
class A { list<int> xs; int k; }
def U : A {
  int s = !switch(k, 1: 10, 20);
  int f = !foldl(0, xs, acc, el, !add(acc, el));
  assert !eq(1, 2), "boom";
}
`
	want := []string{
		"Initializer of 's' in 'U' could not be fully resolved: !cond(!eq(k, 1): 10, 1: 20)",
		"Initializer of 'f' in 'U' could not be fully resolved: !foldl(0, xs, acc, el, !add(acc, el))",
		"assertion failed: boom",
		"assertion failed in this record",
	}
	var got []string
	for _, d := range lowerDiags(t, src) {
		if d.Severity == diag.Error && !strings.Contains(d.Message, "unused template argument") {
			got = append(got, d.Message)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("messages = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}
	}
}
