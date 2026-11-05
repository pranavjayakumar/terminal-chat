package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
)

type endorseFake struct {
	*fakeClient
	err error
	got []string
}

func (f *endorseFake) OwnKeyFingerprint() string { return "SHA256:desk" }
func (f *endorseFake) EndorseKey(_ context.Context, fp string) (client.EndorseResult, error) {
	f.got = append(f.got, fp)
	return client.EndorseResult{By: "SHA256:desk", Key: fp}, f.err
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
	do("/endorse-key")
	if m.mode != modeList || !strings.Contains(checkView(t, m, "own key"), "SHA256:desk") || len(f.got) != 0 {
		t.Fatalf("mode %v got %v", m.mode, f.got)
	}
	m.listKey(kEsc)

	do("/endorse-key SHA256:laptopkey")
	if len(f.got) != 1 || f.got[0] != "SHA256:laptopkey" {
		t.Fatalf("got %v", f.got)
	}
	v := checkView(t, m, "endorsed")
	if m.noticeErr || !strings.Contains(v, "/endorse-key SHA256:desk") {
		t.Fatalf("notice %q view:\n%s", m.notice, v)
	}
	for _, sz := range [][2]int{{100, 30}, {40, 12}, {20, 6}} {
		m.Update(tea.WindowSizeMsg{Width: sz[0], Height: sz[1]})
		checkView(t, m, "endorse list")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.listKey(kEsc)

	f.err = client.ErrKeyTrusted
	do("/endorse-key SHA256:laptopkey")
	if m.noticeErr || !strings.Contains(m.notice, "already trusted") {
		t.Fatalf("trusted: %q", m.notice)
	}
	f.err = errors.New("no device of yours has signed in with that SSH key yet")
	do("/endorse-key SHA256:other")
	if !m.noticeErr || !strings.Contains(m.notice, "no device of yours") {
		t.Fatalf("error: %q", m.notice)
	}

	f.got = nil
	for _, in := range []string{"/endorse-key laptop", "/endorse-key SHA256:a SHA256:b", "/accept-key @me"} {
		do(in)
		if !m.noticeErr {
			t.Errorf("%q: notice %q", in, m.notice)
		}
	}
	if !strings.Contains(m.notice, "/endorse-key") || len(f.got) != 0 {
		t.Fatalf("accept-key @me: %q, calls %v", m.notice, f.got)
	}

	// A backend that can't endorse says so.
	m2, _ := newTestModelFake(t, 100, 30)
	m2.input.SetValue("/endorse-key SHA256:x")
	run(m2, m2.submit())
	if !m2.noticeErr || !strings.Contains(m2.notice, "isn't supported") {
		t.Fatalf("unsupported: %q", m2.notice)
	}
}
