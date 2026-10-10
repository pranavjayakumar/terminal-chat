package tui

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func TestInviteArgs(t *testing.T) {
	for _, c := range []struct {
		args []string
		uses int
		ttl  time.Duration
	}{
		{nil, 0, 0},
		{[]string{"15"}, 15, 0},
		{[]string{"72h"}, 0, 72 * time.Hour},
		{[]string{"15", "3d"}, 15, 72 * time.Hour},
		{[]string{"30m", "2"}, 2, 30 * time.Minute},
		{[]string{"100", "30d"}, 100, 30 * 24 * time.Hour},
	} {
		uses, ttl, err := inviteArgs(c.args)
		if err != nil || uses != c.uses || ttl != c.ttl {
			t.Errorf("%v: %d %v %v", c.args, uses, ttl, err)
		}
	}
	for _, args := range [][]string{
		{"0"}, {"101"}, {"-1"}, {"31d"}, {"721h"}, {"10s"}, {"5", "6"}, {"1h", "2h"}, {"x"}, {"1", "1h", "1"},
	} {
		if _, _, err := inviteArgs(args); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestInviteCommandCopiesOneLiner(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.opt.Version = "v0.2.0"
	m.input.SetValue("/invite 15 3d")
	run(m, m.submit())
	if !f.called("invite 15 72h0m0s") {
		t.Fatalf("calls %v", f.calls)
	}
	code := "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x"
	line := "curl -fsSL " + proto.InstallScriptURL("v0.2.0") + " | sh -s -- '" + code + "'"
	if !strings.Contains(line, "/v0.2.0/install.sh") {
		t.Fatal(line)
	}
	out := m.opt.Out.(*syncBuf).String()
	if !strings.Contains(out, "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(line))+"\a") {
		t.Fatalf("one-liner not copied via OSC 52: %q", out)
	}
	if !strings.Contains(m.notice, "15 uses") || !strings.Contains(m.notice, "install command copied") || !strings.Contains(m.notice, code) {
		t.Fatalf("notice %q", m.notice)
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "invite notice")
	}

	// Bad args: an error notice, no request.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	f.calls = nil
	m.input.SetValue("/invite 500")
	run(m, m.submit())
	if len(f.calls) != 0 || !m.noticeErr || !strings.Contains(m.notice, "usage: /invite") {
		t.Fatalf("calls %v notice %q", f.calls, m.notice)
	}

	// A code with a quote is never copied as a command.
	f.inviteCode = "tc://h:7443/tok#pin'x"
	m.opt.Out.(*syncBuf).b.Reset()
	m.input.SetValue("/invite")
	run(m, m.submit())
	if strings.Contains(m.opt.Out.(*syncBuf).String(), "]52;") || !m.noticeErr {
		t.Fatalf("unsafe code copied; notice %q", m.notice)
	}
}

func TestUpdateNoticeInSidebar(t *testing.T) {
	f := newFake()
	f.status = client.Status{State: client.Online}
	checked := make(chan struct{}, 1)
	m := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true),
		UpdateCheck: func() string { checked <- struct{}{}; return "v0.3.0" }})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Init schedules the check; it never runs on the UI goroutine.
	msg, ok := runIfUpdate(m.Init())
	if !ok {
		t.Fatal("Init didn't produce the update check result")
	}
	m.Update(msg)
	select {
	case <-checked:
	default:
		t.Fatal("update check not run from Init")
	}
	for _, sz := range [][2]int{{100, 30}, {69, 20}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		plain := checkView(t, m, "update notice")
		if m.dims().side && !strings.Contains(strings.Split(plain, "\n")[0], "tc · update v0.3") {
			t.Fatalf("%dx%d: no update notice in sidebar title:\n%s", sz[0], sz[1], plain)
		}
	}
	// Offline status wins over the update notice.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.status = client.Status{State: client.Offline}
	if l := ansi.Strip(strings.Split(m.View(), "\n")[0]); strings.Contains(l, "update") || !strings.Contains(l, "offline") {
		t.Fatalf("offline title: %q", l)
	}

	// No newer release: plain title.
	m2 := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true), UpdateCheck: func() string { return "" }})
	m2.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m2.Update(updateAvailMsg(""))
	m2.status = client.Status{State: client.Online}
	if l := ansi.Strip(strings.Split(m2.View(), "\n")[0]); strings.Contains(l, "update") {
		t.Fatalf("title %q", l)
	}
}

// runIfUpdate runs the batch and returns the update message from it, if any.
// Blocking commands (waitUpdate, ticks) run in goroutines and are abandoned.
func runIfUpdate(c tea.Cmd) (tea.Msg, bool) {
	res := make(chan tea.Msg, 16)
	var walk func(tea.Cmd)
	walk = func(c tea.Cmd) {
		if c == nil {
			return
		}
		go func() {
			msg := c()
			if b, ok := msg.(tea.BatchMsg); ok {
				for _, x := range b {
					walk(x)
				}
				return
			}
			res <- msg
		}()
	}
	walk(c)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-res:
			if u, ok := msg.(updateAvailMsg); ok {
				return u, true
			}
		case <-deadline:
			return nil, false
		}
	}
}

type inviteFake struct {
	*fakeClient
	invites []proto.InviteInfo
	revoked []string
}

func (f *inviteFake) ListInvites(context.Context) ([]proto.InviteInfo, error) { return f.invites, nil }
func (f *inviteFake) RevokeInvite(_ context.Context, id string) error {
	f.revoked = append(f.revoked, id)
	return nil
}

func TestInviteListRevokeCommands(t *testing.T) {
	f := &inviteFake{fakeClient: newFake()}
	f.me.Admin = true
	f.users[0].Admin = true // alice: the user list is what the model trusts
	exp := time.Date(2026, 3, 4, 15, 4, 0, 0, time.Local).UnixMilli()
	f.invites = []proto.InviteInfo{
		{ID: "0123456789ab", Kind: proto.InviteKindUser, UsesLeft: 14, Expires: exp, CreatedBy: 1},
		{ID: "ba9876543210", Kind: proto.InviteKindAdmin, UsesLeft: 1, Expires: exp},
		{ID: "\x1b]52;c;x\a", Kind: "\x1b[2Jodd", UsesLeft: 1, Expires: exp, CreatedBy: 2},
	}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
	}

	do("/invites")
	for _, want := range []string{"0123456789ab team 14 left, exp Mar 4 15:04, by @alice", "ba9876543210 admin 1 left", "by server host", "by @bob"} {
		if !strings.Contains(m.notice, want) {
			t.Fatalf("missing %q in notice %q", want, m.notice)
		}
	}
	if strings.ContainsAny(m.notice, "\x1b\a") {
		t.Fatalf("unsanitized server text in notice %q", m.notice)
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "invites notice")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	do("/invite-revoke 0123456789AB")
	if len(f.revoked) != 1 || f.revoked[0] != "0123456789ab" || !strings.Contains(m.notice, "revoked") {
		t.Fatalf("revoked %v notice %q", f.revoked, m.notice)
	}
	for _, in := range []string{"/invite-revoke", "/invite-revoke xyz", "/invite-revoke 0123456789ab extra", "/invites now"} {
		do(in)
		if !m.noticeErr || !strings.Contains(m.notice, "usage") {
			t.Errorf("%q: notice %q", in, m.notice)
		}
	}
	if len(f.revoked) != 1 {
		t.Fatalf("bad input reached the server: %v", f.revoked)
	}

	f.invites = nil
	do("/invites")
	if m.notice != "no open invites" {
		t.Fatalf("empty: %q", m.notice)
	}

	// Non-admins are told up front; nothing is sent.
	m.me.Admin = false
	f.users[0].Admin = false
	do("/invite-revoke 0123456789ab")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins") || len(f.revoked) != 1 {
		t.Fatalf("non-admin: %q %v", m.notice, f.revoked)
	}
}
