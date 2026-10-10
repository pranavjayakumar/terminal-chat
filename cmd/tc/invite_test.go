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

func TestParseServerInviteListRevoke(t *testing.T) {
	if c, err := parseServerInvite([]string{"--list"}); err != nil || !c.list {
		t.Fatalf("--list: %+v %v", c, err)
	}
	if c, err := parseServerInvite([]string{"--revoke", "0123456789AB"}); err != nil || c.revoke != "0123456789ab" {
		t.Fatalf("--revoke: %+v %v", c, err)
	}
	for _, args := range [][]string{
		{"--revoke", "xyz"}, {"--revoke", ""}, {"--revoke", "0123456789ab0"},
		{"--list", "--revoke", "0123456789ab"}, {"--list", "--uses", "5"}, {"--revoke", "0123456789ab", "--admin"},
	} {
		if _, err := parseServerInvite(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestPrintInvites(t *testing.T) {
	exp := time.Now().Add(time.Hour).UnixMilli()
	names := map[uint64]string{1: "admin", 2: "bob\x1b[2J"}
	var b bytes.Buffer
	printInvites(&b, []proto.InviteInfo{
		{ID: "0123456789ab", Kind: proto.InviteKindUser, UsesLeft: 14, Expires: exp, CreatedBy: 1},
		{ID: "ba9876543210", Kind: proto.InviteKindKey, User: 2, UsesLeft: 1, Expires: exp},
		{ID: "ffffffffffff", Kind: proto.InviteKindAdmin, UsesLeft: 1, Expires: exp, CreatedBy: 7},
	}, func(id uint64) string { return names[id] })
	out := b.String()
	for _, want := range []string{"0123456789ab", "14", "@admin", "key for @bob", "server host", "user 7", "--revoke ID"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("unsanitized name:\n%q", out)
	}
	b.Reset()
	printInvites(&b, nil, nil)
	if !strings.Contains(b.String(), "No open invites") {
		t.Fatal(b.String())
	}
}

// A code with shell or terminal metacharacters (a bad --public) is never
// printed as a command, and its escapes never reach the terminal.
func TestPrintInviteUnsafeCode(t *testing.T) {
	for _, code := range []string{
		"tc://h:7443/tok#pin'; touch /tmp/x; '",
		"tc://h\x1b]52;c;eA==\a:7443/tok#pin",
		"tc://h:7443/tok#pin$(id)",
	} {
		var b bytes.Buffer
		printInvite(&b, proto.InviteRes{Code: code, Uses: 1}, "dev")
		out := b.String()
		if strings.Contains(out, "tc join '") || strings.Contains(out, "| sh") {
			t.Errorf("%q printed as a command:\n%s", code, out)
		}
		if strings.ContainsAny(out, "\x1b\a") {
			t.Errorf("%q: raw control bytes in output %q", code, out)
		}
		if !strings.Contains(out, "unsafe characters") {
			t.Errorf("%q: no warning:\n%s", code, out)
		}
	}
}
