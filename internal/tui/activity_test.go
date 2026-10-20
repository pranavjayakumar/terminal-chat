package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

var actBase = time.Date(2026, 10, 8, 10, 40, 0, 0, time.Local).UnixMilli()

// activityFake gives the fake one item of each kind, newest first, aimed at
// messages the fake really has.
func activityFake(f *fakeClient) {
	f.activity = []client.ActivityItem{
		{Kind: client.ActReaction, Time: actBase + 9*60_000, Chan: 10, Msg: 103, Author: 3, Emoji: "🎉", Preview: "👍 nice 你好世界"},
		{Kind: client.ActReply, Time: actBase + 6*60_000, Chan: 10, Msg: 201, Thread: 103, Author: 3, Preview: "reply two"},
		{Kind: client.ActDM, Time: actBase + 5*60_000, Chan: 20, Msg: 300, Author: 2, Preview: "psst"},
		{Kind: client.ActMention, Time: actBase + 4*60_000, Chan: 10, Msg: 104, Author: 2, Preview: "hey @alice can you look?"},
		{Kind: client.ActHighlight, Time: actBase + 1*60_000, Chan: 10, Msg: 101, Author: 2, Preview: "also bold and italic"},
		{Kind: client.ActMention, Time: actBase, Chan: 10, Msg: 5, Author: 2, Preview: "ancient history"}, // not loaded
	}
}

func TestActivityOpenAndClose(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	press(m, "alt+a")
	if m.mode != modeActivity {
		t.Fatalf("Alt-A: mode %v", m.mode)
	}
	v := checkView(t, m, "activity")
	for _, want := range []string{"activity — 6 items", "🎉 8 Oct · #general · carol · on: 👍 nice", "» ", " 8 Oct · @bob · bob · psst",
		"@  8 Oct · #general · bob · hey @alice can you look?", "*  8 Oct · #general · bob · also bold", "↳  8 Oct · #general · carol · reply two"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	// Newest first, as given.
	if strings.Index(v, "on: 👍") > strings.Index(v, "psst") || strings.Index(v, "psst") > strings.Index(v, "hey @alice") {
		t.Errorf("not in order:\n%s", v)
	}
	press(m, "esc")
	if m.mode != modeNormal {
		t.Fatal("Esc didn't close")
	}
	press(m, "alt+a", "alt+a")
	if m.mode != modeNormal {
		t.Fatal("Alt-A again didn't close")
	}
	m.input.SetValue("/activity")
	press(m, "enter")
	if m.mode != modeActivity || m.input.Value() != "" {
		t.Fatalf("/activity: mode %v input %q", m.mode, m.input.Value())
	}
}

func TestActivityEmpty(t *testing.T) {
	m := newTestModel(t, 80, 24)
	press(m, "alt+a")
	v := checkView(t, m, "empty")
	if !strings.Contains(v, "activity — 0 items") || !strings.Contains(v, "nothing yet") {
		t.Fatalf("empty state:\n%s", v)
	}
	press(m, "down", "up", "enter")
	if m.mode != modeNormal {
		t.Fatal("Enter on an empty list should just close it")
	}
}

func TestActivityKeysMoveSelection(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	press(m, "alt+a")
	step := func(want int, keys ...string) {
		t.Helper()
		press(m, keys...)
		if m.act.sel != want {
			t.Fatalf("after %v: sel = %d, want %d", keys, m.act.sel, want)
		}
		checkView(t, m, fmt.Sprint(keys))
	}
	step(0, "up")
	step(1, "down")
	step(2, "ctrl+n")
	step(1, "ctrl+p")
	step(5, "pgdown")
	step(5, "down")
	step(0, "home")
	step(5, "end")
	step(0, "pgup")
}

func TestActivityEnterJumps(t *testing.T) {
	cases := []struct {
		name        string
		start       uint64 // channel open before
		item        int
		wantChan    uint64
		wantThread  uint64
		wantMsg     uint64
		wantNoticed string
	}{
		{"top-level message", 10, 3, 10, 0, 104, ""},
		{"from another channel", 13, 3, 10, 0, 104, ""},
		{"reply opens its thread", 10, 1, 10, 103, 201, ""},
		{"reaction on a message with replies", 11, 0, 10, 0, 103, ""},
		{"dm", 10, 2, 20, 0, 300, ""},
		{"not loaded", 10, 5, 10, 0, 0, "older message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newTestModelFake(t, 100, 30)
			activityFake(f)
			run(m, m.openChannel(tc.start))
			press(m, "alt+a")
			for i := 0; i < tc.item; i++ {
				press(m, "down")
			}
			press(m, "enter")
			if m.mode != modeNormal && m.mode != modeSelect {
				t.Fatalf("mode %v", m.mode)
			}
			if m.active != tc.wantChan {
				t.Fatalf("active = %d, want %d", m.active, tc.wantChan)
			}
			if got := m.thread.root; got != tc.wantThread {
				t.Fatalf("thread root = %d, want %d", got, tc.wantThread)
			}
			if tc.wantNoticed != "" {
				if !strings.Contains(m.notice, tc.wantNoticed) {
					t.Fatalf("notice = %q", m.notice)
				}
				return
			}
			if m.mode != modeSelect {
				t.Fatalf("not in select mode: %v", m.mode)
			}
			p := m.focus()
			if p.sel < 0 || p.msgs[p.sel].ID != tc.wantMsg {
				t.Fatalf("selected %+v, want message %d", p.sel, tc.wantMsg)
			}
			if tc.wantThread != 0 && p != &m.thread {
				t.Fatal("selection should be in the thread pane")
			}
			checkView(t, m, "after jump")
		})
	}
}

func TestActivityJumpKeepsDraft(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	typeVia(m, "half typed")
	press(m, "alt+a", "down", "down", "enter") // the DM
	if m.active != 20 || m.input.Value() != "" {
		t.Fatalf("active %d input %q", m.active, m.input.Value())
	}
	if got := f.draft(10, 0); got != "half typed" {
		t.Fatalf("draft = %q", got)
	}
}

func TestActivityTextIsSanitized(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	f.activity = []client.ActivityItem{
		{Kind: client.ActMention, Time: actBase, Chan: 10, Msg: 104, Author: 2,
			Preview: "\x1b]0;pwned\a\x1b[31mred\x1b[0m ‮evil\x07 @alice"},
		{Kind: client.ActReaction, Time: actBase, Chan: 10, Msg: 104, Author: 99, Emoji: "\x1b]52;c;aGk=\a👍", Preview: "x\ny"},
		{Kind: client.ActMention, Time: actBase, Chan: 777, Msg: 1, Author: 2, Preview: "unknown channel"},
	}
	u := m.users[2]
	u.Name = "bob\x1b]0;x\a"
	m.users[2] = u
	press(m, "alt+a")
	raw := m.View()
	if strings.Contains(raw, "\x1b]") || strings.ContainsAny(raw, "\a‮") {
		t.Fatalf("control characters leaked: %q", raw)
	}
	plain := checkView(t, m, "hostile")
	if !strings.Contains(plain, "user99") || !strings.Contains(plain, "#?") {
		t.Fatalf("fallback names missing:\n%s", plain)
	}
	press(m, "down", "down", "enter") // channel 777 isn't known
	if !strings.Contains(m.notice, "isn't available") {
		t.Fatalf("notice %q", m.notice)
	}
}

func TestActivityRefreshesWhileOpenKeepingSelection(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	clk := newClock(m)
	activityFake(f)
	press(m, "alt+a", "down", "down") // the DM
	dm := f.activity[2]
	fresh := client.ActivityItem{Kind: client.ActMention, Time: actBase + 20*60_000, Chan: 10, Msg: 104, Author: 3, Preview: "brand new"}
	f.activity = append([]client.ActivityItem{fresh}, f.activity...)
	clk.add(2 * time.Second)
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}, Channels: true}))
	if len(m.act.items) != 7 || idOf(m.act.items[m.act.sel]) != idOf(dm) {
		t.Fatalf("selection moved: sel %d of %d", m.act.sel, len(m.act.items))
	}
	if !strings.Contains(checkView(t, m, "refresh"), "brand new") {
		t.Fatal("new item not shown")
	}
	// The selected item disappears: the selection stays in range.
	f.activity = f.activity[:1]
	clk.add(2 * time.Second)
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
	if m.act.sel != 0 || len(m.act.items) != 1 {
		t.Fatalf("sel %d of %d", m.act.sel, len(m.act.items))
	}
}

func TestActivityFitsAllSizes(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}, {69, 20}, {100, 30}, {20, 6}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		activityFake(f)
		for i := 0; i < 120; i++ { // more than a screen, wide runes, long names
			f.activity = append(f.activity, client.ActivityItem{
				Kind: client.ActivityKind(i%5 + 1), Time: actBase - int64(i)*3_600_000, Chan: []uint64{10, 20, 13, 777}[i%4],
				Msg: uint64(100 + i), Author: uint64(1 + i%5), Emoji: "👍",
				Preview: strings.Repeat("你好 long preview ", 12),
			})
		}
		m.input.SetValue("draft text")
		press(m, "alt+a")
		checkView(t, m, "open")
		for _, k := range []string{"down", "down", "pgdown", "pgdown", "end", "up", "pgup", "home", "esc"} {
			press(m, k)
			checkView(t, m, k)
		}
		press(m, "alt+a", "end", "enter") // jump to something unknown
		checkView(t, m, "jump")
		// Narrow sidebar-less layout with the overlay and a thread open.
		m.openThread(103)
		press(m, "alt+a")
		checkView(t, m, "over thread")
	}
}

// Ctrl-A belongs to the textarea (line start); the overlay is Alt-A.
func TestCtrlAIsLineStartNotActivity(t *testing.T) {
	m := newTestModel(t, 100, 30)
	typeVia(m, "hello")
	press(m, "ctrl+a")
	if m.mode != modeNormal {
		t.Fatalf("Ctrl-A opened mode %v", m.mode)
	}
	typeVia(m, "X")
	if got := m.input.Value(); got != "Xhello" {
		t.Fatalf("input = %q, want the cursor at line start", got)
	}
	if !strings.Contains(strings.Join(sheetPlain(100), "\n"), "⌥A") {
		t.Fatal("help doesn't mention Alt-A")
	}
}

func TestActivityRowsAdaptToWidth(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	m.chans = append(m.chans, client.ChannelView{Channel: proto.Channel{ID: 14, Name: "infrastructure-alerts",
		Kind: proto.ChanPublic, Members: []uint64{1}}, Display: "infrastructure-alerts", Member: true})
	mention := client.ActivityItem{Kind: client.ActMention, Time: actBase, Chan: 14, Msg: 1, Author: 2, Preview: "page me"}
	react := client.ActivityItem{Kind: client.ActReaction, Time: actBase, Chan: 14, Msg: 1, Author: 2, Emoji: "🎉", Preview: "page me"}
	row := func(it client.ActivityItem, w int) string {
		r := ansi.Strip(m.activityRow(it, w, false, false))
		if sw := strWidth(r); sw != w {
			t.Fatalf("w=%d: row is %d cells: %q", w, sw, r)
		}
		return strings.TrimRight(r, " ")
	}
	for _, w := range []int{120, 80, 60} {
		r := row(mention, w)
		for _, want := range []string{"8 Oct", "#infrastructure-alerts", "bob", "page me"} {
			if !strings.Contains(r, want) {
				t.Errorf("w=%d: missing %q in %q", w, want, r)
			}
		}
	}
	for _, w := range []int{59, 50, 48} {
		r := row(mention, w)
		if strings.Contains(r, "8 Oct") || !strings.Contains(r, "bob") || !strings.Contains(r, "#infrastr…") || strings.Contains(r, "#infrastru") {
			t.Errorf("w=%d: want no time, the author, a 10-cell channel: %q", w, r)
		}
	}
	for _, w := range []int{47, 30, 20} {
		r := row(mention, w)
		if strings.Contains(r, "8 Oct") || strings.Contains(r, "bob") || !strings.Contains(r, "#infrastr…") {
			t.Errorf("w=%d: want neither time nor author: %q", w, r)
		}
	}
	if r := row(mention, 40); !strings.Contains(r, "page me") {
		t.Errorf("the preview should survive at 40 cells: %q", r)
	}
	// The reason marker is the first thing on every row.
	for _, w := range []int{120, 59, 40} {
		if r := row(mention, w); !strings.HasPrefix(r, " @ ") {
			t.Errorf("w=%d: mention row starts %q", w, r)
		}
		if r := row(react, w); !strings.HasPrefix(r, " 🎉 ") {
			t.Errorf("w=%d: reaction row starts %q", w, r)
		}
	}
	// Short title when narrow.
	m.act.items = f.activity
	if got := ansi.Strip(m.activityLines(59, 5)[0]); !strings.HasPrefix(got, "activity 6 · Enter · Esc") {
		t.Errorf("narrow title %q", got)
	}
	if got := ansi.Strip(m.activityLines(80, 5)[0]); !strings.HasPrefix(got, "activity — 6 items") {
		t.Errorf("wide title %q", got)
	}
}

func TestActivitySelectionSurvivesReopen(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	press(m, "alt+a", "down", "down") // the DM
	press(m, "esc", "alt+a")
	if m.act.sel != 2 {
		t.Fatalf("selection after Esc + reopen = %d, want 2", m.act.sel)
	}
	press(m, "enter") // jump into the DM, select mode
	if m.mode != modeSelect || m.active != 20 {
		t.Fatalf("mode %v, active %d", m.mode, m.active)
	}
	press(m, "alt+a") // from select mode back to the overlay
	if m.mode != modeActivity || m.act.sel != 2 {
		t.Fatalf("Alt-A from select mode: mode %v sel %d, want the overlay on item 2", m.mode, m.act.sel)
	}
	checkView(t, m, "back")
	// A different list starts at the top again.
	press(m, "esc")
	f.activity = f.activity[:2]
	press(m, "alt+a")
	if m.act.sel != 0 {
		t.Fatalf("changed list kept selection %d", m.act.sel)
	}
}

func TestActivityMarksItemsNewerThanLastOpen(t *testing.T) {
	m, f := newTestModelFake(t, 120, 30)
	clk := newClock(m)
	activityFake(f)
	dots := func() (n int) {
		for _, l := range strings.Split(checkView(t, m, "dots"), "\n") {
			if regexp.MustCompile(`•[@↳»🎉]`).MatchString(l) {
				n++
			}
		}
		return
	}
	press(m, "alt+a")
	if got := dots(); got != 0 {
		t.Fatalf("first open: %d dots, want none (nothing to compare with)", got)
	}
	if f.seen != actBase+9*60_000 {
		t.Fatalf("seen = %d, want the newest item's time", f.seen)
	}
	press(m, "esc")
	// Something new arrives while the overlay is closed.
	fresh := client.ActivityItem{Kind: client.ActMention, Time: actBase + 20*60_000, Chan: 10, Msg: 104, Author: 3, Preview: "brand new"}
	f.activity = append([]client.ActivityItem{fresh}, f.activity...)
	press(m, "alt+a")
	if got := dots(); got != 1 {
		t.Fatalf("%d dots, want 1 (only the new item)\n%s", got, checkView(t, m, "x"))
	}
	if row := activityRowOf(t, checkView(t, m, "x"), "brand new"); !regexp.MustCompile(`•[@↳»🎉]`).MatchString(row) {
		t.Fatalf("the new item has no dot: %q", row)
	}
	if f.seen != fresh.Time {
		t.Fatalf("seen = %d, want %d", f.seen, fresh.Time)
	}
	// Seen now: reopening shows no dots.
	press(m, "esc", "alt+a")
	if got := dots(); got != 0 {
		t.Fatalf("reopened: %d dots, want none", got)
	}
	// An item arriving while the overlay is open gets one, and counts as seen.
	late := client.ActivityItem{Kind: client.ActDM, Time: actBase + 30*60_000, Chan: 20, Msg: 300, Author: 2, Preview: "late one"}
	f.activity = append([]client.ActivityItem{late}, f.activity...)
	clk.add(2 * time.Second)
	m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{20: true}}))
	if row := activityRowOf(t, checkView(t, m, "x"), "late one"); !regexp.MustCompile(`•[@↳»🎉]`).MatchString(row) {
		t.Fatalf("live item has no dot: %q", row)
	}
	if f.seen != late.Time {
		t.Fatalf("seen = %d, want %d", f.seen, late.Time)
	}
}

func activityRowOf(t *testing.T, view, text string) string {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if strings.Contains(l, text) {
			return l
		}
	}
	t.Fatalf("no row with %q in\n%s", text, view)
	return ""
}

// While the overlay is open, only messages in channels I'm in refresh it,
// and never more than once a second.
func TestActivityRefreshIsFilteredAndDebounced(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	clk := newClock(m)
	activityFake(f)
	reads := func() (n int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, c := range f.calls {
			if c == "activity" {
				n++
			}
		}
		return
	}
	mine := map[uint64]bool{10: true}
	press(m, "alt+a")
	base := reads()
	update := func(u client.Update) tea.Cmd {
		_, cmd := m.Update(updateMsg(u))
		return cmd
	}

	// Not about the list: users, presence, typing, a channel I'm not in.
	clk.add(5 * time.Second)
	update(client.Update{Users: true})
	update(client.Update{Presence: true})
	update(client.Update{Typing: map[uint64]bool{10: true}})
	update(client.Update{Msgs: map[uint64]bool{12: true}}) // #random: not a member
	if got := reads(); got != base {
		t.Fatalf("%d extra reads for updates that can't change the list", got-base)
	}

	// The first relevant update refreshes at once …
	update(client.Update{Msgs: mine})
	if got := reads(); got != base+1 {
		t.Fatalf("reads = %d, want %d", got, base+1)
	}
	// … a burst within the second does not, but schedules exactly one refresh.
	for i := 0; i < 10; i++ {
		clk.add(50 * time.Millisecond)
		update(client.Update{Msgs: mine})
	}
	if got := reads(); got != base+1 {
		t.Fatalf("burst caused %d reads", got-base-1)
	}
	if !m.act.pending || !m.act.timer {
		t.Fatalf("no refresh scheduled: pending=%v timer=%v", m.act.pending, m.act.timer)
	}
	clk.add(time.Second)
	m.Update(activityRefreshMsg{})
	if got := reads(); got != base+2 || m.act.pending || m.act.timer {
		t.Fatalf("after the tick: reads %d pending=%v timer=%v", got-base, m.act.pending, m.act.timer)
	}
	// A tick with nothing pending, or after the overlay closed, reads nothing.
	m.Update(activityRefreshMsg{})
	press(m, "esc")
	update(client.Update{Msgs: mine})
	m.Update(activityRefreshMsg{})
	if got := reads(); got != base+2 {
		t.Fatalf("reads = %d", got-base)
	}
}

// bigChannel gives #backend (11) n messages, ids 1..n.
func bigChannel(f *fakeClient, n int) {
	for i := 1; i <= n; i++ {
		f.msgs[11] = append(f.msgs[11], client.Message{Message: proto.Message{ID: uint64(i), Chan: 11, Author: 2,
			Body: fmt.Sprintf("backend message %d", i), Time: actBase + int64(i)*1000}})
	}
	f.chans[0].Last = uint64(n)
}

// Reaching an old message widens the page; that must end with the selection,
// not stay for the rest of the session.
func TestJumpWidensThePageOnlyWhileNeeded(t *testing.T) {
	old := proto.Message{ID: 5, Chan: 11}
	setup := func(t *testing.T) (*Model, *fakeClient) {
		m, f := newTestModelFake(t, 100, 30)
		bigChannel(f, 300)
		m.jumpTo(old)
		if m.mode != modeSelect || m.main.msgs[m.main.sel].ID != 5 {
			t.Fatalf("jump failed: mode %v notice %q", m.mode, m.notice)
		}
		if m.limit(11) <= pageLimit {
			t.Fatalf("limit = %d, want the page widened", m.limit(11))
		}
		return m, f
	}
	t.Run("leaving select mode", func(t *testing.T) {
		m, _ := setup(t)
		press(m, "esc")
		if m.limit(11) != pageLimit || loadedReal(m.main.msgs) != pageLimit {
			t.Fatalf("limit %d, %d messages loaded after Esc", m.limit(11), loadedReal(m.main.msgs))
		}
		checkView(t, m, "after esc")
	})
	t.Run("leaving the channel", func(t *testing.T) {
		m, _ := setup(t)
		run(m, m.openChannel(10))
		if m.limit(11) != pageLimit {
			t.Fatalf("limit of the channel left behind = %d", m.limit(11))
		}
	})
	t.Run("selection moving into the normal page", func(t *testing.T) {
		m, _ := setup(t)
		press(m, "end") // newest message
		if m.mode != modeSelect || m.limit(11) != pageLimit || m.main.msgs[m.main.sel].ID != 300 {
			t.Fatalf("mode %v limit %d selected %d", m.mode, m.limit(11), m.main.msgs[m.main.sel].ID)
		}
		checkView(t, m, "at the end")
	})
	t.Run("selection still far back keeps the page", func(t *testing.T) {
		m, _ := setup(t)
		press(m, "down", "down")
		if m.limit(11) <= pageLimit || m.main.msgs[m.main.sel].ID != 7 {
			t.Fatalf("limit %d selected %d", m.limit(11), m.main.msgs[m.main.sel].ID)
		}
	})
	t.Run("a larger limit from before is restored, not reset", func(t *testing.T) {
		m, f := newTestModelFake(t, 100, 30)
		bigChannel(f, 300)
		m.limits[11] = 150
		m.jumpTo(old)
		if m.limit(11) <= 150 {
			t.Fatalf("limit = %d", m.limit(11))
		}
		press(m, "esc")
		if m.limit(11) != 150 {
			t.Fatalf("limit = %d, want 150 back", m.limit(11))
		}
	})
	t.Run("not found", func(t *testing.T) {
		m, f := newTestModelFake(t, 100, 30)
		bigChannel(f, 300)
		m.jumpTo(proto.Message{ID: 999999, Chan: 11})
		if m.limit(11) != pageLimit || !strings.Contains(m.notice, "older message") {
			t.Fatalf("limit %d notice %q", m.limit(11), m.notice)
		}
	})
}

func TestJumpToThreadReplyWaitsForTheHistory(t *testing.T) {
	reply := proto.Message{ID: 250, Chan: 10, Thread: 103}
	arrive := func(f *fakeClient) {
		f.threads[103] = append(f.threads[103], client.Message{Message: proto.Message{ID: 250, Chan: 10, Thread: 103, Author: 3, Body: "late reply", Time: actBase}})
	}
	t.Run("found on the next update", func(t *testing.T) {
		m, f := newTestModelFake(t, 100, 30)
		m.jumpTo(reply) // its request is still running
		if strings.Contains(m.notice, "older message") || m.mode == modeSelect || !m.jump.waiting {
			t.Fatalf("notice %q mode %v waiting %v", m.notice, m.mode, m.jump.waiting)
		}
		m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{11: true}})) // another channel: nothing yet
		if m.mode == modeSelect {
			t.Fatal("selected without the message")
		}
		arrive(f)
		m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
		if m.mode != modeSelect || m.thread.sel < 0 || m.thread.msgs[m.thread.sel].ID != 250 || m.jump.waiting {
			t.Fatalf("mode %v sel %d waiting %v", m.mode, m.thread.sel, m.jump.waiting)
		}
		checkView(t, m, "selected")
	})
	t.Run("gives up when the request is done", func(t *testing.T) {
		m, _ := newTestModelFake(t, 100, 30)
		m.jumpTo(reply)
		m.Update(threadLoadedMsg{ch: 10, root: 103})
		if m.jump.waiting || !strings.Contains(m.notice, "older message") {
			t.Fatalf("waiting %v notice %q", m.jump.waiting, m.notice)
		}
	})
	t.Run("found when the request finishes", func(t *testing.T) {
		m, f := newTestModelFake(t, 100, 30)
		m.jumpTo(reply)
		arrive(f)
		m.Update(threadLoadedMsg{ch: 10, root: 103})
		if m.mode != modeSelect || m.thread.msgs[m.thread.sel].ID != 250 {
			t.Fatalf("mode %v", m.mode)
		}
	})
	t.Run("leaving drops the wish", func(t *testing.T) {
		m, f := newTestModelFake(t, 100, 30)
		m.jumpTo(reply)
		press(m, "esc") // closes the thread
		arrive(f)
		m.Update(updateMsg(client.Update{Msgs: map[uint64]bool{10: true}}))
		if m.mode == modeSelect || m.jump.waiting {
			t.Fatalf("mode %v waiting %v", m.mode, m.jump.waiting)
		}
	})
}

// The wheel scrolls what is on top: the overlay's list, not the log under it.
func TestActivityWheelMovesTheOverlaySelection(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	activityFake(f)
	wheel := func(b tea.MouseButton) {
		m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: b})
	}
	before := m.main.scroll
	press(m, "alt+a")
	wheel(tea.MouseButtonWheelDown)
	if m.act.sel != 3 {
		t.Fatalf("after wheel down: sel = %d, want 3", m.act.sel)
	}
	wheel(tea.MouseButtonWheelDown)
	wheel(tea.MouseButtonWheelDown)
	if m.act.sel != 5 { // the last of 6, clamped
		t.Fatalf("sel = %d, want 5", m.act.sel)
	}
	wheel(tea.MouseButtonWheelUp)
	if m.act.sel != 2 {
		t.Fatalf("after wheel up: sel = %d, want 2", m.act.sel)
	}
	wheel(tea.MouseButtonWheelUp)
	wheel(tea.MouseButtonWheelUp)
	if m.act.sel != 0 {
		t.Fatalf("sel = %d, want 0", m.act.sel)
	}
	if m.main.scroll != before {
		t.Fatalf("the hidden log scrolled: %d → %d", before, m.main.scroll)
	}
	checkView(t, m, "wheel")
	// With the overlay closed the wheel scrolls the log again.
	press(m, "esc")
	wheel(tea.MouseButtonWheelUp)
	if m.main.scroll == before {
		t.Fatal("the wheel stopped scrolling the log")
	}
}
