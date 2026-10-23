package tui

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

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
		"backspace": {Type: tea.KeyBackspace}, "tab": {Type: tea.KeyTab}, "shift+tab": {Type: tea.KeyShiftTab},
		"left": {Type: tea.KeyLeft}, "right": {Type: tea.KeyRight},
		"f1": {Type: tea.KeyF1}, "f2": {Type: tea.KeyF2}, "end": {Type: tea.KeyEnd},
		"ctrl+end": {Type: tea.KeyCtrlEnd}, "home": {Type: tea.KeyHome}, "ctrl+u": {Type: tea.KeyCtrlU}, "ctrl+a": {Type: tea.KeyCtrlA},
		"alt+a": {Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true},
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
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
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
	// The input box is on screen: a rounded frame with a row inside.
	pl := strings.Split(plain, "\n")
	g := m.geo()
	switch {
	case m.overlayShown() || m.booting() || m.mode == modeViewer:
		// a dialog or the loading screen covers the input
	case g.inY >= 0 && g.boxed:
		top, bot := -1, -1
		for i, l := range pl {
			if strings.Contains(l, "╭") {
				top = i
			}
			if strings.Contains(l, "╰") {
				bot = i
			}
		}
		if top < 0 || bot <= top+1 {
			t.Fatalf("%dx%d after %s: input box not visible\n%s", m.w, m.h, step, plain)
		}
	}
	if m.mode == modeEmoji && m.w >= 30 && m.h >= 12 && !strings.Contains(plain, "⌕") {
		t.Fatalf("%dx%d after %s: emoji picker not visible\n%s", m.w, m.h, step, plain)
	}
	if strings.ContainsAny(plain, "\x1b\a‮") {
		t.Fatalf("%dx%d after %s: control characters leaked", m.w, m.h, step)
	}
	return plain
}

// fitSteps walks most of the UI: typing, F1, the palette, select mode,
// the emoji picker, sidebar sizes, channel moves.
var fitSteps = []string{
	"", "h", "i", "alt+enter", "x", "ctrl+j", "y", "ctrl+j", "z", "ctrl+j", "w", "ctrl+j", "v", "ctrl+j", "u", "ctrl+j", "t",
	"enter", "?", "esc", "ctrl+k", "b", "e", "down", "esc", "pgup", "pgup", "pgup", "pgdown",
	"up", "up", "up", "up", "+", "esc", "esc", "ctrl+b", "ctrl+b", "alt+down", "alt+up", "ctrl+n", "ctrl+p",
	"alt+shift+down", "ctrl+c", "a",
}

func TestViewFitsAllSizes(t *testing.T) {
	steps := fitSteps
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
	m100 := newTestModel(t, 100, 24)
	plain = checkView(t, m100, "start")
	for _, want := range []string{"Channels", "Direct", "# general", "# secret", "@1", "◐ bob", "message #general"} {
		if !strings.Contains(plain, want) {
			t.Errorf("100x24 missing %q\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "# random") {
		t.Error("non-member channel in sidebar")
	}
	if strings.Contains(plain, "syncing") || strings.Contains(plain, "connecting") || strings.Contains(plain, "offline ·") {
		t.Error("connection state shown while online")
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
	if m.mode != modeEmoji || !m.emo.react {
		t.Fatal("+ should open the picker")
	}
	checkView(t, m, "react picker")
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

// Fast typing and IME input arrive as one multi-rune KeyMsg whose String()
// is the text itself: typed words that spell key names must stay text.
func TestTypedWordsThatSpellKeyNames(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	words := []string{"down", " up", " end", " home", " enter", " esc", " tab", " pgup", " ctrl+c", " ?", " e d r"}
	for _, w := range words {
		run(m, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(w)}))
	}
	run(m, m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(" down"), Paste: true}))
	want := strings.Join(words, "") + " down"
	if got := m.input.Value(); got != want {
		t.Fatalf("input %q, want %q", got, want)
	}
	run(m, m.handleKey(keyMsg("enter")))
	if len(f.sent) != 1 || f.sent[0].Body != want {
		t.Fatalf("sent %+v", f.sent)
	}
}

// "?" no longer opens help, so messages can start with it.
func TestQuestionMarkStartsMessage(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	for _, r := range "?? is this on" {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	run(m, m.handleKey(keyMsg("enter")))
	if len(f.sent) != 1 || f.sent[0].Body != "?? is this on" {
		t.Fatalf("sent %+v", f.sent)
	}
	run(m, m.handleKey(keyMsg("f1")))
	if m.mode != modeHelp || !strings.Contains(m.View(), "Keyboard shortcuts") {
		t.Fatal("F1 should open help")
	}
}

func TestNameCommand(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	for _, r := range "/name Alice L." {
		run(m, m.handleKey(keyMsg(string(r))))
	}
	run(m, m.handleKey(keyMsg("enter")))
	if !f.called("name:Alice L.") {
		t.Fatal("SetDisplay not called")
	}
}

// Opening a thread fetches its replies from the server, not just the cache.
func TestOpenThreadFetchesHistory(t *testing.T) {
	m, f := newTestModelFake(t, 120, 30)
	run(m, m.openChannel(10))
	m.enterSelect()
	for i, x := range m.main.msgs {
		if x.ID == 103 {
			m.main.sel = i
		}
	}
	run(m, m.handleKey(keyMsg("r")))
	if !m.thread.open() || !f.called("thread-history:10:103") {
		t.Fatal("thread opened without fetching its history")
	}
}

// The second Ctrl-C quits only while "press Ctrl-C again" is on screen: a
// Ctrl-C long after the notice expired arms again instead of quitting.
func TestCtrlCArmExpires(t *testing.T) {
	m := newTestModel(t, 80, 24)
	now := time.Now()
	m.now = func() time.Time { return now }
	m.handleKey(keyMsg("ctrl+c"))
	now = now.Add(infoTTL + time.Second)
	if cmd := m.handleKey(keyMsg("ctrl+c")); cmd != nil {
		if _, ok := cmd().(tea.QuitMsg); ok {
			t.Fatal("a Ctrl-C after the notice expired quit")
		}
	}
	if !strings.Contains(m.notice, "Ctrl-C again") {
		t.Fatalf("notice = %q, want the Ctrl-C hint again", m.notice)
	}
	now = now.Add(time.Second)
	cmd := m.handleKey(keyMsg("ctrl+c"))
	if cmd == nil {
		t.Fatal("second Ctrl-C within the window didn't quit")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("second Ctrl-C within the window didn't quit")
	}
}
