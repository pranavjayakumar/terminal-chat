package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func TestPrintInvite(t *testing.T) {
	code := proto.FormatInvite("203.0.113.7:7443", "abcdefghijklmnopqrstuvwxyz", "Pin-_x")
	var b bytes.Buffer
	printInvite(&b, proto.InviteRes{Code: code, Uses: 15, Expires: time.Now().Add(72 * time.Hour).UnixMilli()}, "v0.2.0")
	out := b.String()
	for _, want := range []string{
		"15 uses",
		"curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/v0.2.0/install.sh | sh -s -- '" + code + "'",
		"tc join '" + code + "'",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	b.Reset()
	printInvite(&b, proto.InviteRes{Code: code, Uses: 1}, "dev")
	if !strings.Contains(b.String(), "single use") {
		t.Fatalf("single use: %s", b.String())
	}
}
