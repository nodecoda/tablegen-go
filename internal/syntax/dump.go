package syntax

import (
	"fmt"
	"strings"
)

// Dump renders an AST as indented S-expressions. It exists so the tree can be
// inspected, diffed and pinned in tests without a debugger; the format is not a
// compatibility surface.
func DumpFile(f *File) string {
	var b strings.Builder
	for _, o := range f.Objects {
		dumpObject(&b, o, 0)
	}
	return b.String()
}

func indent(b *strings.Builder, n int) {
	b.WriteString(strings.Repeat("  ", n))
}

func linef(b *strings.Builder, n int, format string, args ...any) {
	indent(b, n)
	fmt.Fprintf(b, format, args...)
	b.WriteByte('\n')
}

func dumpObject(b *strings.Builder, o Object, n int) {
	switch t := o.(type) {
	case *ClassStmt:
		linef(b, n, "(class %s)", t.Name)
		for _, a := range t.TemplateArgs {
			dumpDecl(b, a, n+1)
		}
		for _, p := range t.Parents {
			dumpParent(b, p, n+1)
		}
		for _, it := range t.Body {
			dumpBodyItem(b, it, n+1)
		}
	case *DefStmt:
		name := "<anon>"
		if t.Name != nil {
			name = DumpValue(t.Name)
		}
		linef(b, n, "(def %s)", name)
		for _, p := range t.Parents {
			dumpParent(b, p, n+1)
		}
		for _, it := range t.Body {
			dumpBodyItem(b, it, n+1)
		}
	case *DefmStmt:
		name := "<anon>"
		if t.Name != nil {
			name = DumpValue(t.Name)
		}
		linef(b, n, "(defm %s)", name)
		for i, p := range t.Parents {
			kind := "mc"
			if i >= t.ClassStart {
				kind = "class"
			}
			indent(b, n+1)
			b.WriteString("(" + kind + " ")
			b.WriteString(p.Name)
			dumpArgs(b, p.Args)
			b.WriteString(")\n")
		}
	case *MultiClassStmt:
		linef(b, n, "(multiclass %s)", t.Name)
		for _, a := range t.TemplateArgs {
			dumpDecl(b, a, n+1)
		}
		for _, p := range t.Parents {
			dumpParent(b, p, n+1)
		}
		for _, it := range t.Body {
			dumpObject(b, it, n+1)
		}
	case *DefvarStmt:
		linef(b, n, "(defvar %s %s)", t.Name, DumpValue(t.Value))
	case *DefsetStmt:
		linef(b, n, "(defset list<%s> %s)", t.ElemType.String(), t.Name)
		for _, it := range t.Objects {
			dumpObject(b, it, n+1)
		}
	case *DeftypeStmt:
		linef(b, n, "(deftype %s %s)", t.Name, t.Type.String())
	case *ForeachStmt:
		if t.List != nil {
			linef(b, n, "(foreach %s in %s)", t.Var, DumpValue(t.List))
		} else {
			linef(b, n, "(foreach %s in %v)", t.Var, t.Range)
		}
		for _, it := range t.Body {
			dumpObject(b, it, n+1)
		}
	case *IfStmt:
		linef(b, n, "(if %s then", DumpValue(t.Cond))
		for _, it := range t.Then {
			dumpObject(b, it, n+1)
		}
		if t.Else != nil {
			linef(b, n, " else")
			for _, it := range t.Else {
				dumpObject(b, it, n+1)
			}
		}
		linef(b, n, ")")
	case *LetStmt:
		linef(b, n, "(let")
		for _, l := range t.Binds {
			dumpLet(b, l, n+1)
		}
		for _, it := range t.Body {
			dumpObject(b, it, n+1)
		}
		linef(b, n, ")")
	case *AssertStmt:
		linef(b, n, "(assert %s %s)", DumpValue(t.Cond), DumpValue(t.Message))
	case *DumpStmt:
		linef(b, n, "(dump %s)", DumpValue(t.Value))
	case *IncludeStmt:
		linef(b, n, "(include %q)", t.File)
	default:
		linef(b, n, "(? %T)", o)
	}
}

func dumpBodyItem(b *strings.Builder, it BodyItem, n int) {
	switch t := it.(type) {
	case *Decl:
		dumpDecl(b, t, n)
	case *LetItem:
		linef(b, n, "(let-item")
		dumpLet(b, t.Bind, n+1)
		linef(b, n, ")")
	case *AssertStmt:
		dumpObject(b, t, n)
	case *DumpStmt:
		dumpObject(b, t, n)
	case *DefvarStmt:
		dumpObject(b, t, n)
	default:
		linef(b, n, "(?item %T)", it)
	}
}

func dumpDecl(b *strings.Builder, d *Decl, n int) {
	kind := "field"
	if d.IsArg {
		kind = "arg"
	}
	if d.IsField {
		kind = "field!"
	}
	if d.Value == nil {
		linef(b, n, "(%s %s %s)", kind, d.Type.String(), d.Name)
		return
	}
	linef(b, n, "(%s %s %s = %s)", kind, d.Type.String(), d.Name, DumpValue(d.Value))
}

func dumpLet(b *strings.Builder, l *LetBind, n int) {
	if l == nil {
		return
	}
	bits := ""
	if len(l.Bits) > 0 {
		bits = fmt.Sprintf("%v", l.Bits)
	}
	linef(b, n, "(let %s %s%s = %s)", l.Mode, l.Field, bits, DumpValue(l.Value))
}

func dumpParent(b *strings.Builder, p *ParentRef, n int) {
	indent(b, n)
	b.WriteString("(parent ")
	b.WriteString(p.Name)
	dumpArgs(b, p.Args)
	b.WriteString(")\n")
}

func dumpArgs(b *strings.Builder, args []*Arg) {
	for _, a := range args {
		if a.Name != "" {
			b.WriteString(" " + a.Name + "=" + DumpValue(a.Value))
		} else {
			b.WriteString(" " + DumpValue(a.Value))
		}
	}
}

// DumpValue renders a value tree on one line.
func DumpValue(v Value) string {
	if v == nil {
		return "<nil>"
	}
	switch t := v.(type) {
	case *UnsetValue:
		return "?"
	case *IntValue:
		return itoa(int(int64(t.Num)))
	case *BitsValue:
		if t.Num != 0 || len(t.Elems) == 0 {
			var parts []string
			for _, e := range t.Elems {
				parts = append(parts, DumpValue(e))
			}
			return "bits{" + strings.Join(parts, ",") + "}"
		}
		var parts []string
		for _, e := range t.Elems {
			parts = append(parts, DumpValue(e))
		}
		return "bits{" + strings.Join(parts, ",") + "}"
	case *StringValue:
		if t.Code {
			return "code(" + t.Text + ")"
		}
		return fmt.Sprintf("%q", t.Text)
	case *ListValue:
		var parts []string
		for _, e := range t.Elems {
			parts = append(parts, DumpValue(e))
		}
		out := "[" + strings.Join(parts, ", ") + "]"
		if t.ElemType != nil {
			out += "<" + t.ElemType.String() + ">"
		}
		return out
	case *DagValue:
		var parts []string
		for _, a := range t.Args {
			if a.Name != "" {
				parts = append(parts, DumpValue(a.Value)+":"+a.Name)
			} else {
				parts = append(parts, DumpValue(a.Value))
			}
		}
		op := DumpValue(t.Op)
		if t.OpName != "" {
			op += ":" + t.OpName
		}
		return "(" + op + " " + strings.Join(parts, ", ") + ")"
	case *VarValue:
		return t.Name
	case *FieldValue:
		return DumpValue(t.Base) + "." + t.Field
	case *SliceValue:
		var parts []string
		for _, e := range t.Index {
			if e.Hi != nil {
				parts = append(parts, DumpValue(e.Lo)+"..."+DumpValue(e.Hi))
			} else {
				parts = append(parts, DumpValue(e.Lo))
			}
		}
		return DumpValue(t.Base) + "[" + strings.Join(parts, ", ") + "]"
	case *BitRangeValue:
		var parts []string
		for _, bit := range t.Bits {
			parts = append(parts, itoa(int(bit)))
		}
		return DumpValue(t.Base) + "{" + strings.Join(parts, ",") + "}"
	case *PasteValue:
		return DumpValue(t.LHS) + "#" + DumpValue(t.RHS)
	case *ClassApplyValue:
		indentTxt := ""
		for _, a := range t.Args {
			_ = a
			indentTxt += " ?"
		}
		return t.Class + "<...>"
	case *BangValue:
		var parts []string
		for _, a := range t.Args {
			parts = append(parts, DumpValue(a))
		}
		for _, c := range t.Cases {
			if c.Key == nil {
				parts = append(parts, DumpValue(c.Value))
			} else {
				parts = append(parts, DumpValue(c.Key)+": "+DumpValue(c.Value))
			}
		}
		out := "!" + strings.TrimPrefix(t.Op.String(), "!")
		if t.TypeArg.Kind != InvalidType {
			out += "<" + t.TypeArg.String() + ">"
		}
		return out + "(" + strings.Join(parts, ", ") + ")"
	}
	return "<value>"
}
