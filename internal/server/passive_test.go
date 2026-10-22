package server

import (
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// presenceOf waits up to d for a presence ephemeral about user and returns
// its state ("" on timeout). Other frames are dropped.
func (c *tclient) presenceOf(user uint64, d time.Duration) string {
	c.t.Helper()
	c.held = nil
	deadline := time.After(d)
	for {
		select {
		case f, open := <-c.frames:
			if !open {
				return ""
			}
			var e proto.Ephemeral
			if f.Type == proto.TEphemeral && f.Decode(&e) == nil && e.Kind == proto.EphPresence && e.User == user {
				return e.State
			}
		case <-deadline:
			return ""
		}
	}
}

// waitPresence waits up to d for user's presence to become want.
func (c *tclient) waitPresence(user uint64, want string, d time.Duration) bool {
	c.t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if c.presenceOf(user, time.Until(end)) == want {
			return true
		}
	}
	return false
}

// A passive session (tc background) gets everything a normal one does but
// never makes its user look online, can't fake presence or typing, and
// can't join a huddle (PROTOCOL.md §4.3).
func TestPassiveSession(t *testing.T) {
	for _, kind := range []string{"tcp", "quic"} {
		t.Run(kind, func(t *testing.T) {
			ts := startServer(t, kind)
			alice := ts.register(t, "alice", true)
			bob := ts.register(t, "bob", false)
			snap, _ := alice.syncFresh()
			bob.syncFresh()
			gen := generalID(t, snap)
			bobKey, bobID := bob.signer, bob.user.ID
			bob.conn.Close()
			if !alice.waitPresence(bobID, proto.PresOffline, 5*time.Second) {
				t.Fatal("bob closed: never offline")
			}

			pb, bye := ts.connectCaps(t, bobKey, "", "", []string{proto.CapPassive, proto.CapVoice})
			if bye != "" {
				t.Fatalf("passive login: BYE %s", bye)
			}
			if !slices.Contains(pb.welcome.Caps, proto.CapPassive) {
				t.Fatalf("WELCOME caps %v lack %q", pb.welcome.Caps, proto.CapPassive)
			}
			pb.syncFresh()
			if st := alice.presenceOf(bobID, 500*time.Millisecond); st != "" {
				t.Fatalf("a passive login broadcast presence %q", st)
			}
			pb.sendEph(proto.Ephemeral{Kind: proto.EphPresence, State: proto.PresOnline, Status: "here"})
			pb.sendEph(proto.Ephemeral{Kind: proto.EphTyping, Chan: gen})
			if st := alice.presenceOf(bobID, 500*time.Millisecond); st != "" {
				t.Fatalf("a passive session set presence %q", st)
			}
			pb.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: gen}, proto.ErrForbidden)

			// It still hears everything.
			alice.send(gen, "hello bob")
			if ev := pb.event(proto.KMsgNew); ev.Kind != proto.KMsgNew {
				t.Fatalf("passive session missed msg.new: %+v", ev)
			}

			// A normal session alongside it is online as usual; the passive
			// one leaving doesn't change that, the normal one leaving does.
			nb, bye := ts.connect(t, bobKey, "", "")
			if bye != "" {
				t.Fatalf("normal login: BYE %s", bye)
			}
			nb.syncFresh()
			if !alice.waitPresence(bobID, proto.PresOnline, 5*time.Second) {
				t.Fatal("normal login: never online")
			}
			pb.conn.Close()
			if st := alice.presenceOf(bobID, 500*time.Millisecond); st != "" {
				t.Fatalf("passive logout broadcast presence %q", st)
			}
			nb.conn.Close()
			if !alice.waitPresence(bobID, proto.PresOffline, 5*time.Second) {
				t.Fatal("normal logout: never offline")
			}
		})
	}
}

// Without the cap a session counts as usual (older clients, the chat UI).
func TestNonPassiveWelcomeHasNoPassiveCap(t *testing.T) {
	ts := startServer(t, "tcp")
	a := ts.register(t, "alice", true)
	if slices.Contains(a.welcome.Caps, proto.CapPassive) {
		t.Fatalf("WELCOME caps %v echo passive unasked", a.welcome.Caps)
	}
}
