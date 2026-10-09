package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func (u *user) mention(ch uint64) bool {
	v, _ := u.Channel(ch)
	return v.Mention
}

func (u *user) hasGroup(name string, members ...uint64) bool {
	for _, g := range u.Groups() {
		if g.Name == name && len(g.Members) == len(members) {
			return true
		}
	}
	return false
}

func TestUserGroupsRoundTrip(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	carol := login(t, s, addr, "carol", false)
	dave := login(t, s, addr, "dave", false)
	us := []*user{alice, bob, carol, dave}
	eventually(t, "everyone online", func() bool {
		for _, u := range us {
			if !u.online() || u.channel("general") == 0 {
				return false
			}
		}
		return true
	})
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Only admins make groups; a user's name can't be a group's.
	var pe *proto.Error
	if err := bob.SetGroup(ctx, "oncall", []uint64{bob.Me().ID}); !errors.As(err, &pe) || pe.Code != proto.ErrForbidden {
		t.Fatalf("non-admin SetGroup: %v", err)
	}
	if err := alice.SetGroup(ctx, "dave", []uint64{bob.Me().ID}); !errors.As(err, &pe) || pe.Code != proto.ErrConflict {
		t.Fatalf("group named like a user: %v", err)
	}

	if err := alice.SetGroup(ctx, "oncall", []uint64{bob.Me().ID, carol.Me().ID}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "everyone sees the group", func() bool {
		for _, u := range us {
			if !u.hasGroup("oncall", 1, 2) {
				return false
			}
		}
		return true
	})
	if got := bob.MentionNames(); len(got) != 2 || got[0] != "bob" || got[1] != "oncall" {
		t.Fatalf("bob's mention names %v", got)
	}
	if got := dave.MentionNames(); len(got) != 1 {
		t.Fatalf("dave's mention names %v", got)
	}

	// @oncall pings the members and only them; the author never.
	if err := alice.Send(gen, "deploy is stuck, @oncall please look", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "everyone has the message", func() bool {
		for _, u := range us {
			if !u.has(gen, "deploy is stuck, @oncall please look") {
				return false
			}
		}
		return true
	})
	if !bob.mention(gen) || !carol.mention(gen) || dave.mention(gen) || alice.mention(gen) {
		t.Fatalf("mention flags: bob=%v carol=%v dave=%v alice=%v", bob.mention(gen), carol.mention(gen), dave.mention(gen), alice.mention(gen))
	}
	for _, u := range []*user{bob, carol} {
		got := u.Activity(10)
		if len(got) != 1 || got[0].Kind != client.ActMention || got[0].Chan != gen {
			t.Fatalf("%s activity: %+v", u.Me().Name, got)
		}
	}
	if got := dave.Activity(10); len(got) != 0 {
		t.Fatalf("dave activity: %+v", got)
	}

	// Near-misses and a bot's text don't count.
	if err := alice.Send(gen, "@oncalls and x@oncall and @oncall-two", 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "dave has it", func() bool { return dave.has(gen, "@oncalls and x@oncall and @oncall-two") })
	if n := len(bob.Activity(10)); n != 1 {
		t.Fatalf("near-misses made activity: %d items", n)
	}

	// Changing the members moves the mention; a late user gets the groups
	// from the snapshot.
	if err := alice.SetGroup(ctx, "oncall", []uint64{dave.Me().ID}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "mention moves to dave", func() bool {
		return dave.mention(gen) && !bob.mention(gen) && !carol.mention(gen)
	})
	erin := login(t, s, addr, "erin", false)
	eventually(t, "erin has the group", func() bool { return erin.online() && erin.hasGroup("oncall", 1) })

	// Deleting it ends the mention.
	if err := alice.SetGroup(ctx, "oncall", nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "group gone, mention gone", func() bool {
		return len(erin.Groups()) == 0 && len(dave.Groups()) == 0 && !dave.mention(gen)
	})
	groups, err := alice.FetchGroups(ctx)
	if err != nil || len(groups) != 0 {
		t.Fatalf("FetchGroups = %v, %v", groups, err)
	}
}

// notesBodies lists the reminder messages in u's notes.
func notesReminders(u *user) []string {
	var out []string
	for _, v := range u.Channels() {
		if v.Kind != proto.ChanDM || len(v.Members) != 1 {
			continue
		}
		for _, m := range u.Messages(v.ID, 200) {
			if m.ID != 0 && strings.HasPrefix(m.Body, proto.RemindPrefix) {
				out = append(out, m.Body)
			}
		}
	}
	return out
}

func TestReminderDeliveredToNotes(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() && alice.channel("general") != 0 })
	gen := alice.channel("general")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	at, text, err := client.ParseRemind("1h stand up", time.Now())
	if err != nil || text != "stand up" {
		t.Fatal(err)
	}
	pending, err := alice.Remind(ctx, at, "far future", 0)
	if err != nil {
		t.Fatal(err)
	}
	id, err := alice.Remind(ctx, time.Now().Add(600*time.Millisecond), "call mom", gen)
	if err != nil || id == pending {
		t.Fatalf("Remind = %d, %v", id, err)
	}
	if _, err := alice.Remind(ctx, time.Now().Add(400*time.Millisecond), "dropped", 0); err != nil {
		t.Fatal(err)
	}
	rs, err := alice.Reminders(ctx)
	if err != nil || len(rs) != 3 {
		t.Fatalf("Reminders = %+v, %v", rs, err)
	}
	// Cancel one; it must never arrive, and bob can't touch alice's.
	var pe *proto.Error
	if err := bob.DeleteReminder(ctx, id); !errors.As(err, &pe) || pe.Code != proto.ErrNotFound {
		t.Fatalf("bob deleting alice's reminder: %v", err)
	}
	if err := alice.DeleteReminder(ctx, rs[0].ID); err != nil { // the soonest: "dropped"
		t.Fatal(err)
	}

	eventually(t, "reminder arrives in the notes", func() bool { return len(notesReminders(alice)) == 1 })
	if got := notesReminders(alice); got[0] != "⏰ call mom (from #general)" {
		t.Fatalf("delivered %q", got)
	}
	// It counts as unread and a mention in the notes, not for bob.
	var notes client.ChannelView
	for _, v := range alice.Channels() {
		if v.Kind == proto.ChanDM && len(v.Members) == 1 {
			notes = v
		}
	}
	if notes.Unread != 1 || !notes.Mention {
		t.Fatalf("notes view: %+v", notes)
	}
	if n, _ := alice.OpenNotes(ctx); n.ID != notes.ID {
		t.Fatalf("dm.open found %d, not the notes %d", n.ID, notes.ID)
	}
	if got := notesReminders(bob); len(got) != 0 {
		t.Fatalf("bob got %q", got)
	}
	time.Sleep(300 * time.Millisecond)
	if got := notesReminders(alice); len(got) != 1 {
		t.Fatalf("delivered again or dropped one arrived: %q", got)
	}
	rs, _ = alice.Reminders(ctx)
	if len(rs) != 1 || rs[0].Text != "far future" {
		t.Fatalf("pending after delivery: %+v", rs)
	}
}

func reopen(t *testing.T, home string) *user {
	t.Helper()
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go c.Run(rctx)
	t.Cleanup(func() { rcancel(); c.Close() })
	return &user{Client: c, cancel: rcancel, home: home}
}

// A reminder is the server's job: it fires with every client closed.
func TestReminderFiresWithClientClosed(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := alice.Remind(ctx, time.Now().Add(500*time.Millisecond), "while closed", 0); err != nil {
		t.Fatal(err)
	}
	home := alice.home
	alice.cancel()
	alice.Close()
	time.Sleep(900 * time.Millisecond)

	back := reopen(t, home)
	eventually(t, "reminder in the notes", func() bool { return back.online() && len(notesReminders(back)) == 1 })
	if got := notesReminders(back); got[0] != "⏰ while closed" {
		t.Fatalf("delivered %q", got)
	}
}

// A server that was down when a reminder fell due delivers it on the next
// start, once, however many times it restarts after that.
func TestReminderMissedWhileServerDownIsDeliveredOnceOnRestart(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", false)
	eventually(t, "online", alice.online)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, text := range []string{"first", "second"} {
		if _, err := alice.Remind(ctx, time.Now().Add(1500*time.Millisecond), text, 0); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := alice.Remind(ctx, time.Now().Add(time.Hour), "later", 0); err != nil {
		t.Fatal(err)
	}
	home := alice.home
	alice.cancel()
	alice.Close()
	s.stop() // down before they are due
	time.Sleep(1800 * time.Millisecond)

	s2, addr2 := startServer(t, dir, addr)
	if addr2 != addr {
		t.Fatalf("restarted on %s, want %s", addr2, addr)
	}
	back := reopen(t, home)
	eventually(t, "both missed reminders delivered", func() bool { return back.online() && len(notesReminders(back)) == 2 })
	if got := notesReminders(back); got[0] != "⏰ first" || got[1] != "⏰ second" {
		t.Fatalf("delivered %q", got)
	}

	// Another restart delivers nothing new, and the future one is still pending.
	s2.stop()
	time.Sleep(700 * time.Millisecond) // let the UDP port go
	startServer(t, dir, addr)
	time.Sleep(500 * time.Millisecond)
	eventually(t, "back online", back.online)
	if got := notesReminders(back); len(got) != 2 {
		t.Fatalf("after another restart: %q", got)
	}
	rs, err := back.Reminders(ctx)
	if err != nil || len(rs) != 1 || rs[0].Text != "later" {
		t.Fatalf("pending: %+v, %v", rs, err)
	}
}
