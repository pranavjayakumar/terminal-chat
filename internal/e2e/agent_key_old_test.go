//go:build tctamper

package e2e

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// TestAgentLoginOldServer: against a server without agent_key_proof (one
// that doesn't take agent.key's proof), tc agent login says to update the
// server before it changes anything.
func TestAgentLoginOldServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	s.SetTamper(&server.Tamper{Welcome: func(_ uint64, w *proto.Welcome) {
		w.Caps = slices.DeleteFunc(w.Caps, func(c string) bool { return c == proto.CapAgentKeyProof })
	}})
	alice := login(t, s, addr, "alice", true)
	eventually(t, "online", alice.online)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := alice.RegisterAgent(ctx, "rocky", ""); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(t.TempDir(), "agent")
	_, err := client.LoginAgent(ctx, alice.home, home, "rocky")
	if err == nil || !strings.Contains(err.Error(), "update the server") {
		t.Fatalf("LoginAgent against an old server: %v", err)
	}
	if m, _ := filepath.Glob(filepath.Join(home, "*")); len(m) != 0 {
		t.Fatalf("LoginAgent wrote %v before failing", m)
	}
}
