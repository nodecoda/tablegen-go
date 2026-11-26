package record

import "testing"

func intOf(e *Env, name string, t *testing.T) int64 {
	t.Helper()
	b, ok := e.Lookup(name)
	if !ok {
		t.Fatalf("%s: not found", name)
	}
	i, ok := b.Val.(*Int)
	if !ok {
		t.Fatalf("%s: not an int: %T", name, b.Val)
	}
	return i.N
}

// Env keeps one binding outside frames to avoid a one-entry map per loop
// iteration; shadowing must match the frames it replaced.
func TestEnvSingleBinding(t *testing.T) {
	base := (&Env{}).With(map[string]Binding{
		"x": {Val: &Int{N: 1}},
		"y": {Val: &Int{N: 2}},
	}).WithBinding("z", Binding{Val: &Int{N: 3}})

	if got := intOf(base, "z", t); got != 3 {
		t.Errorf("kv binding, got %d", got)
	}
	// A wider frame pushed later shadows the pending single binding.
	wide := base.With(map[string]Binding{"z": {Val: &Int{N: 4}}})
	if got := intOf(wide, "z", t); got != 4 {
		t.Errorf("frame must shadow kv, got %d", got)
	}
	if got := intOf(wide, "x", t); got != 1 {
		t.Errorf("older frame lost, got %d", got)
	}
	// A newer single binding shadows an older one, including through the
	// frame the older one was folded into.
	shadow := base.WithBinding("x", Binding{Val: &Int{N: 5}})
	if got := intOf(shadow, "x", t); got != 5 {
		t.Errorf("WithBinding must shadow a frame, got %d", got)
	}
	if got := intOf(shadow, "z", t); got != 3 {
		t.Errorf("folded kv lost, got %d", got)
	}
	// The receiver is not mutated by either call.
	if got := intOf(base, "x", t); got != 1 {
		t.Errorf("With mutated the receiver, got %d", got)
	}
	if got := intOf(base, "z", t); got != 3 {
		t.Errorf("WithBinding mutated the receiver, got %d", got)
	}

	// StackOn keeps both sides' single bindings, site on top.
	site := (&Env{}).WithBinding("x", Binding{Val: &Int{N: 6}})
	lex := (&Env{}).WithBinding("x", Binding{Val: &Int{N: 7}}).WithBinding("w", Binding{Val: &Int{N: 8}})
	out := site.StackOn(lex)
	if got := intOf(out, "x", t); got != 6 {
		t.Errorf("site kv must win, got %d", got)
	}
	if got := intOf(out, "w", t); got != 8 {
		t.Errorf("lexical kv lost, got %d", got)
	}
}

// Successive WithBinding calls chain, newest first; the whole chain must sit
// above the frames and fold into one frame when a wider map arrives.
func TestEnvBindingChain(t *testing.T) {
	env := (&Env{}).
		WithBinding("a", Binding{Val: &Int{N: 1}}).
		WithBinding("b", Binding{Val: &Int{N: 2}}).
		WithBinding("a", Binding{Val: &Int{N: 3}}).
		With(map[string]Binding{"c": {Val: &Int{N: 4}}})
	if got := intOf(env, "a", t); got != 3 {
		t.Errorf("newest chain entry must win, got %d", got)
	}
	if got := intOf(env, "b", t); got != 2 {
		t.Errorf("middle chain entry lost, got %d", got)
	}
	if got := intOf(env, "c", t); got != 4 {
		t.Errorf("wider frame lost, got %d", got)
	}
	if env.kv != nil {
		t.Error("With must fold the chain")
	}
	if len(env.frames) != 2 {
		t.Errorf("chain folded into %d frames, want 2", len(env.frames))
	}
}

// LexicalOnly keeps loop variables (no owner, no colon) and drops template
// arguments; a pending single binding must follow the same rule.
func TestEnvLexicalOnlySingleBinding(t *testing.T) {
	loop := (&Env{}).WithBinding("i", Binding{Val: &Int{N: 1}}).LexicalOnly()
	if got := intOf(loop, "i", t); got != 1 {
		t.Errorf("loop variable dropped, got %d", got)
	}
	targ := (&Env{}).WithBinding("C:n", Binding{
		Val: &Int{N: 2}, Owner: &Record{Name: "C"}, Sep: ":", Self: "C:n",
	}).LexicalOnly()
	if _, ok := targ.Lookup("C:n"); ok {
		t.Error("template argument kept by LexicalOnly")
	}
}

// With must not grow the receiver's frame slice: environments are copied by
// value and only the copy may be extended.
func TestEnvWithFrameIsolation(t *testing.T) {
	base := (&Env{}).With(map[string]Binding{"a": {Val: &Int{N: 1}}})
	if got := len(base.With(nil).frames); got != 1 {
		t.Errorf("nil With changed frames: %d", got)
	}
	child := base.With(map[string]Binding{"b": {Val: &Int{N: 2}}})
	if len(base.frames) != 1 {
		t.Errorf("With grew the receiver's frames: %d", len(base.frames))
	}
	if len(child.frames) != 2 {
		t.Errorf("child frames: %d", len(child.frames))
	}
}

// HideOwnArgs hides a record's own template arguments; a lone binding kept
// outside frames must be hidden too (see HideOwnArgs).
func TestEnvHideOwnArgsSingleBinding(t *testing.T) {
	r := &Record{Name: "C"}
	own := (&Env{}).WithBinding("C:n", Binding{
		Val: &Int{N: 1}, Owner: r, Sep: ":", Self: "C:n",
	})
	if _, ok := own.HideOwnArgs(r).Lookup("C:n"); ok {
		t.Error("own template argument not hidden")
	}
	other := (&Env{}).WithBinding("D:n", Binding{
		Val: &Int{N: 2}, Owner: &Record{Name: "D"}, Sep: ":", Self: "D:n",
	})
	if _, ok := other.HideOwnArgs(r).Lookup("D:n"); !ok {
		t.Error("foreign template argument hidden")
	}
}

// With converts a small map into a slice frame, so the caller must fill the
// map before the call (lower.go does). A large frame keeps the map.
func TestEnvFrameForms(t *testing.T) {
	vars := map[string]Binding{"x": {Val: &Int{N: 1}}}
	env := (&Env{}).With(vars)
	vars["y"] = Binding{Val: &Int{N: 2}}
	if _, ok := env.Lookup("y"); ok {
		t.Error("small frame aliases the caller's map")
	}
	if got := intOf(env, "x", t); got != 1 {
		t.Errorf("small frame lost its entry: %d", got)
	}

	big := map[string]Binding{}
	for i := 0; i < smallFrame+2; i++ {
		big[string(rune('a'+i))] = Binding{Val: &Int{N: int64(i)}}
	}
	env = (&Env{}).With(big)
	if env.frames[0].m == nil {
		t.Error("large frame must use a map")
	}
	if got := intOf(env, "c", t); got != 2 {
		t.Errorf("large frame lookup: %d", got)
	}
}
