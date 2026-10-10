package server

import (
	"bytes"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// kinds lists the kinds of the next n events c receives, device events
// (which other users cause at any time) aside.
func (c *tclient) kinds(n int) []string {
	c.t.Helper()
	var out []string
	for len(out) < n {
		if k := c.event("").Kind; !isDeviceKind(k) {
			out = append(out, k)
		}
	}
	return out
}

func (c *tclient) member(ts *testServer, ch, user uint64) bool {
	c.t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.members[ch][user]
}

// chan.add into an encrypted channel commits the next epoch with the
// membership change; without it (or with a wrong one) nothing changes.
// The joiner sees chan.join, then key.epoch, live and on replay.
func TestChanAddE2E(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		fastClock(ts)
		alice := ts.registerCaps(t, "alice", true, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		carol := ts.registerCaps(t, "carol", false, e2eCaps)
		var since uint64
		for _, c := range []*tclient{alice, bob, carol} {
			_, sr := c.syncFresh()
			since = max(since, sr.Head)
		}
		sid := ts.sid()
		a1, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		ch := alice.createE2E("sec")
		// Epoch 0: no key exists yet, so no bundle is needed.
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
		r1 := []uint64{alice.user.ID, carol.user.ID}
		e1 := bundle(t, a1, sid, ch.ID, 1, nil, r1, a1)
		alice.rotate(ch.ID, e1)
		alice.keyEpoch()
		carol.keyEpoch()

		add := func(b *proto.Signed) *proto.Error {
			return alice.errOf(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID, Bundle: b})
		}
		r2 := []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
		wantErr(t, "no bundle", add(nil), proto.ErrBadRequest, "key bundle")
		without := bundle(t, a1, sid, ch.ID, 2, &e1, r1, a1)
		wantErr(t, "roster without the joiner", add(&without), proto.ErrStaleEpoch, "roster")
		noBox := bundle(t, a1, sid, ch.ID, 2, &e1, r2, a1)
		wantErr(t, "no box for the joiner", add(&noBox), proto.ErrBadRequest, "box")
		skip := bundle(t, a1, sid, ch.ID, 3, &e1, r2, a1, b1)
		wantErr(t, "not the next epoch", add(&skip), proto.ErrStaleEpoch, "epoch")
		byBob := bundle(t, b1, sid, ch.ID, 2, &e1, r2, a1, b1)
		wantErr(t, "signed by someone else", add(&byBob), proto.ErrForbidden, "")
		huge := proto.Signed{B: make([]byte, proto.MaxBundle), Sig: e1.Sig}
		wantErr(t, "huge", add(&huge), proto.ErrTooLarge, "")
		if alice.member(ts, ch.ID, bob.user.ID) {
			t.Fatal("a refused add made bob a member")
		}
		bob.noKind(proto.KChanJoin, 50*time.Millisecond)

		e2 := bundle(t, a1, sid, ch.ID, 2, &e1, r2, a1, b1)
		if e := add(&e2); e != nil {
			t.Fatal(e)
		}
		if got := bob.kinds(2); !slices.Equal(got, []string{proto.KChanJoin, proto.KKeyEpoch}) {
			t.Fatalf("bob saw %v", got)
		}
		if got := carol.kinds(2); !slices.Equal(got, []string{proto.KChanJoin, proto.KKeyEpoch}) {
			t.Fatalf("carol saw %v", got)
		}
		c, err := getChannel(t.Context(), ts.st.db, ch.ID)
		if err != nil || c.Epoch != 2 || !slices.Contains(c.Members, bob.user.ID) {
			t.Fatalf("channel %+v %v", c, err)
		}
		// Adding a member again is a no-op, bundle or not.
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID, Bundle: &e2}, nil)
		bob.noKind(proto.KKeyEpoch, 50*time.Millisecond)
		// Replay: chan.join, then key.epoch.
		var kinds []string
		for _, ev := range bob.replayCaps(t, ts, since, e2eCaps) {
			if ev.Chan == ch.ID {
				kinds = append(kinds, ev.Kind)
			}
		}
		if !slices.Equal(kinds, []string{proto.KChanJoin, proto.KKeyEpoch}) {
			t.Fatalf("bob replayed %v", kinds)
		}

		// Plaintext channels: unchanged, and a bundle there is an error.
		var plain proto.ChanRes
		alice.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "plain", Private: true}, &plain)
		pb := bundle(t, a1, sid, plain.Chan.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID}, a1, b1)
		alice.fail(proto.OpChanAdd, proto.ChanAddArgs{Chan: plain.Chan.ID, User: bob.user.ID, Bundle: &pb}, proto.ErrBadRequest)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: plain.Chan.ID, User: bob.user.ID}, nil)
	})
}

// After a chan.add returns, an enc for the epoch before it is stale: the
// add and its epoch are one write, so a racing send lands either before
// both (stored under the old epoch) or after both (stale_epoch).
func TestChanAddSendRace(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	alice.syncFresh()
	bob.syncFresh()
	carol.syncFresh()
	sid := ts.sid()
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	c1, _ := carol.registerDevice(ts)
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	r1 := []uint64{alice.user.ID, bob.user.ID}
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, r1, a1, b1)
	alice.rotate(ch.ID, e1)
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, []uint64{alice.user.ID, bob.user.ID, carol.user.ID}, a1, b1, c1)

	// bob sends epoch-1 messages on a second connection while alice adds.
	bob2, _ := ts.connectCaps(t, bob.signer, "", "", e2eCaps)
	bob2.syncFresh()
	var wg sync.WaitGroup
	var stored, stale []uint64
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 20 {
			r := bob2.req(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "x")})
			switch {
			case r.OK:
				var sr proto.SendRes
				proto.Unmarshal(r.Data, &sr)
				stored = append(stored, sr.Msg)
			case r.Err != nil && r.Err.Code == proto.ErrStaleEpoch:
				stale = append(stale, 0)
			default:
				t.Errorf("send: %v", r.Err)
				return
			}
		}
	}()
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID, Bundle: &e2}, nil)
	wg.Wait()
	// Every epoch-1 message the server stored precedes the epoch-2 bundle.
	var epochSeq uint64
	if err := ts.st.db.QueryRow(`SELECT seq FROM epochs WHERE chan = ? AND epoch = 2`, ch.ID).Scan(&epochSeq); err != nil {
		t.Fatal(err)
	}
	for _, id := range stored {
		if id > epochSeq {
			t.Fatalf("epoch-1 message %d stored after epoch 2 (seq %d)", id, epochSeq)
		}
	}
	wantErr(t, "after the add", bob.errOf(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, b1, ch.ID, 1, e1, "late")}),
		proto.ErrStaleEpoch, "")
	bob.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, b1, ch.ID, 2, e2, "new")}, nil)
}

// chan.remove: the creator (while a member) or an admin may remove someone
// else from a private channel; anyone may remove themselves (= leave).
func TestChanRemovePermissions(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	admin := ts.registerCaps(t, "admin", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	dave := ts.registerCaps(t, "dave", false, e2eCaps)
	snap, _ := admin.syncFresh()
	for _, c := range []*tclient{bob, carol, dave} {
		c.syncFresh()
	}
	var priv proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "team", Private: true}, &priv)
	ch := priv.Chan.ID
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: carol.user.ID}, nil)
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: dave.user.ID}, nil)
	rm := func(c *tclient, user uint64) *proto.Error {
		return c.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch, User: user})
	}

	wantErr(t, "member, not creator", rm(carol, dave.user.ID), proto.ErrForbidden, "creator")
	wantErr(t, "unknown channel", admin.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: 99999, User: dave.user.ID}), proto.ErrNotFound, "")
	if e := rm(bob, dave.user.ID); e != nil {
		t.Fatal(e)
	}
	ev := dave.event(proto.KChanLeave)
	var ref proto.UserRef
	proto.Unmarshal(ev.Data, &ref)
	if ref.User != dave.user.ID || ev.Actor != bob.user.ID {
		t.Fatalf("dave's chan.leave %+v actor %d", ref, ev.Actor)
	}
	if e := rm(bob, dave.user.ID); e != nil { // not a member any more: no-op
		t.Fatal(e)
	}
	// An admin who isn't a member may remove someone from a plaintext channel.
	if e := rm(admin, carol.user.ID); e != nil {
		t.Fatal(e)
	}
	carol.event(proto.KChanLeave)
	// Removing yourself is leaving (and needs no bundle).
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: carol.user.ID}, nil)
	if e := rm(carol, carol.user.ID); e != nil {
		t.Fatal(e)
	}
	if bob.member(ts, ch, carol.user.ID) {
		t.Fatal("carol still a member")
	}
	b := proto.Signed{B: []byte{1}, Sig: []byte{2}}
	wantErr(t, "leave with bundle", bob.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch, User: bob.user.ID, Bundle: &b}), proto.ErrBadRequest, "")
	wantErr(t, "bundle in plaintext", bob.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch, User: dave.user.ID, Bundle: &b}), proto.ErrBadRequest, "")
	// The creator who left can't remove anyone any more.
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: carol.user.ID}, nil)
	bob.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch}, nil)
	wantErr(t, "creator after leaving", rm(bob, carol.user.ID), proto.ErrForbidden, "")

	// Not for public channels or DMs.
	gen := generalID(t, snap)
	wantErr(t, "public", admin.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: gen, User: bob.user.ID}), proto.ErrBadRequest, "public")
	var dm proto.ChanRes
	admin.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &dm)
	wantErr(t, "dm", admin.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: dm.Chan.ID, User: bob.user.ID}), proto.ErrForbidden, "")
}

// chan.remove in an encrypted channel commits the epoch without the
// removed user in the same write: they see their chan.leave but never the
// new key.epoch (live, replayed, key.fetch); the others see both, in order.
func TestChanRemoveE2E(t *testing.T) {
	forEachTransport(t, func(t *testing.T, ts *testServer) {
		fastClock(ts)
		alice := ts.registerCaps(t, "alice", false, e2eCaps)
		bob := ts.registerCaps(t, "bob", false, e2eCaps)
		carol := ts.registerCaps(t, "carol", false, e2eCaps)
		admin := ts.registerCaps(t, "admin", true, e2eCaps)
		var since uint64
		for _, c := range []*tclient{alice, bob, carol, admin} {
			_, sr := c.syncFresh()
			since = max(since, sr.Head)
		}
		sid := ts.sid()
		a1, _ := alice.registerDevice(ts)
		b1, _ := bob.registerDevice(ts)
		c1, _ := carol.registerDevice(ts)
		ch := alice.createE2E("sec")
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
		all := []uint64{alice.user.ID, bob.user.ID, carol.user.ID}
		e1 := bundle(t, a1, sid, ch.ID, 1, nil, all, a1, b1, c1)
		alice.rotate(ch.ID, e1)
		for _, c := range []*tclient{alice, bob, carol} {
			c.keyEpoch()
		}

		rm := func(c *tclient, b *proto.Signed) *proto.Error {
			return c.errOf(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID, Bundle: b})
		}
		without := []uint64{alice.user.ID, bob.user.ID}
		wantErr(t, "no bundle", rm(alice, nil), proto.ErrBadRequest, "key bundle")
		still := bundle(t, a1, sid, ch.ID, 2, &e1, all, a1, b1, c1)
		wantErr(t, "roster with carol", rm(alice, &still), proto.ErrStaleEpoch, "roster")
		toCarol := bundle(t, a1, sid, ch.ID, 2, &e1, without, a1, b1, c1)
		wantErr(t, "box for carol", rm(alice, &toCarol), proto.ErrBadRequest, "non-member")
		adm1, _ := admin.registerDevice(ts)
		byAdmin := bundle(t, adm1, sid, ch.ID, 2, &e1, without, a1, b1)
		wantErr(t, "admin non-member with bundle", rm(admin, &byAdmin), proto.ErrForbidden, "")
		if !alice.member(ts, ch.ID, carol.user.ID) {
			t.Fatal("a refused removal removed carol")
		}
		carol.noKind(proto.KChanLeave, 50*time.Millisecond)

		e2 := bundle(t, a1, sid, ch.ID, 2, &e1, without, a1, b1)
		if e := rm(alice, &e2); e != nil {
			t.Fatal(e)
		}
		ev := carol.event(proto.KChanLeave)
		carol.noKind(proto.KKeyEpoch, 100*time.Millisecond)
		if got := bob.kinds(2); !slices.Equal(got, []string{proto.KChanLeave, proto.KKeyEpoch}) {
			t.Fatalf("bob saw %v", got)
		}
		var kinds []string
		for _, rev := range carol.replayCaps(t, ts, since, e2eCaps) {
			if rev.Chan == ch.ID && rev.Seq >= ev.Seq {
				kinds = append(kinds, rev.Kind)
			}
		}
		if !slices.Equal(kinds, []string{proto.KChanLeave}) {
			t.Fatalf("carol replayed %v after her removal", kinds)
		}
		carol.fail(proto.OpKeyFetch, proto.KeyFetchArgs{Chan: ch.ID, From: 1, To: 2}, proto.ErrForbidden)
		// Her epoch-1 enc is stale now; so is any send from her.
		wantErr(t, "carol sends", carol.errOf(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, c1, ch.ID, 1, e1, "x")}),
			proto.ErrForbidden, "")

		// A disabled member is in no roster: removing them needs no bundle.
		e3 := bundle(t, a1, sid, ch.ID, 3, &e2, all, a1, b1, c1)
		alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID, Bundle: &e3}, nil)
		admin.ok(proto.OpUserDisable, proto.UserArgs{User: carol.user.ID}, nil)
		alice.ok(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch.ID, User: carol.user.ID}, nil)
		if stale, _ := ts.EpochStale(t.Context(), ch.ID); !stale {
			t.Fatal("epoch 3 boxes a disabled user's device and isn't stale")
		}
	})
}

// chan.encrypt converts a plaintext private channel (its creator) or DM
// (any member) to e2e with epoch 1 in the same write; Channel.legacy is the
// newest message id then.
func TestChanEncrypt(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	admin := ts.registerCaps(t, "admin", true, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	carol := ts.registerCaps(t, "carol", false, e2eCaps)
	old := ts.register(t, "old", false)
	snap, _ := admin.syncFresh()
	for _, c := range []*tclient{bob, carol, old} {
		c.syncFresh()
	}
	sid := ts.sid()
	b1, _ := bob.registerDevice(ts)
	c1, _ := carol.registerDevice(ts)
	var priv proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "team", Private: true}, &priv)
	ch := priv.Chan.ID
	bob.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: carol.user.ID}, nil)
	bob.send(ch, "before")
	last := carol.send(ch, "last plaintext").Msg
	roster := []uint64{bob.user.ID, carol.user.ID}
	enc := func(c *tclient, chID uint64, b proto.Signed) *proto.Error {
		return c.errOf(proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: chID, Bundle: b})
	}

	old.fail(proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: ch, Bundle: bundle(t, b1, sid, ch, 1, nil, roster, b1, c1)}, proto.ErrUnknownOp)
	wantErr(t, "not the creator", enc(carol, ch, bundle(t, c1, sid, ch, 1, nil, roster, b1, c1)), proto.ErrForbidden, "creator")
	wantErr(t, "not a member", enc(admin, ch, bundle(t, b1, sid, ch, 1, nil, roster, b1, c1)), proto.ErrForbidden, "")
	wantErr(t, "public", enc(admin, generalID(t, snap), bundle(t, b1, sid, ch, 1, nil, roster, b1, c1)), proto.ErrBadRequest, "private")
	wantErr(t, "roster", enc(bob, ch, bundle(t, b1, sid, ch, 1, nil, []uint64{bob.user.ID}, b1)), proto.ErrStaleEpoch, "roster")
	wantErr(t, "no box for carol", enc(bob, ch, bundle(t, b1, sid, ch, 1, nil, roster, b1)), proto.ErrBadRequest, "box")
	if c, _ := getChannel(t.Context(), ts.st.db, ch); c.E2E || c.Epoch != 0 {
		t.Fatalf("a refused conversion changed the channel: %+v", c)
	}
	// Drain what came so far.
	for range 2 {
		carol.event(proto.KMsgNew)
	}

	e1 := bundle(t, b1, sid, ch, 1, nil, roster, b1, c1)
	var r proto.SeqRes
	bob.ok(proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: ch, Bundle: e1}, &r)
	up := carol.event("")
	var c proto.Channel
	if up.Kind != proto.KChanUpdate || proto.Unmarshal(up.Data, &c) != nil || !c.E2E || c.Legacy != last {
		t.Fatalf("carol's first event %s %+v, want chan.update e2e legacy %d", up.Kind, c, last)
	}
	if s, got := carol.keyEpoch(); s != r.Seq || !bytes.Equal(got.B, e1.B) {
		t.Fatalf("carol key.epoch %d, res %d", s, r.Seq)
	}
	wantErr(t, "plaintext after", carol.errOf(proto.OpSend, proto.SendArgs{Chan: ch, Body: "hi", Nonce: newNonce()}), proto.ErrBadRequest, "upgrade tc")
	carol.ok(proto.OpSend, proto.SendArgs{Chan: ch, Nonce: newNonce(), Enc: encFor(t, sid, c1, ch, 1, e1, "secret")}, nil)
	wantErr(t, "again", enc(bob, ch, e1), proto.ErrConflict, "already")

	// A legacy DM (opened by a client without e2e1) can be converted by
	// either member.
	var dm proto.ChanRes
	old.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &dm)
	if dm.Chan.E2E {
		t.Fatal("a legacy client's DM is encrypted")
	}
	old.send(dm.Chan.ID, "legacy hello")
	old2, _ := ts.connectCaps(t, old.signer, "", "", e2eCaps)
	old2.syncFresh()
	o1, _ := old2.registerDevice(ts)
	dmRoster := []uint64{bob.user.ID, old.user.ID}
	slices.Sort(dmRoster)
	old2.ok(proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: dm.Chan.ID, Bundle: bundle(t, o1, sid, dm.Chan.ID, 1, nil, dmRoster, o1, b1)}, nil)
	got, _ := getChannel(t.Context(), ts.st.db, dm.Chan.ID)
	if !got.E2E || got.Epoch != 1 || got.Legacy == 0 {
		t.Fatalf("converted DM %+v", got)
	}
	// Private notes stay plaintext (the server posts reminders there).
	var notes proto.ChanRes
	bob.ok(proto.OpDMOpen, proto.DMOpenArgs{Users: []uint64{bob.user.ID}}, &notes)
	wantErr(t, "notes", enc(bob, notes.Chan.ID, bundle(t, b1, sid, notes.Chan.ID, 1, nil, []uint64{bob.user.ID}, b1)), proto.ErrBadRequest, "notes")
}

// An admin outside an encrypted channel can't make the next bundle, so
// their removal goes without: it commits, the bundle goes stale (sends
// under it are refused) and the next sender rotates.
func TestChanRemoveE2EAdminNonMember(t *testing.T) {
	ts := startServer(t, "tcp")
	fastClock(ts)
	alice := ts.registerCaps(t, "alice", false, e2eCaps)
	bob := ts.registerCaps(t, "bob", false, e2eCaps)
	admin := ts.registerCaps(t, "admin", true, e2eCaps)
	for _, c := range []*tclient{alice, bob, admin} {
		c.syncFresh()
	}
	sid := ts.sid()
	a1, _ := alice.registerDevice(ts)
	b1, _ := bob.registerDevice(ts)
	ch := alice.createE2E("sec")
	alice.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	e1 := bundle(t, a1, sid, ch.ID, 1, nil, []uint64{alice.user.ID, bob.user.ID}, a1, b1)
	alice.rotate(ch.ID, e1)
	bob.keyEpoch()

	// A member remover still needs the bundle.
	alice.fail(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, proto.ErrBadRequest)
	admin.ok(proto.OpChanRemove, proto.ChanAddArgs{Chan: ch.ID, User: bob.user.ID}, nil)
	bob.event(proto.KChanLeave)
	if alice.member(ts, ch.ID, bob.user.ID) {
		t.Fatal("bob still a member")
	}
	if stale, _ := ts.EpochStale(t.Context(), ch.ID); !stale {
		t.Fatal("epoch 1 isn't stale after the removal")
	}
	wantErr(t, "send under the old epoch", alice.errOf(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 1, e1, "x")}),
		proto.ErrStaleEpoch, "rotated")
	e2 := bundle(t, a1, sid, ch.ID, 2, &e1, []uint64{alice.user.ID}, a1)
	alice.rotate(ch.ID, e2)
	bob.noKind(proto.KKeyEpoch, 100*time.Millisecond)
	alice.ok(proto.OpSend, proto.SendArgs{Chan: ch.ID, Nonce: newNonce(), Enc: encFor(t, sid, a1, ch.ID, 2, e2, "after")}, nil)
}

// chan.encrypt deletes the channel's webhooks in the same write and
// retires bots left without one, like hook.delete.
func TestChanEncryptDeletesHooks(t *testing.T) {
	ts := startHookServer(t)
	caps := []string{proto.CapE2E, proto.CapHooks}
	bob := ts.registerCaps(t, "bob", false, caps)
	bob.syncFresh()
	sid := ts.sid()
	b1, _ := bob.registerDevice(ts)
	var priv proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "team", Private: true}, &priv)
	ch := priv.Chan.ID
	var other proto.ChanRes
	bob.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "other", Private: true}, &other)
	h := bob.createHook(ch, "ci")
	keep := bob.createHook(other.Chan.ID, "cd")
	if r := ts.post(t, "POST", h.URL, "text/plain", "before"); r.status != 200 {
		t.Fatalf("hook post before: %d %s", r.status, r.raw)
	}
	// The bot stays a member (retired, it can't post or have devices), so
	// it is on the roster.
	var botID uint64
	ts.st.db.QueryRow(`SELECT id FROM users WHERE name = ?`, h.Name).Scan(&botID)
	roster := []uint64{bob.user.ID, botID}
	slices.Sort(roster)
	bob.ok(proto.OpChanEncrypt, proto.KeyRotateArgs{Chan: ch, Bundle: bundle(t, b1, sid, ch, 1, nil, roster, b1)}, nil)
	var n int
	ts.st.db.QueryRow(`SELECT COUNT(*) FROM hooks WHERE chan = ?`, ch).Scan(&n)
	if n != 0 {
		t.Fatalf("%d hooks left on the encrypted channel", n)
	}
	var retired bool
	ts.st.db.QueryRow(`SELECT bot_retired FROM users WHERE name = ?`, h.Name).Scan(&retired)
	if !retired {
		t.Fatal("orphaned bot not retired")
	}
	if r := ts.post(t, "POST", h.URL, "text/plain", "after"); r.status == 200 {
		t.Fatal("deleted hook still posts")
	}
	if r := ts.post(t, "POST", keep.URL, "text/plain", "elsewhere"); r.status != 200 {
		t.Fatalf("another channel's hook broke: %d", r.status)
	}
}
