package tui

import "strings"

// sheetPlain is the keys sheet's text at width w, one string per row.
func sheetPlain(w int) []string {
	var out []string
	for _, row := range keySheetRows(w) {
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
