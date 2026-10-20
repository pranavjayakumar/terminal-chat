package tui

import (
	"strings"
	"testing"
)

// Every help description is readable in full at any width (40-column
// herdr splits and 80x24 included): long ones wrap instead of being cut.
func TestHelpWrapsAtAllWidths(t *testing.T) {
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for _, w := range []int{40, 60, 80, 120} {
		m := newTestModel(t, w, 24)
		m.openHelp()
		for scroll := 0; scroll <= m.helpMaxScroll(); scroll++ {
			m.helpScroll = scroll
			checkView(t, m, "help")
		}
		sw, _ := m.keySheetSize()
		got := squash(strings.Join(sheetPlain(sw), ""))
		for _, c := range commandTable {
			if c.helpHidden || c.desc == "" {
				continue
			}
			if !strings.Contains(got, squash(c.desc)) {
				t.Errorf("width %d: %q is cut", w, c.desc)
			}
		}
		for _, l := range sheetPlain(sw) {
			if strWidth(l) > sw {
				t.Errorf("width %d: row %q is %d wide", w, l, strWidth(l))
			}
		}
	}
}
