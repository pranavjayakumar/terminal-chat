//go:build tctamper

package e2e

import (
	"slices"
	"testing"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// TestIncarnationOldServer: against a server without cap incarnation the
// client neither sends nor keeps one, and syncs and reconnects as before.
func TestIncarnationOldServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	s.SetTamper(&server.Tamper{Welcome: func(_ uint64, w *proto.Welcome) {
		w.Caps = slices.DeleteFunc(w.Caps, func(c string) bool { return c == proto.CapIncarnation })
	}})
	alice := login(t, s, addr, "alice", true)
	bob := login(t, s, addr, "bob", false)
	eventually(t, "online", func() bool { return alice.online() && bob.online() })
	gen := alice.channel("general")
	ctx := ctxRestore(t)
	alice.send(t, ctx, gen, "one")
	eventually(t, "bob has one", func() bool { return bob.has(gen, "one") })
	bob.offline(t)
	alice.send(t, ctx, gen, "two")
	bob.online2(t)
	eventually(t, "bob replays two", func() bool { return bob.has(gen, "two") && bob.has(gen, "one") })
	if v := cacheMeta(t, bob.home, "server_inc"); len(v) != 0 {
		t.Fatalf("client stored an incarnation from a server without the cap: %x", v)
	}
}
