// Package e2e runs the real client against the real server over loopback.
package e2e

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

type srv struct {
	*server.Server
	stop func()
}

// startServer runs a server on addr ("127.0.0.1:0" picks a port) over dir.
func startServer(t *testing.T, dir, addr string) (*srv, string) {
	t.Helper()
	return startServerClock(t, dir, addr, nil)
}

// startServerClock is startServer with the server's clock (nil: time.Now).
func startServerClock(t *testing.T, dir, addr string, now func() time.Time) (*srv, string) {
	t.Helper()
	return startServerConfig(t, addr, server.Config{DataDir: dir, Now: now})
}

// startServerConfig is startServer with cfg (its Logger is replaced).
func startServerConfig(t *testing.T, addr string, cfg server.Config) (*srv, string) {
	t.Helper()
	cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ln := listenRetry(t, addr, s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, ln); close(done) }()
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-done
			s.Close()
		}
	}
	t.Cleanup(stop)
	return &srv{s, stop}, ln.Addr().String()
}

// listenRetry listens on addr for s. Restarting on a stopped server's port
// may find the UDP socket still bound: quic-go closes it only once the old
// connections' closed-state handlers expire (3 PTO), after Close returns.
// So a fixed port is retried for a while; port 0 never needs it.
func listenRetry(t *testing.T, addr string, s *server.Server) transport.Listener {
	t.Helper()
	_, port, _ := net.SplitHostPort(addr)
	deadline := time.Now().Add(10 * time.Second)
	for {
		ln, err := transport.ListenWith(addr, s.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: s.HTTPCert()})
		if err == nil {
			return ln
		}
		if port == "0" || !errors.Is(err, syscall.EADDRINUSE) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeKey(t *testing.T, dir string) string {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

type user struct {
	*client.Client
	cancel context.CancelFunc
	home   string
}

func login(t *testing.T, s *srv, addr, name string, admin bool) *user {
	t.Helper()
	s.Server.SetPublicAddr(addr)
	code, err := s.CreateInvite(context.Background(), admin, 0)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := client.Login(ctx, home, code, name, writeKey(t, home)); err != nil {
		t.Fatalf("login %s: %v", name, err)
	}
	c, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	rctx, rcancel := context.WithCancel(context.Background())
	go c.Run(rctx)
	u := &user{Client: c, cancel: rcancel, home: home}
	t.Cleanup(func() { rcancel(); c.Close() })
	return u
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (u *user) online() bool { return u.Status().State == client.Online }

func (u *user) channel(name string) uint64 {
	for _, c := range u.Channels() {
		if c.Name == name {
			return c.ID
		}
	}
	return 0
}

func (u *user) has(ch uint64, body string) bool {
	for _, m := range u.Messages(ch, 200) {
		if m.Body == body && m.ID != 0 {
			return true
		}
	}
	return false
}

func TestEndToEnd(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	for _, kind := range []string{"tcp", "quic"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("TC_TRANSPORT", kind)
			dir := t.TempDir()
			s, addr := startServer(t, dir, "127.0.0.1:0")
			alice := login(t, s, addr, "alice", true)
			bob := login(t, s, addr, "bob", false)
			eventually(t, "both online", func() bool { return alice.online() && bob.online() })
			if alice.Status().Transport != kind {
				t.Fatalf("transport %q, want %q", alice.Status().Transport, kind)
			}
			if !alice.Me().Admin || bob.Me().Admin {
				t.Fatal("admin flags wrong")
			}
			gen := alice.channel("general")
			eventually(t, "bob sees #general", func() bool { return bob.channel("general") == gen && gen != 0 })

			// Live message + unread on the other side.
			if err := alice.Send(gen, "hello @bob", 0); err != nil {
				t.Fatal(err)
			}
			eventually(t, "bob receives", func() bool { return bob.has(gen, "hello @bob") })
			cv, _ := bob.Channel(gen)
			if cv.Unread != 1 || !cv.Mention {
				t.Fatalf("bob unread=%d mention=%v", cv.Unread, cv.Mention)
			}
			msgs := bob.Messages(gen, 10)
			bob.MarkRead(gen, msgs[len(msgs)-1].ID)
			if cv, _ := bob.Channel(gen); cv.Unread != 0 {
				t.Fatalf("still unread after MarkRead: %d", cv.Unread)
			}

			// Private channel round trip.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sec, err := alice.CreateChannel(ctx, "secret", true, "shh")
			if err != nil {
				t.Fatal(err)
			}
			alice.Send(sec.ID, "before bob", 0)
			if err := alice.Flush(ctx); err != nil { // stored before bob joins
				t.Fatal(err)
			}
			if err := alice.AddMember(ctx, sec.ID, bob.Me().ID); err != nil {
				t.Fatal(err)
			}
			eventually(t, "bob sees #secret", func() bool { return bob.channel("secret") == sec.ID })
			if _, err := bob.History(ctx, sec.ID, 0); err != nil {
				t.Fatal(err)
			}
			if !bob.has(sec.ID, "before bob") {
				t.Fatal("history didn't load backlog")
			}

			// Reaction + edit propagate.
			var mid uint64
			for _, m := range alice.Messages(gen, 10) {
				if m.Body == "hello @bob" {
					mid = m.ID
				}
			}
			if err := bob.React(ctx, mid, "👍"); err != nil {
				t.Fatal(err)
			}
			if err := alice.Edit(ctx, mid, "hello again"); err != nil {
				t.Fatal(err)
			}
			eventually(t, "edit+react propagate", func() bool {
				for _, m := range bob.Messages(gen, 10) {
					if m.ID == mid {
						return m.Body == "hello again" && len(m.Reactions["👍"]) == 1
					}
				}
				return false
			})
		})
	}
}

// The server goes away; a message sent while offline is delivered exactly
// once after it comes back on the same address.
func TestOfflineOutboxSurvivesServerRestart(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	gen := alice.channel("general")

	s.stop()
	eventually(t, "alice offline", func() bool { return !alice.online() })
	if err := alice.Send(gen, "sent while offline", 0); err != nil {
		t.Fatal(err)
	}
	pending := alice.Messages(gen, 10)
	if len(pending) == 0 || !pending[len(pending)-1].Pending {
		t.Fatal("offline send not shown as pending")
	}

	startServer(t, dir, addr)
	eventually(t, "bob receives after restart", func() bool { return bob.has(gen, "sent while offline") })
	eventually(t, "alice outbox drained", func() bool {
		for _, m := range alice.Messages(gen, 10) {
			if m.Pending {
				return false
			}
		}
		return true
	})
	n := 0
	for _, m := range bob.Messages(gen, 50) {
		if m.Body == "sent while offline" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message delivered %d times", n)
	}
}

func TestRevokedClientStops(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "both online", func() bool { return alice.online() && bob.online() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := alice.DisableUser(ctx, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob stopped", func() bool { return bob.Status().Stopped })
	if bob.Status().Err == "" {
		t.Fatal("no error shown to revoked user")
	}
	_ = proto.ByeRevoked
}

// Two processes share one cache (TUI + `tc send`): the one that didn't
// apply an event must still surface it.
func TestSharedCacheSecondProcessSeesEvents(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	alice := login(t, s, addr, "alice", true)
	eventually(t, "online", alice.online)
	gen := alice.channel("general")

	// A second client process on the same home.
	home := filepath.Dir(alice.Config().KeyPath)
	if alice.Config().KeyPath == "" {
		t.Skip("key path not recorded in config")
	}
	other, err := client.Open(home)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go other.Run(ctx)
	t.Cleanup(func() { cancel(); other.Close() })
	eventually(t, "other online", func() bool { return other.Status().State == client.Online })

	// Drain anything already pending on alice's Updates.
	drain := func() {
		for {
			select {
			case <-alice.Updates():
			default:
				return
			}
		}
	}
	drain()
	other.Send(gen, "from the other process", 0)
	deadline := time.After(10 * time.Second)
	for {
		select {
		case u := <-alice.Updates():
			for _, m := range u.New {
				if m.Body == "from the other process" {
					return
				}
			}
		case <-deadline:
			t.Fatal("first process never surfaced the message applied by the second")
		}
	}
}
