package record_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

// One def exercising the folding operators end to end. The expected object is
// what llvm-tblgen 24.0.0git prints for D in --dump-json, so a regression in
// any operator (or in the value printer) shows up as a field diff.
func TestBangOperatorsFold(t *testing.T) {
	src := `def OP;
class TC {}
def D {
  list<int> xs = [3, 1, 2];
  list<int> f = !filter(x, xs, !gt(x, 1));
  list<int> m = !foreach(x, xs, !add(x, 10));
  list<int> s = !sort(x, xs, x);
  list<string> ss = !sort(x, ["b", "a"], x);
  int fold = !foldl(0, xs, a, b, !add(a, b));
  dag dg = !dag(OP, [1, 2], ["a", "b"]);
  int g1 = !getdagarg<int>(dg, "a");
  dag g2 = !setdagarg(dg, "a", 7);
  dag g3 = !setdagname(dg, 1, "z");
  int fh = !head(xs);
  list<int> tl = !tail(xs);
  int e = !size(xs);
  bit mt = !empty(xs);
  list<int> cc = !listconcat(xs, [4]);
  list<int> sp = !listsplat(0, 3);
  string il = !interleave(["a", "b"], "-");
  int iff = !if(1, 10, 20);
  int cnd = !cond(0: 1, 1: 2, 1: 3);
  string cas = !cast<string>(42);
  bit isa = !isa<int>(42);
  bit ex = !exists<TC>("D");
  string sub = !substr("hello", 1, 3);
  int fnd = !find("hello", "ll");
  string up = !toupper("aB");
  string lo = !tolower("Ab");
  string rp = !repr("hi");
  bit eq = !eq(1, 1);
  bit ne = !ne(1, 2);
  bit lt = !lt(1, 2);
  int an = !and(1, 2);
  int no = !not(0);
  int xr = !xor(3, 1);
  int lg = !logtwo(8);
  int ini = !initialized(xs);
  bit mch = !match("a", "a");
  list<int> fl = !listflatten([[1, 2], [3]]);
  list<int> lr = !listremove([1, 2, 3], [2]);
  list<int> rng = !range(1, 4);
  string sst = !subst("a", "b", "aaa");
  string dgn = !getdagname(dg, 0);
  dag sdop = !setdagop(dg, OP);
  dag sdopn = !setdagopname(dg, "q");
}
`
	want := `{"an":0,"cas":"42","cc":[3,1,2,4],"cnd":2,"dg":{"args":[[1,"a"],[2,"b"]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP 1:$a, 2:$b)"},"dgn":"a","e":3,"eq":1,"ex":0,"f":[3,2],"fh":3,"fl":[1,2,3],"fnd":2,"fold":6,"g1":1,"g2":{"args":[[7,"a"],[2,"b"]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP 7:$a, 2:$b)"},"g3":{"args":[[1,"a"],[2,"z"]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP 1:$a, 2:$z)"},"iff":10,"il":"a-b","ini":1,"isa":1,"lg":3,"lo":"ab","lr":[1,3],"lt":1,"m":[13,11,12],"mch":1,"mt":0,"ne":1,"no":1,"rng":[1,2,3],"rp":"\"hi\"","s":[1,2,3],"sdop":{"args":[[1,"a"],[2,"b"]],"kind":"dag","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP 1:$a, 2:$b)"},"sdopn":{"args":[[1,"a"],[2,"b"]],"kind":"dag","name":"q","operator":{"def":"OP","kind":"def","printable":"OP"},"printable":"(OP:$q 1:$a, 2:$b)"},"sp":[0,0,0],"ss":["a","b"],"sst":"bbb","sub":"ell","tl":[1,2],"up":"AB","xr":2,"xs":[3,1,2]}`
	var got map[string]any
	if err := json.Unmarshal([]byte(lowerJSON(t, src)), &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	var wantObj map[string]any
	if err := json.Unmarshal([]byte(want), &wantObj); err != nil {
		t.Fatalf("want json: %v", err)
	}
	def, ok := got["D"].(map[string]any)
	if !ok {
		t.Fatalf("no D object in %s", lowerJSON(t, src))
	}
	for k := range def {
		if len(k) > 0 && k[0] == '!' {
			delete(def, k) // !name/!locs/... metadata, not a field
		}
	}
	if !reflect.DeepEqual(def, wantObj) {
		t.Errorf("D = %#v\nwant %#v", def, wantObj)
	}
}
