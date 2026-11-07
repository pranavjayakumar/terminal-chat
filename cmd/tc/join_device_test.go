package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// dmBodies runs the account in home once, long enough to read its DM with
// peer, and returns that DM's messages: body, or "<waiting>" for one still
// waiting for its key. wantOpen waits (bounded) until that many are open.
func dmBodies(t *testing.T, ctx context.Context, home, peer string, wantOpen int) []string {
	t.Helper()
	c, err := connectedAt(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ch, err := resolve(ctx, c, "@"+peer)
	if err != nil {
		t.Fatal(err)
	}
	read := func() (out []string, open int) {
		for _, m := range c.Messages(ch, 20) {
			if m.ID == 0 {
				continue
			}
			if m.Sec == client.SecWaiting {
				out = append(out, "<waiting>")
			} else {
				out = append(out, m.Body)
				open++
			}
		}
		return out, open
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, open := read()
		if open >= wantOpen && len(out) >= wantOpen || time.Now().After(deadline) {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// #197: tc join registers this computer's encryption device, so a DM
// written to a user who joined but never ran tc gets a box for them, and
// they read it later without the sender online.
func TestJoinRegistersDevice(t *testing.T) {
	srv := startTestServer(t)
	naHome := loginHome(t, srv, "na")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	code, err := srv.CreateInvite(ctx, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	env, out := realJoinEnv(t)
	env.register = registerDevice
	writeKey(t, filepath.Join(env.sshDir, "id_ed25519"))
	if err := runJoin(ctx, env, code, "nb", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if s := out.String(); strings.Contains(s, "isn't set up for encrypted") || !strings.Contains(s, "end-to-end encryption") {
		t.Fatalf("join output:\n%s", s)
	}

	// na sends from the CLI and exits: nobody is online to share a key.
	t.Setenv("TC_HOME", naHome)
	if err := postTo(ctx, "@nb", "hi before your first tc"); err != nil {
		t.Fatal(err)
	}
	if got := dmBodies(t, ctx, env.home, "na", 1); len(got) != 1 || got[0] != "hi before your first tc" {
		t.Fatalf("nb reads %q", got)
	}

	// Joining (or logging in) again is idempotent: the same device, no new
	// registration, and the DM still opens.
	if err := runJoin(ctx, env, code, "nb", ""); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out.String(), "isn't set up for encrypted") {
		t.Fatalf("second join:\n%s", out)
	}
	if n := activeDevices(t, ctx, env.home); n != 1 {
		t.Fatalf("nb has %d devices after joining twice", n)
	}
}

// #197 (b): a user who got no box (their device came after the epoch) gets
// one from the next sender's rotation, whose backlink also opens what was
// sent before. Here nb joins without registering (an older tc), na writes,
// nb's first run registers but can't read (na is gone), na writes again.
func TestNextSenderBoxesNewDevice(t *testing.T) {
	srv := startTestServer(t)
	naHome := loginHome(t, srv, "na")
	nbHome := loginHome(t, srv, "nb")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	t.Setenv("TC_HOME", naHome)
	if err := postTo(ctx, "@nb", "first"); err != nil {
		t.Fatal(err)
	}
	if got := dmBodies(t, ctx, nbHome, "na", 0); len(got) != 1 || got[0] != "<waiting>" {
		t.Fatalf("before na writes again nb reads %q, want the first waiting", got)
	}
	if err := postTo(ctx, "@nb", "second"); err != nil {
		t.Fatal(err)
	}
	got := dmBodies(t, ctx, nbHome, "na", 2)
	if len(got) != 2 || got[0] != "first" || got[1] != "second" {
		t.Fatalf("nb reads %q", got)
	}
	// One rotation, not one per send.
	if err := postTo(ctx, "@nb", "third"); err != nil {
		t.Fatal(err)
	}
	na, err := client.Open(naHome)
	if err != nil {
		t.Fatal(err)
	}
	defer na.Close()
	for _, cv := range na.Channels() {
		if cv.Kind == proto.ChanDM && cv.E2E && cv.Epoch != 2 {
			t.Fatalf("DM at epoch %d, want 2", cv.Epoch)
		}
	}
}

// activeDevices counts the active devices of the account in home, as its
// client sees them after a run.
func activeDevices(t *testing.T, ctx context.Context, home string) int {
	t.Helper()
	c, err := connectedAt(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.WaitDevice(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.RefreshDevices(ctx, c.Me().ID); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range c.Devices(c.Me().ID) {
		if !d.Revoked {
			n++
		}
	}
	return n
}
