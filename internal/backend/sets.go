package backend

import (
	"fmt"
	"strings"

	"github.com/nodecoda/tablegen-go/internal/record"
)

// PrintSets is the -print-sets action (utils/TableGen/Basic/TableGen.cpp:45):
// every def derived from class Set, with its Elements DAG expanded into an
// ordered list of records. `Name = [ e1 e2 ]`; an empty set prints `[ ]`.
func PrintSets(k *record.Keeper) ([]byte, error) {
	st := newSetTheory(k)
	st.addFieldExpander("Set", "Elements")
	var b strings.Builder
	for _, rec := range k.Defs() { // sorted by name, like getAllDerivedDefinitions
		if !rec.IsSubClassOf("Set") {
			continue
		}
		elts, _, err := st.expand(rec)
		if err != nil {
			return nil, err
		}
		// The C++ driver asserts the expansion succeeded; an unexpandable Set
		// def is a modelling error, not something to print silently.
		if elts == nil && !st.expandable(rec) {
			return nil, fmt.Errorf("couldn't expand Set instance '%s'", rec.Name)
		}
		b.WriteString(rec.Name)
		b.WriteString(" = [")
		for _, e := range elts {
			b.WriteByte(' ')
			b.WriteString(e.Name)
		}
		b.WriteString(" ]\n")
	}
	return []byte(b.String()), nil
}

// recSet is SmallSetVector<const Record *, 16>: insertion order plus membership.
type recSet struct {
	order []*record.Record
	seen  map[*record.Record]bool
}

func (s *recSet) insert(r *record.Record) {
	if s.seen == nil {
		s.seen = map[*record.Record]bool{}
	}
	if s.seen[r] {
		return
	}
	s.seen[r] = true
	s.order = append(s.order, r)
}

func (s *recSet) insertAll(rs []*record.Record) {
	for _, r := range rs {
		s.insert(r)
	}
}

// SetTheory mirrors llvm/TableGen/SetTheory.h: operators keyed by def name and
// expanders keyed by class name, with a memoization cache that also breaks
// recursive set definitions.
type SetTheory struct {
	keeper    *record.Keeper
	expanders map[string]string // class name -> field holding the DAG
	ops       map[string]func(*SetTheory, *record.Dag) ([]*record.Record, error)
	memo      map[*record.Record][]*record.Record
}

func newSetTheory(k *record.Keeper) *SetTheory {
	s := &SetTheory{
		keeper:    k,
		expanders: map[string]string{},
		memo:      map[*record.Record][]*record.Record{},
	}
	// SetTheory::SetTheory (SetTheory.cpp:250): the standard operator table.
	s.ops = map[string]func(*SetTheory, *record.Dag) ([]*record.Record, error){
		"add":        (*SetTheory).opAdd,
		"sub":        (*SetTheory).opSub,
		"and":        (*SetTheory).opAnd,
		"shl":        (*SetTheory).opShl,
		"trunc":      (*SetTheory).opTrunc,
		"rotl":       (*SetTheory).opRotl,
		"rotr":       (*SetTheory).opRotr,
		"decimate":   (*SetTheory).opDecimate,
		"interleave": (*SetTheory).opInterleave,
		"sequence":   (*SetTheory).opSequence,
	}
	return s
}

func (s *SetTheory) addFieldExpander(class, field string) {
	s.expanders[class] = field
}

// expand is SetTheory::expand (SetTheory.cpp:302): find a superclass with a
// registered expander, evaluate that field, and cache the result. A recursive
// definition observes the empty cache entry, exactly like the C++ code.
func (s *SetTheory) expand(rec *record.Record) ([]*record.Record, bool, error) {
	if elts, ok := s.memo[rec]; ok {
		return elts, true, nil
	}
	for _, sc := range rec.SuperClasses() {
		field, ok := s.expanders[sc.Name]
		if !ok {
			continue
		}
		s.memo[rec] = nil
		v, err := rec.Field(field)
		if err != nil {
			return nil, false, err
		}
		var set recSet
		if err := s.evaluate(v, &set); err != nil {
			return nil, false, err
		}
		s.memo[rec] = set.order
		return set.order, true, nil
	}
	return nil, false, nil
}

// expandable reports whether some superclass has an expander, without
// evaluating anything (used only for the driver's assertion message).
func (s *SetTheory) expandable(rec *record.Record) bool {
	for _, sc := range rec.SuperClasses() {
		if _, ok := s.expanders[sc.Name]; ok {
			return true
		}
	}
	return false
}

// evaluate is SetTheory::evaluate (SetTheory.cpp:280): defs may expand, lists
// flatten, everything else must be a DAG whose operator is a def.
func (s *SetTheory) evaluate(v record.Value, out *recSet) error {
	switch x := v.(type) {
	case *record.DefVal:
		elts, ok, err := s.expand(x.Rec)
		if err != nil {
			return err
		}
		if ok {
			out.insertAll(elts)
		} else {
			out.insert(x.Rec)
		}
		return nil
	case *record.List:
		for _, e := range x.Elems {
			if err := s.evaluate(e, out); err != nil {
				return err
			}
		}
		return nil
	case *record.Dag:
		op, ok := x.Op.(*record.DefVal)
		if !ok {
			return fmt.Errorf("bad set expression: %s", x.String())
		}
		fn, ok := s.ops[op.Rec.Name]
		if !ok {
			return fmt.Errorf("unknown set operator: %s", x.String())
		}
		elts, err := fn(s, x)
		if err != nil {
			return err
		}
		out.insertAll(elts)
		return nil
	default:
		return fmt.Errorf("invalid set element: %s", v.String())
	}
}

// evalArgs evaluates every argument of d into one set.
func (s *SetTheory) evalArgs(d *record.Dag) ([]*record.Record, error) {
	var set recSet
	for _, a := range d.Args {
		if err := s.evaluate(a.Value, &set); err != nil {
			return nil, err
		}
	}
	return set.order, nil
}

// opAdd: (add a, b, ...) evaluates and concatenates its arguments.
func (s *SetTheory) opAdd(d *record.Dag) ([]*record.Record, error) {
	return s.evalArgs(d)
}

// opSub: (sub Add, Sub, ...) is set difference; at least two arguments.
func (s *SetTheory) opSub(d *record.Dag) ([]*record.Record, error) {
	if len(d.Args) < 2 {
		return nil, fmt.Errorf("set difference needs at least two arguments: %s", d.String())
	}
	var add recSet
	if err := s.evaluate(d.Args[0].Value, &add); err != nil {
		return nil, err
	}
	var sub recSet
	for _, a := range d.Args[1:] {
		if err := s.evaluate(a.Value, &sub); err != nil {
			return nil, err
		}
	}
	var out []*record.Record
	for _, r := range add.order {
		if !sub.seen[r] {
			out = append(out, r)
		}
	}
	return out, nil
}

// opAnd: (and S1, S2) keeps S1's order.
func (s *SetTheory) opAnd(d *record.Dag) ([]*record.Record, error) {
	if len(d.Args) != 2 {
		return nil, fmt.Errorf("set intersection requires two arguments: %s", d.String())
	}
	var s1, s2 recSet
	if err := s.evaluate(d.Args[0].Value, &s1); err != nil {
		return nil, err
	}
	if err := s.evaluate(d.Args[1].Value, &s2); err != nil {
		return nil, err
	}
	var out []*record.Record
	for _, r := range s1.order {
		if s2.seen[r] {
			out = append(out, r)
		}
	}
	return out, nil
}

// setIntArgs is the (Op Set, Int) shape shared by shl/trunc/rotl/rotr/decimate
// (SetTheory.cpp:96).
func (s *SetTheory) setIntArgs(d *record.Dag) ([]*record.Record, int64, error) {
	if len(d.Args) != 2 {
		return nil, 0, fmt.Errorf("operator requires (Op Set, Int) arguments: %s", d.String())
	}
	set, err := s.evalArgs(&record.Dag{Op: d.Op, Args: d.Args[:1]})
	if err != nil {
		return nil, 0, err
	}
	iv, ok := d.Args[1].Value.(*record.Int)
	if !ok {
		return nil, 0, fmt.Errorf("second argument must be an integer: %s", d.String())
	}
	return set, iv.N, nil
}

// opShl: (shl S, N) removes the first N elements.
func (s *SetTheory) opShl(d *record.Dag) ([]*record.Record, error) {
	set, n, err := s.setIntArgs(d)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("positive shift required: %s", d.String())
	}
	if n < int64(len(set)) {
		return set[n:], nil
	}
	return nil, nil
}

// opTrunc: (trunc S, N) keeps the first N elements.
func (s *SetTheory) opTrunc(d *record.Dag) ([]*record.Record, error) {
	set, n, err := s.setIntArgs(d)
	if err != nil {
		return nil, err
	}
	if n < 0 {
		return nil, fmt.Errorf("positive length required: %s", d.String())
	}
	if n > int64(len(set)) {
		n = int64(len(set))
	}
	return set[:n], nil
}

func (s *SetTheory) rotate(d *record.Dag, reverse bool) ([]*record.Record, error) {
	set, n, err := s.setIntArgs(d)
	if err != nil {
		return nil, err
	}
	if reverse {
		n = -n
	}
	if len(set) == 0 {
		return nil, nil
	}
	size := int64(len(set))
	if n < 0 {
		n = size - (-n % size)
	} else {
		n %= size
	}
	out := make([]*record.Record, 0, size)
	out = append(out, set[n:]...)
	out = append(out, set[:n]...)
	return out, nil
}

// opRotl: (rotl S, N), a negative N rotates right.
func (s *SetTheory) opRotl(d *record.Dag) ([]*record.Record, error) {
	return s.rotate(d, false)
}

// opRotr: (rotr S, N), a negative N rotates left.
func (s *SetTheory) opRotr(d *record.Dag) ([]*record.Record, error) {
	return s.rotate(d, true)
}

// opDecimate: (decimate S, N) picks every N'th element starting with the first.
func (s *SetTheory) opDecimate(d *record.Dag) ([]*record.Record, error) {
	set, n, err := s.setIntArgs(d)
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		return nil, fmt.Errorf("positive stride required: %s", d.String())
	}
	var out []*record.Record
	for i := 0; i < len(set); i += int(n) {
		out = append(out, set[i])
	}
	return out, nil
}

// opInterleave: (interleave S1, S2, ...) takes the n'th element of each
// argument in turn, skipping arguments that ran out.
func (s *SetTheory) opInterleave(d *record.Dag) ([]*record.Record, error) {
	values := make([][]*record.Record, len(d.Args))
	maxSize := 0
	for i, a := range d.Args {
		var set recSet
		if err := s.evaluate(a.Value, &set); err != nil {
			return nil, err
		}
		values[i] = set.order
		if len(set.order) > maxSize {
			maxSize = len(set.order)
		}
	}
	var out []*record.Record
	for n := 0; n < maxSize; n++ {
		for _, v := range values {
			if n < len(v) {
				out = append(out, v[n])
			}
		}
	}
	return out, nil
}

// opSequence: (sequence "Format", From, To [, Stride]) looks defs up by name.
// From/To are limited to 30 bits (SetTheory.cpp:180), and a named set is
// re-expanded so sequences may produce sets.
func (s *SetTheory) opSequence(d *record.Dag) ([]*record.Record, error) {
	step := int64(1)
	if len(d.Args) > 4 {
		return nil, fmt.Errorf(`bad args to (sequence "Format", From, To): %s`, d.String())
	}
	if len(d.Args) == 4 {
		iv, ok := d.Args[3].Value.(*record.Int)
		if !ok {
			return nil, fmt.Errorf("stride must be an integer: %s", d.String())
		}
		step = iv.N
	}
	format, ok := d.Args[0].Value.(*record.Str)
	if !ok {
		return nil, fmt.Errorf("format must be a string: %s", d.String())
	}
	from, ok := d.Args[1].Value.(*record.Int)
	if !ok {
		return nil, fmt.Errorf("from must be an integer: %s", d.String())
	}
	if from.N < 0 || from.N >= 1<<30 {
		return nil, fmt.Errorf("from out of range")
	}
	to, ok := d.Args[2].Value.(*record.Int)
	if !ok {
		return nil, fmt.Errorf("to must be an integer: %s", d.String())
	}
	if to.N < 0 || to.N >= 1<<30 {
		return nil, fmt.Errorf("to out of range")
	}

	if from.N <= to.N {
		step *= 1
	} else {
		step *= -1
	}
	var out []*record.Record
	for cur := from.N; ; cur += step {
		if step > 0 && cur > to.N {
			break
		}
		if step < 0 && cur < to.N {
			break
		}
		name := printfUint32(format.S, uint32(cur))
		rec := s.keeper.Def(name)
		if rec == nil {
			return nil, fmt.Errorf("no def named '%s': %s", name, d.String())
		}
		// Try to reevaluate Rec in case it is a set.
		if elts, ok, err := s.expand(rec); err != nil {
			return nil, err
		} else if ok {
			out = append(out, elts...)
		} else {
			out = append(out, rec)
		}
	}
	return out, nil
}

// printfUint32 renders `format(Format, unsigned(From))`: C printf semantics
// with a single unsigned argument, so `%u` becomes Go's `%d`.
func printfUint32(format string, n uint32) string {
	var b strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			b.WriteByte(format[i])
			continue
		}
		if i+1 < len(format) && format[i+1] == '%' {
			b.WriteString("%%")
			i++
			continue
		}
		j := i + 1
		for j < len(format) && strings.ContainsRune("-+ #0", rune(format[j])) {
			j++
		}
		for j < len(format) && (format[j] >= '0' && format[j] <= '9' || format[j] == '.') {
			j++
		}
		if j < len(format) && (format[j] == 'u' || format[j] == 'i' || format[j] == 'd') {
			b.WriteString(format[i:j])
			b.WriteByte('d')
			i = j
			continue
		}
		b.WriteByte(format[i])
	}
	return fmt.Sprintf(b.String(), n)
}
