package backend

import (
	"strings"

	"github.com/nodecoda/tablegen-go/internal/record"
)

// PrintRecords renders the default llvm-tblgen action: every class (sorted by
// name), every def (sorted by name), each in Record::dump form.
func PrintRecords(k *record.Keeper) ([]byte, error) {
	var b strings.Builder
	b.WriteString("------------- Classes -----------------\n")
	for _, c := range k.Classes() {
		t, err := c.DumpText()
		if err != nil {
			return nil, err
		}
		b.WriteString("class ")
		b.WriteString(t)
	}
	b.WriteString("------------- Defs -----------------\n")
	for _, d := range k.Defs() {
		t, err := d.DumpText()
		if err != nil {
			return nil, err
		}
		b.WriteString("def ")
		b.WriteString(t)
	}
	return []byte(b.String()), nil
}
