package tui

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time      { return c.t }
func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }
func newClock(m *Model) *clock {
	c := &clock{t: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	m.now = c.now
	return c
}
func notes(m *Model) *fakeNotifier { return m.opt.Notifier.(*fakeNotifier) }
func newMsg(id, ch, author uint64, body string) proto.Message {
	return proto.Message{ID: id, Chan: ch, Author: author, Body: body}
}

func deliver(m *Model, xs ...proto.Message) {
	run(m, m.applyUpdate(client.Update{New: xs}))
}

func TestNotifyRules(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	nt := notes(m)
	want := [][2]string{}
	check := func(step string) {
		t.Helper()
		if got := nt.got(); !slices.Equal(got, want) {
			t.Fatalf("%s:\n got %q\nwant %q", step, got, want)
		}
	}

	deliver(m, newMsg(301, 20, 2, "\n  \nhi there\nsecond line"))
	want = append(want, [2]string{"bob (DM)", "hi there"})
	check("DM")

	clk.add(10 * time.Second)
	deliver(m,
		newMsg(302, 10, 2, "@alice in the focused active channel"),
		newMsg(303, 11, 1, "@alice my own message"),
		newMsg(304, 11, 2, "no mention here"),
		newMsg(305, 11, 3, "@alicex is not me"),
	)
	check("ignored")

	deliver(m, newMsg(306, 11, 3, "hey @alice look"))
	want = append(want, [2]string{"#backend · carol", "hey @alice look"})
	check("mention in another channel")

	clk.add(10 * time.Second)
	m.Update(tea.BlurMsg{})
	deliver(m, newMsg(307, 10, 2, "@alice while blurred"))
	want = append(want, [2]string{"#general · bob", "@alice while blurred"})
	check("blurred active channel")

	m.opt.Notifier = nil // TC_NOTIFY=off
	clk.add(10 * time.Second)
	deliver(m, newMsg(308, 13, 2, "@alice"))
	check("off")
}

func TestNotifyCoalesce(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	nt := notes(m)

	deliver(m, newMsg(301, 20, 2, "one"))
	gen := m.notes[20].gen
	clk.add(time.Second)
	deliver(m, newMsg(302, 20, 2, "two"))
	clk.add(time.Second)
	deliver(m, newMsg(303, 20, 2, "three"))
	deliver(m, newMsg(304, 20, 2, "four"))
	if n := len(nt.got()); n != 1 {
		t.Fatalf("want only the first notification inside the window, got %d", n)
	}
	m.Update(notifyFlushMsg{ch: 20, gen: gen - 1}) // stale timer
	if len(nt.got()) != 1 {
		t.Fatal("stale flush fired")
	}
	clk.add(3 * time.Second)
	m.Update(notifyFlushMsg{ch: 20, gen: gen})
	got := nt.got()
	if len(got) != 2 || got[1] != [2]string{"bob (DM)", "3 new messages"} {
		t.Fatalf("summary: %q", got)
	}
	if m.notes[20] != nil {
		t.Fatal("window not closed")
	}

	// Global cap: a second channel within 2 s waits for its window.
	deliver(m, newMsg(310, 11, 2, "@alice soon"))
	if len(nt.got()) != 2 {
		t.Fatal("notified within the global cap")
	}
	g11 := m.notes[11].gen
	clk.add(5 * time.Second)
	m.Update(notifyFlushMsg{ch: 11, gen: g11})
	got = nt.got()
	if len(got) != 3 || got[2] != [2]string{"#backend · bob", "@alice soon"} {
		t.Fatalf("deferred single: %q", got)
	}

	// A flush still inside the cap reschedules instead of firing.
	deliver(m, newMsg(311, 13, 2, "@alice a"))
	deliver(m, newMsg(312, 13, 2, "@alice b"))
	g13 := m.notes[13].gen
	clk.add(time.Second)
	if _, cmd := m.Update(notifyFlushMsg{ch: 13, gen: g13}); cmd == nil || len(nt.got()) != 3 {
		t.Fatal("flush inside the cap should reschedule")
	}

	// Opening the channel drops what's pending for it.
	run(m, m.openChannel(13))
	clk.add(10 * time.Second)
	m.Update(notifyFlushMsg{ch: 13, gen: g13})
	if len(nt.got()) != 3 {
		t.Fatalf("pending notification for an opened channel fired: %q", nt.got())
	}
}

func newHerdrModel(t *testing.T, w, h int) (*Model, *fakeClient, *fakeHerdr) {
	t.Helper()
	f := newFake()
	hd := &fakeHerdr{
		panes: []notify.Pane{
			{ID: "w1:p1", Tab: "w1:t1", Agent: "tc"},
			{ID: "w1:p2", Tab: "w1:t2", Label: "server"},
			{ID: "w1:p3", Tab: "w1:t1", Agent: "claude"},
			{ID: "w1:p4", Tab: "w1:t3", Label: "build\x1b", Terminal: "make"},
		},
		text: map[string]string{
			"w1:p2": "\x1b[32mlistening on :7443\x1b[0m   \r\n\x1b]0;title\x07panic: ```boom```\n\n\n",
			"w1:p4": "ok",
		},
	}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, Herdr: hd, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m, f, hd
}

func TestHerdrBadge(t *testing.T) {
	m, f, hd := newHerdrModel(t, 80, 24)
	gen := m.badgeGen
	if m.badgeWant != "tc · 3 unread · 1@" || gen == 0 {
		t.Fatalf("want %q gen %d", m.badgeWant, gen)
	}
	if len(hd.gotTitles()) != 0 {
		t.Fatal("reported before the debounce")
	}
	_, cmd := m.Update(badgeMsg{gen})
	run(m, cmd)
	if got := hd.gotTitles(); !slices.Equal(got, []string{"tc · 3 unread · 1@"}) {
		t.Fatalf("titles %q", got)
	}
	// Unrelated updates don't re-report.
	m.Update(updateMsg(client.Update{Channels: true}))
	_, cmd = m.Update(badgeMsg{m.badgeGen})
	run(m, cmd)
	if len(hd.gotTitles()) != 1 || m.badgeGen != gen {
		t.Fatalf("re-reported an unchanged badge: %q", hd.gotTitles())
	}

	// Two changes inside the debounce: only the last is reported.
	f.chans[0].Unread = 5
	m.Update(updateMsg(client.Update{Channels: true}))
	stale := m.badgeGen
	for i := range f.chans {
		f.chans[i].Unread, f.chans[i].Mention = 0, false
	}
	m.Update(updateMsg(client.Update{Channels: true}))
	_, cmd = m.Update(badgeMsg{stale})
	run(m, cmd)
	_, cmd = m.Update(badgeMsg{m.badgeGen})
	run(m, cmd)
	if got := hd.gotTitles(); !slices.Equal(got, []string{"tc · 3 unread · 1@", ""}) {
		t.Fatalf("titles %q", got)
	}

	f.chans[0].Unread = 1
	m.Update(updateMsg(client.Update{Channels: true}))
	_, cmd = m.Update(badgeMsg{m.badgeGen})
	run(m, cmd)
	m.Close()
	m.Close()
	if got := hd.gotTitles(); !slices.Equal(got, []string{"tc · 3 unread · 1@", "", "tc · 1 unread", ""}) {
		t.Fatalf("titles after close %q", got)
	}

	// Outside herdr nothing is reported.
	m2, _ := newTestModelFake(t, 80, 24)
	if m2.badgeTick() != nil || m2.badgeWant != "" {
		t.Fatal("badge outside herdr")
	}
	m2.Close()
}

func submitText(m *Model, s string) {
	m.input.SetValue(s)
	run(m, m.handleKey(keyMsg("enter")))
}

func TestSharePaneFlow(t *testing.T) {
	m0, _ := newTestModelFake(t, 80, 24)
	submitText(m0, "/share-pane")
	if !m0.noticeErr || !strings.Contains(m0.notice, "herdr") || m0.input.Value() != "/share-pane" {
		t.Fatalf("outside herdr: %q", m0.notice)
	}

	m, f, hd := newHerdrModel(t, 80, 24)
	submitText(m, "/share-pane")
	if m.mode != modePanePick {
		t.Fatalf("mode %v, notice %q", m.mode, m.notice)
	}
	var ids []string
	for _, p := range m.share.panes {
		ids = append(ids, p.ID)
	}
	if !slices.Equal(ids, []string{"w1:p3", "w1:p2", "w1:p4"}) {
		t.Fatalf("picker order %q", ids)
	}
	v := ansi.Strip(checkView(t, m, "picker"))
	if !strings.Contains(v, "claude  w1:p3") || !strings.Contains(v, "server  w1:p2") || strings.Contains(v, "w1:p1") || strings.Contains(v, "\x1b[31m") {
		t.Fatalf("picker view:\n%s", v)
	}
	run(m, m.handleKey(keyMsg("down")))
	run(m, m.handleKey(keyMsg("enter")))
	if m.mode != modeSharePreview {
		t.Fatalf("mode %v, notice %q", m.mode, m.notice)
	}
	if !slices.Equal(hd.reads, []string{"w1:p2:60"}) {
		t.Fatalf("reads %q", hd.reads)
	}
	want := "📋 `server`\n```\nlistening on :7443\npanic: ʼʼʼboomʼʼʼ\n```"
	if m.share.body != want {
		t.Fatalf("body\n got %q\nwant %q", m.share.body, want)
	}
	v = ansi.Strip(checkView(t, m, "preview"))
	if !strings.Contains(v, "listening on :7443") || !strings.Contains(v, "📋 `server`") {
		t.Fatalf("preview view:\n%s", v)
	}
	for _, w := range []int{40, 20} {
		m.Update(tea.WindowSizeMsg{Width: w, Height: 8})
		checkView(t, m, "narrow preview")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	run(m, m.handleKey(keyMsg("esc")))
	if m.mode != modeNormal || len(f.sent) != 0 {
		t.Fatal("Esc should cancel without sending")
	}

	submitText(m, "/share-pane BUILD")
	if m.mode != modeSharePreview || m.share.pane.ID != "w1:p4" {
		t.Fatalf("by label: mode %v pane %q notice %q", m.mode, m.share.pane.ID, m.notice)
	}
	run(m, m.handleKey(keyMsg("enter")))
	if len(f.sent) != 1 || f.sent[0].Chan != 10 || f.sent[0].Body != "📋 `build`\n```\nok\n```" {
		t.Fatalf("sent %+v", f.sent)
	}
	if m.mode != modeNormal {
		t.Fatal("still in preview")
	}

	submitText(m, "/share-pane w1:p3")
	if m.share.pane.ID != "w1:p3" {
		t.Fatal("by id")
	}
	run(m, m.handleKey(keyMsg("esc")))
	submitText(m, "/share-pane w1:p1")
	if !m.noticeErr || m.mode != modeNormal {
		t.Fatalf("own pane should not be shareable: %q", m.notice)
	}
	submitText(m, "/share-pane nope")
	if !m.noticeErr || !strings.Contains(m.notice, "no pane") {
		t.Fatalf("unknown pane: %q", m.notice)
	}
}

func TestShareBody(t *testing.T) {
	var b strings.Builder
	for i := 0; b.Len() < 3*proto.MaxBody; i++ {
		b.WriteString("\x1b[1mline ")
		b.WriteString(strings.Repeat("é", i%50))
		b.WriteString("\x1b[0m\n")
	}
	b.WriteString("THE END\n")
	body := shareBody("pane `x`", b.String())
	if len(body) > proto.MaxBody {
		t.Fatalf("body %d bytes > MaxBody", len(body))
	}
	if !strings.HasPrefix(body, "📋 `pane 'x'`\n```\n… (") || !strings.HasSuffix(body, "THE END\n```") {
		t.Fatalf("shape: %q … %q", body[:60], body[len(body)-40:])
	}
	if !sanitize.Valid(body) || strings.Contains(body, "\x1b") {
		t.Fatal("body has control characters")
	}
	inner := strings.SplitN(body, "\n", 4)[3] // after the note line
	if !strings.HasPrefix(inner, "line ") {
		t.Fatalf("tail should start on a line boundary: %q", inner[:20])
	}
	if got := stripANSI("a\x1b(Bb\x1b=c\x1bP1;2q\x1b\\d\x1b[38;5;1me\r\n"); got != "abcde\n" {
		t.Fatalf("stripANSI %q", got)
	}
}

func TestAutoAway(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	m.opt.AwayAfter = 15 * time.Minute
	m.lastKey = clk.now()
	if m.awayTick() == nil {
		t.Fatal("no away timer")
	}

	clk.add(10 * time.Minute)
	_, cmd := m.Update(awayTickMsg{})
	run(m, cmd)
	if len(f.awayCalls()) != 0 || cmd == nil {
		t.Fatal("away too early, or no re-arm")
	}
	clk.add(6 * time.Minute)
	_, cmd = m.Update(awayTickMsg{})
	run(m, cmd)
	if !slices.Equal(f.awayCalls(), []bool{true}) || !m.autoAway {
		t.Fatalf("auto away: %v", f.awayCalls())
	}
	m.Update(awayTickMsg{}) // a stray tick while away
	if len(f.awayCalls()) != 1 {
		t.Fatal("double away")
	}
	run(m, m.handleKey(keyMsg("x")))
	if !slices.Equal(f.awayCalls(), []bool{true, false}) || m.autoAway {
		t.Fatalf("key should end auto away: %v", f.awayCalls())
	}
	run(m, m.handleKey(keyMsg("y")))
	if len(f.awayCalls()) != 2 {
		t.Fatal("back sent twice")
	}

	// Manual away is never overridden.
	submitText(m, "/away lunch")
	if !slices.Equal(f.awayCalls(), []bool{true, false, true}) || !m.manualAway {
		t.Fatalf("/away: %v", f.awayCalls())
	}
	clk.add(time.Hour)
	_, cmd = m.Update(awayTickMsg{})
	run(m, cmd)
	run(m, m.handleKey(keyMsg("z")))
	if len(f.awayCalls()) != 3 {
		t.Fatalf("auto logic touched a manual away: %v", f.awayCalls())
	}
	submitText(m, "/back")
	if !slices.Equal(f.awayCalls(), []bool{true, false, true, false}) || m.manualAway {
		t.Fatalf("/back: %v", f.awayCalls())
	}
	clk.add(16 * time.Minute)
	_, cmd = m.Update(awayTickMsg{})
	run(m, cmd)
	if !slices.Equal(f.awayCalls(), []bool{true, false, true, false, true}) {
		t.Fatalf("auto away after /back: %v", f.awayCalls())
	}

	// Disabled.
	m2, f2 := newTestModelFake(t, 80, 24)
	clk2 := newClock(m2)
	if m2.awayTick() != nil {
		t.Fatal("timer with AwayAfter 0")
	}
	clk2.add(time.Hour)
	m2.Update(awayTickMsg{})
	if len(f2.awayCalls()) != 0 {
		t.Fatal("away while disabled")
	}
}

func TestAwayAfterEnv(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 15 * time.Minute, "0": 0, "5": 5 * time.Minute, "x": 0, "-1": 0, "0.5": 30 * time.Second} {
		if got := awayAfter(in); got != want {
			t.Errorf("%q: %v, want %v", in, got, want)
		}
	}
}

// TestBotsNeverNotify: webhook bot messages don't notify or get the
// mention gutter, even in a DM or with @me in them (L5).
func TestBotsNeverNotify(t *testing.T) {
	f := newFake()
	f.users = append(f.users, proto.User{ID: 9, Name: "ci-bot", Bot: true})
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	newClock(m)
	deliver(m, newMsg(301, 20, 9, "dm from a bot"), newMsg(302, 11, 9, "fix-@alice failed"))
	if got := notes(m).got(); len(got) != 0 {
		t.Fatalf("bot notified: %q", got)
	}
	deliver(m, newMsg(303, 11, 3, "@alice from carol"))
	if got := notes(m).got(); len(got) != 1 {
		t.Fatalf("person's mention: %q", got)
	}

	gutter := func(body string, author uint64) bool {
		f.msgs[10] = append(f.msgs[10], client.Message{Message: proto.Message{ID: 400 + uint64(len(f.msgs[10])), Chan: 10, Author: author,
			Body: body, Time: f.msgs[10][len(f.msgs[10])-1].Time + 1}})
		m.openChannel(10)
		m.reloadPane(&m.main)
		for _, l := range strings.Split(m.View(), "\n") {
			if l = ansi.Strip(l); strings.Contains(l, body) {
				return strings.Contains(l, "│▎")
			}
		}
		t.Fatalf("%q not on screen:\n%s", body, ansi.Strip(m.View()))
		return false
	}
	if !gutter("@alice person ping", 2) {
		t.Fatal("no mention gutter for a person")
	}
	if gutter("@alice bot ping", 9) {
		t.Fatal("mention gutter for a bot")
	}
}
