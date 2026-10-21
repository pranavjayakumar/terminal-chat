package tui

import (
	"context"
	"encoding/base64"
	"fmt"
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
	// The full code is on screen (not truncated) even in a narrow pane.
	if m.mode != modeViewer || !strings.Contains(checkView(t, m, "invite"), "15 uses") {
		t.Fatalf("mode %v notice %q", m.mode, m.notice)
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "invite viewer")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if plain := checkView(t, m, "invite viewer"); !strings.Contains(plain, code) {
		t.Fatalf("code not shown in full:\n%s", plain)
	}
	m.viewerKey(kEsc)
	if m.mode != modeNormal {
		t.Fatalf("Esc should close to normal mode, got %v", m.mode)
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
		if sz[0] >= 69 && !strings.Contains(plain, "↑ v0.3") {
			t.Fatalf("%dx%d: no update notice in the status bar:\n%s", sz[0], sz[1], plain)
		}
	}
	// Offline status wins over the update notice.
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.status = client.Status{State: client.Offline}
	var l string
	for _, r := range strings.Split(ansi.Strip(m.View()), "\n") {
		if strings.Contains(r, "tc ·") {
			l = r
		}
	}
	if strings.Contains(l, "update") || !strings.Contains(strings.ToLower(l), "offline") {
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
	forbid  bool // answer forbidden, like a server without member_invites
}

func (f *inviteFake) ListInvites(context.Context) ([]proto.InviteInfo, error) {
	if f.forbid {
		return nil, proto.Errf(proto.ErrForbidden, "")
	}
	return f.invites, nil
}
func (f *inviteFake) RevokeInvite(_ context.Context, id string) error {
	if f.forbid {
		return proto.Errf(proto.ErrForbidden, "")
	}
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
	listed := ""
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
		listed = m.notice
		if m.mode == modeList { // one invite per row
			listed = m.list.title + "\n" + strings.Join(m.list.rows, "\n")
		}
	}

	do("/invites")
	if m.mode != modeList || len(m.list.rows) != 3 { // #111: a notice cut all but the first
		t.Fatalf("/invites: mode %v rows %q", m.mode, m.list.rows)
	}
	for _, want := range []string{"0123456789ab team, 14 left, exp Mar 4 15:04, by @alice", "ba9876543210 admin, 1 left", "by server host", "by @bob"} {
		if !strings.Contains(listed, want) {
			t.Fatalf("missing %q in list %q", want, listed)
		}
	}
	if v := checkView(t, m, "invites"); strings.ContainsAny(v, "\x1b\a") {
		t.Fatalf("unsanitized server text shown")
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "invites list")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.mode = modeNormal

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

	// Members ask the server (cap member_invites decides); a forbidden
	// answer says what it means.
	m.me.Admin = false
	f.users[0].Admin = false
	f.invites = []proto.InviteInfo{{ID: "0123456789ab", Kind: proto.InviteKindUser, UsesLeft: 1, Expires: exp, CreatedBy: 1,
		Keys: 2, KeySrc: "github:carol\x1b[2J"}}
	do("/invites")
	if m.noticeErr || !strings.Contains(listed, "only (2 keys), 1 left") {
		t.Fatalf("member list: %q", listed)
	}
	m.mode = modeNormal
	do("/invite-revoke 0123456789ab")
	if m.noticeErr || len(f.revoked) != 2 {
		t.Fatalf("member revoke: %q %v", m.notice, f.revoked)
	}
	f.forbid = true
	do("/invite-revoke 0123456789ab")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins can revoke invites on this server") {
		t.Fatalf("forbidden revoke: %q", m.notice)
	}
	do("/invites")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins can list invites") {
		t.Fatalf("forbidden list: %q", m.notice)
	}
}

func (f *inviteFake) CreateGitHubInvite(_ context.Context, login string, ttl time.Duration) (proto.InviteRes, error) {
	f.record(fmt.Sprintf("ghinvite %s %s", login, ttl))
	if f.forbid {
		return proto.InviteRes{}, proto.Errf(proto.ErrForbidden, "")
	}
	return proto.InviteRes{Code: "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x", Uses: 1,
		Expires: time.Now().Add(time.Hour).UnixMilli(), Keys: 2}, nil
}

// /invite --key github:user fetches through the backend and says the
// invite is bound; bad forms never reach it.
func TestInviteKeyCommand(t *testing.T) {
	f := &inviteFake{fakeClient: newFake()}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true), Version: "v0.2.0"})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
	}
	do("/invite --key github:carol 3d")
	if !f.called("ghinvite carol 72h0m0s") {
		t.Fatalf("calls %v", f.calls)
	}
	if v := checkView(t, m, "key invite"); m.noticeErr || !strings.Contains(v, "invite created · for github:carol only (2 keys) · 1 use") {
		t.Fatalf("notice %q view:\n%s", m.notice, v)
	}
	m.viewerKey(kEsc)
	if !strings.Contains(m.opt.Out.(*syncBuf).String(), "]52;c;") {
		t.Fatal("one-liner not copied")
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		plain := checkView(t, m, "key invite notice")
		if sz[0] == 40 && !strings.Contains(plain, "invite created") {
			t.Fatalf("40 columns: 'copied' cut off:\n%s", plain)
		}
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// The help overlay keeps a gap after the long key.
	for _, w := range []int{60, 100} {
		var row string
		for _, p := range sheetPlain(w) {
			if strings.HasPrefix(p, "/invite --key") {
				row = p
			}
		}
		if !strings.Contains(row, "/invite --key github:user ") || ansi.StringWidth(row) > w {
			t.Fatalf("width %d: help row %q", w, row)
		}
	}
	// Specific usage errors.
	do("/invite --key carol")
	if !strings.Contains(m.notice, "did you mean github:carol") {
		t.Fatalf("hint: %q", m.notice)
	}
	f.calls = nil
	for _, in := range []string{"/invite --key", "/invite --key carol", "/invite --key github:a_b", "/invite --key github:x --key github:y",
		"/invite --key github:x 5", "/invite --key github:\x1b[2J"} {
		do(in)
		if !m.noticeErr {
			t.Errorf("%q: notice %q", in, m.notice)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("bad input reached the backend: %v", f.calls)
	}
	f.forbid = true
	do("/invite --key github:carol")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins can invite people on this server") {
		t.Fatalf("forbidden: %q", m.notice)
	}
	// A backend without key-bound invites says so.
	m2, _ := newTestModelFake(t, 100, 30)
	m2.input.SetValue("/invite --key github:carol")
	run(m2, m2.submit())
	if !m2.noticeErr || !strings.Contains(m2.notice, "aren't supported") {
		t.Fatalf("unsupported: %q", m2.notice)
	}
}

func TestOSC52TmuxPassthrough(t *testing.T) {
	plain := osc52("hi", false)
	wrapped := osc52("hi", true)
	if !strings.HasPrefix(wrapped, plain+"\x1bPtmux;\x1b\x1b]52;c;") || !strings.HasSuffix(wrapped, "\a\x1b\\") {
		t.Fatalf("bad tmux wrap %q", wrapped)
	}
}

func TestClipNeverClaimsWithoutAttempt(t *testing.T) {
	ctx := context.Background()
	if how, _ := clip(ctx, Options{}, "x"); how != "" {
		t.Fatalf("how %q with no route", how)
	}
	var b syncBuf
	called := false
	ext := func(context.Context, string) error { called = true; return nil }
	how, _ := clip(ctx, Options{Out: &b, ExtCopy: ext}, "x")
	if how != "system" || !called {
		t.Fatalf("how %q called %v", how, called)
	}
	// Over SSH the tool would reach the remote clipboard: terminal only.
	called = false
	how, _ = clip(ctx, Options{Out: &b, ExtCopy: ext, SSH: true}, "x")
	if how != "terminal" || called {
		t.Fatalf("ssh: how %q called %v", how, called)
	}
}

// Narrow panes: the whole code and the command are readable (wrapped, with
// shell continuations), copy result first.
func TestInviteViewWrapsNarrow(t *testing.T) {
	m, _ := newTestModelFake(t, 40, 20)
	m.opt.Version = "v0.2.0"
	m.input.SetValue("/invite")
	run(m, m.submit())
	if m.mode != modeViewer {
		t.Fatal("no invite view")
	}
	plain := checkView(t, m, "invite 40")
	code := "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x"
	rows := m.view.lines
	if !strings.Contains(rows[0], "clipboard") {
		t.Errorf("copy result not first:\n%s", plain)
	}
	// Unfold like sh: backslash-newline vanishes, adjacent quotes join.
	i := 0
	for rows[i] != "install command:" {
		i++
	}
	cmd := strings.Join(rows[i+1:], "\n")
	cmd = strings.ReplaceAll(strings.ReplaceAll(cmd, "\\\n", ""), "''", "")
	cmd = strings.ReplaceAll(cmd, "\n", "")
	if want := "curl -fsSL " + proto.InstallScriptURL("v0.2.0") + " | sh -s -- '" + code + "'"; strings.Join(strings.Fields(cmd), " ") != want && cmd != want {
		t.Errorf("unfolded command differs:\n%q\n%q", cmd, want)
	}
	if !strings.Contains(plain, " \\") || !strings.Contains(plain, "y copy") {
		t.Errorf("missing continuations or hint:\n%s", plain)
	}
	for _, sz := range [][2]int{{40, 12}, {30, 10}, {24, 8}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "invite narrow")
	}
}

// The invite viewer takes the whole width, so selecting the install
// command with the mouse doesn't pick up sidebar text.
func TestInviteViewerFullWidth(t *testing.T) {
	m, _ := newTestModelFake(t, 120, 30)
	m.showInvite(inviteShownMsg{summary: "invite created", code: "tc://h:1/abc#pin", line: "curl -fsSL https://example.com/install.sh | sh -s -- 'tc://h:1/abc#pin'", how: "system"})
	v := checkView(t, m, "invite")
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "curl") && strings.Contains(l, "│") {
			t.Fatalf("install command beside the sidebar: %q", l)
		}
	}
	m.handleKey(keyMsg("esc"))
	if !m.sidebarShown() {
		t.Fatal("sidebar not back after Esc")
	}
}
