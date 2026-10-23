package server

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Voice on the frame stream (PROTOCOL.md §14.4).

var tcpVoiceCaps = []string{proto.CapVoice, proto.CapVoiceTCP, proto.CapE2E}

func (ts *testServer) tcpVoiceClient(t *testing.T, name string) *vclient {
	t.Helper()
	c := ts.registerCaps(t, name, false, tcpVoiceCaps)
	snap, _ := c.syncFresh()
	return &vclient{tclient: c, gen: generalID(t, snap)}
}

func (v *vclient) sendVoice(b []byte) {
	v.t.Helper()
	if err := v.codec.WriteRaw(proto.TVoice, b); err != nil {
		v.t.Fatal(err)
	}
}

// voiceFrames collects VOICE frames until none arrive for d.
func (v *vclient) voiceFrames(d time.Duration) [][]byte {
	var out [][]byte
	for {
		select {
		case f, ok := <-v.frames:
			if !ok {
				return out
			}
			if f.Type == proto.TVoice {
				out = append(out, f.Payload)
			}
		case <-time.After(d):
			return out
		}
	}
}

func TestHuddleStreamRelay(t *testing.T) {
	ts := startServer(t, "tcp")
	if !slices.Contains(ts.tcpVoiceClient(t, "probe").welcome.Caps, proto.CapVoiceTCP) {
		t.Fatal("WELCOME lacks voice_tcp")
	}
	a, b := ts.tcpVoiceClient(t, "ana"), ts.tcpVoiceClient(t, "bo")
	out := ts.tcpVoiceClient(t, "cy") // a member, not in the huddle
	g := a.gen
	a.join(g)
	a.roster(g)
	b.join(g)
	b.rosterIs(g, a.user.ID, b.user.ID)
	a.rosterIs(g, a.user.ID, b.user.ID)

	const n = 30
	var sent [][]byte
	for i := range n {
		f := a.frame(uint32(i), fmt.Sprintf("opus-%03d", i))
		sent = append(sent, f)
		a.sendVoice(f)
		pace(i)
	}
	got := b.voiceFrames(300 * time.Millisecond)
	// Voice on the stream may drop a frame that waited too long (a loaded
	// -race run can stall the writer); what arrives is unchanged and in order.
	if len(got) < n-3 {
		t.Fatalf("bo got %d of %d frames", len(got), n)
	}
	j := 0
	for i := range got {
		for j < n && !bytes.Equal(got[i], sent[j]) {
			j++
		}
		if j == n {
			t.Fatalf("frame %d changed or out of order", i)
		}
		j++
	}
	if back := a.voiceFrames(100 * time.Millisecond); len(back) != 0 {
		t.Fatalf("sender got %d of its own frames back", len(back))
	}

	// Outsiders can't inject: not in the huddle, a forged src, a frame
	// over the size limit. None reaches anyone, and nobody outside the
	// huddle receives voice.
	forged := proto.Datagram{Kind: proto.DgAudio, Src: a.src, Seq: 99, Payload: []byte("forged")}.Marshal()
	out.sendVoice(forged)
	b.sendVoice(forged) // b is in the huddle, but a's src isn't b's
	big := proto.Datagram{Kind: proto.DgAudio, Src: b.src, Seq: 1, Payload: make([]byte, proto.MaxDatagram)}.Marshal()
	b.sendVoice(big)
	if got := a.voiceFrames(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("ana got %d injected frames", len(got))
	}
	if got := out.voiceFrames(100 * time.Millisecond); len(got) != 0 {
		t.Fatalf("a non-participant got %d frames", len(got))
	}

	// The per-session bucket (burst 100) applies on the stream too.
	for i := range 300 {
		b.sendVoice(b.frame(uint32(i), "flood"))
	}
	if got := a.voiceFrames(300 * time.Millisecond); len(got) > dgBurst+5 {
		t.Fatalf("flood: %d frames relayed, bucket burst is %d", len(got), dgBurst)
	}
}

// TestHuddleStreamVoiceNeedsCap: an older client (voice1 without
// voice_tcp) on TCP still can't join, and VOICE frames on a session that
// has datagrams (QUIC) are ignored.
func TestHuddleStreamVoiceNeedsCap(t *testing.T) {
	ts := startServer(t, "tcp")
	old := ts.voiceClient(t, "old")
	old.fail(proto.OpHuddleJoin, proto.HuddleJoinArgs{Chan: old.gen}, proto.ErrBadRequest)

	tq := startServer(t, "quic")
	a := tq.registerCaps(t, "ana", false, tcpVoiceCaps)
	snap, _ := a.syncFresh()
	va := &vclient{tclient: a, gen: generalID(t, snap)}
	va.dg, _ = transport.Datagrams(a.conn)
	vb := tq.voiceClient(t, "bo")
	va.join(va.gen)
	va.roster(va.gen)
	vb.join(vb.gen)
	vb.rosterIs(vb.gen, va.user.ID, vb.user.ID)
	va.sendVoice(va.frame(0, "on the stream"))
	if got := vb.drain(200 * time.Millisecond); len(got) != 0 {
		t.Fatalf("stream voice from a QUIC session relayed: %d", len(got))
	}
}

// ---- the stream queue's policy, on a session over an in-memory pipe ----

// pipeSession is a live session whose peer reads the pipe through r.
func pipeSession(t *testing.T) (*session, *huddles, net.Conn) {
	t.Helper()
	c, peer := net.Pipe()
	t.Cleanup(func() { c.Close(); peer.Close() })
	srv := &Server{hud: newHuddles()}
	sess := &session{srv: srv, conn: pipeConn{c}, codec: proto.NewCodec(c), wake: make(chan struct{}, 1), done: make(chan struct{}), live: true}
	t.Cleanup(sess.close)
	return sess, srv.hud, peer
}

// pipeConn is a net.Pipe end as a transport.Conn.
type pipeConn struct{ net.Conn }

func (pipeConn) Exporter() ([]byte, error)    { return nil, nil }
func (pipeConn) Kind() string                 { return "tcp" }
func (c pipeConn) CloseWithCode(string) error { return c.Close() }

// participants puts sess (as listener) and a sender in one huddle.
func participants(hs *huddles, sess *session) (from, to *participant) {
	h := &huddle{ch: 1}
	to = &participant{sess: sess, src: 1, h: h}
	from = &participant{sess: &session{}, src: 2, h: h}
	h.parts = []*participant{to, from}
	hs.bySess[to.sess], hs.bySess[from.sess] = to, from
	return from, to
}

func voiceItem(from, to *participant, i int) dgItem {
	b := binary.BigEndian.AppendUint32(nil, uint32(i))
	return dgItem{b: b, from: from, to: to}
}

func TestStreamVoiceQueueBounded(t *testing.T) {
	sess, hs, _ := pipeSession(t) // nobody reads: the writer isn't running
	from, to := participants(hs, sess)
	for i := range voiceQueueLen + 5 {
		if !sess.pushVoice(voiceItem(from, to, i)) {
			t.Fatalf("frame %d refused", i)
		}
	}
	if len(sess.vq) != voiceQueueLen {
		t.Fatalf("%d queued, bound is %d", len(sess.vq), voiceQueueLen)
	}
	if first := binary.BigEndian.Uint32(sess.vq[0].it.b); first != 5 {
		t.Fatalf("oldest kept is %d: the newest audio should win", first)
	}
	// Backed-up chat: voice isn't queued at all.
	sess.vq = nil
	sess.queued = voiceBackedUp + 1
	if sess.pushVoice(voiceItem(from, to, 0)) {
		t.Fatal("voice queued behind backed-up chat")
	}
	sess.queued = 0
	// A write stuck on a full socket: voice is dropped at once.
	sess.writing.Store(time.Now().Add(-2 * voiceStall).UnixNano())
	if sess.pushVoice(voiceItem(from, to, 0)) {
		t.Fatal("voice queued behind a stuck write")
	}
}

// slowReader reads frames off the pipe as a link of rate bytes/s would,
// recording when each arrived.
type arrived struct {
	t  proto.FrameType
	b  []byte
	at time.Time
}

func slowReader(peer net.Conn, rate int) (*sync.Mutex, *[]arrived) {
	var mu sync.Mutex
	var got []arrived
	go func() {
		codec := proto.NewCodec(struct {
			io.Reader
			io.Writer
		}{peer, io.Discard})
		codec.SetMaxFrame(proto.MaxFrame)
		for {
			f, err := codec.Read()
			if err != nil {
				return
			}
			time.Sleep(time.Duration(len(f.Payload)+5) * time.Second / time.Duration(rate))
			mu.Lock()
			got = append(got, arrived{f.Type, f.Payload, time.Now()})
			mu.Unlock()
		}
	}()
	return &mu, &got
}

// TestStreamVoiceSlowLink: 400-byte voice frames at 50/s into a 10 KB/s
// link. Chat pushed mid-way is written before queued voice and arrives
// promptly; voice that does arrive is never more than ~voiceMaxAge late;
// the rest is dropped instead of queueing without bound.
func TestStreamVoiceSlowLink(t *testing.T) {
	sess, hs, peer := pipeSession(t)
	from, to := participants(hs, sess)
	mu, got := slowReader(peer, 10000)
	go sess.writer()
	start := time.Now()
	sentAt := map[uint32]time.Time{}
	var chatAt time.Time
	for i := range 150 { // 3 s
		b := make([]byte, 400)
		binary.BigEndian.PutUint32(b, uint32(i))
		sentAt[uint32(i)] = time.Now()
		sess.pushVoice(dgItem{b: b, from: from, to: to})
		if i == 75 {
			chatAt = time.Now()
			sess.push(outFrame{t: proto.TEvent, payload: []byte("chat")})
		}
		time.Sleep(time.Until(start.Add(time.Duration(i+1) * 20 * time.Millisecond)))
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var voice int
	var worst time.Duration
	chatLat := time.Duration(-1)
	for _, a := range *got {
		switch a.t {
		case proto.TEvent:
			chatLat = a.at.Sub(chatAt)
		case proto.TVoice:
			voice++
			worst = max(worst, a.at.Sub(sentAt[binary.BigEndian.Uint32(a.b)]))
		}
	}
	t.Logf("chat %v; voice %d of 150 delivered, worst latency %v", chatLat, voice, worst)
	if chatLat < 0 || chatLat > 250*time.Millisecond {
		t.Fatalf("chat latency %v", chatLat)
	}
	if voice < 30 || voice > 100 {
		t.Fatalf("%d voice frames through a link that carries ~73", voice)
	}
	// Structural bound: voiceMaxAge in the queue + the frame ahead on the
	// link + its own transfer (~80 ms here), plus scheduling slack under
	// -race on a loaded machine. Unbounded, it'd grow past 1.5 s in 3 s.
	if worst > voiceMaxAge+250*time.Millisecond {
		t.Fatalf("voice %v late", worst)
	}
}

// TestStreamVoiceEndedParticipation: frames queued for or from a
// participation that ended are never written (§14.1).
func TestStreamVoiceEndedParticipation(t *testing.T) {
	sess, hs, peer := pipeSession(t)
	from, to := participants(hs, sess)
	mu, got := slowReader(peer, 1<<30)
	sess.pushVoice(voiceItem(from, to, 1))
	delete(hs.bySess, from.sess) // the sender left
	go sess.writer()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(*got) != 0 {
		t.Fatalf("%d frames from an ended participation written", len(*got))
	}
}

// TestStreamVoiceStuck: a write blocked past voiceStuck with the socket not
// draining closes the session as a slow consumer; a long write on a slow
// link that is still moving doesn't.
func TestStreamVoiceStuck(t *testing.T) {
	sess, _, _ := pipeSession(t)
	unsent := 300 << 10
	sess.unsent = func() (int, bool) { return unsent, true }
	sess.writing.Store(time.Now().Add(-2 * voiceStuck).UnixNano())
	for range 3 { // a big frame, slow but moving
		unsent -= 20 << 10
		sess.checkVoiceStuck(time.Now())
	}
	if sess.closed {
		t.Fatal("closed over a long write that is draining")
	}
	sess.checkVoiceStuck(time.Now()) // nothing moved since the last tick
	if !sess.closed {
		t.Fatal("a write stuck for 20 s, socket not draining, didn't close the session")
	}
}
