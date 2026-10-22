package tui

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// /notify ends with whether tc background is on, its shell command as a
// hint (dropped in narrow splits like the others), and nothing when the
// state can't be read. Every frame fits.
func TestNotifyShowsBackground(t *testing.T) {
	for _, c := range []struct {
		on, known bool
		want      string
	}{
		{true, true, "when tc is closed: on (shell: tc background off)"},
		{false, true, "when tc is closed: off (shell: tc background on)"},
		{false, false, ""},
	} {
		for _, sz := range [][2]int{{30, 10}, {40, 12}, {80, 24}, {200, 60}} {
			m := newTestModel(t, sz[0], sz[1])
			m.opt.Background = func() (bool, bool) { return c.on, c.known }
			runInput(t, m, "/notify")
			name := fmt.Sprintf("%v/%v at %dx%d", c.on, c.known, sz[0], sz[1])
			checkView(t, m, name)
			last := m.list.rows[len(m.list.rows)-1]
			has := slices.ContainsFunc(m.list.rows, func(r string) bool { return strings.HasPrefix(r, "when tc is closed") })
			switch {
			case c.want == "" && has:
				t.Fatalf("%s: unknown state shown: %q", name, m.list.rows)
			case c.want != "" && m.dims().mainW >= 50 && last != c.want:
				t.Fatalf("%s: last row %q, want %q", name, last, c.want)
			case c.want != "" && m.dims().mainW < 50 && last != strings.SplitN(c.want, " (", 2)[0]:
				t.Fatalf("%s: narrow row %q", name, last)
			}
			m.Update(tea.KeyMsg{Type: tea.KeyEsc})
		}
	}
}
