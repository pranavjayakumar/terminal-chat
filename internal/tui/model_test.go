package tui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func keyMsg(s string) tea.KeyMsg {
	named := map[string]tea.KeyMsg{
		"enter": {Type: tea.KeyEnter}, "esc": {Type: tea.KeyEscape}, "up": {Type: tea.KeyUp},
		"down": {Type: tea.KeyDown}, "pgup": {Type: tea.KeyPgUp}, "pgdown": {Type: tea.KeyPgDown},
		"ctrl+k": {Type: tea.KeyCtrlK}, "ctrl+b": {Type: tea.KeyCtrlB}, "ctrl+c": {Type: tea.KeyCtrlC},
		"ctrl+n": {Type: tea.KeyCtrlN}, "ctrl+p": {Type: tea.KeyCtrlP}, "ctrl+j": {Type: tea.KeyCtrlJ},
		"alt+down": {Type: tea.KeyDown, Alt: true}, "alt+up": {Type: tea.KeyUp, Alt: true},
		"alt+shift+down": {Type: tea.KeyShiftDown, Alt: true}, "alt+enter": {Type: tea.KeyEnter, Alt: true},
		"backspace": {Type: tea.KeyBackspace}, "tab": {Type: tea.KeyTab},
		"f1": {Type: tea.KeyF1}, "f2": {Type: tea.KeyF2}, "end": {Type: tea.KeyEnd},
		"ctrl+end": {Type: tea.KeyCtrlEnd}, "home": {Type: tea.KeyHome}, "ctrl+u": {Type: tea.KeyCtrlU},
	}
	if k, ok := named[s]; ok {
		if k.String() != s {
			panic("key name mismatch: " + k.String() + " != " + s)
		}
		return k
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newTestModelFake(t *testing.T, w, h int) (*Model, *fakeClient) {
	t.Helper()
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Bell: true, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f
}

func newTestModel(t *testing.T, w, h int) *Model {
	m, _ := newTestModelFake(t, w, h)
	return m
}

// checkView asserts the frame fits the terminal and the input is visible.
func checkView(t *testing.T, m *Model, step string) string {
	t.Helper()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != m.h {
		t.Fatalf("%dx%d after %s: %d lines, want %d\n%s", m.w, m.h, step, len(lines), m.h, ansi.Strip(v))
	}
	for i, l := range lines {
		if w := strWidth(l); w > m.w {
			t.Fatalf("%dx%d after %s: line %d is %d wide: %q", m.w, m.h, step, i, w, ansi.Strip(l))
		}
	}
	plain := ansi.Strip(v)
	// The input sits right under the last separator rule, on screen.
	pl := strings.Split(plain, "\n")
	sep := -1
	for i, l := range pl {
		if strings.Contains(l, strings.Repeat("─", min(m.dims().mainW, 10))) {
			sep = i
		}
	}
	if sep < 0 || sep >= len(pl)-1 {
		t.Fatalf("%dx%d after %s: input line not visible\n%s", m.w, m.h, step, plain)
	}
	in := pl[sep+1]
	if i := strings.LastIndex(in, "│"); i >= 0 {
		in = in[i+len("│"):]
	}
	if m.input.Value() == "" && m.mode != modeReact && !strings.HasPrefix(in, "> ") {
		t.Fatalf("%dx%d after %s: empty input has no prompt: %q", m.w, m.h, step, pl[sep+1])
	}
	if m.mode == modeReact && !strings.Contains(pl[sep+1], "react: ") {
		t.Fatalf("%dx%d after %s: react prompt not visible: %q", m.w, m.h, step, pl[sep+1])
	}
	if strings.ContainsAny(plain, "\x1b\a‮") {
		t.Fatalf("%dx%d after %s: control characters leaked", m.w, m.h, step)
	}
	return plain
}

func TestViewFitsAllSizes(t *testing.T) {
	steps := []string{
		"", "h", "i", "alt+enter", "x", "ctrl+j", "y", "ctrl+j", "z", "ctrl+j", "w", "ctrl+j", "v", "ctrl+j", "u", "ctrl+j", "t",
		"enter", "?", "esc", "ctrl+k", "b", "e", "down", "esc", "pgup", "pgup", "pgup", "pgdown",
		"up", "up", "up", "up", "+", "esc", "esc", "ctrl+b", "ctrl+b", "alt+down", "alt+up", "ctrl+n", "ctrl+p",
		"alt+shift+down", "ctrl+c", "a",
	}
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}, {69, 20}, {100, 30}, {20, 6}} {
		m := newTestModel(t, sz[0], sz[1])
		for _, s := range steps {
			step := "start"
			if s != "" {
				step = s
				run(m, m.handleKey(keyMsg(s)))
			}
			checkView(t, m, step)
		}
		// Thread open (select the message with replies, r).
		m.mode = modeNormal
		m.input.Reset()
		run(m, m.openChannel(10))
		m.enterSelect()
		for i, x := range m.main.msgs {
			if x.ID == 103 {
				m.main.sel = i
			}
		}
		run(m, m.handleKey(keyMsg("r")))
		if !m.thread.open() {
			t.Fatalf("%v: r didn't open thread", sz)
		}
		plain := checkView(t, m, "thread")
		if !strings.Contains(plain, "thread") {
			t.Fatalf("%v: thread pane not shown\n%s", sz, plain)
		}
		run(m, m.handleKey(keyMsg("esc")))
		checkView(t, m, "close thread")
		// Notice + typing line.
		m.f().typing[10] = []uint64{2, 3}
		m.Update(updateMsg(client.Update{Typing: map[uint64]bool{10: true}}))
		m.setErr("x", context.DeadlineExceeded)
		if m.sidebarShown() {
			m.status.State = client.Connecting
		}
		checkView(t, m, "typing+notice")
	}
}

func (m *Model) f() *fakeClient { return m.c.(*fakeClient) }

func TestLayout40x12(t *testing.T) {
	m := newTestModel(t, 40, 12)
	plain := checkView(t, m, "start")
	if strings.Contains(plain, "CHANNELS") {
		t.Fatal("sidebar shown at 40 columns")
	}
	if !strings.Contains(plain, "#general") {
		t.Fatalf("header missing:\n%s", plain)
	}
	m80 := newTestModel(t, 80, 24)
	plain = checkView(t, m80, "start")
	for _, want := range []string{"CHANNELS", "DIRECT", "# general", "# secret", "@", "◐ bob", "message #general"} {
		if !strings.Contains(plain, want) {
			t.Errorf("80x24 missing %q\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "# random") {
		t.Error("non-member channel in sidebar")
	}
	if strings.Contains(plain, "online") {
		t.Error("status shown while online")
	}
}

func TestSendAndMarkRead(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	if f.reads[10] != 139 {
		t.Fatalf("opening at bottom should mark read up to 139, got %d", f.reads[10])
	}
	for _, r := range "hi there" {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	run(m, m.handleKey(keyMsg("enter")))
	if len(f.sent) != 1 || f.sent[0].Body != "hi there" || f.sent[0].Chan != 10 {
		t.Fatalf("sent %+v", f.sent)
	}
	if m.input.Value() != "" {
		t.Fatal("input not cleared")
	}
	// Scrolled up: no mark read; a new message shows the pill.
	run(m, m.handleKey(keyMsg("pgup")))
	if m.main.scroll == 0 {
		t.Fatal("pgup didn't scroll")
	}
	f.msgs[10] = append(f.msgs[10][:len(f.msgs[10])-2], client.Message{Message: proto.Message{ID: 150, Chan: 10, Author: 2, Body: "new!", Time: f.msgs[10][len(f.msgs[10])-3].Time + 1000}})
	f.chans[1].Last, f.chans[1].Unread = 150, 1
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}, Channels: true}))
	if f.reads[10] == 150 {
		t.Fatal("marked read while scrolled up")
	}
	if !strings.Contains(checkView(t, m, "new"), "↓ 1 new") {
		t.Fatal("no new-messages pill")
	}
	for i := 0; i < 50 && m.main.scroll > 0; i++ {
		run(m, m.handleKey(keyMsg("pgdown")))
	}
	if f.reads[10] != 150 {
		t.Fatalf("back at bottom should mark read, got %d", f.reads[10])
	}
}

func TestHistoryAtTopOnce(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.histMore = true
	// Hold the request in flight: scroll to top repeatedly.
	m.histBusy[10] = false
	for i := 0; i < 20; i++ {
		m.handleKey(keyMsg("pgup")) // cmds dropped: request stays "in flight"
	}
	if !m.histBusy[10] {
		t.Fatal("no history request at top")
	}
	cmd := m.loadOlder()
	if cmd != nil {
		t.Fatal("second history request while one is in flight")
	}
	m.Update(historyMsg{ch: 10, more: false})
	if m.histBusy[10] || !m.histDone[10] {
		t.Fatal("history state not updated")
	}
	if m.loadOlder() != nil {
		t.Fatal("history requested after server said no more")
	}
}

func TestSelectModeActions(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	run(m, m.handleKey(keyMsg("up")))
	if m.mode != modeSelect {
		t.Fatal("up on empty input should enter select mode")
	}
	sel, _ := m.selected()
	if sel.Failed == "" {
		t.Fatalf("last message should be the failed one: %+v", sel)
	}
	run(m, m.handleKey(keyMsg("r")))
	if !f.called("retry") {
		t.Fatal("r on failed should retry")
	}
	run(m, m.handleKey(keyMsg("d")))
	if !f.called("discard") {
		t.Fatal("d on failed should discard")
	}
	// Select own message 103 and react with +1 (already mine -> unreact).
	for i, x := range m.main.msgs {
		if x.ID == 103 {
			m.main.sel = i
		}
	}
	run(m, m.handleKey(keyMsg("+")))
	if m.mode != modeReact {
		t.Fatal("+ should prompt")
	}
	checkView(t, m, "react prompt")
	run(m, m.handleKey(keyMsg("+")))
	run(m, m.handleKey(keyMsg("1")))
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("unreact 👍") {
		t.Fatalf("calls %v", f.calls)
	}
	run(m, m.handleKey(keyMsg("e")))
	if m.editing != 103 || !strings.HasPrefix(m.input.Value(), "👍 nice") {
		t.Fatalf("edit: %d %q", m.editing, m.input.Value())
	}
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("edit") || m.editing != 0 {
		t.Fatal("edit not sent")
	}
	m.enterSelect()
	for i, x := range m.main.msgs {
		if x.ID == 103 {
			m.main.sel = i
		}
	}
	run(m, m.handleKey(keyMsg("d")))
	if m.mode != modeConfirmDelete {
		t.Fatal("d should confirm")
	}
	run(m, m.handleKey(keyMsg("y")))
	if !f.called("delete") {
		t.Fatal("delete not sent")
	}
	run(m, m.handleKey(keyMsg("y"))) // copy
	if !strings.Contains(m.opt.Out.(*syncBuf).String(), "\x1b]52;c;") {
		t.Fatal("no OSC 52 copy")
	}
	run(m, m.handleKey(keyMsg("esc")))
	if m.mode != modeNormal {
		t.Fatal("esc should leave select mode")
	}
}

func TestBell(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	out := m.opt.Out.(*syncBuf)
	dm := proto.Message{ID: 301, Chan: 20, Author: 2, Body: "hi"}
	run(m, m.applyUpdate(client.Update{New: []proto.Message{dm}}))
	if strings.Count(out.String(), "\a") != 1 {
		t.Fatalf("want one bell for a DM, got %q", out.String())
	}
	m.lastBell = m.lastBell.Add(-2 * bellEvery)
	run(m, m.applyUpdate(client.Update{New: []proto.Message{
		{ID: 302, Chan: 10, Author: 2, Body: "@alice in focused channel"},
		{ID: 303, Chan: 11, Author: 1, Body: "@alice my own"},
		{ID: 304, Chan: 11, Author: 2, Body: "no mention"},
	}}))
	if strings.Count(out.String(), "\a") != 1 {
		t.Fatalf("unexpected bell: %q", out.String())
	}
	run(m, m.applyUpdate(client.Update{New: []proto.Message{{ID: 305, Chan: 11, Author: 2, Body: "hey @alice"}}}))
	if strings.Count(out.String(), "\a") != 2 {
		t.Fatalf("no bell for mention: %q", out.String())
	}
	m.opt.Bell = false
	m.lastBell = m.lastBell.Add(-2 * bellEvery)
	run(m, m.applyUpdate(client.Update{New: []proto.Message{dm}}))
	if strings.Count(out.String(), "\a") != 2 {
		t.Fatal("bell rang with TC_BELL=0")
	}
}

func TestCtrlCTwiceQuits(t *testing.T) {
	m := newTestModel(t, 80, 24)
	if cmd := m.handleKey(keyMsg("ctrl+c")); cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("quit on first ctrl+c")
		}
	}
	cmd := m.handleKey(keyMsg("ctrl+c"))
	if cmd == nil {
		t.Fatal("second ctrl+c didn't quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("second ctrl+c didn't quit")
	}
	// Esc never quits.
	m2 := newTestModel(t, 80, 24)
	for i := 0; i < 5; i++ {
		if cmd := m2.handleKey(keyMsg("esc")); cmd != nil {
			if _, ok := cmd().(tea.QuitMsg); ok {
				t.Fatal("esc quit")
			}
		}
	}
}

func TestChannelCycling(t *testing.T) {
	m := newTestModel(t, 80, 24)
	order := []uint64{}
	for _, v := range m.sidebarOrder() {
		order = append(order, v.ID)
	}
	start := m.active
	run(m, m.handleKey(keyMsg("alt+down")))
	if m.active == start {
		t.Fatal("alt+down didn't move")
	}
	run(m, m.handleKey(keyMsg("alt+up")))
	if m.active != start {
		t.Fatal("alt+up didn't return")
	}
	// Visiting secret (13) marked it read, so the next unread after general is backend.
	run(m, m.handleKey(keyMsg("alt+shift+down")))
	if m.active != 11 {
		t.Fatalf("next unread: got %d (order %v)", m.active, order)
	}
}

func TestResetReload(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.users[1].Name = "robert"
	m.Update(updateMsg(client.Update{Reset: true}))
	if !strings.Contains(checkView(t, m, "reset"), "robert") {
		t.Fatal("reset didn't reload names")
	}
	f.status = client.Status{State: client.Offline, Stopped: true, Err: "revoked"}
	m.Update(updateMsg(client.Update{Status: true}))
	if !strings.Contains(m.notice, "revoked") {
		t.Fatalf("stop reason not shown: %q", m.notice)
	}
}
