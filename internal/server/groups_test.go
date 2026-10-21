package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func TestGroupSetAdminOnlyAndEvent(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	carol := ts.register(t, "carol", false)
	alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	if !slices.Contains(alice.welcome.Caps, proto.CapGroups) {
		t.Fatalf("caps %v lack groups", alice.welcome.Caps)
	}

	bob.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{bob.user.ID}}, proto.ErrForbidden)

	var r proto.SeqRes
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{carol.user.ID, bob.user.ID, bob.user.ID}}, &r)
	if r.Seq == 0 {
		t.Fatal("seq 0")
	}
	// Everyone sees it, members or not (alice is the admin and not a member).
	for _, c := range []*tclient{alice, bob, carol} {
		ev := c.event(proto.KGroupUpdate)
		var g proto.Group
		proto.Unmarshal(ev.Data, &g)
		want := []uint64{bob.user.ID, carol.user.ID}
		if g.Name != "oncall" || !slices.Equal(g.Members, want) || ev.Chan != 0 || ev.Actor != alice.user.ID {
			t.Fatalf("%s: %+v %+v", c.user.Name, ev, g)
		}
	}

	// Same members: no-op.
	r = proto.SeqRes{Seq: 9}
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{bob.user.ID, carol.user.ID}}, &r)
	if r.Seq != 0 {
		t.Fatalf("repeat set seq %d", r.Seq)
	}
	alice.quiet(100 * time.Millisecond)

	var gl proto.GroupsRes
	bob.ok(proto.OpGroupsList, struct{}{}, &gl)
	if len(gl.Groups) != 1 || gl.Groups[0].Name != "oncall" || len(gl.Groups[0].Members) != 2 {
		t.Fatalf("groups.list: %+v", gl)
	}

	// Empty members deletes; deleting again is a no-op.
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall"}, &r)
	if r.Seq == 0 {
		t.Fatal("delete seq 0")
	}
	ev := bob.event(proto.KGroupUpdate)
	var g proto.Group
	proto.Unmarshal(ev.Data, &g)
	if g.Name != "oncall" || len(g.Members) != 0 {
		t.Fatalf("delete event: %+v", g)
	}
	r = proto.SeqRes{Seq: 9}
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall"}, &r)
	if r.Seq != 0 {
		t.Fatalf("repeat delete seq %d", r.Seq)
	}
	bob.ok(proto.OpGroupsList, struct{}{}, &gl)
	if len(gl.Groups) != 0 {
		t.Fatalf("after delete: %+v", gl)
	}
}

func TestGroupValidation(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	b := []uint64{bob.user.ID}

	for _, name := range []string{"", "Oncall", "has space", "-x", strings.Repeat("a", 33), "ci-bot", "me"} {
		alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: name, Members: b}, proto.ErrBadRequest)
	}
	// Deleting a (pre-reservation) "me" group is allowed: a no-op here.
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "me"}, nil)
	// A group can't shadow a user.
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "bob", Members: b}, proto.ErrConflict)
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "alice", Members: b}, proto.ErrConflict)
	// Unknown member.
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "team", Members: []uint64{999}}, proto.ErrNotFound)
	// Member limit.
	many := make([]uint64, proto.MaxGroupMembers+1)
	for i := range many {
		many[i] = uint64(i + 1)
	}
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "team", Members: many}, proto.ErrBadRequest)
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "team", Members: []uint64{bob.user.ID, alice.user.ID}}, nil)

	// A bot can't be a member.
	var cr proto.ChanRes
	alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "ci"}, &cr)
	var hr proto.HookCreateRes
	alice.ok(proto.OpHookCreate, proto.HookCreateArgs{Chan: cr.Chan.ID, Name: "deploy"}, &hr)
	var ul proto.UsersRes
	alice.ok(proto.OpUsersList, struct{}{}, &ul)
	var botID uint64
	for _, u := range ul.Users {
		if u.Bot {
			botID = u.ID
		}
	}
	if botID == 0 {
		t.Fatal("no bot user")
	}
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "bots", Members: []uint64{botID}}, proto.ErrBadRequest)
}

func TestGroupLimit(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	alice.syncFresh()
	ctx := context.Background()
	me := []uint64{alice.user.ID}
	for i := 0; i < proto.MaxGroups; i++ {
		if _, err := ts.opGroupSet(ctx, alice.user, proto.GroupSetArgs{Name: fmt.Sprintf("g%d", i), Members: me}); err != nil {
			t.Fatalf("group %d: %v", i, err)
		}
	}
	alice.fail(proto.OpGroupSet, proto.GroupSetArgs{Name: "onemore", Members: me}, proto.ErrConflict)
	// Deleting one frees a slot.
	if _, err := ts.opGroupSet(ctx, alice.user, proto.GroupSetArgs{Name: "g0"}); err != nil {
		t.Fatal(err)
	}
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "onemore", Members: me}, nil)
}

func TestGroupSnapshotAndReplay(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "backend", Members: []uint64{bob.user.ID}}, nil)
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{alice.user.ID}}, nil)

	snap, sr := bob.syncFreshAgain(t, ts)
	if len(snap.Groups) != 2 || snap.Groups[0].Name != "backend" || snap.Groups[1].Name != "oncall" {
		t.Fatalf("snapshot groups: %+v", snap.Groups)
	}

	// Replay from before the groups carries the events to a non-member too.
	frames := bob.replayFrom(t, ts, sr.Head-2)
	n := 0
	for _, f := range frames {
		var ev proto.Event
		f.Decode(&ev)
		if ev.Kind == proto.KGroupUpdate {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("replayed %d group events, want 2", n)
	}
}

func TestGroupBlocksUserName(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	alice.syncFresh()
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{alice.user.ID}}, nil)

	code, err := ts.CreateInvite(context.Background(), false, 0)
	if err != nil {
		t.Fatal(err)
	}
	inv, _ := proto.ParseInvite(code)
	_, bye := ts.connect(t, newKey(t), inv.Token, "oncall")
	if bye != proto.ByeNameTaken {
		t.Fatalf("bye %q, want name_taken", bye)
	}
}

// Disabling a user removes them from every group (and deletes groups left
// empty), so group lists never keep pointing at disabled accounts.
func TestDisableUserPrunesGroups(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	bob := ts.register(t, "bob", false)
	alice.syncFresh()
	bob.syncFresh()
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "oncall", Members: []uint64{alice.user.ID, bob.user.ID}}, nil)
	alice.ok(proto.OpGroupSet, proto.GroupSetArgs{Name: "solo", Members: []uint64{bob.user.ID}}, nil)
	alice.ok(proto.OpUserDisable, proto.UserArgs{User: bob.user.ID}, nil)
	var res proto.GroupsRes
	alice.ok(proto.OpGroupsList, proto.Empty{}, &res)
	if len(res.Groups) != 1 || res.Groups[0].Name != "oncall" || len(res.Groups[0].Members) != 1 || res.Groups[0].Members[0] != alice.user.ID {
		t.Fatalf("groups after disable: %+v", res.Groups)
	}
}
