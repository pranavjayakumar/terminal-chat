package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

var catchupSizes = [][2]int{{30, 10}, {40, 12}, {80, 24}, {120, 40}, {200, 60}}

// manyUnread is the fake with #general read up to 110: 29 unread messages,
// more than a small screen holds.
func manyUnread() *fakeClient {
	f := newFake()
	f.chans[1].Read, f.chans[1].Unread = 110, 29
	return f
}

func sizedModel(t *testing.T, f *fakeClient, w, h int) *Model {
	t.Helper()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

const newDivider = "─ new ─"

// The rows of the message pane as drawn (the main pane only).
func paneText(m *Model) []string {
	w := m.paneWidth(&m.main)
	rows, _ := m.paneLines(&m.main, w)
	h := m.paneHeight(&m.main)
	end := len(rows) - m.main.scroll
	out := []string{}
	for i := max(end-h, 0); i < end; i++ {
		out = append(out, rows[i].text())
	}
	return out
}

func TestNewDividerAboveFirstUnread(t *testing.T) {
	for _, sz := range catchupSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		plain := checkView(t, m, "open")
		if sz[1] >= 24 && !strings.Contains(plain, newDivider) {
			t.Fatalf("%v: no new divider\n%s", sz, plain)
		}
		rows, owner := m.paneLines(&m.main, m.paneWidth(&m.main))
		r := m.main.memoNewRow
		if r < 0 || !strings.Contains(rows[r].text(), " new ") {
			t.Fatalf("%v: divider row %d", sz, r)
		}
		// The first owned row after it is message 138, the first unread one.
		for i := r + 1; i < len(owner); i++ {
			if owner[i] >= 0 {
				if id := m.main.msgs[owner[i]].ID; id != 138 {
					t.Fatalf("%v: divider above %d, want 138", sz, id)
				}
				break
			}
		}
		if f.reads[10] != 139 && m.main.scroll == 0 {
			t.Fatalf("%v: at the bottom with everything shown but read up to %d", sz, f.reads[10])
		}
	}
}

func TestNoDividerWhenCaughtUp(t *testing.T) {
	f := newFake()
	f.chans[1].Read, f.chans[1].Unread = 139, 0
	m := sizedModel(t, f, 120, 40)
	if plain := checkView(t, m, "open"); strings.Contains(plain, newDivider) {
		t.Fatalf("divider with nothing unread\n%s", plain)
	}
	if m.main.memoNewRow != -1 {
		t.Fatalf("memoNewRow %d", m.main.memoNewRow)
	}
}

// Many unread: the channel opens with the divider as the top row and only
// what has been on screen is read; End reads the rest.
func TestOpensAtFirstUnread(t *testing.T) {
	for _, sz := range catchupSizes {
		f := manyUnread()
		m := sizedModel(t, f, sz[0], sz[1])
		checkView(t, m, "open")
		if m.main.scroll == 0 {
			t.Fatalf("%v: opened at the bottom with 29 unread", sz)
		}
		if top := paneText(m); !strings.Contains(top[0], " new ") {
			t.Fatalf("%v: top row %q, want the divider", sz, top[0])
		}
		if got := f.reads[10]; got >= 139 || (got != 0 && got <= 110) {
			t.Fatalf("%v: read up to %d on open", sz, got)
		}
		press(m, "end")
		checkView(t, m, "end")
		if f.reads[10] != 139 {
			t.Fatalf("%v: End read up to %d", sz, f.reads[10])
		}
		if m.main.scroll != 0 {
			t.Fatalf("%v: End left scroll at %d", sz, m.main.scroll)
		}
	}
}

// Paging down moves the read marker with what comes into view, never past it.
func TestReadFollowsScrolling(t *testing.T) {
	f := manyUnread()
	m := sizedModel(t, f, 80, 24)
	prev := f.reads[10]
	for i := 0; m.main.scroll > 0 && i < 40; i++ {
		press(m, "pgdown")
		checkView(t, m, "pgdown")
		if f.reads[10] < prev {
			t.Fatalf("read marker went back: %d < %d", f.reads[10], prev)
		}
		if m.main.scroll > 0 && f.reads[10] >= 139 {
			t.Fatalf("read everything while scrolled up %d", m.main.scroll)
		}
		prev = f.reads[10]
	}
	if m.main.scroll != 0 || f.reads[10] != 139 {
		t.Fatalf("scroll %d read %d", m.main.scroll, f.reads[10])
	}
	// The divider stays where it was while the channel is open.
	if m.main.memoNewRow < 0 {
		t.Fatal("divider gone after reading")
	}
}

// Nothing is marked read before the first layout shows the first unread.
func TestMarkReadWaitsForLayout(t *testing.T) {
	f := manyUnread()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	if f.reads[10] != 0 {
		t.Fatalf("read up to %d before anything was drawn", f.reads[10])
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.main.scroll == 0 || f.reads[10] >= 139 {
		t.Fatalf("scroll %d read %d", m.main.scroll, f.reads[10])
	}
}

// A history fetch in flight may bring the first unread: wait for it.
func TestMarkReadWaitsForHistory(t *testing.T) {
	f := newFake()
	// Unread is a thread reply (145): nothing cached shows a divider, but
	// the page being fetched might.
	f.chans[1].Read, f.chans[1].Unread, f.chans[1].Last = 139, 1, 145
	f.msgs[10] = f.msgs[10][len(f.msgs[10])-5:] // under 30 cached: open fetches
	m := sizedModel(t, f, 80, 24)
	if !m.main.place || !m.histBusy[10] {
		t.Fatalf("place %v busy %v", m.main.place, m.histBusy[10])
	}
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
	if f.reads[10] != 0 {
		t.Fatalf("read %d while history loads", f.reads[10])
	}
	checkView(t, m, "loading")
	m.Update(historyMsg{ch: 10, gen: m.histGen(10)})
	if m.main.place || f.reads[10] != 145 {
		t.Fatalf("after history: place %v read %d", m.main.place, f.reads[10])
	}
	// A failed fetch settles it too.
	f2 := newFake()
	f2.chans[1].Read, f2.chans[1].Unread, f2.chans[1].Last = 139, 1, 145
	f2.msgs[10] = f2.msgs[10][len(f2.msgs[10])-5:]
	m2 := sizedModel(t, f2, 80, 24)
	m2.Update(historyMsg{ch: 10, gen: m2.histGen(10), err: client.ErrOffline})
	if m2.main.place || f2.reads[10] != 145 {
		t.Fatalf("after a failed fetch: place %v read %d", m2.main.place, f2.reads[10])
	}
}

func TestDividerCompact(t *testing.T) {
	f := manyUnread()
	m := sizedModel(t, f, 80, 24)
	m.ui.Compact = true
	run(m, m.openChannel(13))
	run(m, m.openChannel(10))
	plain := checkView(t, m, "compact")
	if top := paneText(m); !strings.Contains(top[0], " new ") {
		t.Fatalf("compact: top row %q", top[0])
	}
	if !strings.Contains(plain, newDivider) {
		t.Fatalf("no divider compact\n%s", plain)
	}
	rows, _ := m.paneLines(&m.main, m.paneWidth(&m.main))
	r := m.main.memoNewRow
	if r+1 >= len(rows) || rows[r+1].b == nil {
		t.Fatal("compact: a blank row under the divider")
	}
}

func TestReadAll(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	runInput(t, m, "/read-all")
	if f.reads[11] != 300 || f.reads[13] != 400 || f.reads[10] != 139 {
		t.Fatalf("reads %v", f.reads)
	}
	if !strings.Contains(m.notice, "marked") || !strings.Contains(m.notice, "read") {
		t.Fatalf("notice %q", m.notice)
	}
	for _, v := range m.chans {
		if v.Unread != 0 || v.Mention {
			t.Fatalf("%s still unread: %+v", v.Name, v)
		}
	}
	checkView(t, m, "read-all")
	plain := ansi.Strip(m.View())
	if row := sidebarRow(t, plain, "secret"); strings.Contains(row, "@1") {
		t.Fatalf("secret still has its badge: %q", row)
	}
	runInput(t, m, "/read-all")
	if m.notice != "nothing unread" {
		t.Fatalf("second /read-all: %q", m.notice)
	}
}

func TestReadAllWhileSyncing(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.status.State = client.Syncing
	runInput(t, m, "/read-all")
	if f.reads[11] != 0 || !m.noticeErr || !strings.Contains(m.notice, "syncing") {
		t.Fatalf("reads %v notice %q", f.reads, m.notice)
	}
	if m.input.Value() != "/read-all" {
		t.Fatalf("input %q, want it kept", m.input.Value())
	}
}

func ctrlCaret() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlCaret} }

func TestBackToPreviousChannel(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	m.Update(ctrlCaret())
	if m.active != 10 || m.notice != "no previous channel yet" {
		t.Fatalf("active %d notice %q", m.active, m.notice)
	}
	run(m, m.openChannel(13))
	_, cmd := m.Update(ctrlCaret())
	run(m, cmd)
	if m.active != 10 {
		t.Fatalf("back went to %d", m.active)
	}
	_, cmd = m.Update(ctrlCaret())
	run(m, cmd)
	if m.active != 13 {
		t.Fatalf("back again went to %d", m.active)
	}
	checkView(t, m, "back")
	// From select mode too: the key leaves it and goes back.
	press(m, "up")
	_, cmd = m.Update(ctrlCaret())
	run(m, cmd)
	if m.active != 10 || m.mode != modeNormal {
		t.Fatalf("from select: active %d mode %v", m.active, m.mode)
	}
}

func TestPaletteRecentFirst(t *testing.T) {
	m, _ := newTestModelFake(t, 100, 30)
	run(m, m.openChannel(13))
	run(m, m.openChannel(20))
	run(m, m.openChannel(10))
	press(m, "alt+k")
	p := m.pal.shown
	if len(p) < 3 || p[0].group != "Recent" || p[0].sw.ch != 20 || p[1].sw.ch != 13 {
		t.Fatalf("first rows %+v %+v", p[0], p[1])
	}
	for _, it := range p {
		if it.sw != nil && it.sw.ch == 10 && it.group == "Recent" {
			t.Fatal("the open channel listed as recent")
		}
	}
	plain := checkView(t, m, "palette")
	if !strings.Contains(plain, "Recent") {
		t.Fatalf("no Recent title\n%s", plain)
	}
	// A query ranks by match as before.
	typeVia(m, "back")
	if m.pal.shown[0].sw == nil || m.pal.shown[0].sw.ch != 11 {
		t.Fatalf("query: first %+v", m.pal.shown[0])
	}
}

func paletteRow(t *testing.T, m *Model, label string) palItem {
	t.Helper()
	for _, it := range m.pal.items {
		if it.label == label {
			return it
		}
	}
	t.Fatalf("no palette row %q", label)
	return palItem{}
}

func TestPaletteMuteAndBack(t *testing.T) {
	m, _ := newTestModelFake(t, 100, 30)
	run(m, m.openChannel(13))
	press(m, "alt+k")
	paletteRow(t, m, "Previous channel")
	run(m, m.palRun(paletteRow(t, m, "Mute #secret")))
	if !m.muted(13) {
		t.Fatal("Mute didn't mute")
	}
	press(m, "alt+k")
	run(m, m.palRun(paletteRow(t, m, "Unmute #secret")))
	if m.muted(13) {
		t.Fatal("Unmute didn't unmute")
	}
	press(m, "alt+k")
	run(m, m.palRun(paletteRow(t, m, "Previous channel")))
	if m.active != 10 {
		t.Fatalf("Previous channel opened %d", m.active)
	}
	press(m, "alt+k")
	run(m, m.palRun(paletteRow(t, m, "Mark all read")))
	if !strings.Contains(m.notice, "read") {
		t.Fatalf("Mark all read: %q", m.notice)
	}
}

func TestSelectMineAndCopyCode(t *testing.T) {
	f := newFake()
	var copied []string
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true),
		ExtCopy: func(_ context.Context, s string) error { copied = append(copied, s); return nil }})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	press(m, "up", "home", "E")
	if x, _ := m.selected(); x.Local != 8 {
		t.Fatalf("E selected %+v", x.Message)
	}
	// Not counting the pending/failed ones: drop them and E finds 103.
	f.msgs[10] = f.msgs[10][:len(f.msgs[10])-2]
	m.reloadPane(&m.main)
	press(m, "home", "E")
	if x, _ := m.selected(); x.ID != 103 {
		t.Fatalf("E selected %d", x.ID)
	}
	checkView(t, m, "E")
	selectID(t, m, 102)
	press(m, "Y")
	if len(copied) != 1 || copied[0] != "panic: nil map write at handler.go:42 with a very long trailing line that must be truncated\n  at main.go:7" {
		t.Fatalf("Y copied %q", copied)
	}
	selectID(t, m, 104)
	press(m, "Y")
	if len(copied) != 1 || m.notice != "no code block in this message" {
		t.Fatalf("Y on prose: %q %q", copied, m.notice)
	}
}

func TestSelectMineNone(t *testing.T) {
	f := newFake()
	f.msgs[10] = f.msgs[10][:3] // bob's and carol's only
	m := sizedModel(t, f, 80, 24)
	press(m, "up", "E")
	if m.notice != "no message of yours loaded here" || m.mode != modeSelect {
		t.Fatalf("notice %q mode %v", m.notice, m.mode)
	}
}

func TestCodeOnly(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"no code", ""},
		{"a\n```go\nx := 1\n```\nb", "x := 1"},
		{"```\none\n```\nmid\n```sh\ntwo\nthree\n```", "one\n\ntwo\nthree"},
		{"```\nunclosed\nrest", "unclosed\nrest"},
		{"```\n\x1b[31mred\n```", "[31mred"},
	} {
		if got := codeOnly(c.in); got != c.want {
			t.Errorf("codeOnly(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAltHJoinsHuddleInModern(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	if m.menuBarShown() {
		t.Skip("modern skin expected")
	}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}, Alt: true})
	run(m, cmd)
	if m.input.Value() != "" {
		t.Fatalf("alt+h typed %q", m.input.Value())
	}
	if m.notice == "" {
		t.Fatal("alt+h did nothing")
	}
	checkView(t, m, "alt+h")
}

func TestSidebarShowsAwayText(t *testing.T) {
	for _, sz := range [][2]int{{100, 24}, {200, 40}} {
		m, _ := newTestModelFake(t, sz[0], sz[1])
		plain := checkView(t, m, "away")
		if row := sidebarRow(t, plain, "bob"); !strings.Contains(row, "lunch") {
			t.Fatalf("%v: bob's row %q", sz, row)
		}
	}
	// Hostile away text is cut to one clean line.
	f := newFake()
	f.users[1].Name = "bob"
	m := sizedModel(t, f, 100, 24)
	m.presence[2] = client.Presence{State: proto.PresAway, Status: "\x1b[31mgone\nfishing ‮x"}
	m.reloadChans()
	plain := checkView(t, m, "hostile away")
	if row := sidebarRow(t, plain, "bob"); strings.ContainsAny(row, "\x1b‮") {
		t.Fatalf("row %q", row)
	}
}

// A jump (search, activity) into a channel with unread messages selects its
// target and marks nothing it skipped: the unread ones above stay unread.
func TestJumpDoesNotReadWhatItSkips(t *testing.T) {
	f := manyUnread()
	f.chans[1].Read, f.chans[1].Unread = 0, 0
	m := sizedModel(t, f, 80, 24)
	run(m, m.openChannel(13))
	f.reads = map[uint64]uint64{}
	f.chans[1].Read, f.chans[1].Unread = 110, 29
	m.reloadChans()
	run(m, m.jumpTo(proto.Message{ID: 130, Chan: 10}))
	if x, _ := m.selected(); x.ID != 130 {
		t.Fatalf("jump selected %d", x.ID)
	}
	if f.reads[10] != 0 {
		t.Fatalf("jump read up to %d", f.reads[10])
	}
	// Updates while the target is on screen don't read past the gap either.
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}, Channels: true}))
	if f.reads[10] > 110 && m.main.scroll > 0 {
		t.Fatalf("read up to %d over unseen messages", f.reads[10])
	}
	checkView(t, m, "jump")
	// Going to the newest reads it all, as before.
	press(m, "esc", "end")
	if f.reads[10] != 139 {
		t.Fatalf("End read %d", f.reads[10])
	}
}

// tc opens on its cache; the sync after connecting brings the night's
// messages. They get a divider instead of being read at once.
func TestDividerForWhatSyncBrought(t *testing.T) {
	f := newFake()
	f.chans[1].Read, f.chans[1].Unread = 139, 0
	f.status = client.Status{State: client.Syncing}
	m := sizedModel(t, f, 80, 24)
	if m.main.hasNew {
		t.Fatal("divider with nothing unread")
	}
	// The sync lands 20 messages that the cache now holds as unread.
	f.chans[1].Read, f.chans[1].Unread = 110, 29
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}, Channels: true}))
	if f.reads[10] != 0 {
		t.Fatalf("read %d while syncing", f.reads[10])
	}
	f.status = client.Status{State: client.Online, Transport: "quic"}
	m.Update(updateMsg(client.Update{Status: true}))
	if !m.main.hasNew || m.main.scroll == 0 {
		t.Fatalf("hasNew %v scroll %d", m.main.hasNew, m.main.scroll)
	}
	if top := paneText(m); !strings.Contains(top[0], " new ") {
		t.Fatalf("top row %q", top[0])
	}
	if f.reads[10] >= 139 {
		t.Fatalf("read %d at once", f.reads[10])
	}
	checkView(t, m, "synced")
}

func TestReadAllDropsTheDivider(t *testing.T) {
	m, _ := sizedModel(t, manyUnread(), 80, 24), 0
	if !m.main.hasNew {
		t.Fatal("no divider to drop")
	}
	runInput(t, m, "/read-all")
	if m.main.hasNew || m.main.memoNewRow >= 0 && strings.Contains(strings.Join(paneText(m), "\n"), " new ") {
		t.Fatal("divider stays after /read-all")
	}
	checkView(t, m, "read-all")
}

// Sending while the view waits to settle on the divider keeps it at the
// bottom, by what was just sent.
func TestSendEndsPlacement(t *testing.T) {
	f := newFake()
	f.chans[1].Read, f.chans[1].Unread, f.chans[1].Last = 139, 1, 145
	f.msgs[10] = f.msgs[10][len(f.msgs[10])-5:]
	m := sizedModel(t, f, 80, 24)
	if !m.main.place {
		t.Fatal("expected a pending placement")
	}
	runInput(t, m, "hi")
	if m.main.place || m.main.scroll != 0 {
		t.Fatalf("place %v scroll %d after send", m.main.place, m.main.scroll)
	}
}

func TestDividerMoreAbove(t *testing.T) {
	f := newFake()
	f.chans[1].Read, f.chans[1].Unread = 0, 40
	m := sizedModel(t, f, 80, 24)
	plain := checkView(t, m, "more above")
	if !strings.Contains(plain, "new · more above") {
		t.Fatalf("no 'more above'\n%s", plain)
	}
	m.histDone[10] = true
	m.main.ver++
	if plain := checkView(t, m, "all loaded"); strings.Contains(plain, "more above") {
		t.Fatal("'more above' with the whole history loaded")
	}
}

func TestAltHSwitchesFromAnotherHuddle(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	m.hud.in, m.hud.cur = true, 13 // in #secret's huddle, looking at #general
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'h'}, Alt: true})
	run(m, cmd)
	if strings.Contains(m.notice, "left") && !strings.Contains(m.notice, "general") {
		t.Fatalf("alt+h left the other huddle: %q", m.notice)
	}
	checkView(t, m, "alt+h")
}
