package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

var voiceCaps = []string{proto.CapVoice, proto.CapE2E}

type vclient struct {
	*tclient
	dg  transport.DatagramConn
	gen uint64
	src uint32
}

func (ts *testServer) voiceClient(t *testing.T, name string) *vclient {
	t.Helper()
	c := ts.registerCaps(t, name, false, voiceCaps)
	snap, _ := c.syncFresh()
	v := &vclient{tclient: c, gen: generalID(t, snap)}
	v.dg, _ = transport.Datagrams(c.conn)
	return v
}

// roster waits for the next huddle ephemeral about ch.
func (c *tclient) roster(ch uint64) []proto.Part {
	c.t.Helper()
	var skipped []proto.Frame
	defer func() { c.held = append(skipped, c.held...) }()
	for {
		f := c.recv("huddle roster")
		if f.Type == proto.TEphemeral {
			var e proto.Ephemeral
			if f.Decode(&e) == nil && e.Kind == proto.EphHuddle && e.Chan == ch {
				return e.Parts
			}
		}
		skipped = append(skipped, f)
	}
}

// rosterIs waits until a roster of ch lists exactly want (in order).
func (c *tclient) rosterIs(ch uint64, want ...uint64) []proto.Part {
	c.t.Helper()
	for {
		p := c.roster(ch)
		if slices.Equal(users(p), want) {
			return p
		}
	}
}

func users(parts []proto.Part) []uint64 {
	out := make([]uint64, len(parts))
	for i, p := range parts {
		out[i] = p.User
	}
	return out
}

func (v *vclient) join(ch uint64) uint32 {
	v.t.Helper()
	var r proto.HuddleJoinRes
	v.ok(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch}, &r)
	if r.Src == 0 {
		v.t.Fatal("src 0")
	}
	v.src = r.Src
	return r.Src
}

func (v *vclient) frame(seq uint32, payload string) []byte {
	return proto.Datagram{Kind: proto.DgAudio, Src: v.src, Seq: seq, Payload: []byte(payload)}.Marshal()
}

func (v *vclient) sendDg(b []byte) {
	v.t.Helper()
	if err := v.dg.SendDatagram(b); err != nil {
		v.t.Fatal(err)
	}
}

// drain collects datagrams until none arrive for d.
func (v *vclient) drain(d time.Duration) [][]byte {
	var out [][]byte
	for {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		b, err := v.dg.ReceiveDatagram(ctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, b)
	}
}

// pace sends frames at roughly real-time voice rate so the token bucket
// and quic-go's queues aren't the thing under test.
func pace(i int) {
	if i%10 == 9 {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestHuddleRelay(t *testing.T) {
	ts := startServer(t, "quic")
	a, b, c := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo"), ts.voiceClient(t, "cy")
	g := a.gen

	a.join(g)
	for _, x := range []*vclient{a, b, c} {
		if got := users(x.roster(g)); !slices.Equal(got, []uint64{a.user.ID}) {
			t.Fatalf("%s roster %v", x.user.Name, got)
		}
	}
	b.join(g)
	c.join(g)
	for _, x := range []*vclient{a, b, c} {
		x.roster(g)
		if got := users(x.roster(g)); !slices.Equal(got, []uint64{a.user.ID, b.user.ID, c.user.ID}) {
			t.Fatalf("%s roster %v", x.user.Name, got)
		}
	}

	const n = 60
	var sent [][]byte
	for i := range n {
		f := a.frame(uint32(i), fmt.Sprintf("opus-%03d", i))
		sent = append(sent, f)
		a.sendDg(f)
		pace(i)
	}
	for _, x := range []*vclient{b, c} {
		got := x.drain(300 * time.Millisecond)
		// Loopback rarely loses datagrams, but they're unreliable by design.
		if len(got) < n*9/10 {
			t.Fatalf("%s got %d/%d frames", x.user.Name, len(got), n)
		}
		for _, f := range got {
			if !slices.ContainsFunc(sent, func(s []byte) bool { return bytes.Equal(s, f) }) {
				t.Fatalf("%s got altered frame %x", x.user.Name, f)
			}
		}
	}
	if got := a.drain(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("sender got its own %d frames back", len(got))
	}

	// Reordered and duplicated frames are forwarded as-is: ordering is the
	// receivers' jitter buffer's job, never the relay's.
	for _, i := range []int{5, 3, 4, 4} {
		b.sendDg(b.frame(uint32(i), "x"))
	}
	if got := a.drain(200 * time.Millisecond); len(got) < 3 {
		t.Fatalf("reordered frames: got %d", len(got))
	}
	c.drain(100 * time.Millisecond)

	// Spoofed src, wrong kind, garbage: dropped.
	dropped := ts.hud.dropped.Load()
	b.sendDg(proto.Datagram{Kind: proto.DgAudio, Src: a.src, Payload: []byte("spoof")}.Marshal())
	b.sendDg(proto.Datagram{Kind: proto.DgAudioEnc, Src: b.src, Payload: make([]byte, 20)}.Marshal())
	b.sendDg([]byte{0x01, 0x00})
	b.sendDg(append([]byte{0x7f}, make([]byte, 20)...))
	if got := append(a.drain(200*time.Millisecond), c.drain(50*time.Millisecond)...); len(got) != 0 {
		t.Fatalf("bad frames relayed: %x", got)
	}
	waitFor(t, "drop counter", func() bool { return ts.hud.dropped.Load() >= dropped+4 })

	// After leaving, c neither sends nor receives.
	c.ok(proto.OpHuddleLeave, struct{}{}, nil)
	if got := users(a.roster(g)); !slices.Equal(got, []uint64{a.user.ID, b.user.ID}) {
		t.Fatalf("after leave %v", got)
	}
	c.sendDg(c.frame(0, "ghost"))
	a.sendDg(a.frame(1000, "to-b"))
	if got := b.drain(200 * time.Millisecond); len(got) != 1 || !bytes.Equal(got[0], a.frame(1000, "to-b")) {
		t.Fatalf("b got %x", got)
	}
	if got := c.drain(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("left participant got %d frames", len(got))
	}
	c.ok(proto.OpHuddleLeave, struct{}{}, nil) // not in a huddle: no-op
}

func TestHuddleDisconnectAndRateLimit(t *testing.T) {
	ts := startServer(t, "quic")
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	g := a.gen
	a.join(g)
	b.join(g)
	a.roster(g)
	a.roster(g)

	// A flood is cut to the bucket (burst 100 + refill) at the server.
	relayed := ts.hud.relayed.Load()
	for i := range 400 {
		a.sendDg(a.frame(uint32(i), "flood"))
	}
	b.drain(300 * time.Millisecond)
	if n := ts.hud.relayed.Load() - relayed; n > 150 || n < 50 {
		t.Fatalf("relayed %d of a 400-frame flood", n)
	}

	b.rosterIs(g, a.user.ID, b.user.ID)
	a.conn.Close()
	b.rosterIs(g, b.user.ID)
	b.ok(proto.OpHuddleLeave, struct{}{}, nil)
	b.rosterIs(g)
	ts.hud.mu.RLock()
	defer ts.hud.mu.RUnlock()
	if len(ts.hud.byChan) != 0 || len(ts.hud.bySess) != 0 {
		t.Fatal("huddle state leaked")
	}
}

func TestHuddleMembership(t *testing.T) {
	ts := startServer(t, "quic")
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	var cr proto.ChanRes
	a.ok(proto.OpChanCreate, proto.ChanCreateArgs{Name: "ops"}, &cr)
	ch := cr.Chan.ID

	b.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch}, proto.ErrForbidden)
	b.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: 999999}, proto.ErrNotFound)
	b.ok(proto.OpChanJoin, proto.ChanArgs{Chan: ch}, nil)
	a.join(ch)
	b.roster(ch)
	b.join(ch)
	a.roster(ch)
	a.roster(ch)

	// A member who joins the channel later learns of the live huddle.
	c := ts.voiceClient(t, "cy")
	c.ok(proto.OpChanJoin, proto.ChanArgs{Chan: ch}, nil)
	if got := users(c.roster(ch)); len(got) != 2 {
		t.Fatalf("late member roster %v", got)
	}
	// So does a session that syncs while it runs.
	d := ts.voiceClient(t, "di")
	d.ok(proto.OpChanJoin, proto.ChanArgs{Chan: ch}, nil)
	d.roster(ch)
	d2, bye := ts.connectCaps(t, d.signer, "", "", voiceCaps)
	if bye != "" {
		t.Fatal(bye)
	}
	d2.syncFresh()
	if got := users(d2.roster(ch)); len(got) != 2 {
		t.Fatalf("sync roster %v", got)
	}

	// Leaving the channel drops b from the huddle at once; b is told it's
	// out (an empty roster), not who is still talking.
	b.rosterIs(ch, a.user.ID, b.user.ID)
	b.ok(proto.OpChanLeave, proto.ChanArgs{Chan: ch}, nil)
	b.rosterIs(ch)
	a.rosterIs(ch, a.user.ID)
	b.sendDg(b.frame(0, "gone"))
	if got := a.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatal("ex-member's frame relayed")
	}

	// Joining another huddle moves you.
	a.join(a.gen)
	a.rosterIs(ch)
	a.rosterIs(a.gen, a.user.ID)
}

func TestHuddleCapsAndTransport(t *testing.T) {
	ts := startServer(t, "quic")
	old := ts.registerCaps(t, "old", false, nil)
	snap, _ := old.syncFresh()
	old.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: generalID(t, snap)}, proto.ErrUnknownOp)
	if !slices.Contains(old.welcome.Caps, proto.CapVoice) {
		t.Fatal("WELCOME lacks voice1")
	}
	v := ts.voiceClient(t, "v")
	v.join(v.gen)
	v.rosterIs(v.gen, v.user.ID)
	deadline := time.After(200 * time.Millisecond)
	for _, f := range old.held {
		old.frames <- f
	}
	old.held = nil
	for done := false; !done; {
		select {
		case f := <-old.frames:
			var e proto.Ephemeral
			if f.Type == proto.TEphemeral && f.Decode(&e) == nil && e.Kind == proto.EphHuddle {
				t.Fatal("huddle sent to a session without voice1")
			}
		case <-deadline:
			done = true
		}
	}

	tcp := startServer(t, "tcp")
	tv := tcp.voiceClient(t, "t")
	tv.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: tv.gen}, proto.ErrBadRequest)
}

func TestHuddleFull(t *testing.T) {
	ts := startServer(t, "quic")
	var vs []*vclient
	for i := range proto.MaxHuddleParts + 1 {
		vs = append(vs, ts.voiceClient(t, fmt.Sprintf("u%d", i)))
	}
	g := vs[0].gen
	for _, v := range vs[:proto.MaxHuddleParts] {
		v.join(g)
	}
	vs[proto.MaxHuddleParts].fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: g}, proto.ErrConflict)
	vs[0].join(g) // rejoining your own full huddle is fine
}

func TestHuddleEncrypted(t *testing.T) {
	ts := startServer(t, "quic")
	a, b := ts.voiceClient(t, "ana"), ts.voiceClient(t, "bo")
	ch := a.createE2E("sec").ID
	a.ok(proto.OpChanAdd, proto.ChanAddArgs{Chan: ch, User: b.user.ID}, nil)
	da, _ := a.registerDevice(ts)
	db, _ := b.registerDevice(ts)
	stream := make([]byte, 16)
	rand.Read(stream)

	a.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch}, proto.ErrBadRequest)
	a.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch, Dev: db.ID[:], Stream: stream}, proto.ErrForbidden)
	var r proto.HuddleJoinRes
	a.ok(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch, Dev: da.ID[:], Stream: stream}, &r)
	a.src = r.Src
	parts := b.roster(ch)
	if len(parts) != 1 || !bytes.Equal(parts[0].Dev, da.ID[:]) || !bytes.Equal(parts[0].Stream, stream) {
		t.Fatalf("roster %+v", parts)
	}
	b.ok(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: ch, Dev: db.ID[:], Stream: stream}, &r)
	b.src = r.Src

	a.sendDg(a.frame(0, "plaintext"))
	enc := proto.Datagram{Kind: proto.DgAudioEnc, Src: a.src, Epoch: 1, TS: 1, Payload: make([]byte, 40)}.Marshal()
	a.sendDg(enc)
	got := b.drain(300 * time.Millisecond)
	if len(got) != 1 || !bytes.Equal(got[0], enc) {
		t.Fatalf("encrypted huddle relayed %x", got)
	}

	// Revoking a device ends its participation at once.
	a.ok(proto.OpDeviceRevoke, proto.DeviceRevokeArgs{Revoke: revokeBySSH(t, a.signer, ts.sid(), a.user.ID, da.ID)}, nil)
	b.rosterIs(ch, b.user.ID)
	a.sendDg(proto.Datagram{Kind: proto.DgAudioEnc, Src: a.src, Seq: 1, Epoch: 1, TS: 1, Payload: make([]byte, 40)}.Marshal())
	if got := b.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatal("revoked device's frame relayed")
	}

	// huddle.leave is never rate limited (a refused cleanup leaves a ghost).
	for range 80 {
		b.ok(proto.OpHuddleLeave, struct{}{}, nil)
	}

	// Public channel: dev/stream are dropped, never relayed.
	var pr proto.HuddleJoinRes
	b.ok(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: b.gen, Dev: db.ID[:], Stream: stream}, &pr)
	for _, p := range b.roster(b.gen) {
		if p.Dev != nil || p.Stream != nil {
			t.Fatalf("public roster leaks device: %+v", p)
		}
	}
}
