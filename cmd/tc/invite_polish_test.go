package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func TestParseInviteCmdErrors(t *testing.T) {
	if c, err := parseInviteCmd([]string{"--ttl", "3d"}); err != nil || c.ttl != 72*time.Hour {
		t.Fatalf("3d: %+v %v", c, err)
	}
	for args, want := range map[string]string{
		"--ttl 45d":   "days must be a whole number 1-30",
		"--ttl bogus": "ttl",
		"--admin":     "-admin",
		"extra":       `unexpected argument "extra"`,
	} {
		_, err := parseInviteCmd(strings.Fields(args))
		if err == nil || errors.Is(err, errUsage) || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want reason %q", args, err, want)
		}
	}
	if _, err := parseInviteCmd([]string{"-h"}); !errors.Is(err, errUsage) {
		t.Errorf("-h: %v", err)
	}
}

func TestInviteErrFriendly(t *testing.T) {
	if got := inviteErr(&proto.Error{Code: proto.ErrForbidden}, "invite people", "").Error(); !strings.Contains(got, "only admins can invite people") {
		t.Fatal(got)
	}
	if got := inviteErr(&proto.Error{Code: proto.ErrNotFound}, "revoke invites", "abc").Error(); !strings.Contains(got, "no open invite abc of yours") {
		t.Fatal(got)
	}
}

func TestPrintInvitesAligned(t *testing.T) {
	var b bytes.Buffer
	exp := time.Now().Add(time.Hour).UnixMilli()
	printInvites(&b, []proto.InviteInfo{
		{ID: "aaaaaaaaaaaa", Kind: proto.InviteKindUser, UsesLeft: 1, Expires: exp},
		{ID: "bbbbbbbbbbbb", Kind: proto.InviteKindUser, UsesLeft: 10, Expires: exp, Keys: 1, KeySrc: "github:torvalds"},
	}, nil, "x")
	lines := strings.Split(b.String(), "\n")
	end := strings.Index(lines[0], "USES LEFT") + len("USES LEFT")
	for _, l := range lines[1:3] {
		if l[end-1] < '0' || l[end-1] > '9' || l[end] != ' ' {
			t.Errorf("USES column misaligned: %q (header %q)", l, lines[0])
		}
	}
}
