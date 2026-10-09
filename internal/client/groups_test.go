package client

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// groupSeq numbers the group events applied by setGroup.
var groupSeq uint64 = 1000

// setGroup feeds a group.update event through the connection's apply path.
func setGroup(t *testing.T, c *Client, name string, members ...uint64) {
	t.Helper()
	groupSeq++
	ev := proto.Event{Seq: groupSeq, Kind: proto.KGroupUpdate, Time: 1, Actor: alice,
		Data: proto.MustRaw(proto.Group{Name: name, Members: members})}
	u, err := c.applyEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Channels {
		t.Fatal("group.update didn't flag a channel refresh")
	}
}

func groupNames(c *Client) []string {
	var out []string
	for _, g := range c.Groups() {
		out = append(out, g.Name)
	}
	return out
}

func TestGroupEventsApplied(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	setGroup(t, c, "oncall", bob, alice)
	setGroup(t, c, "backend", carol)
	if got := groupNames(c); !slices.Equal(got, []string{"backend", "oncall"}) {
		t.Fatalf("groups %v", got)
	}
	if got := c.MentionNames(); !slices.Equal(got, []string{"alice", "oncall"}) {
		t.Fatalf("mention names %v", got)
	}
	// An update replaces the members; an empty one deletes.
	setGroup(t, c, "oncall", bob)
	if got := c.MentionNames(); !slices.Equal(got, []string{"alice"}) {
		t.Fatalf("mention names after leaving %v", got)
	}
	setGroup(t, c, "backend")
	if got := groupNames(c); !slices.Equal(got, []string{"oncall"}) {
		t.Fatalf("groups after delete %v", got)
	}
	// A malformed event is skipped without wedging sync.
	groupSeq++
	if _, err := c.applyEvent(proto.Event{Seq: groupSeq, Kind: proto.KGroupUpdate, Data: proto.MustRaw(proto.Group{Name: "BAD NAME", Members: []uint64{1}})}); err != nil {
		t.Fatal(err)
	}
	if got := groupNames(c); !slices.Equal(got, []string{"oncall"}) {
		t.Fatalf("bad name stored: %v", got)
	}
	// Replaying an old event (seq at or below last_seq) changes nothing.
	if _, err := c.applyEvent(proto.Event{Seq: 1, Kind: proto.KGroupUpdate, Data: proto.MustRaw(proto.Group{Name: "old", Members: []uint64{alice}})}); err != nil {
		t.Fatal(err)
	}
	if got := groupNames(c); !slices.Equal(got, []string{"oncall"}) {
		t.Fatalf("stale event applied: %v", got)
	}
}

func TestGroupsClearedOnResetAndWipe(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	setGroup(t, c, "oncall", alice)
	if err := resetCache(c.db, serverID); err != nil {
		t.Fatal(err)
	}
	if got := c.Groups(); len(got) != 0 {
		t.Fatalf("groups after reset: %v", got)
	}
	setGroup(t, c, "oncall", alice)
	if err := wipe(c.db); err != nil {
		t.Fatal(err)
	}
	if got := c.Groups(); len(got) != 0 {
		t.Fatalf("groups after wipe: %v", got)
	}
}

func TestMentionedWholeWordRules(t *testing.T) {
	names := []string{"alice", "oncall"}
	for body, want := range map[string]bool{
		"@oncall":                 true,
		"ping @oncall please":     true,
		"@ONCALL":                 true,
		"(@oncall)":               true,
		"@oncall, look":           true,
		"@oncall.":                true,
		"hi @alice":               true,
		"@oncalls":                false,
		"@oncall-prod":            false, // a longer name
		"@oncall_2":               false,
		"x@oncall":                false, // email-like
		"user@oncall.example.com": false,
		"oncall":                  false, // no @
		"@on call":                false,
		"":                        false,
	} {
		if got := Mentioned(body, names); got != want {
			t.Errorf("Mentioned(%q) = %v, want %v", body, got, want)
		}
	}
	if Mentioned("@oncall", nil) || Mentioned("@oncall", []string{""}) {
		t.Error("no names must never match")
	}
}

func TestGroupMentionCountsForMembersOnly(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: bob, Body: "deploy is stuck, @oncall", Time: 1000})
	if v := chanView(t, c, general); v.Unread != 1 || v.Mention {
		t.Fatalf("before the group exists: %+v", v)
	}

	setGroup(t, c, "oncall", carol, bob) // alice isn't in it
	if v := chanView(t, c, general); v.Unread != 1 || v.Mention {
		t.Fatalf("non-member: %+v", v)
	}
	if got := c.Activity(10); len(got) != 0 {
		t.Fatalf("non-member activity: %+v", got)
	}

	setGroup(t, c, "oncall", carol, alice)
	if v := chanView(t, c, general); v.Unread != 1 || !v.Mention {
		t.Fatalf("member: %+v", v)
	}
	got := c.Activity(10)
	if len(got) != 1 || got[0].Kind != ActMention || got[0].Msg != 1 || got[0].Chan != general {
		t.Fatalf("member activity: %+v", got)
	}

	// Removing me from the group (or the group) takes the mention away.
	setGroup(t, c, "oncall", carol)
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("after removal: %+v", v)
	}
	setGroup(t, c, "oncall", alice)
	setGroup(t, c, "oncall")
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("after delete: %+v", v)
	}
}

func TestGroupMentionExclusions(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	setGroup(t, c, "oncall", alice, bob)
	// My own message, a bot's, a deleted one, and near-misses never count.
	addMsg(t, c, proto.Message{ID: 1, Chan: general, Author: alice, Body: "@oncall mine", Time: 1000})
	addMsg(t, c, proto.Message{ID: 2, Chan: general, Author: ciBot, Body: "build failed @oncall", Time: 2000})
	addMsg(t, c, proto.Message{ID: 3, Chan: general, Author: carol, Body: "@oncall oops", Time: 3000, Deleted: true})
	addMsg(t, c, proto.Message{ID: 4, Chan: general, Author: carol, Body: "@oncalls and x@oncall and @oncall-two", Time: 4000})
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("excluded messages made a mention: %+v", v)
	}
	if got := c.Activity(10); len(got) != 0 {
		t.Fatalf("excluded messages in activity: %+v", got)
	}

	// A real one from a person does.
	addMsg(t, c, proto.Message{ID: 5, Chan: general, Author: carol, Body: "Heads up @OnCall", Time: 5000})
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("person's mention missed: %+v", v)
	}
	got := c.Activity(10)
	if len(got) != 1 || got[0].Msg != 5 || got[0].Kind != ActMention {
		t.Fatalf("activity: %+v", got)
	}

	// Read it and the mention is gone.
	if err := c.MarkRead(general, 5); err != nil {
		t.Fatal(err)
	}
	if v := chanView(t, c, general); v.Mention {
		t.Fatalf("after read: %+v", v)
	}
}

func TestGroupMentionInChannelIAmNotIn(t *testing.T) {
	c, _ := localClient(t)
	seedLocal(t, c)
	setGroup(t, c, "oncall", alice)
	addMsg(t, c, proto.Message{ID: 1, Chan: secretChan, Author: bob, Body: "@oncall psst", Time: 1000})
	for _, it := range c.Activity(10) {
		t.Fatalf("activity from a channel I'm not in: %+v", it)
	}
}

func TestGroupFromSnapshotAndSetGroupOp(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapGroups})
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()

	// bootstrap by hand so the snapshot carries groups.
	var a proto.SyncArgs
	r := fc.expectReq(proto.OpSync, &a)
	fc.write(proto.TSnapshot, proto.Snapshot{
		Users: users,
		Chans: []proto.Channel{{ID: general, Name: "general", Kind: proto.ChanPublic, Members: []uint64{alice, bob}}},
		Read:  map[uint64]uint64{},
		Groups: []proto.Group{
			{Name: "oncall", Members: []uint64{alice}}, {Name: "backend", Members: []uint64{bob}},
		},
	})
	fc.write(proto.TSnapshot, proto.Snapshot{Msgs: []proto.Message{{ID: 10, Chan: general, Author: bob, Body: "@oncall help"}}})
	fc.ok(r.ID, proto.SyncRes{Head: 20, Reset: true})
	waitFor(t, "online", online(c))

	if got := groupNames(c); !slices.Equal(got, []string{"backend", "oncall"}) {
		t.Fatalf("groups from snapshot: %v", got)
	}
	if v := chanView(t, c, general); !v.Mention {
		t.Fatalf("snapshot group mention missed: %+v", v)
	}

	// SetGroup sends a lowercase, @-less name and the members.
	done := make(chan error, 1)
	go func() { done <- c.SetGroup(context.Background(), "@OnCall", []uint64{alice, bob}) }()
	var ga proto.GroupSetArgs
	r = fc.expectReq(proto.OpGroupSet, &ga)
	if ga.Name != "oncall" || !slices.Equal(ga.Members, []uint64{alice, bob}) {
		t.Fatalf("group.set %+v", ga)
	}
	fc.ok(r.ID, proto.SeqRes{Seq: 21})
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Local validation never reaches the server.
	if err := c.SetGroup(context.Background(), "Bad Name", []uint64{alice}); err == nil {
		t.Fatal("invalid group name accepted")
	}
	if err := c.SetGroup(context.Background(), "big", make([]uint64, proto.MaxGroupMembers+1)); err == nil {
		t.Fatal("oversized group accepted")
	}

	// FetchGroups replaces the cache.
	fetched := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := c.FetchGroups(ctx)
		fetched <- err
	}()
	r = fc.expectReq(proto.OpGroupsList, nil)
	fc.ok(r.ID, proto.GroupsRes{Groups: []proto.Group{{Name: "team", Members: []uint64{alice}}}})
	if err := <-fetched; err != nil {
		t.Fatal(err)
	}
	if got := groupNames(c); !slices.Equal(got, []string{"team"}) {
		t.Fatalf("groups after fetch: %v", got)
	}
}

func TestGroupOpsNeedServerSupport(t *testing.T) {
	testEnv(t)
	fs := newFakeServer(t) // advertises no caps
	c := open(t, login(t, fs))
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	if err := c.SetGroup(context.Background(), "oncall", []uint64{alice}); err == nil {
		t.Fatal("SetGroup against a server without groups")
	}
	if _, err := c.FetchGroups(context.Background()); err == nil {
		t.Fatal("FetchGroups against a server without groups")
	}
}

func TestGroupOpsOffline(t *testing.T) {
	c, _ := localClient(t)
	if err := c.SetGroup(context.Background(), "oncall", []uint64{alice}); err != ErrOffline {
		t.Fatalf("err %v, want ErrOffline", err)
	}
}
