package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
)

type endorseFake struct {
	*fakeClient
	err error
	got []string
	own string
}

// Real-length fingerprints: the views must show and copy all 50 characters.
const (
	fpDesk   = "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s"
	fpLaptop = "SHA256:Xq3vJ8rT0bLmN2pQ9sKdW4eYhZ7cFgA1uI6oVxR5tE0"
	fpOther  = "SHA256:Ab3vJ8rT0bLmN2pQ9sKdW4eYhZ7cFgA1uI6oVxR5tE0"
)

func (f *endorseFake) OwnKeyFingerprint() string { return f.own }
func (f *endorseFake) EndorseKey(_ context.Context, fp string) (client.EndorseResult, error) {
	f.got = append(f.got, fp)
	return client.EndorseResult{By: fpDesk, Key: fp}, f.err
}

// onScreen: s is on screen whole, though rows may cut it at the width.
func onScreen(t *testing.T, m *Model, s string) bool {
	t.Helper()
	var b strings.Builder
	for _, l := range strings.Split(ansi.Strip(checkView(t, m, "key view")), "\n") {
		b.WriteString(strings.TrimSpace(l))
	}
	return strings.Contains(b.String(), s)
}

// /endorse-key (#146): without an argument it shows this computer's key;
// with a fingerprint it vouches for that key and says what to run on the
// other computer. /accept-key @me points here instead of trusting a key on
// a prompt.
func TestEndorseKeyCommand(t *testing.T) {
	f := &endorseFake{fakeClient: newFake()}
	m := New(context.Background(), f, Options{Out: &syncBuf{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	do := func(in string) {
		t.Helper()
		m.input.SetValue(in)
		run(m, m.submit())
	}
	// Not connected yet: no key to show, and it says so.
	do("/endorse-key")
	if m.mode != modeViewer || !onScreen(t, m, "unknown until connected") {
		t.Fatalf("no key yet: mode %v\n%s", m.mode, ansi.Strip(m.View()))
	}
	m.viewerKey(kEsc)

	f.own = fpDesk
	for _, sz := range [][2]int{{200, 60}, {100, 30}, {40, 12}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		f.got = nil
		do("/endorse-key")
		if m.mode != modeViewer || !onScreen(t, m, fpDesk) || m.view.copy != fpDesk || len(f.got) != 0 {
			t.Fatalf("%dx%d own key: mode %v copy %q got %v\n%s", sz[0], sz[1], m.mode, m.view.copy, f.got, ansi.Strip(m.View()))
		}
		m.viewerKey(kEsc)

		f.got = nil
		do("/endorse-key " + fpLaptop)
		if len(f.got) != 1 || f.got[0] != fpLaptop {
			t.Fatalf("got %v", f.got)
		}
		if m.noticeErr || m.mode != modeViewer || !onScreen(t, m, fpLaptop) || !onScreen(t, m, "/endorse-key "+fpDesk) ||
			m.view.copy != "/endorse-key "+fpDesk {
			t.Fatalf("%dx%d endorsed: notice %q copy %q\n%s", sz[0], sz[1], m.notice, m.view.copy, ansi.Strip(m.View()))
		}
		m.Update(tea.WindowSizeMsg{Width: 20, Height: 6})
		checkView(t, m, "endorsed tiny")
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		m.viewerKey(kEsc)
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	f.err = client.ErrKeyTrusted
	do("/endorse-key " + fpLaptop)
	if m.noticeErr || !strings.Contains(m.notice, "already trusted") {
		t.Fatalf("trusted: %q", m.notice)
	}
	f.err = errors.New("no device of yours has signed in with that SSH key yet")
	do("/endorse-key " + fpOther)
	if !m.noticeErr || !strings.Contains(m.notice, "no device of yours") {
		t.Fatalf("error: %q", m.notice)
	}

	f.got = nil
	// A cut fingerprint is caught here, before the backend sees it.
	do("/endorse-key " + fpLaptop[:30])
	if !m.noticeErr || !strings.Contains(m.notice, "looks cut off") || len(f.got) != 0 {
		t.Fatalf("cut: %q, calls %v", m.notice, f.got)
	}
	for _, in := range []string{"/endorse-key laptop", "/endorse-key SHA256:a SHA256:b", "/accept-key @me"} {
		do(in)
		if !m.noticeErr {
			t.Errorf("%q: notice %q", in, m.notice)
		}
	}
	if !strings.Contains(m.notice, "/endorse-key") || len(f.got) != 0 || strWidth(m.notice) > 80 {
		t.Fatalf("accept-key @me: %q, calls %v", m.notice, f.got)
	}
	// The client's refusal to accept my own key names the TUI's command.
	m.setErr("accept-key", client.ErrOwnKeyAccept)
	if strings.Contains(m.notice, "tc devices") || !strings.Contains(m.notice, "/endorse-key") {
		t.Fatalf("own key accept: %q", m.notice)
	}

	// A backend that can't endorse says so.
	m2, _ := newTestModelFake(t, 100, 30)
	m2.input.SetValue("/endorse-key " + fpLaptop)
	run(m2, m2.submit())
	if !m2.noticeErr || !strings.Contains(m2.notice, "isn't supported") {
		t.Fatalf("unsupported: %q", m2.notice)
	}
}
