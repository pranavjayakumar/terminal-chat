package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// QA v0.6.4: tc tail printed only what the local cache held. A member added
// to a private channel gets no old messages from sync (PROTOCOL.md §6: they
// come from history), so tail on a computer that hadn't opened the channel in
// tc showed none of its recent messages.
func TestTailFetchesRecentHistory(t *testing.T) {
	srv := startTestServer(t)
	aliceHome := loginHome(t, srv, "alice")
	bobHome := loginHome(t, srv, "bob")

	run := func(home string) *client.Client {
		c, err := client.Open(home)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() { c.Run(ctx); close(stopped) }()
		t.Cleanup(func() { cancel(); <-stopped; c.Close() })
		until(t, "online", func() bool { return c.Status().State == client.Online })
		return c
	}
	alice := run(aliceHome)
	bob := run(bobHome) // synced before the channel exists: no snapshot later
	until(t, "bob's device registered", func() bool {
		sid, ok := bob.ServerID()
		if !ok {
			return false
		}
		k, err := bob.Trust().DeviceKeys(sid, bob.Me().ID)
		if err != nil {
			return false
		}
		_, ok, _ = bob.Trust().Device(sid, k.ID)
		return ok
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TC_HOME", aliceHome)
	for i := range 3 {
		if err := postTo(ctx, "#secret", fmt.Sprintf("before bob %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	var bobID uint64
	for _, u := range alice.Users() {
		if u.Name == "bob" {
			bobID = u.ID
		}
	}
	if err := alice.AddMember(ctx, ch.ID, bobID); err != nil {
		t.Fatal(err)
	}
	until(t, "bob is in #secret", func() bool {
		for _, v := range bob.Channels() {
			if v.ID == ch.ID {
				return v.Member
			}
		}
		return false
	})

	out := &syncBuf{}
	tctx, tcancel := context.WithCancel(ctx)
	tdone := make(chan error, 1)
	go func() { tdone <- tail(tctx, bob, ch.ID, out) }()
	until(t, "tail prints the messages from before bob was added", func() bool {
		s := out.String()
		return strings.Contains(s, "alice: before bob 0") && strings.Contains(s, "alice: before bob 2")
	})
	tcancel()
	if err := <-tdone; err != nil {
		t.Fatal(err)
	}
	if s := out.String(); strings.Count(s, "before bob 1") != 1 {
		t.Fatalf("tail output:\n%s", s)
	}
}
