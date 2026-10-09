package e2e

import (
	"context"
	"testing"
	"time"
)

func (u *user) msgID(ch uint64, body string) uint64 {
	for _, m := range u.Messages(ch, 200) {
		if m.Body == body && m.ID != 0 {
			return m.ID
		}
	}
	return 0
}

func (u *user) pinned(ch uint64) []uint64 {
	var ids []uint64
	for _, m := range u.Pins(ch) {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestPinRoundTrip(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	gen := alice.channel("general")
	eventually(t, "bob sees #general", func() bool { return bob.channel("general") == gen && gen != 0 })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, b := range []string{"first", "second"} {
		if err := alice.Send(gen, b, 0); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "bob receives", func() bool { return bob.has(gen, "second") })
	first, second := bob.msgID(gen, "first"), bob.msgID(gen, "second")

	if err := bob.Pin(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := alice.Pin(ctx, second); err != nil {
		t.Fatal(err)
	}
	if err := alice.Pin(ctx, second); err != nil { // no-op
		t.Fatal(err)
	}
	eventually(t, "both see both pins, newest first", func() bool {
		a, b := alice.pinned(gen), bob.pinned(gen)
		return len(a) == 2 && len(b) == 2 && a[0] == second && b[0] == second && a[1] == first
	})
	cv, _ := alice.Channel(gen)
	if len(cv.Pins) != 2 {
		t.Fatalf("ChannelView.Pins = %v", cv.Pins)
	}
	for _, m := range bob.Messages(gen, 10) {
		if (m.ID == first || m.ID == second) != m.Pinned {
			t.Fatalf("message %d Pinned=%v", m.ID, m.Pinned)
		}
	}

	// A user who logs in later gets the pins with the snapshot, and can
	// list them (the bodies come from pins.list).
	carol := login(t, s, addr, "carol", false)
	eventually(t, "carol online with pins", func() bool {
		return carol.online() && carol.channel("general") == gen && len(carol.channelPins(gen)) == 2
	})
	if err := carol.FetchPins(ctx, gen); err != nil {
		t.Fatal(err)
	}
	if got := carol.Pins(gen); len(got) != 2 || got[0].Body != "second" || got[1].Body != "first" {
		t.Fatalf("carol pins: %+v", got)
	}

	// Unpin, and delete-unpins.
	if err := bob.Unpin(ctx, first); err != nil {
		t.Fatal(err)
	}
	eventually(t, "unpin propagates", func() bool { return len(alice.pinned(gen)) == 1 })
	if err := alice.Delete(ctx, second); err != nil {
		t.Fatal(err)
	}
	eventually(t, "delete unpins", func() bool {
		return len(alice.pinned(gen)) == 0 && len(bob.pinned(gen)) == 0 && len(carol.channelPins(gen)) == 0
	})
	if err := bob.Pin(ctx, second); err == nil {
		t.Fatal("pinned a deleted message")
	}
}

func (u *user) channelPins(ch uint64) []uint64 {
	v, _ := u.Channel(ch)
	return v.Pins
}
