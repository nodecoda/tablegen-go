package record

import (
	"strconv"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/syntax"
)

// Class-application sites and their locations.
//
// llvm-tblgen creates a VarDefInit node for every class application while
// parsing (VarDefInit::get), keyed by (class, parsed argument Init pointers).
// That node carries the location of the first parse of its shape. Resolution
// substitutes the arguments and re-keys the node
// (VarDefInit::get(Loc, Class, NewArgs)), reusing the node's own location, so
// the location printed for an anonymous instance is the parse site of the
// node whose resolved arguments were registered first - not necessarily the
// site whose values are identical, and not necessarily the instantiation site.
//
// The parsed Init pointers that make two applications the same node are:
//   - template-argument references, interned by their *qualified* name
//     (VarInit::get(QualifyName(Class, name), type)), so `argNo` in two
//     different classes is two different Inits;
//   - defvar references, textually substituted by the defvar's Init;
//   - every compound node (UnOpInit/BinOpInit/...), interned structurally.
//
// shapeLoc reproduces the first half: the location of the first parse of a
// shape. resolvedSite reproduces the second: the first shape whose resolved
// arguments produced a given anonymous def. Both are keyed by deterministic
// structural fingerprints so output stays reproducible.

// siteFor returns the location llvm-tblgen stamps on the anonymous def that
// application t instantiates with the resolved-argument key `resolved`.
func (k *Keeper) siteFor(t *syntax.ClassApplyValue, env *Env, resolved string) diag.Pos {
	if p, ok := k.resolvedSite[resolved]; ok {
		return p
	}
	p := k.shapeLoc[k.shapeKey(t, env)]
	if p.File == "" {
		p = t.Span().Start
	}
	if k.resolvedSite == nil {
		k.resolvedSite = map[string]diag.Pos{}
	}
	k.resolvedSite[resolved] = p
	return p
}

// noteShapeSites records the parse location of every class application in v.
// TGParser registers the node after parsing the argument list, so nested
// applications are seen first.
func (k *Keeper) noteShapeSites(v syntax.Value, env *Env) {
	switch t := v.(type) {
	case nil:
	case *syntax.ClassApplyValue:
		for _, a := range t.Args {
			k.noteShapeSites(a.Value, env)
		}
		key := k.shapeKey(t, env)
		if _, ok := k.shapeLoc[key]; !ok {
			if k.shapeLoc == nil {
				k.shapeLoc = map[string]diag.Pos{}
			}
			k.shapeLoc[key] = t.Span().Start
		}
	case *syntax.BitsValue:
		for _, e := range t.Elems {
			k.noteShapeSites(e, env)
		}
	case *syntax.ListValue:
		for _, e := range t.Elems {
			k.noteShapeSites(e, env)
		}
	case *syntax.DagValue:
		k.noteShapeSites(t.Op, env)
		for _, a := range t.Args {
			k.noteShapeSites(a.Value, env)
		}
	case *syntax.VarValue:
		k.noteShapeSites(t.Lit, env)
	case *syntax.FieldValue:
		k.noteShapeSites(t.Base, env)
	case *syntax.SliceValue:
		k.noteShapeSites(t.Base, env)
		for _, e := range t.Index {
			k.noteShapeSites(e.Lo, env)
			k.noteShapeSites(e.Hi, env)
		}
	case *syntax.BitRangeValue:
		k.noteShapeSites(t.Base, env)
	case *syntax.PasteValue:
		k.noteShapeSites(t.LHS, env)
		k.noteShapeSites(t.RHS, env)
	case *syntax.BangValue:
		for _, a := range t.Args {
			k.noteShapeSites(a, env)
		}
		for _, c := range t.Cases {
			k.noteShapeSites(c.Key, env)
			k.noteShapeSites(c.Value, env)
		}
	}
}

// templateArgKey reports the qualified key TGParser's QualifyName would use for
// name inside env's enclosing record, when name is one of its template
// arguments rather than a field.
func templateArgKey(env *Env, name string, force bool) (string, *Ty, bool) {
	if env == nil || env.owner == nil {
		return "", nil, false
	}
	own := env.owner
	if !force && own.Val(name) != nil {
		return "", nil, false
	}
	sep := env.ownerSep
	if sep == "" {
		sep = ":"
	}
	for i := range own.TArgs {
		if own.TArgs[i].Name == name {
			return own.Name + sep + name, own.TArgs[i].Ty, true
		}
	}
	// Multiclass records do not cache their template arguments; the frame
	// carries them under qualified keys instead.
	q := own.Name + sep + name
	if bnd, ok := env.Lookup(q); ok {
		return q, bnd.Ty, true
	}
	return "", nil, false
}

func (k *Keeper) shapeKey(t *syntax.ClassApplyValue, env *Env) string {
	var b strings.Builder
	writeShapeKey(&b, t, env)
	return b.String()
}

// writeShapeKey serialises a parsed value the way VarDefInit pooling sees it.
func writeShapeKey(b *strings.Builder, v syntax.Value, env *Env) {
	switch x := v.(type) {
	case nil:
		b.WriteString("nil")
	case *syntax.UnsetValue:
		b.WriteByte('?')
	case *syntax.IntValue:
		b.WriteByte('#')
		b.WriteString(strconv.FormatUint(x.Num, 10))
	case *syntax.BitsValue:
		b.WriteByte('{')
		for _, e := range x.Elems {
			writeShapeKey(b, e, env)
			b.WriteByte(',')
		}
		if x.Num != 0 {
			b.WriteString("0b")
			b.WriteString(strconv.FormatUint(x.Num, 10))
		}
		b.WriteByte('}')
	case *syntax.StringValue:
		if x.Code {
			b.WriteString("code")
		}
		b.WriteString(strconv.Quote(x.Text))
	case *syntax.ListValue:
		b.WriteByte('[')
		for _, e := range x.Elems {
			writeShapeKey(b, e, env)
			b.WriteByte(',')
		}
		b.WriteByte(']')
		if x.ElemType != nil {
			b.WriteString(x.ElemType.String())
		}
	case *syntax.DagValue:
		b.WriteByte('(')
		writeShapeKey(b, x.Op, env)
		for _, a := range x.Args {
			b.WriteByte(' ')
			b.WriteString(a.Name)
			b.WriteByte('=')
			writeShapeKey(b, a.Value, env)
			b.WriteByte(',')
		}
		b.WriteByte(')')
	case *syntax.VarValue:
		// A defvar is substituted textually by the parser, so LLVM pools the
		// value it was folded to, not the name.
		if x.Subst && x.Lit != nil {
			writeShapeKey(b, x.Lit, env)
			return
		}
		if x.GlobalRef {
			b.WriteString("G")
			b.WriteString(strconv.Quote(x.Name))
			return
		}
		// A class or multiclass template argument is a VarInit keyed by its
		// qualified name and declared type; the qualification is what keeps
		// two same-named arguments of different classes apart. The parser
		// already qualified the reference, which stays valid when the field is
		// inherited by an instance.
		if !x.Lexical {
			if x.ArgQual != "" {
				b.WriteByte('q')
				b.WriteString(strconv.Quote(x.ArgQual))
				b.WriteByte(':')
				if x.Ty.Kind != syntax.InvalidType {
					b.WriteString(x.Ty.String())
				}
				return
			}
			if q, ty, ok := templateArgKey(env, x.Name, x.ArgRef); ok {
				b.WriteByte('q')
				b.WriteString(strconv.Quote(q))
				b.WriteByte(':')
				if ty != nil {
					b.WriteString(ty.String())
				}
				return
			}
		}
		tyText := ""
		if x.Ty.Kind != syntax.InvalidType {
			tyText = x.Ty.String()
		}
		// A field reference is VarInit(field, declaredType): the declared type
		// distinguishes same-named fields of different classes.
		if env != nil && env.owner != nil {
			if rv := env.owner.Val(x.Name); rv != nil && rv.Ty != nil {
				tyText = rv.Ty.String()
			}
		}
		b.WriteByte('v')
		b.WriteString(strconv.Itoa(len(x.Name)))
		b.WriteByte(':')
		b.WriteString(x.Name)
		if tyText != "" {
			b.WriteByte(':')
			b.WriteString(tyText)
		}
	case *syntax.FieldValue:
		writeShapeKey(b, x.Base, env)
		b.WriteByte('.')
		b.WriteString(x.Field)
	case *syntax.SliceValue:
		writeShapeKey(b, x.Base, env)
		b.WriteByte('[')
		for _, e := range x.Index {
			writeShapeKey(b, e.Lo, env)
			b.WriteByte(':')
			writeShapeKey(b, e.Hi, env)
			b.WriteByte(',')
		}
		b.WriteByte(']')
	case *syntax.BitRangeValue:
		writeShapeKey(b, x.Base, env)
		b.WriteByte('{')
		for _, bit := range x.Bits {
			b.WriteString(strconv.FormatInt(int64(bit), 10))
			b.WriteByte(',')
		}
		b.WriteByte('}')
	case *syntax.PasteValue:
		writeShapeKey(b, x.LHS, env)
		b.WriteByte('#')
		writeShapeKey(b, x.RHS, env)
	case *syntax.ClassApplyValue:
		b.WriteString(x.Class)
		b.WriteByte('<')
		for _, a := range x.Args {
			b.WriteString(a.Name)
			b.WriteByte('=')
			writeShapeKey(b, a.Value, env)
			b.WriteByte(',')
		}
		b.WriteByte('>')
	case *syntax.BangValue:
		b.WriteByte('!')
		b.WriteString(strconv.Itoa(int(x.Op)))
		b.WriteByte('(')
		for _, a := range x.Args {
			writeShapeKey(b, a, env)
			b.WriteByte(',')
		}
		for _, c := range x.Cases {
			writeShapeKey(b, c.Key, env)
			b.WriteByte('=')
			writeShapeKey(b, c.Value, env)
			b.WriteByte(',')
		}
		for _, n := range x.VarNames {
			b.WriteString(n)
			b.WriteByte(',')
		}
		b.WriteByte(')')
	default:
		b.WriteString("?unknown")
	}
}
