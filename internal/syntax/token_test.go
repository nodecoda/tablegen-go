package syntax

import "testing"

// These counts are the C++ inventory, not a guess: 24 keywords (12 reserved +
// 12 object starts) and 58 bang operators (TGLexer.cpp:631-673).
func TestInventoryCounts(t *testing.T) {
	if got := KeywordCount(); got != 24 {
		t.Errorf("keyword count = %d, want 24", got)
	}
	if got := BangOpCount(); got != 58 {
		t.Errorf("bang operator count = %d, want 58", got)
	}
}

// TableGen distinguishes the two namespaces by the '!' sigil, so a name may
// legitimately appear in both. Pin the exact overlap: a new one is a table bug.
func TestKeywordAndBangOverlapIsExactlyKnown(t *testing.T) {
	want := map[string]bool{"dag": true, "if": true, "foreach": true}
	got := map[string]bool{}
	for name := range keywords {
		if _, ok := bangOps[name]; ok {
			got[name] = true
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected keyword/bang overlap %q", name)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("expected keyword/bang overlap %q", name)
		}
		if keywords[name] == bangOps[name] {
			t.Errorf("%q maps to the same Kind in both namespaces", name)
		}
	}
}

// Range checks rely on declaration order, so pin the boundaries.
func TestKindOrderingIsContiguous(t *testing.T) {
	if AssertTok+1 != Class {
		t.Error("object starts are not contiguous")
	}
	if MultiClass+1 != XConcat {
		t.Error("bang operators do not immediately follow object starts")
	}
	if XRepr+1 != Id {
		t.Error("string-valued tokens do not immediately follow bang operators")
	}
}

func TestStringValuedCoversIdStrVarCode(t *testing.T) {
	for _, k := range []Kind{Id, StrVal, VarName, CodeFragment} {
		if !k.IsStringValued() {
			t.Errorf("%s should be string-valued", k)
		}
	}
	for _, k := range []Kind{IntVal, Equal, XEq, Def} {
		if k.IsStringValued() {
			t.Errorf("%s should not be string-valued", k)
		}
	}
}

func TestObjectStarts(t *testing.T) {
	want := []Kind{AssertTok, Class, Def, Defm, Defset, Deftype, Defvar, Dump, Foreach, If, Let, MultiClass}
	for _, k := range want {
		if !k.IsObjectStart() {
			t.Errorf("%s should be an object start", k)
		}
	}
	if Id.IsObjectStart() || XEq.IsObjectStart() {
		t.Error("non-object-start kinds reported as object starts")
	}
}

func TestBangNamesRoundTrip(t *testing.T) {
	for name, k := range bangOps {
		if got := k.String(); got != "!"+name {
			t.Errorf("Kind.String() for %q = %q", name, got)
		}
	}
}
