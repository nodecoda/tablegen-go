package syntax

import (
	"strconv"
	"strings"
)

// InitText renders a value the way Init::getAsString prints the Init tree the
// parser built for it. TGParser::CheckTemplateArgValues appends the raw
// argument to its diagnostic (TGParser.cpp:4778), so the spelling here is
// observable: `!cast<B>("A0")` keeps the failing cast, and a reference to a
// class template argument prints its qualified name.
func InitText(v Value) string {
	switch t := v.(type) {
	case nil:
		return "?"
	case *UnsetValue:
		return "?" // UnsetInit::getAsString, Record.cpp:503
	case *IntValue:
		return strconv.FormatInt(int64(t.Num), 10) // IntInit::getAsString, Record.cpp:567
	case *BitsValue:
		// BitsInit::getAsString, Record.cpp:509: "{ a, b }" in source order.
		parts := make([]string, 0, len(t.Elems))
		for _, e := range t.Elems {
			parts = append(parts, InitText(e))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	case *StringValue:
		// StringInit::getAsString, Record.h:752: quotes for SF_String,
		// "[{...}]" for SF_Code.
		if t.Code {
			return "[{" + t.Text + "}]"
		}
		return `"` + t.Text + `"`
	case *ListValue:
		parts := make([]string, 0, len(t.Elems))
		for _, e := range t.Elems {
			parts = append(parts, InitText(e))
		}
		return "[" + strings.Join(parts, ", ") + "]" // Record.cpp:747
	case *DagValue:
		// DagInit::getAsString, Record.cpp:2712: names are printed ":$name".
		op := InitText(t.Op)
		if t.OpName != "" {
			op += ":$" + t.OpName
		}
		parts := make([]string, 0, len(t.Args))
		for _, a := range t.Args {
			s := InitText(a.Value)
			if a.Name != "" {
				s += ":$" + a.Name
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			return "(" + op + ")"
		}
		return "(" + op + " " + strings.Join(parts, ", ") + ")"
	case *VarValue:
		// VarInit::getAsString prints the stored name, which TGParser
		// qualified when the name is a template argument (QualifyName,
		// TGParser.cpp:118-127).
		if t.ArgRef && t.ArgQual != "" {
			return t.ArgQual
		}
		return t.Name
	case *FieldValue:
		return InitText(t.Base) + "." + t.Field // FieldRefInit, Record.h:1459
	case *SliceValue:
		parts := make([]string, 0, len(t.Index))
		for _, e := range t.Index {
			if e.Hi != nil {
				parts = append(parts, InitText(e.Lo)+"..."+InitText(e.Hi))
			} else {
				parts = append(parts, InitText(e.Lo))
			}
		}
		return InitText(t.Base) + "[" + strings.Join(parts, ", ") + "]"
	case *BitRangeValue:
		// A bit range folds to a BitsInit of VarBitInits, printed "{ b{3} }".
		parts := make([]string, 0, len(t.Bits))
		for _, bit := range t.Bits {
			parts = append(parts, InitText(t.Base)+"{"+strconv.FormatInt(int64(bit), 10)+"}")
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	case *PasteValue:
		return "!strconcat(" + InitText(t.LHS) + ", " + InitText(t.RHS) + ")" // Record.cpp:1148
	case *ClassApplyValue:
		if t.FoldName != "" {
			return t.FoldName // VarDefInit::Fold produced a DefInit, Record.cpp:1295
		}
		parts := make([]string, 0, len(t.Args))
		for _, a := range t.Args {
			parts = append(parts, InitText(a.Value))
		}
		return t.Class + "<" + strings.Join(parts, ", ") + ">"
	case *BangValue:
		var out strings.Builder
		out.WriteString("!" + strings.TrimPrefix(t.Op.String(), "!"))
		if t.TypeArg.Kind != InvalidType {
			out.WriteString("<" + t.TypeArg.String() + ">")
		}
		parts := make([]string, 0, len(t.Args)+len(t.Cases))
		for _, a := range t.Args {
			parts = append(parts, InitText(a))
		}
		for _, c := range t.Cases {
			if c.Key == nil {
				parts = append(parts, InitText(c.Value))
			} else {
				parts = append(parts, InitText(c.Key)+": "+InitText(c.Value))
			}
		}
		out.WriteString("(" + strings.Join(parts, ", ") + ")")
		return out.String()
	}
	return DumpValue(v)
}
