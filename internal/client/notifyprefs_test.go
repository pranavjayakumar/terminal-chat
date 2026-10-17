package client

import (
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func TestNotifyPrefsPersist(t *testing.T) {
	c, home := localClient(t)
	now := time.Now()
	p := c.NotifyPrefs()
	if l, over := p.Level(general, now); l != LevelMentions || over || p.Volume != DefaultCueVolume || !p.Sound("ring") {
		t.Fatalf("defaults: %v %v %+v", l, over, p)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.SetChanNotify(general, LevelOff, 0))
	must(c.SetChanNotify(dm, LevelAll, now.Add(time.Hour).UnixMilli()))
	must(c.SetNotifySetting("notify.default", "all"))
	must(c.SetNotifySetting("notify.content", "name"))
	must(c.SetNotifySetting("notify.quiet", "22:00-08:00"))
	must(c.SetNotifySetting("sound.ring", "off"))
	must(c.SetNotifySetting("sound.volume", "30"))
	for _, bad := range [][2]string{{"notify.default", "loud"}, {"notify.content", "all"}, {"notify.quiet", "25:00-01:00"},
		{"sound.volume", "101"}, {"sound.evil", "off"}, {"other", "x"}, {"sound.mic", "maybe"}} {
		if c.SetNotifySetting(bad[0], bad[1]) == nil {
			t.Errorf("%s=%s accepted", bad[0], bad[1])
		}
	}
	if c.SetChanNotify(general, "loud", 0) == nil {
		t.Error("bad level accepted")
	}
	c.Close()
	c = reopen(t, home)
	p = c.NotifyPrefs()
	if l, over := p.Level(general, now); l != LevelOff || !over || !p.Muted(general, now) {
		t.Fatalf("general %v %v", l, over)
	}
	if l, _ := p.Level(dm, now); l != LevelAll {
		t.Fatalf("dm %v", l)
	}
	// The timed override lapses back to the global default (all).
	if l, over := p.Level(dm, now.Add(2*time.Hour)); l != LevelAll || over {
		t.Fatalf("dm after expiry %v %v", l, over)
	}
	if l, over := p.Level(99, now); l != LevelAll || over {
		t.Fatalf("unset channel %v %v", l, over)
	}
	if p.Content != "name" || p.Quiet != "22:00-08:00" || p.Sound("ring") || !p.Sound("join") || p.Volume != 30 {
		t.Fatalf("prefs %+v", p)
	}
	must(c.SetChanNotify(general, LevelDefault, 0))
	must(c.SetNotifySetting("notify.default", ""))
	if l, over := c.NotifyPrefs().Level(general, now); l != LevelMentions || over {
		t.Fatalf("after reset %v %v", l, over)
	}
}

func TestQuietHours(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 3, 29, h, m, 0, 0, time.Local) }
	wrap := NotifyPrefs{Quiet: "22:00-08:00"}
	for _, c := range []struct {
		p    NotifyPrefs
		t    time.Time
		want bool
	}{
		{wrap, at(23, 0), true}, {wrap, at(7, 59), true}, {wrap, at(8, 0), false}, {wrap, at(21, 59), false}, {wrap, at(22, 0), true},
		{NotifyPrefs{Quiet: "12:00-13:30"}, at(13, 0), true}, {NotifyPrefs{Quiet: "12:00-13:30"}, at(13, 30), false},
		{NotifyPrefs{}, at(23, 0), false}, {NotifyPrefs{Quiet: "junk"}, at(23, 0), false},
	} {
		if got := c.p.InQuiet(c.t); got != c.want {
			t.Errorf("%q at %s: %v", c.p.Quiet, c.t.Format("15:04"), got)
		}
	}
	for _, bad := range []string{"", "22:00", "22:00-22:00", "24:00-01:00", "1:5-2:00", "a-b"} {
		if _, _, err := ParseQuiet(bad); err == nil {
			t.Errorf("ParseQuiet(%q) ok", bad)
		}
	}
}

func TestInMyThread(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 10, Chan: general, Author: alice, Body: "my root"})
	addMsg(t, c, proto.Message{ID: 20, Chan: general, Author: bob, Body: "bob root"})
	addMsg(t, c, proto.Message{ID: 22, Chan: general, Author: carol, Thread: 20, Body: "before me"})
	addMsg(t, c, proto.Message{ID: 23, Chan: general, Author: alice, Thread: 20, Body: "my reply"})
	for _, x := range []struct {
		m    proto.Message
		want bool
	}{
		{proto.Message{ID: 11, Author: bob, Thread: 10}, true},    // reply to my root
		{proto.Message{ID: 24, Author: bob, Thread: 20}, true},    // after my reply
		{proto.Message{ID: 22, Author: carol, Thread: 20}, false}, // before my reply
		{proto.Message{ID: 12, Author: alice, Thread: 10}, false}, // mine
		{proto.Message{ID: 30, Author: bob}, false},               // not a reply
	} {
		if got := c.InMyThread(x.m); got != x.want {
			t.Errorf("%+v: %v", x.m, got)
		}
	}
}
