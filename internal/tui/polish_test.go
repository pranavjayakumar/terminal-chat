package tui

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
)

// A wider terminal never gets a narrower message column.
func TestColumnNeverShrinksWhenWider(t *testing.T) {
	prev := 0
	for w := 1; w <= 240; w++ {
		cw := columnWidth(w)
		if cw < prev {
			t.Fatalf("region %d: column %d, narrower than %d at %d", w, cw, prev, w-1)
		}
		if cw > w {
			t.Fatalf("region %d: column %d is wider than the region", w, cw)
		}
		prev = cw
	}
}

// Opening and closing the thread pane moves the column one way only.
func TestThreadSlideGlidesTheColumn(t *testing.T) {
	m, _, clk := motionModel(t, 160, 48, "")
	clk.add(5 * time.Second)
	before := m.geo().CX
	run(m, m.openThread(103))
	clk.add(time.Second)
	after := m.geo().CX
	clk.add(-time.Second)
	prev := before
	for i := 0; i <= 8; i++ {
		cx := m.geo().CX
		if cx > prev || cx < after {
			t.Fatalf("opening, frame %d: column at %d (from %d to %d, last %d)", i, cx, before, after, prev)
		}
		prev = cx
		clk.add(25 * time.Millisecond)
	}
	clk.add(time.Second)
	press(m, "esc")
	prev = after
	for i := 0; i <= 8; i++ {
		cx := m.geo().CX
		if cx < prev || cx > before {
			t.Fatalf("closing, frame %d: column at %d (from %d back to %d, last %d)", i, cx, after, before, prev)
		}
		prev = cx
		clk.add(25 * time.Millisecond)
	}
}

// Reaction pills and the reply count wrap in a narrow pane rather than
// being cut off.
func TestFooterWrapsInThreadPane(t *testing.T) {
	m, _ := newTestModelFake(t, 160, 48)
	run(m, m.openThread(103))
	v := checkView(t, m, "thread")
	if !strings.Contains(v, "↳ 2 replies") {
		t.Fatalf("reply count cut off:\n%s", v)
	}
}

// A toast shares the typing row when both fit, and never sits on top of
// message text.
func TestToastBesideTyping(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.typing[10] = []uint64{2}
	m.reloadTyping()
	m.setInfo("copied")
	g := m.geo()
	row := strings.Split(ansi.Strip(m.View()), "\n")[g.noteY]
	if !strings.Contains(row, "typing") || !strings.Contains(row, "copied") {
		t.Fatalf("typing row %q", row)
	}
}

// The panel of a dialog that nearly fills the screen leaves equal room above
// and below.
func TestDialogBalanced(t *testing.T) {
	for _, h := range []int{20, 24, 30, 40} {
		m := newTestModel(t, 80, h)
		r := m.dialogBox(80, h-2)
		if top, bot := r.y0, h-r.y1; top != bot {
			t.Errorf("height %d: %d rows above, %d below", h, top, bot)
		}
	}
}

// The cells beside a floating popup are blank, so text under it doesn't run
// into its frame.
func TestPopupGutter(t *testing.T) {
	m := newTestModel(t, 60, 20)
	typeVia(m, "yay :ta")
	m.View()
	r, _, _ := m.popupRect(m.geo())
	c := m.lastCanvas
	for y := r.y0; y < r.y1; y++ {
		for x := r.x1; x < m.geo().CX+m.geo().CW; x++ {
			if s := c.at(x, y).s; s != " " && s != "" {
				t.Fatalf("row %d: %q right of the popup at %d", y, s, x)
			}
		}
	}
}

// Unread badges stay short however much is unread.
func TestBadgesCapped(t *testing.T) {
	m, f := newTestModelFake(t, 120, 40)
	f.chans[3].Unread = 1500
	f.chans[0].Unread = 250
	m.Update(updateMsg(client.Update{Channels: true}))
	v := checkView(t, m, "badges")
	if !strings.Contains(v, "@99+") || !strings.Contains(v, "99+") || strings.Contains(v, "1500") || strings.Contains(v, "250") {
		t.Fatalf("badges:\n%s", v)
	}
}

// Muting or unmuting doesn't move anyone else's meter in the huddle bar.
func TestHuddleBarStillOnMute(t *testing.T) {
	m, h := newHuddleModel(t, 100, 24)
	h.rosters[m.active] = []uint64{2}
	h.update(m)
	runInput(t, m, "/huddle")
	h.update(m)
	at := func() int {
		for _, l := range strings.Split(ansi.Strip(m.View()), "\n") {
			if i := strings.Index(l, "♪ huddle"); i >= 0 {
				return strWidth(l[:strings.Index(l, "bob")])
			}
		}
		t.Fatal("no huddle bar")
		return 0
	}
	muted := at()
	h.talking = true
	h.update(m)
	if live := at(); live != muted {
		t.Fatalf("bob's meter moved from %d to %d", muted, live)
	}
}

// Settings subtitles that don't fit end in "…".
func TestSettingsSubtitlesEllipsis(t *testing.T) {
	m := newTestModel(t, 80, 24)
	m.openSettings("notifications")
	v := checkView(t, m, "settings")
	if !strings.Contains(v, "when turned…") {
		t.Fatalf("subtitle not truncated with …:\n%s", v)
	}
}

// The keys sheet opened from Settings closes back to Settings.
func TestKeysSheetReturnsToSettings(t *testing.T) {
	m := newTestModel(t, 100, 30)
	m.openSettings("keys")
	press(m, "enter")
	if m.mode != modeHelp {
		t.Fatalf("mode %v, want the keys sheet", m.mode)
	}
	press(m, "esc")
	if m.mode != modeSettings || setPages[m.set.page].id != "keys" {
		t.Fatalf("mode %v page %d after closing the sheet", m.mode, m.set.page)
	}
	press(m, "esc")
	press(m, "f1", "esc")
	if m.mode != modeNormal {
		t.Fatalf("F1 from the chat closed to mode %v", m.mode)
	}
}

// In a narrow pane with two people typing, a toast never lands on the
// failed-send line above.
func TestToastNarrowKeepsMessages(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {60, 20}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		f.typing[10] = []uint64{2, 3}
		m.reloadTyping()
		m.setInfo("copied to the clipboard ok")
		v := ansi.Strip(m.View())
		if strings.Contains(v, "! failed") && !strings.Contains(v, "! failed — r") {
			t.Fatalf("%v: failed line covered:\n%s", sz, v)
		}
		if !strings.Contains(v, "copied") {
			t.Fatalf("%v: toast missing:\n%s", sz, v)
		}
	}
}

// A dialog's rise never covers the status bar.
func TestDialogRiseKeepsStatusBar(t *testing.T) {
	m, _, clk := motionModel(t, 80, 24, "")
	clk.add(5 * time.Second)
	press(m, "f1")
	for i := 0; i < 6; i++ {
		lines := strings.Split(ansi.Strip(m.View()), "\n")
		if !strings.Contains(lines[23], "#general") {
			t.Fatalf("frame %d: status bar covered: %q", i, lines[23])
		}
		clk.add(20 * time.Millisecond)
	}
}

// While the thread pane slides out, the main input stays left of it.
func TestClosingPaneNotOverdrawn(t *testing.T) {
	m, _, clk := motionModel(t, 130, 20, "")
	clk.add(5 * time.Second)
	run(m, m.openThread(103))
	clk.add(time.Second)
	m.View()
	press(m, "esc")
	for i := 0; i < 8; i++ {
		m.View()
		g := m.geo()
		c := m.lastCanvas
		for y := 0; y < g.statusY; y++ {
			for x := g.RX; x < m.w; x++ {
				if s := c.at(x, y).s; strings.ContainsAny(s, "╭╮╰╯⌥⏎") {
					t.Fatalf("frame %d: %q from the input over the pane at %d,%d", i, s, x, y)
				}
			}
		}
		clk.add(20 * time.Millisecond)
	}
}

// Dozens of reactions fold into +N after three rows.
func TestFooterCapped(t *testing.T) {
	m, f := newTestModelFake(t, 60, 40)
	r := map[string][]uint64{}
	for _, e := range []string{"😀", "😃", "😄", "😁", "😆", "😅", "🤣", "😂", "🙂", "🙃", "😉", "😊", "😇", "🥰", "😍", "🤩", "😘", "😗", "😚", "😙", "🥲", "😋", "😛", "😜"} {
		r[e] = []uint64{2}
	}
	f.msgs[10][len(f.msgs[10])-3].Reactions = r
	f.msgs[10][len(f.msgs[10])-3].Replies = 4
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
	v := checkView(t, m, "reactions")
	if !strings.Contains(v, "↳ 4 replies") || !strings.Contains(v, "☺+") || !regexpPlus.MatchString(v) {
		t.Fatalf("footer:\n%s", v)
	}
}

var regexpPlus = regexp.MustCompile(`\+\d+ `)
