package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// Tests for the tui-reviewer fixes.

func typeText(m *Model, s string) {
	for _, r := range s {
		run(m, m.handleKey(keyMsg(string(r))))
	}
}

// 1. A visible but unfocused pane still marks read; the bell still rings
// for the active channel when blurred.
func TestMarkReadWhileBlurred(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	nt := m.opt.Notifier.(*fakeNotifier)
	m.Update(tea.BlurMsg{})
	x := client.Message{Message: proto.Message{ID: 160, Chan: 10, Author: 2, Body: "while away", Time: f.msgs[10][len(f.msgs[10])-3].Time + 1000}}
	f.msgs[10] = append(f.msgs[10], x)
	f.chans[1].Last, f.chans[1].Unread = 160, 1
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}, Channels: true}))
	if f.reads[10] != 160 {
		t.Fatalf("blurred pane at bottom should mark read, got %d", f.reads[10])
	}
	run(m, m.applyUpdate(client.Update{New: []proto.Message{{ID: 161, Chan: 10, Author: 2, Body: "@alice ping"}}}))
	if len(nt.got()) != 1 {
		t.Fatal("mention in the active channel should notify while the terminal is blurred")
	}
}

// 2. Unread summary in the header when the sidebar is hidden.
func TestHeaderUnreadSummary(t *testing.T) {
	m := newTestModel(t, 40, 12)
	first := strings.Split(checkView(t, m, "start"), "\n")[0]
	if !strings.HasSuffix(strings.TrimRight(first, " "), "· 3 unread · 1@") || !strings.HasPrefix(first, "#general") {
		t.Fatalf("header: %q", first)
	}
	// Narrower than the summary allows: the name wins.
	m.Update(tea.WindowSizeMsg{Width: 20, Height: 8})
	first = strings.Split(checkView(t, m, "narrow"), "\n")[0]
	if !strings.HasPrefix(first, "#general") {
		t.Fatalf("narrow header lost the channel: %q", first)
	}
	// Sidebar shown: no summary.
	m80 := newTestModel(t, 80, 24)
	if strings.Contains(strings.Split(checkView(t, m80, "start"), "\n")[0], "unread") {
		t.Fatal("summary shown with the sidebar visible")
	}
}

// 3. Help overlay: aliases, order, scrolling, "↓ more".
func TestHelpOverlay(t *testing.T) {
	m := newTestModel(t, 40, 12)
	run(m, m.handleKey(keyMsg("f1")))
	if m.mode != modeHelp {
		t.Fatal("F1 should open help")
	}
	v := checkView(t, m, "help")
	if !strings.Contains(v, "↓ more") {
		t.Fatalf("truncated help has no more hint:\n%s", v)
	}
	if lines := strings.Split(v, "\n"); !strings.HasPrefix(lines[2], "Esc") {
		t.Fatalf("Esc should come first: %q", lines[2])
	}
	run(m, m.handleKey(keyMsg("pgdown")))
	if m.helpScroll == 0 {
		t.Fatal("pgdown didn't scroll help")
	}
	for i := 0; i < 20; i++ {
		run(m, m.handleKey(keyMsg("down")))
	}
	if v := checkView(t, m, "help end"); strings.Contains(v, "↓ more") || !strings.Contains(v, "//text") {
		t.Fatalf("help end:\n%s", v)
	}
	run(m, m.handleKey(keyMsg("esc")))
	if m.mode != modeNormal {
		t.Fatal("esc should close help")
	}
	m.input.SetValue("/help")
	m.submit()
	if m.mode != modeHelp || m.helpScroll != 0 {
		t.Fatal("/help should open help at the top")
	}
}

// 4. Newlines past the visible cap are kept; the visible height stays capped.
func TestInputManyLines(t *testing.T) {
	m := newTestModel(t, 80, 24)
	typeText(m, "a")
	for i := 0; i < 12; i++ {
		run(m, m.handleKey(keyMsg("alt+enter")))
		typeText(m, "b")
	}
	if n := strings.Count(m.input.Value(), "\n"); n != 12 {
		t.Fatalf("want 12 newlines, got %d", n)
	}
	if d := m.dims(); d.inputH != min(maxInputH, m.h/3) {
		t.Fatalf("visible input height %d", d.inputH)
	}
	checkView(t, m, "many lines")
}

// 5. Esc twice clears the draft; Ctrl-U clears it at once.
func TestEscClearsDraft(t *testing.T) {
	m := newTestModel(t, 80, 24)
	now := time.Now()
	m.now = func() time.Time { return now }
	typeText(m, "draft")
	run(m, m.handleKey(keyMsg("esc")))
	if m.input.Value() != "draft" || !strings.Contains(m.notice, "Esc again") {
		t.Fatalf("first esc: %q notice %q", m.input.Value(), m.notice)
	}
	now = now.Add(3 * time.Second) // too slow: starts over
	run(m, m.handleKey(keyMsg("esc")))
	if m.input.Value() != "draft" {
		t.Fatal("esc after the window cleared the draft")
	}
	now = now.Add(time.Second)
	run(m, m.handleKey(keyMsg("esc")))
	if m.input.Value() != "" {
		t.Fatal("second esc didn't clear the draft")
	}
	typeText(m, "one")
	run(m, m.handleKey(keyMsg("ctrl+j")))
	typeText(m, "two")
	run(m, m.handleKey(keyMsg("ctrl+u")))
	if m.input.Value() != "" {
		t.Fatalf("ctrl+u left %q", m.input.Value())
	}
}

// 6. Notices expire by generation; errors clear on success.
func TestNoticeExpiry(t *testing.T) {
	m := newTestModel(t, 80, 24)
	_, cmd := m.Update(infoMsg("hello"))
	if cmd == nil {
		t.Fatal("info notice scheduled no expiry")
	}
	gen := m.noticeGen
	m.Update(infoMsg("newer"))
	m.Update(noticeExpireMsg{gen})
	if m.notice != "newer" {
		t.Fatal("stale expiry cleared a newer notice")
	}
	m.Update(noticeExpireMsg{m.noticeGen})
	if m.notice != "" {
		t.Fatal("expiry didn't clear")
	}
	m.Update(errMsg{"x", context0Err{}})
	if !m.noticeErr {
		t.Fatal("no error notice")
	}
	m.Update(okMsg{})
	if m.notice != "" {
		t.Fatal("success didn't clear the error")
	}
	// Sticky hints don't expire.
	m.setHint("editing")
	if _, cmd := m.Update(noopMsg{}); cmd != nil {
		t.Fatal("sticky hint scheduled expiry")
	}
	// A successful send clears an error.
	m.Update(errMsg{"x", context0Err{}})
	typeText(m, "ok")
	run(m, m.handleKey(keyMsg("enter")))
	if m.noticeErr {
		t.Fatal("send didn't clear the error")
	}
}

type context0Err struct{}

func (context0Err) Error() string { return "boom" }

// 7. Select mode hint and navigation.
func TestSelectModeNav(t *testing.T) {
	m := newTestModel(t, 80, 24)
	run(m, m.handleKey(keyMsg("up")))
	if v := checkView(t, m, "select"); !strings.Contains(v, "e edit · d delete · r thread · + react · y copy · Esc") {
		t.Fatalf("no select hint:\n%s", v)
	}
	last := m.main.sel
	run(m, m.handleKey(keyMsg("pgup")))
	if m.main.sel >= last-3 {
		t.Fatalf("pgup moved selection only %d -> %d", last, m.main.sel)
	}
	run(m, m.handleKey(keyMsg("home")))
	if m.main.sel != 0 {
		t.Fatalf("home: %d", m.main.sel)
	}
	checkView(t, m, "home")
	run(m, m.handleKey(keyMsg("G")))
	if m.main.sel != len(m.main.msgs)-1 {
		t.Fatalf("G: %d", m.main.sel)
	}
	run(m, m.handleKey(keyMsg("g")))
	run(m, m.handleKey(keyMsg("pgdown")))
	if m.main.sel == 0 {
		t.Fatal("pgdown didn't move")
	}
	run(m, m.handleKey(keyMsg("esc")))
	if v := checkView(t, m, "left select"); strings.Contains(v, "e edit") {
		t.Fatal("select hint left behind")
	}
}

// 8. The status row is reserved: the log keeps its height.
func TestStatusRowReserved(t *testing.T) {
	m := newTestModel(t, 80, 24)
	h := m.dims().bodyH
	m.f().typing[10] = []uint64{2}
	m.Update(updateMsg(client.Update{Typing: map[uint64]bool{10: true}}))
	m.setInfo("note")
	if m.dims().bodyH != h {
		t.Fatalf("body height changed %d -> %d", h, m.dims().bodyH)
	}
	v := checkView(t, m, "status")
	if !strings.Contains(v, "note · bob is typing…") {
		t.Fatalf("status row:\n%s", v)
	}
}

// 9. Sidebar toggles: too narrow notice, F2 alias.
func TestSidebarToggle(t *testing.T) {
	m := newTestModel(t, 30, 10)
	run(m, m.handleKey(keyMsg("ctrl+b")))
	if m.sidebarShown() || !strings.Contains(m.notice, "too narrow") {
		t.Fatalf("narrow toggle: shown=%v notice %q", m.sidebarShown(), m.notice)
	}
	checkView(t, m, "narrow toggle")
	m2 := newTestModel(t, 80, 24)
	run(m2, m2.handleKey(keyMsg("f2")))
	if m2.sidebarShown() {
		t.Fatal("F2 didn't hide the sidebar")
	}
	run(m2, m2.handleKey(keyMsg("f2")))
	if !m2.sidebarShown() {
		t.Fatal("F2 didn't show the sidebar")
	}
}

// 10. Replies or reactions end a group.
func TestGroupBreaksAfterFooter(t *testing.T) {
	a := msg(1, 2, "a")
	b := msg(2, 2, "b")
	b.Time = a.Time + 60_000
	a.Replies = 1
	if grouped(a, b) {
		t.Fatal("grouped after a message with replies")
	}
	a.Replies = 0
	a.Reactions = map[string][]uint64{"👍": {3}}
	if grouped(a, b) {
		t.Fatal("grouped after a message with reactions")
	}
	a.Reactions = map[string][]uint64{"👍": {}}
	if !grouped(a, b) {
		t.Fatal("empty reaction lists should not break the group")
	}
}

// 11. End / Ctrl-End jump to the newest message.
func TestEndJumpsToBottom(t *testing.T) {
	m := newTestModel(t, 80, 24)
	run(m, m.handleKey(keyMsg("pgup")))
	run(m, m.handleKey(keyMsg("pgup")))
	if m.main.scroll == 0 {
		t.Fatal("didn't scroll")
	}
	run(m, m.handleKey(keyMsg("end")))
	if m.main.scroll != 0 {
		t.Fatal("end didn't jump to bottom")
	}
	run(m, m.handleKey(keyMsg("pgup")))
	typeText(m, "draft")
	run(m, m.handleKey(keyMsg("ctrl+end")))
	if m.main.scroll != 0 || m.input.Value() != "draft" {
		t.Fatalf("ctrl+end: scroll %d input %q", m.main.scroll, m.input.Value())
	}
}

// 12. Search jump: loaded hits are selected and visible; others get a hint.
func TestSearchJump(t *testing.T) {
	m := newTestModel(t, 80, 24)
	target := m.main.msgs[0]
	run(m, m.jumpTo(target.Message))
	if m.mode != modeSelect || m.main.msgs[m.main.sel].ID != target.ID {
		t.Fatal("loaded hit not selected")
	}
	lines, owner := m.paneLines(&m.main, m.dims().msgW)
	h := m.dims().bodyH
	start := len(lines) - h - m.main.scroll
	visible := false
	for i := max(start, 0); i < len(lines)-m.main.scroll; i++ {
		visible = visible || owner[i] == m.main.sel
	}
	if !visible {
		t.Fatal("selected hit not on screen")
	}
	if !strings.Contains(ansi.Strip(m.View()), "morning!") {
		t.Fatal("hit text not on screen")
	}
	m.leaveSelect()
	run(m, m.jumpTo(proto.Message{ID: 5, Chan: 10, Author: 2, Body: "ancient"}))
	if !strings.Contains(m.notice, "older message — scroll up to find it") {
		t.Fatalf("notice %q", m.notice)
	}
}
