package tui

import (
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
)

func TestDecideTable(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.Local)
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	base := func() client.NotifyPrefs {
		return client.NotifyPrefs{Chans: map[uint64]client.ChanNotify{}, CueOff: map[string]bool{}}
	}
	with := func(f func(*client.NotifyPrefs)) client.NotifyPrefs { p := base(); f(&p); return p }
	all := with(func(p *client.NotifyPrefs) { p.Chans[1] = client.ChanNotify{Level: client.LevelAll} })
	off := with(func(p *client.NotifyPrefs) { p.Chans[1] = client.ChanNotify{Level: client.LevelOff} })
	cases := []struct {
		name  string
		p     client.NotifyPrefs
		at    time.Time
		kind  noteKind
		popup bool
		cue   notify.Cue
	}{
		{"default mention", base(), now, noteMention, true, notify.CueMention},
		{"default message", base(), now, noteMessage, false, notify.CueNone},
		{"default ring", base(), now, noteRing, true, notify.CueRing},
		{"default channel huddle", base(), now, noteHuddle, false, notify.CueNone},
		{"all message", all, now, noteMessage, true, notify.CueMessage},
		{"all huddle", all, now, noteHuddle, true, notify.CueHuddle},
		{"all mention", all, now, noteMention, true, notify.CueMention},
		{"off mention", off, now, noteMention, false, notify.CueNone},
		{"off ring", off, now, noteRing, false, notify.CueNone},
		{"timed mute active", with(func(p *client.NotifyPrefs) {
			p.Chans[1] = client.ChanNotify{Level: client.LevelOff, Until: ms(time.Hour)}
		}), now, noteMention, false, notify.CueNone},
		{"timed mute lapsed", with(func(p *client.NotifyPrefs) {
			p.Chans[1] = client.ChanNotify{Level: client.LevelOff, Until: ms(-time.Minute)}
		}), now, noteMention, true, notify.CueMention},
		{"global all", with(func(p *client.NotifyPrefs) { p.Default = client.LevelAll }), now, noteMessage, true, notify.CueMessage},
		{"global off, channel mentions", with(func(p *client.NotifyPrefs) {
			p.Default = client.LevelOff
			p.Chans[1] = client.ChanNotify{Level: client.LevelMentions}
		}), now, noteMention, true, notify.CueMention},
		{"dnd", with(func(p *client.NotifyPrefs) { p.DNDUntil = ms(time.Hour) }), now, noteRing, false, notify.CueNone},
		{"dnd over", with(func(p *client.NotifyPrefs) { p.DNDUntil = ms(-time.Second) }), now, noteMention, true, notify.CueMention},
		{"quiet wraps midnight", with(func(p *client.NotifyPrefs) { p.Quiet = "22:00-08:00" }), time.Date(2026, 10, 9, 23, 30, 0, 0, time.Local), noteMention, false, notify.CueNone},
		{"after quiet", with(func(p *client.NotifyPrefs) { p.Quiet = "22:00-08:00" }), time.Date(2026, 10, 9, 8, 0, 0, 0, time.Local), noteMention, true, notify.CueMention},
		{"sounds off: silent popup", with(func(p *client.NotifyPrefs) { p.SoundOff = true }), now, noteMention, true, notify.CueNone},
		{"ring sound off", with(func(p *client.NotifyPrefs) { p.CueOff["ring"] = true }), now, noteRing, true, notify.CueNone},
		{"ring sound off keeps mention", with(func(p *client.NotifyPrefs) { p.CueOff["ring"] = true }), now, noteMention, true, notify.CueMention},
	}
	for _, c := range cases {
		popup, cue := decide(c.p, c.at, 1, c.kind)
		if popup != c.popup || cue != c.cue {
			t.Errorf("%s: got (%v, %d), want (%v, %d)", c.name, popup, cue, c.popup, c.cue)
		}
	}
}

func lastNote(t *testing.T, m *Model) notify.Note {
	t.Helper()
	s := notes(m).sent()
	if len(s) == 0 {
		t.Fatal("no notification")
	}
	return s[len(s)-1]
}

// Muted channels: no popup even for a mention, dim in the sidebar with no
// count but a direct mention's @, left out of the herdr unread summary.
// Level all: every message, soft cue, 30 s window; a mention in it still
// gets the mention cue.
func TestNotifyLevels(t *testing.T) {
	m, f := newTestModelFake(t, 100, 24)
	clk := newClock(m)
	nt := notes(m)
	runInput(t, m, "/notify off #backend")
	runInput(t, m, "/notify all #secret")
	if !strings.Contains(m.notice, "#secret notifies: all") {
		t.Fatalf("notice %q", m.notice)
	}
	deliver(m, newMsg(401, 11, 2, "@alice in a muted channel"))
	if len(nt.got()) != 0 {
		t.Fatalf("muted channel notified: %q", nt.got())
	}
	deliver(m, newMsg(402, 13, 3, "plain chatter"))
	if n := lastNote(t, m); n.Cue != notify.CueMessage || n.Title != "#secret · carol" || n.Tag != "tc-13" {
		t.Fatalf("level all: %+v", n)
	}
	deliver(m, newMsg(404, 13, 3, "more chatter"))
	if len(nt.got()) != 1 {
		t.Fatal("level-all window didn't coalesce")
	}
	clk.add(5 * time.Second)
	// A mention doesn't wait out the 30 s window.
	deliver(m, newMsg(403, 13, 3, "hey @alice"))
	if n := lastNote(t, m); n.Cue != notify.CueMention || n.Body != "hey @alice" {
		t.Fatalf("mention in an all window: %+v", n)
	}

	// Sidebar and badge: backend (3 unread, muted) is dim with no count;
	// a mention there shows @.
	v := ansi.Strip(checkView(t, m, "muted sidebar"))
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "# backend") && strings.Contains(l, " 3") {
			t.Fatalf("muted channel shows its count: %q", l)
		}
	}
	if s := m.unreadSummary(); strings.Contains(s, "5 unread") {
		t.Fatalf("summary counts the muted channel: %q", s)
	}
	f.mu.Lock()
	for i := range f.chans {
		if f.chans[i].ID == 11 {
			f.chans[i].Mention = true
		}
	}
	f.mu.Unlock()
	m.reloadAll()
	if v := ansi.Strip(checkView(t, m, "muted mention")); !strings.Contains(v, "@") {
		t.Fatalf("muted mention lost its @:\n%s", v)
	}

	// The header marks the open muted channel.
	run(m, m.openChannel(11))
	if v := ansi.Strip(checkView(t, m, "muted header")); !strings.Contains(strings.Split(v, "\n")[0], "🔕") {
		t.Fatalf("no 🔕 in header:\n%s", v)
	}
	runInput(t, m, "/notify off for 2h")
	if v := ansi.Strip(checkView(t, m, "timed mute header")); !strings.Contains(v, "🔕 until "+m.now().Add(2*time.Hour).Local().Format("15:04")) {
		t.Fatalf("timed mute header:\n%s", v)
	}
	runInput(t, m, "/notify default")
	if m.muted(11) {
		t.Fatal("default didn't unmute")
	}
}

func TestNotifyReplyInMyThread(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	newClock(m)
	f.myThreads = map[uint64]bool{500: true}
	x := newMsg(501, 11, 2, "a reply, no mention")
	x.Thread = 500
	deliver(m, x)
	if n := lastNote(t, m); n.Cue != notify.CueMention {
		t.Fatalf("reply: %+v", n)
	}
	y := newMsg(502, 13, 2, "a reply elsewhere")
	y.Thread = 600
	deliver(m, y)
	if len(notes(m).sent()) != 1 {
		t.Fatal("a reply in someone else's thread notified")
	}
}

func TestNotifyContentPrivacy(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	runInput(t, m, "/notify content name")
	deliver(m, newMsg(601, 20, 2, "secret plans"))
	if n := lastNote(t, m); n.Title != "bob (DM)" || n.Body != "new message" {
		t.Fatalf("name: %+v", n)
	}
	clk.add(time.Minute)
	runInput(t, m, "/notify content none")
	deliver(m, newMsg(602, 11, 3, "@alice secret"))
	if n := lastNote(t, m); n.Title != "tc" || n.Body != "new message" {
		t.Fatalf("none: %+v", n)
	}
	m.opt.NotifyEnv.Content = "full" // env wins over the stored setting
	m.refreshPrefs()
	clk.add(time.Minute)
	deliver(m, newMsg(603, 13, 3, "@alice visible"))
	if n := lastNote(t, m); n.Body != "@alice visible" {
		t.Fatalf("env full: %+v", n)
	}
	runInput(t, m, "/notify content loud")
	if !m.noticeErr {
		t.Fatal("bad content mode accepted")
	}
}

func TestNotifyDNDQuietSounds(t *testing.T) {
	m, _ := newTestModelFake(t, 80, 24)
	clk := newClock(m)
	runInput(t, m, "/dnd 2h")
	deliver(m, newMsg(701, 20, 2, "during dnd"))
	if len(notes(m).sent()) != 0 {
		t.Fatal("notified during DND")
	}
	if v := ansi.Strip(checkView(t, m, "dnd mark")); !strings.Contains(v, "🔕 dnd "+m.now().Add(2*time.Hour).Local().Format("15:04")) {
		t.Fatalf("no DND mark:\n%s", v)
	}
	runInput(t, m, "/dnd off")
	clk.add(time.Minute)
	deliver(m, newMsg(702, 20, 2, "after dnd"))
	if len(notes(m).sent()) != 1 {
		t.Fatal("no notification after DND")
	}
	runInput(t, m, "/dnd") // toggles on for an hour
	if !m.nt.prefs.DND(m.now()) {
		t.Fatal("/dnd didn't toggle on")
	}
	runInput(t, m, "/dnd")
	if m.nt.prefs.DND(m.now()) {
		t.Fatal("/dnd didn't toggle off")
	}
	runInput(t, m, "/dnd until 13:30")
	if got := time.UnixMilli(m.nt.prefs.DNDUntil).Format("15:04"); got != "13:30" || !m.nt.prefs.DND(m.now()) {
		t.Fatalf("until: %s", got)
	}
	runInput(t, m, "/dnd off")

	runInput(t, m, "/notify quiet 25:00-01:00")
	if !m.noticeErr {
		t.Fatal("bad quiet hours accepted")
	}
	runInput(t, m, "/sounds off")
	clk.add(time.Minute)
	deliver(m, newMsg(703, 11, 2, "@alice silent"))
	if n := lastNote(t, m); n.Cue != notify.CueNone {
		t.Fatalf("sounds off: %+v", n)
	}
	runInput(t, m, "/sounds on")
	runInput(t, m, "/sounds mention off")
	clk.add(time.Minute)
	deliver(m, newMsg(704, 13, 2, "@alice quiet mention"))
	if n := lastNote(t, m); n.Cue != notify.CueNone {
		t.Fatalf("mention sound off: %+v", n)
	}
	runInput(t, m, "/sounds")
	if !strings.Contains(m.notice, "mention off") || !strings.Contains(m.notice, "ring on") {
		t.Fatalf("/sounds: %q", m.notice)
	}
	for _, bad := range []string{"/sounds evil on", "/sounds volume 200", "/sounds maybe"} {
		runInput(t, m, bad)
		if !m.noticeErr {
			t.Errorf("%s accepted", bad)
		}
	}
	m.opt.NotifyEnv.Sound = "off"
	m.refreshPrefs()
	if !m.nt.prefs.SoundOff {
		t.Fatal("TC_SOUND=off ignored")
	}
}

// /notify's summary and the test notifications; every frame fits.
func TestNotifyCommandsFit(t *testing.T) {
	for _, sz := range [][2]int{{20, 6}, {40, 12}, {80, 24}, {200, 60}} {
		m, _ := newTestModelFake(t, sz[0], sz[1])
		newClock(m)
		runInput(t, m, "/notify")
		if sz[0] >= 80 && (!strings.Contains(m.notice, "#general: mentions (default)") || !strings.Contains(m.notice, "this device")) ||
			sz[0] < 80 && !strings.HasPrefix(m.notice, "mentions · dnd off") {
			t.Fatalf("%d cols: summary %q", sz[0], m.notice)
		}
		checkView(t, m, "notify summary")
		runInput(t, m, "/notify test")
		for range 3 {
			m.Update(notifyTestMsg{})
		}
		var cues []notify.Cue
		for _, n := range notes(m).sent() {
			cues = append(cues, n.Cue)
		}
		if len(cues) != 4 || cues[2] != notify.CueRing {
			t.Fatalf("test cues %v", cues)
		}
		checkView(t, m, "after test")
		for _, bad := range []string{"/notify loud", "/notify all #nope", "/notify off for soon", "/notify default for 2h", "/notify global"} {
			runInput(t, m, bad)
			if !m.noticeErr {
				t.Errorf("%q accepted", bad)
			}
			checkView(t, m, bad)
		}
	}
}

func startHuddle(m *Model, h *huddleFake, ch, by uint64) {
	h.hmu.Lock()
	h.rosters[ch] = []uint64{by}
	h.hmu.Unlock()
	m.Update(updateMsg(client.Update{Huddle: true, HuddleStarted: []client.HuddleStart{{Chan: ch, By: by}}}))
}

// A DM huddle rings: one popup with the ring cue (resent once), a status
// line prompt, stopped by joining, the huddle ending, Esc (then a cooldown)
// or 30 s. Muted DMs and DND never ring.
func TestRingLifecycle(t *testing.T) {
	m, h := newHuddleModel(t, 80, 24)
	clk := newClock(m)
	startHuddle(m, h, 20, 2)
	n := lastNote(t, m)
	if n.Cue != notify.CueRing || !strings.Contains(n.Title, "bob started a huddle") {
		t.Fatalf("ring: %+v", n)
	}
	if v := ansi.Strip(checkView(t, m, "ringing")); !strings.Contains(v, "📞 bob") {
		t.Fatalf("no ring prompt:\n%s", v)
	}
	gen := m.nt.ring.gen
	m.Update(ringMsg{gen: gen, resend: true})
	if len(notes(m).sent()) != 2 {
		t.Fatal("no resend")
	}
	// /huddle answers it, wherever we are.
	runInput(t, m, "/huddle")
	if ch, in := h.InHuddle(); !in || ch != 20 {
		t.Fatalf("answered into %d %v", ch, in)
	}
	if m.nt.ring.ch != 0 {
		t.Fatal("still ringing after joining")
	}
	runInput(t, m, "/leave-huddle")

	// Ends when the huddle ends.
	clk.add(ringRepeat)
	startHuddle(m, h, 20, 2)
	h.hmu.Lock()
	h.rosters[20] = nil
	h.hmu.Unlock()
	h.update(m)
	if m.nt.ring.ch != 0 {
		t.Fatal("still ringing after the huddle ended")
	}

	// Esc dismisses; the same DM doesn't ring again for a while.
	clk.add(ringRepeat)
	startHuddle(m, h, 20, 2)
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.nt.ring.ch != 0 {
		t.Fatal("Esc didn't dismiss")
	}
	before := len(notes(m).sent())
	h.rosters[20] = nil
	startHuddle(m, h, 20, 2)
	if len(notes(m).sent()) != before || m.nt.ring.ch != 0 {
		t.Fatal("rang again during the cooldown")
	}
	clk.add(ringCooldown + time.Second)

	// Unanswered: stops after 30 s.
	h.rosters[20] = nil
	startHuddle(m, h, 20, 2)
	m.Update(ringMsg{gen: m.nt.ring.gen})
	if m.nt.ring.ch != 0 {
		t.Fatal("ring didn't time out")
	}
	clk.add(ringCooldown + time.Second)

	// A muted DM never rings; nor during DND.
	runInput(t, m, "/notify off #general") // not the DM: still rings
	runInput(t, m, "/dnd 1h")
	h.rosters[20] = nil
	before = len(notes(m).sent())
	startHuddle(m, h, 20, 2)
	if len(notes(m).sent()) != before || m.nt.ring.ch != 0 {
		t.Fatal("rang during DND")
	}
	runInput(t, m, "/dnd off")
	m.active = 20
	runInput(t, m, "/notify off")
	m.active = 10
	h.rosters[20] = nil
	startHuddle(m, h, 20, 2)
	if len(notes(m).sent()) != before {
		t.Fatal("a muted DM rang")
	}

	// Looking at the DM (focused) doesn't ring, or stops it.
	runInput(t, m, "/notify default #general")
	m.active = 20
	runInput(t, m, "/notify default")
	h.rosters[20] = nil
	startHuddle(m, h, 20, 2)
	if m.nt.ring.ch != 0 {
		t.Fatal("rang in the open DM")
	}
}

// Channel huddle starts are silent by default (the 🎧 marker says it); at
// level all one popup per channel per 5 minutes.
func TestChannelHuddleStart(t *testing.T) {
	m, h := newHuddleModel(t, 80, 24)
	clk := newClock(m)
	startHuddle(m, h, 11, 2)
	if len(notes(m).sent()) != 0 {
		t.Fatal("channel huddle start popped up at level mentions")
	}
	runInput(t, m, "/notify all #backend")
	h.rosters[11] = nil
	startHuddle(m, h, 11, 2)
	n := lastNote(t, m)
	if n.Cue != notify.CueHuddle || !strings.Contains(n.Title, "bob started a huddle in #backend") {
		t.Fatalf("huddle popup %+v", n)
	}
	clk.add(time.Minute)
	h.rosters[11] = nil
	startHuddle(m, h, 11, 2)
	if len(notes(m).sent()) != 1 {
		t.Fatal("flapping huddle popped up again")
	}
	clk.add(huddleNoteGap)
	h.rosters[11] = nil
	startHuddle(m, h, 11, 2)
	if len(notes(m).sent()) != 2 {
		t.Fatal("no popup after the gap")
	}
}

func TestRingFits(t *testing.T) {
	for _, sz := range [][2]int{{20, 6}, {40, 12}, {200, 60}} {
		m, h := newHuddleModel(t, sz[0], sz[1])
		newClock(m)
		startHuddle(m, h, 20, 4) // the long-named user
		checkView(t, m, "ring "+strconv.Itoa(sz[0]))
		runInput(t, m, "/dnd 2h")
		checkView(t, m, "ring + dnd")
	}
}

// A peer (or server) flapping a DM huddle rings once; a starter who isn't
// in the DM never rings (rosters aren't end-to-end authenticated).
func TestRingFlapAndFakeCaller(t *testing.T) {
	m, h := newHuddleModel(t, 80, 24)
	clk := newClock(m)
	for range 100 {
		startHuddle(m, h, 20, 2)
		h.hmu.Lock()
		h.rosters[20] = nil
		h.hmu.Unlock()
		h.update(m)
		clk.add(500 * time.Millisecond)
	}
	if n := len(notes(m).sent()); n != 1 {
		t.Fatalf("%d rings from a flapping huddle", n)
	}
	clk.add(time.Hour)
	startHuddle(m, h, 20, 3) // carol isn't in the DM with bob
	if n := len(notes(m).sent()); n != 1 || m.nt.ring.ch != 0 {
		t.Fatalf("a non-member's start rang (%d)", n)
	}
}

// Esc closes an open mic before it dismisses a ring; the ring prompt
// shows even in narrow panes; /notify off for (no duration) is an error.
func TestEscMicBeforeRing(t *testing.T) {
	m, h := newHuddleModel(t, 40, 12)
	newClock(m)
	runInput(t, m, "/huddle")
	runInput(t, m, "/talk")
	startHuddle(m, h, 20, 2)
	if v := ansi.Strip(checkView(t, m, "narrow ring")); !strings.Contains(v, "📞") || !strings.Contains(v, "🎧 ●") {
		t.Fatalf("ring or mic hidden:\n%s", v)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if h.Talking() || m.nt.ring.ch == 0 {
		t.Fatalf("first Esc: talking=%v ring=%d", h.Talking(), m.nt.ring.ch)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.nt.ring.ch != 0 {
		t.Fatal("second Esc didn't dismiss the ring")
	}
	runInput(t, m, "/notify off for")
	if !m.noticeErr || m.muted(10) {
		t.Fatalf("'/notify off for' muted forever (notice %q)", m.notice)
	}
	runInput(t, m, "/sounds OFF")
	if m.noticeErr || !m.nt.prefs.SoundOff {
		t.Fatalf("/sounds OFF: %q", m.notice)
	}
	// /dnd stops a ring that's going.
	runInput(t, m, "/sounds on")
	clkm := newClock(m)
	clkm.add(time.Hour)
	h.hmu.Lock()
	h.rosters[20] = nil
	h.hmu.Unlock()
	startHuddle(m, h, 20, 2)
	if m.nt.ring.ch == 0 {
		t.Fatal("no ring")
	}
	runInput(t, m, "/dnd 1h")
	if m.nt.ring.ch != 0 {
		t.Fatal("ring survived /dnd")
	}
}
