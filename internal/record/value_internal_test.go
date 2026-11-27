package record

import (
	"testing"

	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// bitsFromInt/bitsToInt are the two directions of
// IntInit::convertInitializerTo(BitsRecTy) and
// BitsInit::convertInitializerToInt (Record.cpp). A bits<N> accepts
// [-2^(N-1), 2^N-1], keeps bits least-significant first, and a width wider
// than 63 leaves the upper bits clear instead of sign-extending.
func TestBitsRoundTrip(t *testing.T) {
	cases := []struct {
		n     int64
		width int
		ok    bool
	}{
		{0, 0, true},
		{1, 0, false},
		{5, 4, true},
		{-1, 4, true},
		{8, 4, true},
		{15, 4, true},
		{16, 4, false},
		{-8, 4, true},
		{-9, 4, false},
		{-1, 70, true},
	}
	for _, c := range cases {
		bits, ok := bitsFromInt(c.n, c.width)
		if ok != c.ok {
			t.Errorf("bitsFromInt(%d, %d) ok = %v, want %v", c.n, c.width, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(bits.B) != c.width {
			t.Errorf("bitsFromInt(%d, %d) width = %d", c.n, c.width, len(bits.B))
		}
		back, ok := bitsToInt(bits)
		if !ok {
			t.Errorf("bitsToInt(bitsFromInt(%d, %d)) not convertible", c.n, c.width)
			continue
		}
		// bitsToInt reads the stored bits as unsigned data; a negative
		// literal was written as its low `width` bits.
		want := c.n
		if c.width > 0 && c.width < 64 {
			want = c.n & ((int64(1) << uint(c.width)) - 1)
		}
		if c.width < 64 && back != want {
			t.Errorf("round trip %d/%d = %d, want %d", c.n, c.width, back, want)
		}
	}
	// An unresolved bit makes the value non-convertible, like
	// BitsInit::convertInitializerToInt.
	if _, ok := bitsToInt(Bits{B: []Value{&Bit{B: true}, &Unset{}}}); ok {
		t.Error("bitsToInt accepted an unset bit")
	}
	// VarBitInit resolves through the referenced bit.
	if n, ok := bitsToInt(Bits{B: []Value{&BitRef{Var: "x", Index: 0, Value: &Bit{B: true}}}}); !ok || n != 1 {
		t.Errorf("bitsToInt(BitRef) = %d, %v", n, ok)
	}
}

// TestValueStringForms pins the Init::getAsString forms (Record.cpp) the JSON
// and record printers rely on.
func TestValueStringForms(t *testing.T) {
	if got := (&Str{S: "a\tb"}).String(); got != "\"a\tb\"" {
		t.Errorf("string literal prints %q", got)
	}
	if got := (&Str{S: "x", Code: true}).String(); got != "[{x}]" {
		t.Errorf("code literal prints %q", got)
	}
	bits := &Bits{B: []Value{&Bit{B: true}, &Unset{}, &Bit{B: false}}}
	if got := bits.String(); got != "{ 0, ?, 1 }" {
		t.Errorf("bits print %q", got)
	}
	list := &List{Elems: []Value{&Int{N: 1}, &Str{S: "s"}}}
	if got := list.String(); got != `[1, "s"]` {
		t.Errorf("list prints %q", got)
	}
	dag := &Dag{Op: &DefVal{Rec: &Record{Name: "op"}}, OpName: "n",
		Args: []DagArg{{Value: &Int{N: 1}, Name: "a", HasName: true}, {Value: &Str{S: "s"}}}}
	if got := dag.String(); got != `(op:$n 1:$a, "s")` {
		t.Errorf("dag prints %q", got)
	}
	// DagInit::getAsString prints `:$` for any non-null ArgName, so an empty
	// StringInit name renders as a bare colon-dollar (Record.cpp:2719-2724).
	dag.Args[1].HasName = true
	if got := dag.String(); got != `(op:$n 1:$a, "s":$)` {
		t.Errorf("dag with empty name prints %q", got)
	}
	dag.Args[1].HasName = false
	// SourceString keeps the operator as written, which is what a fatal
	// diagnostic shows for an Init tree that has not been folded.
	dag.OpSrc = &syntax.VarValue{Name: "OPNAME"}
	if got := dag.SourceString(); got != `(OPNAME:$n 1:$a, "s")` {
		t.Errorf("dag source prints %q", got)
	}
	if got := (&Unresolved{Text: "!add(u, 2)"}).String(); got != "!add(u, 2)" {
		t.Errorf("unresolved prints %q", got)
	}
	if got := (&BitRef{Var: "b", Index: 3}).String(); got != "b{3}" {
		t.Errorf("varbit prints %q", got)
	}
	if got := (&DefVal{Rec: &Record{Name: "defname"}}).String(); got != "defname" {
		t.Errorf("defval prints %q", got)
	}
}

func TestValueTypes(t *testing.T) {
	if k := (&Unset{}).Type().Kind; k != AnyTy {
		t.Errorf("unset type %v", k)
	}
	if k := (&Bits{B: make([]Value, 3)}).Type(); k.Kind != BitsTy || k.Bits != 3 {
		t.Errorf("bits type %+v", k)
	}
	if k := (&Str{Code: true}).Type().Kind; k != CodeTy {
		t.Errorf("code type %v", k)
	}
	if k := (&Str{}).Type().Kind; k != StringTy {
		t.Errorf("string type %v", k)
	}
	if k := (&List{}).Type().Kind; k != ListTy {
		t.Errorf("list type %v", k)
	}
	if k := (&Dag{}).Type().Kind; k != DagTy {
		t.Errorf("dag type %v", k)
	}
	if k := (&BitRef{}).Type().Kind; k != BitTy {
		t.Errorf("varbit type %v", k)
	}
	if k := (&Unresolved{Ty: intTy}).Type(); k != intTy {
		t.Errorf("unresolved keeps its Init type")
	}
	if k := (&Unresolved{}).Type().Kind; k != AnyTy {
		t.Errorf("untyped unresolved is any, got %v", k)
	}
}

func TestCommonElem(t *testing.T) {
	if commonElem(nil) != nil {
		t.Error("empty list has no element type")
	}
	if k := commonElem([]Value{&Int{N: 1}, &Int{N: 2}}).Kind; k != IntTy {
		t.Errorf("uniform list element %v", k)
	}
	if k := commonElem([]Value{&Int{N: 1}, &Str{}}).Kind; k != AnyTy {
		t.Errorf("mixed list element should be any, got %v", k)
	}
}

// StackOn puts the current application site's frames on top of the lexical
// environment (MapResolver keeps resolving inside the caller's Init,
// TGParser.cpp:382), while owner stays lexical for QualifyName.
func TestEnvStackOn(t *testing.T) {
	lex := (&Env{}).WithBinding("x", Binding{Val: &Int{N: 1}})
	site := (&Env{owner: &Record{Name: "Site"}, ownerSep: ":"}).
		WithBinding("x", Binding{Val: &Int{N: 2}}).
		WithBinding("y", Binding{Val: &Int{N: 3}})
	out := site.StackOn(lex)
	if b, ok := out.Lookup("x"); !ok || b.Val.(*Int).N != 2 {
		t.Errorf("innermost frame must win, got %+v %v", b, ok)
	}
	if b, ok := out.Lookup("y"); !ok || b.Val.(*Int).N != 3 {
		t.Errorf("site-only binding lost: %+v %v", b, ok)
	}
	if out.owner != nil {
		t.Errorf("owner comes from base, got %v", out.owner)
	}
	if got := site.StackOn(nil); got != site {
		t.Error("nil base returns the receiver")
	}

	k := &Keeper{}
	// The keeper travels with the bindings when the site has none.
	if out := (&Env{}).StackOn((&Env{}).WithKeeper(k)); out.keeper != k {
		t.Error("keeper not inherited from base")
	}
	if out := (&Env{keeper: k}).StackOn(&Env{}); out.keeper != k {
		t.Error("keeper not inherited from site")
	}

	// hidden names from either side stay hidden after stacking.
	base := (&Env{hidden: map[string]bool{"a": true}})
	cur := (&Env{hidden: map[string]bool{"b": true}}).WithBinding("a", Binding{Val: &Int{N: 1}})
	out = cur.StackOn(base)
	if _, ok := out.Lookup("a"); ok {
		t.Error("hidden name leaked through StackOn")
	}
	if _, ok := out.Lookup("b"); ok {
		t.Error("hidden name leaked through StackOn")
	}
	if out.implicitTo != base.implicitTo || out.ownerSep != base.ownerSep {
		t.Error("lexical fields must come from base")
	}
}
