package client

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/voice"
)

type nopDuplex3 struct{}

func (nopDuplex3) Close() error { return nil }

func fakeVoice3(t *testing.T, enc bool, keys voice.Keys) *voice.Session {
	t.Helper()
	cfg := voice.Config{Src: 1, Enc: enc, Send: func([]byte) error { return nil }}
	if keys != nil {
		cfg.Keys = keys
	}
	vs, err := voice.Start(cfg, voice.Platform{Codec: nopCodec3{}, Open: func(func(out, in []int16)) (voice.Duplex, error) {
		return nopDuplex3{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

type nopCodec3 struct{}
type nopEnc3 struct{}
type nopDec3 struct{}

func (nopCodec3) NewEncoder() (voice.Encoder, error)         { return nopEnc3{}, nil }
func (nopCodec3) NewDecoder() (voice.Decoder, error)         { return nopDec3{}, nil }
func (nopEnc3) Encode(pcm []int16, out []byte) (int, error)  { out[0] = 1; return 1, nil }
func (nopDec3) Decode(d []byte, pcm []int16) (int, error)    { return len(pcm), nil }
func (nopDec3) DecodeFEC(d []byte, pcm []int16) (int, error) { return len(pcm), nil }
func (nopDec3) Conceal(pcm []int16) (int, error)             { return len(pcm), nil }

// Once the watcher blocks sending (stale epoch) and its background refresh
// fails (here: the server refuses our owed rotation for a moment), the only
// thing that ever clears huddleKeys.blocked is a later background refresh,
// and one is started only while the epoch is still stale. If the epoch
// becomes fresh any other way — the owed-rotation worker (rotateOwed) pays
// it, or another member's rotation arrives — huddleStillValid takes the
// "fresh" branch, primes, returns true, and leaves blocked set: the mic is
// silently dead for the rest of the participation while the UI shows us in
// the huddle.
func TestHuddleKeys_BreakBlockedNeverClearsWhenFreshAgain(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	signer, err := identity.Load(d.c.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	laptop := newPeer(t, alice, signer)
	d.f.event(proto.KDeviceNew, 0, alice, proto.Device{Cert: laptop.cert})
	waitFor(t, "laptop verified", func() bool {
		_, ok, _ := d.c.Trust().Device(testSID(), laptop.dev.ID)
		return ok
	})
	d.c.mu.Lock()
	s := d.c.sess
	d.c.mu.Unlock()

	k := &huddleKeys{c: d.c, sid: testSID(), ch: dm, me: alice, dev: d.me.ID,
		parts: map[uint32]proto.Part{}, cache: map[[2]uint32]cachedKey{}}
	_, _ = rand.Read(k.stream[:])
	p := &participation{ch: dm, src: 1, sess: s, keys: k}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if !d.c.huddleStillValid(ctx, p) {
		t.Fatal("not valid before the revoke")
	}
	if _, _, ok := k.Send(); !ok {
		t.Fatal("can't send before the revoke")
	}

	d.f.mu.Lock()
	d.f.onRotate = func(f *e2eFake, id uint32, a proto.KeyRotateArgs) {
		f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrStaleEpoch, "nope")})
	}
	d.f.mu.Unlock()
	if _, err := d.c.RevokeDevice(ctx, laptop.dev.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rotation owed", func() bool {
		owed, err := d.c.Trust().RotationOwed(testSID(), dm)
		return err == nil && owed
	})

	// The watcher's 1 s check: stale → blocked, refresh in the background,
	// which fails while the server refuses.
	rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
	defer rcancel()
	if !d.c.huddleStillValid(rctx, p) {
		t.Fatal("participation ended instead of pausing")
	}
	waitFor(t, "background refresh done", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		return !k.refreshing
	})
	if _, _, ok := k.Send(); ok {
		t.Fatal("sending while a rotation is owed")
	}

	// The server accepts again; the owed-rotation worker pays it.
	d.f.mu.Lock()
	d.f.onRotate = nil
	d.f.mu.Unlock()
	waitFor(t, "owed rotation paid", func() bool {
		owed, err := d.c.Trust().RotationOwed(testSID(), dm)
		return err == nil && !owed && k.fresh()
	})

	// Next watcher checks: the epoch is fresh and verified.
	for range 3 {
		if !d.c.huddleStillValid(ctx, p) {
			t.Fatal("participation ended")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, _, ok := k.Send(); !ok {
		k.mu.Lock()
		b := k.blocked
		k.mu.Unlock()
		t.Fatalf("still can't send under a fresh epoch (blocked=%v): mic dead until rejoin", b)
	}
}

// watchHuddle ends an invalid participation with c.LeaveHuddle(), which
// tears down whatever is current — not necessarily p. If the user switches
// huddles (JoinHuddle → LeaveHuddle → new join) while p's watcher is inside
// its once-a-second huddleStillValid check (several trust-store and SQLite
// reads; or its select picked the ticker over the just-cancelled ctx), the
// old watcher then leaves the NEW huddle (or, mid-join, bumps gen so the
// new join fails as "superseded"). This drives that interleaving
// directly: p1 is no longer current, its watcher still runs one check.
func TestHuddle_BreakStaleWatcherLeavesNewHuddle(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	d.c.mu.Lock()
	s := d.c.sess
	d.c.mu.Unlock()

	// p1: a participation in dm whose check fails (plaintext keys in what
	// is now an encrypted channel — any reason to end it will do).
	_, p1cancel := context.WithCancel(context.Background())
	defer p1cancel()
	p1 := &participation{ch: dm, src: 1, sess: s, vs: fakeVoice3(t, false, nil), cancel: p1cancel}

	// p2: the huddle the user switched to, now current.
	k := &huddleKeys{c: d.c, sid: testSID(), ch: dm, me: alice, dev: d.me.ID,
		parts: map[uint32]proto.Part{}, cache: map[[2]uint32]cachedKey{}}
	var stream [e2ecrypto.VoiceStreamSize]byte
	k.stream = stream
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	p2 := &participation{ch: dm, src: 2, sess: s, vs: fakeVoice3(t, true, k), keys: k, cancel: cancel2}
	d.c.hud.mu.Lock()
	d.c.hud.cur = p2
	d.c.hud.mu.Unlock()

	wctx, wcancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer wcancel()
	done := make(chan struct{})
	go func() { d.c.watchHuddle(wctx, p1); close(done) }()
	select {
	case <-done:
	case <-wctx.Done():
	}
	d.c.hud.mu.Lock()
	cur := d.c.hud.cur
	d.c.hud.mu.Unlock()
	if cur != p2 || ctx2.Err() != nil {
		t.Fatalf("the old participation's watcher tore down the current huddle (cur=%p, want %p)", cur, p2)
	}
}

// prevEpochGrace is meant for "briefly after the head moves" (frames
// sealed just before a rotation). But head()/prime() start the grace
// whenever the head differs from headEp — including the very first read,
// when headEp is 0. So every fresh join accepts the previous epoch for
// 10 s, however long ago it was replaced. The previous epoch is exactly
// the one a revoked (stolen) device still holds: the rotation that
// excluded it is the head. With group-symmetric voice keys it can then
// inject audio labelled as any member's unrevoked device to everyone who
// joins, for 10 s per join.
func TestHuddleKeys_BreakGraceOnFirstHeadRead(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	signer, err := identity.Load(d.c.cfg.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	laptop := newPeer(t, alice, signer)
	d.f.event(proto.KDeviceNew, 0, alice, proto.Device{Cert: laptop.cert})
	waitFor(t, "laptop verified", func() bool {
		_, ok, _ := d.c.Trust().Device(testSID(), laptop.dev.ID)
		return ok
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := d.c.RevokeDevice(ctx, laptop.dev.ID, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "rotated past the laptop", func() bool {
		h, ok, _ := d.c.Trust().ChainHead(testSID(), dm)
		owed, _ := d.c.Trust().RotationOwed(testSID(), dm)
		return ok && h.Epoch >= 2 && !owed
	})
	h, _, _ := d.c.Trust().ChainHead(testSID(), dm)
	time.Sleep(50 * time.Millisecond) // the head moved "a while" ago

	// A fresh join, long after the rotation.
	k := &huddleKeys{c: d.c, sid: testSID(), ch: dm, me: alice, dev: d.me.ID,
		parts: map[uint32]proto.Part{}, cache: map[[2]uint32]cachedKey{}}
	var stream [e2ecrypto.VoiceStreamSize]byte
	k.setParts([]proto.Part{{Src: 7, User: bob, Dev: d.b1.dev.ID[:], Stream: stream[:]}})
	k.prime()
	if _, ok := k.Recv(7, h.Epoch); !ok {
		t.Fatalf("bob's frames under the head %d refused", h.Epoch)
	}
	if _, ok := k.Recv(7, h.Epoch-1); ok {
		t.Fatalf("fresh join accepts epoch %d (replaced before we joined; the revoked laptop holds its K)", h.Epoch-1)
	}
}
