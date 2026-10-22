package client

import (
	"crypto/rand"
	"testing"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// tc background is a second Client on the chat window's TC_HOME (shared
// cache.db). Encrypted DMs that arrive before their key are cached as
// waiting; when the key arrives, whichever process re-judges them first
// flips the shared rows to e2e, and only that process reports them in
// Update.New (reopenMsgs: "UPDATE ... WHERE sec = waiting", n == 0 ->
// skip). If the background wins while the chat window is open, the
// background stays silent (UI lock) and the chat window never learns the
// messages are new: the DM never notifies anywhere.
//
// Known limitation, mitigated rather than fixed: tc background now drops
// its connection within a second of a chat window opening (run.go
// waitUIClosed), so the two processes only overlap for that second. The
// client-level race this test shows remains for any two clients on one
// cache (tc tail beside the chat window, too).
func TestSharedCache_BreakWaitingOpenedByBackgroundNeverNotifiesUI(t *testing.T) {
	t.Skip("known limitation: two clients on one cache race to report a waiting message (see above)")
	fs, c := e2eClient(t)
	c.timing.devWait = 5 * time.Second
	// c keeps e2eClient's 200 ms worker tick: it re-judges often, and still loses
	run(t, c)
	d := &encDM{fs: fs, c: c, b1: newPeer(t, bob, nil), k1: newK(t)}
	sn := proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: d.b1.cert}}}
	d.f = startE2E(t, fs, sn, proto.DevicesListRes{})
	d.me = d.f.myKeys(c)
	d.e1 = mkBundle(t, d.b1.dev, dm, 1, nil, dmRoster, d.k1, nil, d.me, d.b1.dev)
	d.seq1 = d.f.keyEpoch(dm, bob, d.e1)
	waitFor(t, "epoch 1", status(c, dm, E2EOK))
	f := d.f

	k2 := newK(t)
	e2 := mkBundle(t, d.b1.dev, dm, 2, &d.e1, dmRoster, k2, &d.k1, d.b1.dev) // no box for us
	f.keyEpoch(dm, bob, e2)
	waitFor(t, "waiting for epoch 2", func() bool { st := c.E2EStatus(dm); return st.State == E2EWaitingKey && st.Epoch == 2 })
	id := f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, k2, 2, e2, plainMsg{T: "msg", TS: 1, Body: "are you there?"})})
	waitFor(t, "applied", func() bool { return lastSeq(t, c) == id })
	if m, _ := findMsg(c, id); m.Sec != SecWaiting {
		t.Fatalf("before the key: %+v", m)
	}
	drain(c)

	// tc background starts on the same home and connects.
	bg := open(t, c.home)
	bg.SetPassive(true)
	bg.timing.keyNeed = time.Hour
	run(t, bg)
	fb := resumeE2E(t, fs, f, nil)
	waitFor(t, "background online", func() bool { return bg.Status().State == Online })
	f.mu.Lock()
	fb.mu.Lock()
	seq := max(f.seq, fb.seq)
	f.seq, fb.seq = seq, seq
	fb.mu.Unlock()
	f.mu.Unlock()

	// The key share reaches the background first and it decrypts the DM.
	eph, ct, err := e2ecrypto.SealShare(rand.Reader, d.me.X25519Pub, e2ecrypto.ShareAD{ServerID: testSID(), Chan: dm, Epoch: 2, Recipient: d.me.ID}, k2)
	if err != nil {
		t.Fatal(err)
	}
	share := proto.KeyShareArgs{Chan: dm, Epoch: 2, Dev: d.me.ID[:], Eph: eph[:], CT: ct[:]}
	if s := fb.event(proto.KKeyShare, dm, bob, share); s != seq+1 {
		t.Fatalf("seq %d, want %d", s, seq+1)
	}
	waitFor(t, "background decrypted", func() bool { m, _ := findMsg(bg, id); return m.Sec == SecE2E })

	// The same event reaches the chat window a moment later.
	if s := f.event(proto.KKeyShare, dm, bob, share); s != seq+1 {
		t.Fatalf("seq %d, want %d", s, seq+1)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case u := <-c.Updates():
			for _, m := range u.New {
				if m.ID == id {
					return
				}
			}
		case <-deadline:
			m, _ := findMsg(c, id)
			t.Fatalf("chat window never got Update.New for the DM the background opened (cache: sec %v body %q)", m.Sec, m.Body)
		}
	}
}
