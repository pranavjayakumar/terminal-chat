package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
)

// Every help description is readable in full at any width (40-column
// herdr splits and 80x24 included): long ones wrap instead of being cut.
func TestHelpWrapsAtAllWidths(t *testing.T) {
	squash := func(s string) string { return strings.Join(strings.Fields(s), "") }
	for _, w := range []int{40, 60, 80, 120} {
		m := newTestModel(t, w, 24)
		m.openHelp()
		var all strings.Builder
		for scroll := 0; ; scroll++ {
			m.helpScroll = scroll
			if scroll > m.helpMaxScroll() {
				break
			}
			checkView(t, m, "help")
		}
		// Collect every row by scrolling one body page at a time.
		m.Update(tea.WindowSizeMsg{Width: w, Height: 400})
		m.helpScroll = 0
		for _, l := range m.helpLines(m.dims().mainW, m.dims().bodyH) {
			all.WriteString(ansi.Strip(l))
		}
		got := squash(all.String())
		for _, kv := range helpLines {
			if kv[1] == "" {
				continue
			}
			if !strings.Contains(got, squash(kv[1])) {
				t.Errorf("width %d: %q is cut", w, kv[1])
			}
		}
	}
}
