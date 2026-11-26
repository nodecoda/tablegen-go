package syntax

import "testing"

// TestResolveTypesCommonSuperclass pins Record.cpp:326-353 resolveTypes: two
// sibling record types resolve to their common superclass, and the same rule
// applies element-wise inside list<...>. llvm-tblgen reports the resolved type
// in its diagnostics ("... of type 'A'"), so getting this wrong shows up as a
// wrong error message rather than as a wrong value - which is exactly the kind
// of bug the differential harness only found once stderr was compared.
func TestResolveTypesCommonSuperclass(t *testing.T) {
	f := mustParse(t, `
class A<int dummy = 0>;
class B : A<1>;
class C : A<2>;
class X<int cc, B b, C c, list<B> lb, list<A> la> {
  A i = !if(cc, b, c);
  A k = !cond(cc: b, 1: c);
  list<A> l = !if(cc, lb, la);
}

def d : X<1, B, C, [], []>;
`)
	body := f.Objects[3].(*ClassStmt).Body
	cases := []struct {
		name string
		i    int
		want string
	}{
		{"!if", 0, "A"},
		{"!cond", 1, "A"},
		{"!if list", 2, "list<A>"},
	}
	for _, c := range cases {
		got := body[c.i].(*Decl).Value.Type()
		if got.String() != c.want {
			t.Errorf("%s type = %q, want %q", c.name, got.String(), c.want)
		}
	}
}
