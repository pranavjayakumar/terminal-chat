package tui

import "strings"

// sheetPlain is the keys sheet's text at width w, one string per row.
func sheetPlain(w int) []string { return sheetPlainKeys(w, keyNames{}) }

func sheetPlainKeys(w int, kn keyNames) []string {
	var out []string
	for _, row := range keySheetRows(w, kn) {
		var b strings.Builder
		for _, c := range row {
			for strWidth(b.String()) < c.x {
				b.WriteByte(' ')
			}
			b.WriteString(c.text)
		}
		out = append(out, b.String())
	}
	return out
}
