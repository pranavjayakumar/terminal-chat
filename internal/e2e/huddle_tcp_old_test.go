//go:build tctamper

package e2e

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
)

// TestHuddleOverTCPOldServer: a server without voice_tcp (older than
// §14.4) still gets the clear "needs UDP" error from a client on TCP.
func TestHuddleOverTCPOldServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	s.SetTamper(&server.Tamper{Welcome: func(_ uint64, w *proto.Welcome) {
		w.Caps = slices.DeleteFunc(w.Caps, func(c string) bool { return c == proto.CapVoiceTCP })
	}})
	ana := loginVia(t, s, addr, "tcp", "ana", true)
	ana.SetVoicePlatform((&fakeAudio{}).platform)
	err := ana.JoinHuddle(context.Background(), ana.channel("general"))
	if !errors.Is(err, client.ErrNoUDP) {
		t.Fatalf("join over TCP with an old server: %v", err)
	}
}
