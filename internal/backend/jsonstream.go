package backend

import (
	"strconv"
	"unicode/utf8"

	"github.com/nodecoda/tablegen-go/internal/record"
)

// jsonBuf is the streaming replacement for the map[string]any tree JSON()
// used to build. encoding/json sorts map keys, so the stream emits keys in
// exactly that order; appendString mirrors the encoder's string escaping with
// SetEscapeHTML(false). json_ref_test.go keeps the old tree encoder and
// asserts both produce identical bytes.
type jsonBuf struct {
	b []byte
}

func (w *jsonBuf) raw(s string) { w.b = append(w.b, s...) }
func (w *jsonBuf) ch(c byte)    { w.b = append(w.b, c) }
func (w *jsonBuf) num(n int64)  { w.b = strconv.AppendInt(w.b, n, 10) }
func (w *jsonBuf) str(s string) { w.b = appendJSONString(w.b, s) }
func (w *jsonBuf) null()        { w.b = append(w.b, "null"...) }
func (w *jsonBuf) boolean(v bool) {
	if v {
		w.raw("true")
	} else {
		w.raw("false")
	}
}

const jsonHex = "0123456789abcdef"

// appendJSONString is encodeState.string with htmlSafe=false: only quote,
// backslash and control characters are escaped, plus U+2028/U+2029 and
// invalid UTF-8 bytes.
func appendJSONString(b []byte, s string) []byte {
	b = append(b, '"')
	start := 0
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			if c >= 0x20 && c != '"' && c != '\\' {
				i++
				continue
			}
			b = append(b, s[start:i]...)
			switch c {
			case '"', '\\':
				b = append(b, '\\', c)
			case '\n':
				b = append(b, '\\', 'n')
			case '\r':
				b = append(b, '\\', 'r')
			case '\t':
				b = append(b, '\\', 't')
			default:
				b = append(b, '\\', 'u', '0', '0', jsonHex[c>>4], jsonHex[c&0xF])
			}
			i++
			start = i
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b = append(b, s[start:i]...)
			b = append(b, `\ufffd`...)
			i += size
			start = i
			continue
		}
		if r == '\u2028' || r == '\u2029' {
			b = append(b, s[start:i]...)
			b = append(b, '\\', 'u', '2', '0', '2', jsonHex[r&0xF])
			i += size
			start = i
			continue
		}
		i += size
	}
	b = append(b, s[start:]...)
	return append(b, '"')
}

// writeJSONValue emits one value exactly as JSONBackend.cpp describes it,
// matching translate() in the previous implementation.
func writeJSONValue(w *jsonBuf, v record.Value) {
	switch x := v.(type) {
	case *record.Unset:
		w.null()
	case *record.Bit:
		if x.B {
			w.num(1)
		} else {
			w.num(0)
		}
	case *record.Bits:
		w.ch('[')
		for i, bit := range x.B {
			if i > 0 {
				w.ch(',')
			}
			writeJSONValue(w, bit)
		}
		w.ch(']')
	case *record.Int:
		w.num(x.N)
	case *record.Str:
		w.str(x.S)
	case *record.List:
		w.ch('[')
		for i, e := range x.Elems {
			if i > 0 {
				w.ch(',')
			}
			writeJSONValue(w, e)
		}
		w.ch(']')
	case *record.Dag:
		// encoding/json sorts object keys: args, kind, name, operator, printable.
		w.raw(`{"args":[`)
		for i, a := range x.Args {
			if i > 0 {
				w.ch(',')
			}
			w.ch('[')
			writeJSONValue(w, a.Value)
			w.ch(',')
			if !a.HasName {
				w.null()
			} else {
				w.str(a.Name)
			}
			w.ch(']')
		}
		w.raw(`],"kind":"dag"`)
		if x.OpName != "" {
			w.raw(`,"name":`)
			w.str(x.OpName)
		}
		w.raw(`,"operator":`)
		writeJSONValue(w, x.Op)
		w.raw(`,"printable":`)
		w.str(x.String())
		w.ch('}')
	case *record.BitRef:
		if _, unset := x.Value.(*record.Unset); unset {
			w.raw(`{"index":`)
			w.num(int64(x.Index))
			w.raw(`,"kind":"varbit","printable":`)
			w.str(x.Var + "{" + strconv.Itoa(x.Index) + "}")
			w.raw(`,"var":`)
			w.str(x.Var)
			w.ch('}')
			return
		}
		writeJSONValue(w, x.Value)
	case *record.Unresolved:
		switch x.Kind {
		case "var":
			w.raw(`{"kind":"var","printable":`)
			w.str(x.Text)
			w.raw(`,"var":`)
			w.str(x.Var)
			w.ch('}')
		case "varbit":
			w.raw(`{"index":`)
			w.num(int64(x.Index))
			w.raw(`,"kind":"varbit","printable":`)
			w.str(x.Text)
			w.raw(`,"var":`)
			w.str(x.Var)
			w.ch('}')
		default:
			w.raw(`{"kind":"complex","printable":`)
			w.str(x.Text)
			w.ch('}')
		}
	case *record.DefVal:
		w.raw(`{"def":`)
		w.str(x.Rec.Name)
		w.raw(`,"kind":"def","printable":`)
		w.str(x.Rec.Name)
		w.ch('}')
	default:
		w.raw(`{"kind":"complex","printable":`)
		w.str(v.String())
		w.ch('}')
	}
}
