package backend

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"tblgen/internal/diag"
	"tblgen/internal/record"
)

// Detailed is the --print-detailed-records action
// (lib/TableGen/DetailedRecordsBackend.cpp): global variables, then every
// class with template args / superclasses / fields, then every def with its
// defm sequence, superclasses and fields, each item carrying its source
// location as `<basename>:<line>` (SrcMgr.getFormattedLocationNoOffset with
// IncludePath=false).
//
// The C++ backend closes the report with Records.dumpAllocationStats, which
// counts LLVM's internal Init allocator pools. Those numbers depend on LLVM's
// uniqued Init pools, not on the TableGen program, so this implementation
// stops right after the section heading: every byte we emit is a prefix of
// llvm-tblgen's output, and the harness compares that prefix.
func Detailed(k *record.Keeper) ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "DETAILED RECORDS for file %s\n", k.InputFile())

	globals := k.Globals()
	detailedHeading(&b, "Global Variables", strconv.Itoa(len(globals)))
	b.WriteString("\n")
	for _, g := range globals {
		b.WriteString(g.Name)
		b.WriteString(" = ")
		b.WriteString(g.Text())
		b.WriteString("\n")
	}

	classes := k.Classes()
	detailedHeading(&b, "Classes", strconv.Itoa(len(classes)))
	for _, c := range classes {
		detailedRecordHead(&b, c.Name, c)
		detailedTemplateArgs(&b, c)
		detailedSuperclasses(&b, c)
		if err := detailedFields(&b, c); err != nil {
			return nil, err
		}
	}

	defs := k.Defs()
	detailedHeading(&b, "Records", strconv.Itoa(len(defs)))
	for _, d := range defs {
		name := d.Name
		if name == "" {
			// printRecords prints the empty name as a pair of quotes.
			name = `""`
		}
		detailedRecordHead(&b, name, d)
		detailedDefms(&b, d)
		detailedSuperclasses(&b, d)
		if err := detailedFields(&b, d); err != nil {
			return nil, err
		}
	}

	detailedHeading(&b, "Memory Allocation Stats", "")
	return []byte(b.String()), nil
}

func detailedHeading(b *strings.Builder, title, count string) {
	b.WriteString("\n-------------------- ")
	b.WriteString(title)
	if count != "" {
		b.WriteString(" (")
		b.WriteString(count)
		b.WriteString(")")
	}
	b.WriteString(" --------------------\n")
}

func detailedRecordHead(b *strings.Builder, name string, r *record.Record) {
	b.WriteString("\n")
	b.WriteString(name)
	b.WriteString("  |")
	b.WriteString(detailedLoc(r.Locs))
	b.WriteString("|\n")
}

func detailedTemplateArgs(b *strings.Builder, r *record.Record) {
	if len(r.TArgs) == 0 {
		b.WriteString("  Template args: (none)\n")
		return
	}
	b.WriteString("  Template args:\n")
	for _, ta := range r.TArgs {
		b.WriteString("    ")
		b.WriteString(r.TemplateArgText(ta))
		b.WriteString("  |")
		b.WriteString(detailedPos(ta.Loc))
		b.WriteString("|\n")
	}
}

func detailedSuperclasses(b *strings.Builder, r *record.Record) {
	scs := r.SuperClasses()
	if len(scs) == 0 {
		b.WriteString("  Superclasses: (none)\n")
		return
	}
	b.WriteString("  Superclasses:")
	for _, sc := range scs {
		// Indirect superclasses are enclosed in parentheses.
		if r.HasDirectSuperClass(sc) {
			b.WriteString(" ")
			b.WriteString(sc.Name)
		} else {
			b.WriteString(" (")
			b.WriteString(sc.Name)
			b.WriteString(")")
		}
	}
	b.WriteString("\n")
}

func detailedFields(b *strings.Builder, r *record.Record) error {
	if len(r.Vals) == 0 {
		b.WriteString("  Fields: (none)\n")
		return nil
	}
	b.WriteString("  Fields:\n")
	for _, v := range r.Vals {
		t, err := r.ValueText(v)
		if err != nil {
			return err
		}
		b.WriteString("    ")
		b.WriteString(t)
		b.WriteString("  |")
		b.WriteString(detailedPos(v.Loc))
		b.WriteString("|\n")
	}
	return nil
}

// detailedDefms prints the defm instantiation locations, which are stored in
// invocation order and printed in reverse (DetailedRecordsBackend.cpp:139).
func detailedDefms(b *strings.Builder, r *record.Record) {
	if len(r.Locs) < 2 {
		return
	}
	b.WriteString("  Defm sequence:")
	for i := len(r.Locs) - 1; i >= 0; i-- {
		b.WriteString(" |")
		b.WriteString(detailedPos(r.Locs[i]))
		b.WriteString("|")
	}
	b.WriteString("\n")
}

func detailedLoc(locs []diag.Pos) string {
	if len(locs) == 0 {
		return ""
	}
	return detailedPos(locs[0])
}

// detailedPos renders SrcMgr.getFormattedLocationNoOffset: the basename only,
// then the line.
func detailedPos(p diag.Pos) string {
	return filepath.Base(p.File) + ":" + strconv.Itoa(p.Line)
}
