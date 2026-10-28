package tui

import (
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/proto"
)

// /invite --for @user mints an add-key invite (#59): the backend's
// CreateInvite with that user's id, shown and copied like any invite.
func TestInviteForCommand(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	m.opt.Version = "v0.2.0"
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
	}
	do("/invite --for @bob")
	if !f.called("addkey 2") {
		t.Fatalf("calls %v notice %q", f.calls, m.notice)
	}
	v := checkView(t, m, "add-key invite")
	if m.noticeErr || m.mode != modeViewer || !strings.Contains(v, "add-key invite for @bob") {
		t.Fatalf("notice %q view:\n%s", m.notice, v)
	}
	code := "tc://chat.example.com:7443/abcdefghijklmnopqrstuvwxyz#Pin-_x"
	line := "curl -fsSL " + proto.InstallScriptURL("v0.2.0") + " | sh -s -- '" + code + "'"
	if !strings.Contains(m.opt.Out.(*syncBuf).String(), "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(line))+"\a") {
		t.Fatal("one-liner not copied")
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "add-key invite viewer")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.viewerKey(kEsc)

	// @me is the caller.
	f.calls = nil
	do("/invite --for @me")
	if !f.called("addkey 1") {
		t.Fatalf("@me: calls %v notice %q", f.calls, m.notice)
	}
	m.viewerKey(kEsc)

	// Bad input never reaches the backend.
	f.calls = nil
	for _, in := range []string{"/invite --for", "/invite --for @nobody", "/invite --for @bob 3", "/invite --for @bob --key github:bob",
		"/invite 3 --for @bob", "/invite --for \x1b[2J"} {
		do(in)
		if !m.noticeErr || strings.Contains(m.notice, "\x1b") {
			t.Errorf("%q: notice %q", in, m.notice)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("bad input reached the backend: %v", f.calls)
	}
	// Bots can't sign in.
	f.users = append(f.users, proto.User{ID: 9, Name: "ci-bot", Bot: true})
	m.reloadUsers()
	do("/invite --for @ci-bot")
	if !m.noticeErr || !strings.Contains(m.notice, "bot") || len(f.calls) != 0 {
		t.Fatalf("bot: %q %v", m.notice, f.calls)
	}
	// Members are told it's an admin's job.
	f.addKeyErr = proto.Errf(proto.ErrForbidden, "only an admin can add an SSH key to an account")
	do("/invite --for @bob")
	if !m.noticeErr || !strings.Contains(m.notice, "only admins can add an SSH key") {
		t.Fatalf("forbidden: %q", m.notice)
	}
	// The help sheet lists it.
	found := false
	for _, p := range sheetPlain(100) {
		found = found || strings.HasPrefix(p, "/invite --for @user")
	}
	if !found {
		t.Fatal("/invite --for missing from help")
	}
}
