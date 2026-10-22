package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/background"
	"github.com/skailama/tc/internal/client"
)

// tc background is on for bob's home (team A). Bob joins team B into the
// same home (`tc join <invite>`: client.Login wipes cache.db and rewrites
// config.json). The running notifier only watches config.json while it is
// *not* connected (waitConfig), so it stays connected to team A as bob and
// keeps applying team A's events into the cache that now belongs to team B:
// team A's messages land in team B's cache (channel ids even collide:
// both #general) and last_seq becomes team A's, and it keeps popping up
// team A notifications for an account bob no longer uses here.
func TestBackground_BreakKeepsOldAccountAfterRelogin(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	sa, addrA := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, sa, addrA, "alice", true)
	bob := login(t, sa, addrA, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	genA := alice.channel("general")
	home := bob.home
	bob.cancel()
	bob.Close()

	out := &noteLog{}
	rctx, stop := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- background.Run(rctx, background.Config{Home: home, Version: "test", Out: out, Poll: 200 * time.Millisecond})
	}()
	defer func() { stop(); <-done }()
	eventually(t, "background online", func() bool {
		st, err := background.ReadStatus(home)
		return err == nil && st.State == "online"
	})

	// Bob joins team B into the same home.
	sb, addrB := startServer(t, t.TempDir(), "127.0.0.1:0")
	sb.Server.SetPublicAddr(addrB)
	code, err := sb.CreateInvite(context.Background(), true, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Login(ctx, home, code, "bob-b", writeKey(t, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second) // give the notifier its Poll ticks to notice

	// Team A keeps talking.
	before := len(out.get())
	if err := alice.Send(genA, "@bob team A only", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)

	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Me().Name != "bob-b" {
		t.Fatalf("config is %q, want bob-b", c.Me().Name)
	}
	for _, m := range c.Messages(genA, 100) {
		if m.Body == "@bob team A only" {
			t.Errorf("team A's message was written into team B's cache (chan id %d)", genA)
		}
	}
	if n := len(out.get()) - before; n > 0 {
		t.Errorf("after switching to team B, %d notification(s) for team A: %+v", n, out.get()[before:])
	}
}
