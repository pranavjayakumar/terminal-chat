package e2e

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/voice"
)

type countDuplex struct{ open *atomic.Int32 }

func (d countDuplex) Close() error { d.open.Add(-1); return nil }

// Two JoinHuddle calls in flight at once (a double-pressed /huddle, the TUI
// joining while a previous join is still opening the sound card) both pass
// LeaveHuddle while cur is nil, both get a src from the server (the second
// silently ends the first server-side), and both then assign c.hud.cur. The
// loser's voice.Session is never closed: its audio device (microphone!)
// stays open with goroutines reading datagrams, and LeaveHuddle can't reach
// it any more.
func TestHuddle_BreakConcurrentJoinsLeakDevice(t *testing.T) {
	us := huddlers(t, "quic", "ana")
	ana := us[0]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ops, err := ana.CreateChannel(ctx, "ops", false, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "ops cached", func() bool { _, ok := ana.Channel(ops.ID); return ok })

	var open, entered atomic.Int32
	both := make(chan struct{})
	ana.SetVoicePlatform(func() (voice.Platform, error) {
		return voice.Platform{Codec: fCodec{}, Open: func(func(out, in []int16)) (voice.Duplex, error) {
			if entered.Add(1) == 2 {
				close(both)
			}
			select { // opening a real device takes a while
			case <-both:
			case <-time.After(3 * time.Second):
			}
			open.Add(1)
			return countDuplex{&open}, nil
		}}, nil
	})
	var wg sync.WaitGroup
	var oks atomic.Int32
	for _, ch := range []uint64{ana.channel("general"), ops.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The loser of the race gives up ("superseded" or "dropped").
			if ana.JoinHuddle(ctx, ch) == nil {
				oks.Add(1)
			}
		}()
	}
	wg.Wait()
	if oks.Load() > 1 {
		t.Fatalf("%d concurrent joins both succeeded", oks.Load())
	}
	ana.LeaveHuddle()
	time.Sleep(200 * time.Millisecond)
	if n := open.Load(); n != 0 {
		t.Fatalf("%d audio device(s) still open after LeaveHuddle (Opens: %d)", n, entered.Load())
	}
}

// A roster that drops us can arrive after huddle.join's RES but before
// JoinHuddle installs c.hud.cur (voice.Start opens the sound card in
// between). huddleEphemeral sees cur == nil and only stores the roster;
// JoinHuddle then installs the participation and applies that roster
// without checking it still lists our src. The client believes it is in a
// huddle the server has already ended for it: InHuddle stays true, the mic
// keeps capturing and every frame is dropped at the relay, with no later
// roster to correct it.
func TestHuddle_BreakDroppedWhileOpeningDevice(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ops, err := ana.CreateChannel(ctx, "ops", false, "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bo sees ops", func() bool { _, ok := bo.Channel(ops.ID); return ok })
	if err := bo.Join(ctx, ops.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bo member", func() bool { c, ok := bo.Channel(ops.ID); return ok && c.Member })

	bo.SetVoicePlatform(func() (voice.Platform, error) {
		return voice.Platform{Codec: fCodec{}, Open: func(func(out, in []int16)) (voice.Duplex, error) {
			// While the device opens, bo leaves the channel (from another
			// device, or the TUI racing): the server ends the participation.
			eventually(t, "bo in roster", func() bool { return len(bo.Huddle(ops.ID)) == 1 })
			if err := bo.Leave(ctx, ops.ID); err != nil {
				t.Error(err)
			}
			eventually(t, "roster dropped", func() bool { return len(bo.Huddle(ops.ID)) == 0 })
			return nopDuplex{}, nil
		}}, nil
	})
	if err := bo.JoinHuddle(ctx, ops.ID); err == nil {
		t.Fatal("join succeeded although the server dropped us meanwhile")
	}
	if ch, ok := bo.InHuddle(); ok {
		t.Fatalf("client still in huddle %d after the server dropped it (removed from channel)", ch)
	}
}
