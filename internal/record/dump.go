package record

import (
	"github.com/nodecoda/tablegen-go/internal/diag"
	"github.com/nodecoda/tablegen-go/internal/syntax"
)

// Dumps are LLVM's `dump` statement. TGParser::ParseDump (TGParser.cpp:4816)
// wraps a bare def/defvar operand in !repr, stores the message on the record
// when there is one, and otherwise resolves and prints it immediately.
// Record::emitRecordDumps (Record.cpp:3125) resolves a def's messages with a
// final RecordResolver once the record is complete (addDefOne,
// TGParser.cpp:606; VarDefInit::instantiate, Record.cpp:2408-2419), so an
// inherited dump runs once per concrete def and sees the final field values.
// dumpMessage (Error.cpp:178-186) prints a string message as a note and any
// other value as the error "dump value is not of type string".

// emitDump evaluates one message and prints the note (or the type error).
func (l *Lowerer) emitDump(loc diag.Pos, msg syntax.Value, env *Env, rec *Record) {
	v, err := l.k.evalSyntax(msg, env, rec)
	if err != nil {
		l.errorAt(err, spanAt(&loc))
		return
	}
	l.k.emitDumpValue(loc, msg, v)
}

// emitDumpValue applies dumpMessage to an evaluated message value.
func (k *Keeper) emitDumpValue(loc diag.Pos, msg syntax.Value, v Value) {
	span := diag.Span{Start: loc, End: loc}
	if isDefName(msg) {
		// ParseDump's implicit !repr: only a directly named def is wrapped.
		if d, ok := v.(*DefVal); ok {
			text, err := d.Rec.DumpText()
			if err != nil {
				k.diags.Error("E_SEMANTIC", err.Error(), span)
				return
			}
			k.diags.Note("E_DUMP", text, span)
			return
		}
	}
	if s, ok := v.(*Str); ok {
		k.diags.Note("E_DUMP", s.S, span)
		return
	}
	k.diags.Error("E_DUMP_TYPE", "dump value is not of type string", span)
}

// emitDumps is the keeper-level form used when a record is completed outside
// the Lowerer (VarDefInit::instantiate, Record.cpp:2408-2419).
func (k *Keeper) emitDumps(rec *Record) {
	for _, d := range rec.Dumps {
		v, err := k.evalSyntax(d.Msg, d.Env, rec)
		if err != nil {
			span := spanAt(&d.Loc)
			if ee, ok := err.(*EvalError); ok && ee.Span.Start.File != "" && !ee.NoLoc {
				span = ee.Span
			}
			k.diags.Error("E_SEMANTIC", err.Error(), span)
			continue
		}
		k.emitDumpValue(d.Loc, d.Msg, v)
	}
}

// isDefName reports whether the parsed operand is a lexical reference to a
// record, which TGParser::ParseDump turns into !repr. A defvar or defset is
// not a DefInit and keeps its own value.
func isDefName(v syntax.Value) bool {
	vv, ok := v.(*syntax.VarValue)
	return ok && vv.GlobalRef && !vv.GlobalVar
}

// emitRecordDumps is Record::emitRecordDumps: every stored dump runs in
// insertion order (parents first, then the body).
func (l *Lowerer) emitRecordDumps(rec *Record) {
	for _, d := range rec.Dumps {
		l.emitDump(d.Loc, d.Msg, d.Env, rec)
	}
}
