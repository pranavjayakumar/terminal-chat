package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

func collectStarts(u *user) chan client.HuddleStart {
	ch := make(chan client.HuddleStart, 16)
	go func() {
		for up := range u.Updates() {
			for _, s := range up.HuddleStarted {
				ch <- s
			}
		}
	}()
	return ch
}

// A huddle started live is reported once, to others, never to the starter;
// a session that syncs while it runs takes it as a baseline (no ring).
func TestHuddleStartedLiveOnly(t *testing.T) {
	s, addr, us := huddlersSrv(t, "quic", "ana", "bo")
	ana, bo := us[0], us[1]
	gen := ana.channel("general")
	anaStarts, boStarts := collectStarts(ana), collectStarts(bo)
	time.Sleep(2500 * time.Millisecond) // past the post-sync baseline
	ana.SetVoicePlatform((&fakeAudio{}).platform)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := ana.JoinHuddle(ctx, gen); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-boStarts:
		if s.Chan != gen || s.By != ana.Me().ID {
			t.Fatalf("start %+v", s)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("bo never saw the huddle start")
	}
	select {
	case s := <-anaStarts:
		t.Fatalf("the starter was told: %+v", s)
	case <-time.After(300 * time.Millisecond):
	}

	// cy logs in while it runs: she sees the roster, not a start.
	cy := loginQUIC(t, s, addr, "cy", false)
	cyStarts := collectStarts(cy)
	eventually(t, "cy sees the roster", func() bool { return len(cy.Huddle(gen)) == 1 })
	select {
	case st := <-cyStarts:
		t.Fatalf("a synced roster rang: %+v", st)
	case <-time.After(2500 * time.Millisecond):
	}
	// A second joiner isn't a new start.
	bo.SetVoicePlatform((&fakeAudio{}).platform)
	if err := bo.JoinHuddle(ctx, gen); err != nil {
		t.Fatal(err)
	}
	eventually(t, "two in", func() bool { return len(cy.Huddle(gen)) == 2 })
	select {
	case st := <-cyStarts:
		t.Fatalf("a join rang as a start: %+v", st)
	case <-time.After(500 * time.Millisecond):
	}
}
