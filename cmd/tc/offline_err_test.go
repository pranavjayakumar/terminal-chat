package main

import (
	"strings"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// QA v0.6.4: a 33rd session (32 tc tail running) was told the server "may be
// down or blocked by a firewall", though the server answered and refused it
// for the account's session cap.
func TestOfflineErrorSessionCap(t *testing.T) {
	cap := offlineError("127.0.0.1:7443", (&client.ByeError{Code: proto.ByeTooManySessions}).Error()).Error()
	if strings.Contains(cap, "firewall") || !strings.Contains(cap, "close some") {
		t.Fatalf("session cap: %q", cap)
	}
	down := offlineError("127.0.0.1:7443", "dial tcp: connection refused").Error()
	if !strings.Contains(down, "can't reach the server at 127.0.0.1:7443") {
		t.Fatalf("unreachable: %q", down)
	}
}
