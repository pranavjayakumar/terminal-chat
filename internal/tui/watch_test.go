package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/screen"
)

// watchFake adds WatchBackend to the huddle fake.
type watchFake struct {
	*huddleFake
	wmu      sync.Mutex
	live     map[uint64]uint64 // chan → who shares
	sharing  *client.ShareInfo
	capture  client.Capture
	stops    int
	watching uint64
	frame    client.WatchFrame
	shareErr error
	stopped  error
}

func (w *watchFake) StartShare(ctx context.Context, label string, capture client.Capture) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.shareErr != nil {
		return w.shareErr
	}
	w.sharing = &client.ShareInfo{Label: label, Watchers: 2, Scrubbed: 1}
	w.capture = capture
	return nil
}

func (w *watchFake) StopShare() {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.sharing = nil
	w.stops++
}

func (w *watchFake) Sharing() (client.ShareInfo, bool) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.sharing == nil {
		return client.ShareInfo{}, false
	}
	return *w.sharing, true
}

func (w *watchFake) ShareStopped() error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	err := w.stopped
	w.stopped = nil
	return err
}

func (w *watchFake) LiveIn(ch uint64) (uint64, bool) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	u, ok := w.live[ch]
	return u, ok
}

func (w *watchFake) Watch(_ context.Context, user uint64) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.watching = user
	w.frame.User = user
	return nil
}

func (w *watchFake) StopWatch() {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	w.watching = 0
}

func (w *watchFake) Watching(ver uint64) (client.WatchFrame, bool) {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.watching == 0 {
		return client.WatchFrame{}, false
	}
	f := w.frame
	if f.Grid != nil {
		f.Grid = f.Grid.Clone()
	}
	return f, true
}

func newWatchModel(t *testing.T, w, h int) (*Model, *watchFake, *fakeHerdr) {
	t.Helper()
	hf := &huddleFake{fakeClient: newFake(), rosters: map[uint64][]uint64{}}
	wf := &watchFake{huddleFake: hf, live: map[uint64]uint64{}}
	fh := &fakeHerdr{text: map[string]string{}, panes: []notify.Pane{
		{ID: "w1:p1", Tab: "w1:t1", Label: "tc"},
		{ID: "w1:p2", Tab: "w1:t1", Label: "build"},
		{ID: "w1:p3", Tab: "w1:t2", Label: "logs"},
	}}
	m := New(context.Background(), wf, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, Herdr: fh, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, wf, fh
}

func (w *watchFake) update(m *Model) {
	_, cmd := m.Update(updateMsg(client.Update{Huddle: true, Watch: true}))
	run(m, cmd)
}

func TestPairNeedsHuddleAndHerdr(t *testing.T) {
	m, w, _ := newWatchModel(t, 100, 24)
	runInput(t, m, "/pair")
	if !m.noticeErr || !strings.Contains(m.notice, "/huddle first") {
		t.Fatalf("outside a huddle: %q", m.notice)
	}
	runInput(t, m, "/huddle")
	m.opt.Herdr = nil
	runInput(t, m, "/pair")
	if !m.noticeErr || !strings.Contains(m.notice, "needs herdr") || !strings.Contains(m.notice, "HERDR_PANE_ID") {
		t.Fatalf("without herdr: %q", m.notice)
	}
	if w.sharing != nil {
		t.Fatal("started sharing")
	}
}

func TestPairLiveBannerAndEsc(t *testing.T) {
	m, w, fh := newWatchModel(t, 100, 24)
	runInput(t, m, "/huddle")
	fh.text["w1:p2"] = "should not be read for /pair"
	runInput(t, m, "/pair")
	if w.sharing == nil || w.sharing.Label != "build" {
		t.Fatalf("not streaming the only other pane in our tab: %+v (notice %q)", w.sharing, m.notice)
	}
	w.update(m)
	if !strings.Contains(m.notice, "you're live: build") {
		t.Fatalf("notice %q", m.notice)
	}
	// The banner is on every screen: chat, dialogs, the keys sheet, tiny panes.
	for _, size := range [][2]int{{100, 24}, {40, 10}, {20, 5}, {200, 60}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := ansi.Strip(checkView(t, m, "streaming"))
		if !strings.Contains(v, "LIVE") {
			t.Fatalf("%dx%d: no LIVE banner\n%s", size[0], size[1], v)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	runInput(t, m, "/help")
	if v := ansi.Strip(m.View()); !strings.Contains(v, "LIVE build · 2 watching") {
		t.Fatalf("banner hidden by a dialog:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.mode = modeNormal
	// The capture reads the pane's visible screen (fake: ReadPane fallback).
	if txt, err := w.capture(context.Background()); err != nil || !strings.Contains(txt, "should not be read") {
		t.Fatalf("capture: %q %v", txt, err)
	}
	// Esc on the chat stops the stream at once, before the mic or anything else.
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if w.stops != 1 || w.sharing != nil {
		t.Fatalf("Esc didn't stop the stream (%d stops)", w.stops)
	}
	w.update(m)
	if v := ansi.Strip(checkView(t, m, "stopped")); strings.Contains(v, "LIVE") {
		t.Fatalf("banner after stop:\n%s", v)
	}
	runInput(t, m, "/unpair")
	if !m.noticeErr || !strings.Contains(m.notice, "not streaming") {
		t.Fatalf("/unpair when idle: %q", m.notice)
	}
}

func TestPairPickerAndStopReason(t *testing.T) {
	m, w, fh := newWatchModel(t, 100, 24)
	fh.panes = append(fh.panes, notify.Pane{ID: "w1:p4", Tab: "w1:t1", Label: "editor"})
	runInput(t, m, "/huddle")
	runInput(t, m, "/pair")
	if m.mode != modePanePick || !m.share.pair {
		t.Fatalf("two panes in our tab: no picker (mode %v)", m.mode)
	}
	if v := ansi.Strip(checkView(t, m, "picker")); !strings.Contains(v, "stream a pane live") {
		t.Fatalf("picker title:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	run(m, cmd)
	if w.sharing == nil || w.sharing.Label != "editor" {
		t.Fatalf("picked %+v", w.sharing)
	}
	runInput(t, m, "/pair nope")
	if !m.noticeErr || !strings.Contains(m.notice, "no pane") {
		t.Fatalf("bad pane: %q", m.notice)
	}
	// The stream stops by itself (the pane closed): say why.
	w.update(m)
	w.sharing, w.stopped = nil, errors.New("pane w1:p4 is gone")
	w.update(m)
	if !m.noticeErr || !strings.Contains(m.notice, "stopped streaming editor") || !strings.Contains(m.notice, "gone") {
		t.Fatalf("stop reason: %q", m.notice)
	}
}

func TestWatchViewer(t *testing.T) {
	m, w, _ := newWatchModel(t, 100, 24)
	runInput(t, m, "/huddle")
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}, Alt: true})
	if !m.noticeErr || !strings.Contains(m.notice, "nobody is sharing") {
		t.Fatalf("⌥W with nobody live: %q", m.notice)
	}
	m.clearNotice()
	w.live[m.active] = 2
	w.update(m)
	if v := ansi.Strip(checkView(t, m, "bob live")); !strings.Contains(v, "watch bob") {
		t.Fatalf("huddle bar has no watch control:\n%s", v)
	}
	g := screen.Parse("\x1b[32m$ go test ./...\x1b[0m\r\nok  tc/internal/tui  28.4s\r\n世界 wide "+strings.Repeat("x", 150)+"\r\n", 300, 100)
	w.frame = client.WatchFrame{Label: "build pane", Grid: g, Ver: 1}
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'w'}, Alt: true})
	run(m, cmd)
	if m.mode != modeWatch || w.watching != 2 {
		t.Fatalf("⌥W didn't open the viewer: mode %v watching %d (%q)", m.mode, w.watching, m.notice)
	}
	w.update(m)
	for _, size := range [][2]int{{100, 24}, {30, 6}, {12, 3}, {220, 70}} {
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		v := ansi.Strip(checkView(t, m, "watching"))
		if size[1] >= 6 && (!strings.Contains(v, "28.4s") || !strings.Contains(v, "@bob")) {
			t.Fatalf("%dx%d: pane not shown\n%s", size[0], size[1], v)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	// Panning right shows the rest of the long line.
	for range 10 {
		m.Update(tea.KeyMsg{Type: tea.KeyRight})
	}
	if m.wt.x == 0 {
		t.Fatal("no pan")
	}
	checkView(t, m, "panned")
	// y copies the pane's text.
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if cmd == nil {
		t.Fatal("y copied nothing")
	}
	// The stream ends: the last picture stays, marked.
	w.frame.Ended = true
	w.frame.Ver++
	w.update(m)
	if v := ansi.Strip(checkView(t, m, "ended")); !strings.Contains(v, "ended") {
		t.Fatalf("not marked ended:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeNormal || w.watching != 0 || m.wt.grid != nil {
		t.Fatalf("Esc didn't leave: mode %v watching %d", m.mode, w.watching)
	}
	checkView(t, m, "left")
}

func TestWatchTargetJoins(t *testing.T) {
	m, w, _ := newWatchModel(t, 100, 24)
	m.SetWatchTarget("@bob")
	w.update(m)
	if m.mode == modeWatch {
		t.Fatal("watching before bob is live")
	}
	ch := m.active
	w.rosters[ch] = []uint64{2}
	w.live[ch] = 2
	w.update(m)
	if in, ok := w.InHuddle(); !ok || in != ch || m.mode != modeWatch || w.watching != 2 {
		t.Fatalf("tc watch @bob: in %v %v mode %v watching %d (%q)", in, ok, m.mode, w.watching, m.notice)
	}
}
