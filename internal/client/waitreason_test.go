package client

import (
	"slices"
	"time"
	"testing"

	"github.com/skailama/tc/internal/proto"
)

func waitOf(c *Client, id uint64) (Sec, WaitReason, bool) {
	m, ok := findMsg(c, id)
	return m.Sec, m.Wait, ok
}

// A waiting message says why (#94): no key for its epoch, an unknown
// device, a missing thread root, or a device under its author's changed
// key, which /accept-key opens at once.
func TestWaitReasons(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	msg := func(body string) plainMsg { return plainMsg{T: "msg", TS: 1, Body: body} }

	noKey := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, newK(t), 2, d.e1, msg("epoch 2"))})
	stranger := newPeer(t, bob, d.b1.ssh) // same key, device never announced
	noDev := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, stranger.dev, d.k1, 1, d.e1, msg("unknown device"))})
	noRoot := d.f.post(proto.Message{Chan: dm, Author: bob, Thread: 999, Enc: sealAs(t, d.b1.dev, d.k1, 1, d.e1,
		plainMsg{T: "msg", TS: 1, Body: "reply", ThreadRef: make([]byte, 16)})})

	// bob on a new computer: a device under a new SSH key.
	b3 := newPeer(t, bob, nil)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "key change", func() bool { return len(d.c.PendingKeys(bob)) == 1 })
	changed := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, b3.dev, d.k1, 1, d.e1, msg("from the new laptop"))})

	for _, c := range []struct {
		id   uint64
		want WaitReason
	}{{noKey, WaitKey}, {noDev, WaitDevice}, {changed, WaitKeyChange}} {
		waitFor(t, "waiting "+c.want.String(), func() bool {
			sec, w, ok := waitOf(d.c, c.id)
			return ok && sec == SecWaiting && w == c.want
		})
	}
	waitFor(t, "waiting for the root", func() bool {
		for _, m := range d.c.Thread(999) {
			if m.ID == noRoot {
				return m.Sec == SecWaiting && m.Wait == WaitRoot
			}
		}
		return false
	})

	if err := d.c.AcceptKeyChanges(bob, d.c.PendingKeys(bob), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "opens after /accept-key", func() bool {
		m, ok := findMsg(d.c, changed)
		return ok && m.Sec == SecE2E && m.Body == "from the new laptop"
	})
	if sec, w, _ := waitOf(d.c, noDev); sec != SecWaiting || w != WaitDevice {
		t.Fatalf("unknown device after accept: %v %v", sec, w)
	}
}

// A message that waited for an unknown device turns into "changed key" when
// that device's cert arrives under a changed key, and the UI is told to
// redraw its channel.
func TestWaitReasonRelabels(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	b3 := newPeer(t, bob, nil)
	id := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, b3.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "early"})})
	waitFor(t, "unknown device", func() bool { _, w, ok := waitOf(d.c, id); return ok && w == WaitDevice })
	drain(d.c)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	deadline := time.After(testWait)
	for {
		select {
		case u := <-d.c.Updates():
			if !u.Msgs[dm] {
				continue
			}
			if _, w, _ := waitOf(d.c, id); w == WaitKeyChange {
				return
			}
		case <-deadline:
			_, w, _ := waitOf(d.c, id)
			t.Fatalf("no redraw with the new reason (now %v)", w)
		}
	}
}

// My own devices under another SSH key of mine (an add-key invite) can be
// accepted too, and what they sent opens (#94).
func TestWaitOwnKeyChangeAccepted(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	other := newPeer(t, alice, nil)
	d.f.event(proto.KDeviceNew, 0, alice, proto.DeviceRegisterArgs{Cert: other.cert})
	waitFor(t, "own key change", func() bool { return len(d.c.PendingKeys(alice)) == 1 })
	if len(d.c.KeyChanges()) != 0 {
		t.Fatal("my own key change shows as someone's")
	}
	id := d.f.post(proto.Message{Chan: dm, Author: alice, Enc: sealAs(t, other.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "from my other computer"})})
	waitFor(t, "waiting on my key change", func() bool {
		sec, w, ok := waitOf(d.c, id)
		return ok && sec == SecWaiting && w == WaitKeyChange
	})
	if err := d.c.AcceptKeyChanges(alice, d.c.PendingKeys(alice), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "opens", func() bool {
		m, ok := findMsg(d.c, id)
		return ok && m.Sec == SecE2E && m.Body == "from my other computer"
	})
	if !slices.Equal(d.c.PendingKeys(alice), nil) {
		t.Fatalf("still pending: %v", d.c.PendingKeys(alice))
	}
}
