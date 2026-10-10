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

func TestParseServerInviteAdmin(t *testing.T) {
	c, err := parseServerInvite([]string{"--admin"})
	if err != nil || !c.opts.Admin || c.opts.Uses != 1 || c.opts.TTL != proto.MaxAdminInviteTTL {
		t.Fatalf("--admin: %+v %v", c.opts, err)
	}
	if c, err := parseServerInvite([]string{"--admin", "--ttl", "2h"}); err != nil || c.opts.TTL != 2*time.Hour {
		t.Fatalf("--admin --ttl 2h: %+v %v", c.opts, err)
	}
	for _, args := range [][]string{
		{"--admin", "--uses", "2"},
		{"--admin", "--uses", "100", "--ttl", "720h"},
		{"--admin", "--ttl", "25h"},
		{"--admin", "--ttl", "168h"},
	} {
		if _, err := parseServerInvite(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
	// Team invites keep their limits and defaults.
	c, err = parseServerInvite([]string{"--uses", "15", "--ttl", "72h"})
	if err != nil || c.opts.Admin || c.opts.Uses != 15 || c.opts.TTL != 72*time.Hour {
		t.Fatalf("team: %+v %v", c.opts, err)
	}
	if c, _ := parseServerInvite(nil); c.opts.TTL != proto.DefaultInviteTTL || c.opts.Uses != 1 {
		t.Fatalf("defaults: %+v", c.opts)
	}
}
