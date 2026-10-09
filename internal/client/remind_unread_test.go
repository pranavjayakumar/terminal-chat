package client

import (
	"database/sql"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

func TestUnreadRemindersInNotes(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	const notes = 150
	if err := c.tx(func(tx *sql.Tx) error {
		return upsertChan(tx, proto.Channel{ID: notes, Kind: proto.ChanDM, Members: []uint64{alice}})
	}); err != nil {
		t.Fatal(err)
	}
	// Ordinary notes to myself are not unread; a delivered reminder is.
	addMsg(t, c, proto.Message{ID: 1, Chan: notes, Author: alice, Body: "buy milk", Time: 1})
	if v := chanView(t, c, notes); v.Unread != 0 || v.Mention {
		t.Fatalf("plain note: %+v", v)
	}
	addMsg(t, c, proto.Message{ID: 2, Chan: notes, Author: alice, Body: "⏰ call mom", Time: 2})
	if v := chanView(t, c, notes); v.Unread != 1 || !v.Mention {
		t.Fatalf("reminder: %+v", v)
	}
	addMsg(t, c, proto.Message{ID: 3, Chan: notes, Author: alice, Body: "⏰ and again (from #general)", Time: 3})
	addMsg(t, c, proto.Message{ID: 4, Chan: notes, Author: alice, Body: "⏰ deleted one", Time: 4, Deleted: true})
	if v := chanView(t, c, notes); v.Unread != 2 {
		t.Fatalf("two reminders: %+v", v)
	}
	// A reminder-looking message in a DM with someone else isn't one.
	addMsg(t, c, proto.Message{ID: 5, Chan: dm, Author: alice, Body: "⏰ x", Time: 5})
	if v := chanView(t, c, dm); v.Unread != 0 {
		t.Fatalf("dm: %+v", v)
	}
	if err := c.MarkRead(notes, 3); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, notes); v.Unread != 0 || v.Mention {
		t.Fatalf("after read: %+v", v)
	}
}
