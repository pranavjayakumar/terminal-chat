package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// QA v0.6.4: tc send from a computer that just signed in (tc login) into an
// encrypted channel failed with "1 message(s) not sent: no key yet" while
// the key.share was on its way, but left the message queued: the next tc
// (or tc send) delivered it anyway, late and out of order, after the user
// had been told it wasn't sent and had likely sent it again. tc send now
// waits for the key, and a message it reports as not sent is never sent.
func TestSendFromNewDeviceIntoEncryptedChannel(t *testing.T) {
	srv := startTestServer(t)
	aliceHome := loginHome(t, srv, "alice")
	runAlice := func() (*client.Client, func()) {
		c, err := client.Open(aliceHome)
		if err != nil {
			t.Fatal(err)
		}
		rctx, rcancel := context.WithCancel(context.Background())
		stopped := make(chan struct{})
		go func() { c.Run(rctx); close(stopped) }()
		until(t, "alice online", func() bool { return c.Status().State == client.Online })
		return c, func() { rcancel(); <-stopped; c.Close() }
	}
	alice, stop := runAlice()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.Send(ch.ID, "first", 0); err != nil {
		t.Fatal(err)
	}
	if err := alice.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := alice.Config()
	stop() // nobody is online to share the key with the new computer

	// A second computer with the same SSH key (tc login).
	home2 := t.TempDir()
	if _, err := client.Login(ctx, home2, "tc://"+cfg.Addr+"#"+cfg.Pin, "", filepath.Join(aliceHome, "id_ed25519")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TC_HOME", home2)
	sctx, scancel := context.WithTimeout(ctx, 25*time.Second)
	sendErr := postTo(sctx, "#secret", "from the new computer")
	scancel()
	if sendErr == nil {
		t.Fatal("sent without anyone to share the key")
	}

	alice, stop = runAlice()
	defer stop()
	// The next tc send gets the key (alice shares it) and must deliver only
	// its own message: the earlier one was reported as not sent.
	if err := postTo(ctx, "#secret", "second run"); err != nil {
		t.Fatalf("second tc send: %v", err)
	}
	count := func(body string) int {
		n := 0
		for _, m := range alice.Messages(ch.ID, 50) {
			if m.Body == body && m.ID != 0 {
				n++
			}
		}
		return n
	}
	until(t, "the second run's message", func() bool { return count("second run") == 1 })
	// A later full client on the new computer doesn't send it either.
	c2, err := client.Open(home2)
	if err != nil {
		t.Fatal(err)
	}
	c2ctx, c2cancel := context.WithCancel(context.Background())
	c2done := make(chan struct{})
	go func() { c2.Run(c2ctx); close(c2done) }()
	until(t, "new computer online", func() bool { return c2.Status().State == client.Online })
	time.Sleep(2 * time.Second)
	c2cancel()
	<-c2done
	c2.Close()
	if got := count("from the new computer"); got != 0 {
		t.Fatalf("tc send said %q, yet the message arrived later (%d copies)", sendErr, got)
	}
}
