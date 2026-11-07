package server

import (
	"bytes"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Pane sharing (PROTOCOL.md §14.5): relay over QUIC datagrams and over the
// TCP fallback's VOICE frames.

func (ts *testServer) watchClient(t *testing.T, name, tr string) *vclient {
	t.Helper()
	caps := []string{proto.CapVoice, proto.CapE2E, proto.CapWatch}
	if tr == "tcp" {
		caps = append(caps, proto.CapVoiceTCP)
	}
	c := ts.registerCaps(t, name, false, caps)
	snap, _ := c.syncFresh()
	v := &vclient{tclient: c, gen: generalID(t, snap)}
	if tr == "quic" {
		v.dg, _ = transport.Datagrams(c.conn)
	}
	return v
}

func (v *vclient) screenChunk(seq uint32, payload string) []byte {
	return proto.Datagram{Kind: proto.DgScreen, Src: v.src, Seq: seq, Payload: []byte(payload)}.Marshal()
}

func (v *vclient) sendAny(b []byte) {
	v.t.Helper()
	if v.dg != nil {
		v.sendDg(b)
		return
	}
	v.sendVoice(b)
}

func (v *vclient) recvAny(d time.Duration) [][]byte {
	if v.dg != nil {
		return v.drain(d)
	}
	return v.voiceFrames(d)
}

// partOf is user's entry in parts.
func partOf(parts []proto.Part, user uint64) proto.Part {
	for _, p := range parts {
		if p.User == user {
			return p
		}
	}
	return proto.Part{}
}

// rosterWhere waits for a roster of ch that satisfies ok.
func (c *tclient) rosterWhere(ch uint64, ok func([]proto.Part) bool) []proto.Part {
	c.t.Helper()
	for {
		if p := c.roster(ch); ok(p) {
			return p
		}
	}
}

func TestWatchRelay(t *testing.T) {
	for _, tr := range []string{"quic", "tcp"} {
		t.Run(tr, func(t *testing.T) { testWatchRelay(t, tr) })
	}
}

func testWatchRelay(t *testing.T, tr string) {
	ts := startServer(t, tr)
	a, b, c := ts.watchClient(t, "ana", tr), ts.watchClient(t, "bo", tr), ts.watchClient(t, "cy", tr)
	if !slices.Contains(a.welcome.Caps, proto.CapWatch) {
		t.Fatal("WELCOME lacks watch1")
	}
	g := a.gen
	// Not in a huddle: sharing and watching are refused (stopping isn't).
	a.fail(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, proto.ErrBadRequest)
	a.fail(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, proto.ErrBadRequest)
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{}, nil)
	for _, x := range []*vclient{a, b, c} {
		x.join(g)
	}
	a.rosterIs(g, a.user.ID, b.user.ID, c.user.ID)

	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)
	parts := b.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, a.user.ID).Live })
	if partOf(parts, b.user.ID).Live || partOf(parts, c.user.ID).Live {
		t.Fatalf("roster %+v", parts)
	}
	// One stream per huddle.
	b.fail(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, proto.ErrConflict)
	b.ok(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, nil)
	parts = a.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, b.user.ID).Watch })
	if partOf(parts, c.user.ID).Watch {
		t.Fatal("c marked watching")
	}

	const n = 40
	var sent [][]byte
	for i := range n {
		f := a.screenChunk(uint32(i), fmt.Sprintf("chunk-%03d", i))
		sent = append(sent, f)
		a.sendAny(f)
		pace(i)
	}
	got := b.recvAny(300 * time.Millisecond)
	if len(got) < n*9/10 {
		t.Fatalf("watcher got %d/%d chunks", len(got), n)
	}
	for _, f := range got {
		if !slices.ContainsFunc(sent, func(s []byte) bool { return bytes.Equal(s, f) }) {
			t.Fatalf("altered chunk %x", f)
		}
	}
	// Only watchers get chunks; never the streamer itself.
	if got := c.recvAny(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("non-watcher got %d chunks", len(got))
	}
	if got := a.recvAny(50 * time.Millisecond); len(got) != 0 {
		t.Fatalf("streamer got %d chunks back", len(got))
	}

	// Only the live participant streams; a sealed kind in a plaintext
	// huddle and a spoofed src are dropped.
	c.ok(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, nil)
	a.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, c.user.ID).Watch })
	b.sendAny(b.screenChunk(0, "not live"))
	a.sendAny(proto.Datagram{Kind: proto.DgScreenEnc, Src: a.src, Payload: make([]byte, 20)}.Marshal())
	b.sendAny(proto.Datagram{Kind: proto.DgScreen, Src: a.src, Payload: []byte("spoof")}.Marshal())
	if got := append(c.recvAny(200*time.Millisecond), b.recvAny(50*time.Millisecond)...); len(got) != 0 {
		t.Fatalf("bad chunks relayed: %q", got)
	}

	// Voice still flows to everyone, watching or not.
	a.sendAny(a.frame(1, "voice"))
	if got := c.recvAny(200 * time.Millisecond); len(got) != 1 || got[0][0] != proto.DgAudio {
		t.Fatalf("voice: %x", got)
	}
	b.recvAny(50 * time.Millisecond)

	// Stopping: nothing is relayed after, and the roster says so.
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: false}, nil)
	b.rosterWhere(g, func(p []proto.Part) bool { return !partOf(p, a.user.ID).Live })
	a.sendAny(a.screenChunk(100, "after stop"))
	if got := b.recvAny(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("chunk relayed after stop: %q", got)
	}
	// Someone else may share now; leaving the huddle ends their stream.
	b.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)
	a.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, b.user.ID).Live })
	b.ok(proto.OpHuddleLeave, struct{}{}, nil)
	parts = a.rosterIs(g, a.user.ID, c.user.ID)
	if slices.ContainsFunc(parts, func(p proto.Part) bool { return p.Live }) {
		t.Fatalf("stream outlived its participation: %+v", parts)
	}
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)
}

// A session without watch1 gets unknown_op, and its roster still parses.
func TestWatchNeedsCap(t *testing.T) {
	ts := startServer(t, "quic")
	a := ts.voiceClient(t, "ana")
	a.join(a.gen)
	a.fail(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, proto.ErrUnknownOp)
	a.fail(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, proto.ErrUnknownOp)
}

// The screen bucket is the sender's own: a flood of chunks is cut at the
// bucket and doesn't starve its voice.
func TestWatchRateLimit(t *testing.T) {
	ts := startServer(t, "quic")
	a, b := ts.watchClient(t, "ana", "quic"), ts.watchClient(t, "bo", "quic")
	g := a.gen
	a.join(g)
	b.join(g)
	a.ok(proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil)
	b.ok(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, nil)
	a.rosterWhere(g, func(p []proto.Part) bool { return partOf(p, b.user.ID).Watch })
	for i := range 400 {
		a.sendDg(a.screenChunk(uint32(i), "flood"))
	}
	a.sendDg(a.frame(1, "voice"))
	got := b.drain(300 * time.Millisecond)
	screens, voices := 0, 0
	for _, f := range got {
		if f[0] == proto.DgScreen {
			screens++
		} else {
			voices++
		}
	}
	if screens > scBurst+20 || voices != 1 {
		t.Fatalf("relayed %d chunks (burst %d), %d voice", screens, scBurst, voices)
	}
}
