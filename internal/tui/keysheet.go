package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// The keyboard sheet (F1, /help): grouped key columns, then every slash
// command (from commandTable, like the / menu) below them.

type keyGroup struct {
	title string
	keys  [][2]string
}

var keyGroups = []keyGroup{
	{"Navigate", [][2]string{
		{"⌥K", "command palette"}, {"⌥↑ ⌥↓", "previous / next channel"}, {"⌃P ⌃N", "previous / next channel"},
		{"⌥⇧↑ ⌥⇧↓", "previous / next unread"}, {"⌥A", "activity"}, {"⌃B F2", "sidebar size"},
		{"PgUp PgDn", "scroll history"}, {"End", "jump to newest"}, {"esc", "back out of anything"},
	}},
	{"Messages", [][2]string{
		{"↑", "select (empty input)"}, {"r", "reply in thread"}, {"+", "react (emoji picker)"}, {"e", "edit yours"},
		{"d", "delete yours"}, {"y", "copy"}, {"p", "pin / unpin"}, {"v", "view full"}, {"r d", "failed: retry, discard"},
	}},
	{"Write", [][2]string{
		{"⏎", "send"}, {"⌥⏎ ⌃J", "new line"}, {"@", "mention someone"}, {"#", "link a channel"},
		{"/", "commands"}, {"⌥E", "emoji picker"}, {":fi", "emoji by name"}, {"⇥", "complete"}, {"⌃U", "clear draft"}, {"esc esc", "clear draft"},
	}},
	{"Huddle", [][2]string{
		{"/huddle", "join here"}, {"⌃T hold", "push to talk"}, {"⌃T tap", "open mic on / off"}, {"/leave-huddle", "leave"},
	}},
	{"App", [][2]string{
		{"/settings", "settings"}, {"/search", "search"}, {"F1", "this sheet"}, {"⌃C ⌃C", "quit"},
	}},
}

// keySheetRows lays the sheet out at width w: rows of (x, text, kind)
// cells. Columns: 3 at ≥108, 2 at ≥70, else 1.
type sheetCell struct {
	x    int
	text string
	kind int // 0 title, 1 key, 2 description, 3 command, 4 command description
}

func keySheetRows(w int) [][]sheetCell {
	cols := 1
	switch {
	case w >= 108:
		cols = 3
	case w >= 70:
		cols = 2
	}
	colW := w / cols
	place := []int{0, 1, 2, 0, 2}
	if cols == 2 {
		place = []int{0, 1, 1, 0, 0}
	}
	if cols == 1 {
		place = []int{0, 0, 0, 0, 0}
	}
	colRows := make([][][]sheetCell, cols)
	for gi, g := range keyGroups {
		ci := place[gi]
		x := ci * colW
		rows := &colRows[ci]
		if len(*rows) > 0 {
			*rows = append(*rows, nil)
		}
		*rows = append(*rows, []sheetCell{{x, g.title, 0}}, nil)
		dx := 12 // one description column per group, past its widest keycap
		for _, k := range g.keys {
			dx = max(dx, strWidth(k[0])+3)
		}
		for _, k := range g.keys {
			*rows = append(*rows, []sheetCell{{x, k[0], 1}, {x + dx, truncate(k[1], max(colW-dx-2, 4)), 2}})
		}
	}
	n := 0
	for _, r := range colRows {
		n = max(n, len(r))
	}
	out := make([][]sheetCell, n)
	for _, r := range colRows {
		for i, cells := range r {
			out[i] = append(out[i], cells...)
		}
	}
	out = append(out, nil, []sheetCell{{0, "Commands", 0}}, nil)
	for _, c := range commandTable {
		if c.helpHidden || c.name == "" && c.help == "" {
			continue
		}
		k := c.helpKey()
		dx := min(28, w/3)
		if w < 60 {
			// narrow: the key on its own row, the description under it
			out = append(out, []sheetCell{{0, k, 3}})
			for _, l := range strings.Split(ansi.Wrap(c.desc, max(w-2, 4), ""), "\n") {
				out = append(out, []sheetCell{{2, l, 4}})
			}
			continue
		}
		dx = max(dx, strWidth(k)+2)
		for i, l := range strings.Split(ansi.Wrap(c.desc, max(w-dx, 4), ""), "\n") {
			row := []sheetCell{{dx, l, 4}}
			if i == 0 {
				row = append([]sheetCell{{0, k, 3}}, row...)
			}
			out = append(out, row)
		}
	}
	return out
}

func (m *Model) keySheetSize() (int, int) {
	r := m.dialogBox(112, 36)
	return max(r.x1-r.x0-6, 10), max(r.y1-r.y0-4, 1)
}

// helpMaxScroll is the sheet's last scroll position.
func (m *Model) helpMaxScroll() int {
	w, h := m.keySheetSize()
	return max(len(keySheetRows(w))-h, 0)
}

func (m *Model) drawKeys(c *canvas) {
	th := m.st.th
	r := m.dialog(c, 112, 36, "Keyboard shortcuts")
	w, h := m.keySheetSize()
	for _, sub := range []string{"↑↓ scroll · /settings · type / for every command", "↑↓ scroll"} {
		if 23+strWidth(sub)+8 <= r.x1-r.x0 { // clear of the esc on the right
			c.put(r.x0+23, r.y0+1, sub, fgc(th.faint), "")
			break
		}
	}
	rows := keySheetRows(w)
	scroll := min(max(m.helpScroll, 0), max(len(rows)-h, 0))
	x0, y0 := r.x0+3, r.y0+3
	old := c.setClip(rect{x0, y0, x0 + w, y0 + h})
	defer c.setClip(old)
	for i := scroll; i < len(rows) && i-scroll < h; i++ {
		y := y0 + i - scroll
		for _, cl := range rows[i] {
			switch cl.kind {
			case 0:
				c.put(x0+cl.x, y, cl.text, fgb(th.accent), "")
			case 1:
				m.keycap(c, x0+cl.x, y, cl.text)
			case 2, 4:
				c.put(x0+cl.x, y, cl.text, fgc(th.dim), "")
			case 3:
				c.put(x0+cl.x, y, cl.text, fgc(th.fg), "")
			}
		}
	}
	if len(rows)-scroll > h {
		c.put(x0+w-8, y0+h-1, " ↓ more ", fgc(th.faint).on(th.panel), "")
	}
}
