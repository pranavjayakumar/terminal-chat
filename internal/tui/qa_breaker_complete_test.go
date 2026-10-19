package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// The highlighted item must stay the same person when the list is rebuilt
// under the popup (a member leaves/joins, a reconnect reloads users).
func TestPopupSelectionFollowsReload_Break(t *testing.T) {
	t.Skip("issue #102")
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "hi @")
	if got := strings.Join(compTexts(m), " "); got != "@bob @carol @averyveryverylongusername" {
		t.Fatalf("@ candidates: %s", got)
	}
	press(m, "down")
	if m.comp.items[m.comp.sel].text != "@carol" {
		t.Fatalf("down: %v sel %d", compTexts(m), m.comp.sel)
	}
	// bob leaves #general: the list re-sorts under the popup.
	f.chans[1].Members = []uint64{1, 3}
	m.reloadChans()
	if !m.compShown() {
		t.Fatal("popup gone after reload")
	}
	if got := m.comp.items[m.comp.sel].text; got != "@carol" {
		t.Errorf("highlight jumped from @carol to %s after a reload: %v", got, compTexts(m))
	}
	press(m, "enter")
	if v := m.input.Value(); v != "hi @carol " {
		t.Fatalf("Enter after reload completed the wrong person: %q", v)
	}
}

// Same, triggered by a new user arriving (reloadUsers) while ↓ is held on a
// name.
func TestPopupSelectionFollowsNewUser_Break(t *testing.T) {
	t.Skip("issue #102")
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "ping @")
	press(m, "down", "down")
	want := m.comp.items[m.comp.sel].text
	f.users = append(f.users, proto.User{ID: 9, Name: "aaron"})
	f.chans[1].Members = append(f.chans[1].Members, 9)
	m.reloadChans()
	m.reloadUsers()
	if got := m.comp.items[m.comp.sel].text; got != want {
		t.Errorf("highlight moved from %s to %s when aaron joined: %v", want, got, compTexts(m))
	}
}

// Mentioning yourself by your exact name ("as @alice said") is an exact
// name like any other: Enter must not rewrite it to a longer name.
func TestExactSelfMentionNotRewritten_Break(t *testing.T) {
	t.Skip("issue #116")
	m, f := newTestModelFake(t, 80, 24)
	f.users = append(f.users, proto.User{ID: 9, Name: "alice-bot"})
	m.reloadUsers()
	typeKeys(m, "as @alice")
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "as @alice" {
		t.Errorf("Enter on exact own name: sent %+v, input %q", f.sent, m.input.Value())
	}
}

// Every popup frame fits, at every small size, with hostile display names
// and topics (escapes, wide chars, ZWJ, combining marks, bidi, very long).
func TestPopupHostileNamesFitTinySizes_Break(t *testing.T) {
	hostile := []string{
		"\x1b[31mred\x1b]0;pwned\a",
		"‮evil override",
		strings.Repeat("你", 80),
		"👨‍👩‍👧‍👦👨‍👩‍👧‍👦👨‍👩‍👧‍👦",
		"é́́́́́ combining",
		"zero​width‍⁠",
		"\u0085c1\u009bcontrol",
		strings.Repeat("x", 5000),
		"tab\there\nnewline",
	}
	for _, w := range []int{1, 2, 4, 13, 30} {
		for _, h := range []int{1, 2, 3, 6, 12} {
			f := newFake()
			for i, s := range hostile {
				f.users = append(f.users, proto.User{ID: uint64(50 + i), Name: fmt.Sprintf("bo%d", i), Display: s})
				f.chans = append(f.chans, client.ChannelView{Channel: proto.Channel{ID: uint64(50 + i), Name: fmt.Sprintf("ch%d", i), Kind: proto.ChanPublic, Topic: s, Members: []uint64{1}}, Display: fmt.Sprintf("ch%d", i), Member: true})
			}
			m := newModelWith(t, f, w, h)
			for _, in := range []string{"/", "@", "@bo", "#", "#ch"} {
				press(m, "ctrl+u")
				typeKeys(m, in)
				safeView(t, m, fmt.Sprintf("%dx%d %q", w, h, in))
				press(m, "up")
				safeView(t, m, fmt.Sprintf("%dx%d %q up", w, h, in))
				press(m, "down", "down")
				safeView(t, m, fmt.Sprintf("%dx%d %q down", w, h, in))
			}
		}
	}
}

func newModelWith(t *testing.T, f *fakeClient, w, h int) *Model {
	t.Helper()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// safeView checks the frame's size and that no control characters leak,
// without checkView's input-visibility rule (not guaranteed at 1-2 rows).
func safeView(t *testing.T, m *Model, step string) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s: View panicked: %v", step, r)
		}
	}()
	v := m.View()
	lines := strings.Split(v, "\n")
	if len(lines) != m.h {
		t.Fatalf("%s: %d lines, want %d", step, len(lines), m.h)
	}
	for i, l := range lines {
		if sw := strWidth(l); sw > m.w {
			t.Fatalf("%s: line %d is %d wide (> %d): %q", step, i, sw, m.w, l)
		}
	}
	if strings.ContainsAny(v, "\a‮\u0085\u009b") {
		t.Fatalf("%s: control characters leaked", step)
	}
}

// Completing an @word on a soft-wrapped line of wide characters replaces
// exactly that word.
func TestCompleteOnWrappedWideLine_Break(t *testing.T) {
	m, f := newTestModelFake(t, 30, 20)
	typeKeys(m, strings.Repeat("你好", 20)+" é @bo")
	if !m.compShown() {
		t.Fatalf("popup not shown: %q", m.input.Value())
	}
	press(m, "enter")
	want := strings.Repeat("你好", 20) + " é @bob "
	if v := m.input.Value(); v != want {
		t.Fatalf("wrapped wide line: %q, want %q", v, want)
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != strings.TrimRight(want, " ") {
		t.Fatalf("sent %+v", f.sent)
	}
}

// Mid-word completion with text in the middle of a long line.
func TestCompleteMidLine_Break(t *testing.T) {
	m, f := newTestModelFake(t, 40, 20)
	typeKeys(m, "hi @b there and more text that wraps around the input")
	for range len([]rune(" there and more text that wraps around the input")) {
		send(m, tea.KeyMsg{Type: tea.KeyLeft})
	}
	typeKeys(m, "o")
	if !m.compShown() {
		t.Fatal("popup not shown mid-line")
	}
	press(m, "enter")
	if v := m.input.Value(); v != "hi @bob there and more text that wraps around the input" {
		t.Fatalf("mid-line: %q", v)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent %+v", f.sent)
	}
}

// Pasted (or typed) bidi overrides and other invisible format characters
// are drawn raw in the input line: the terminal reorders the rest of the
// line, so what is on screen is not what Enter sends (or which command it
// runs). Everything rendered is supposed to go through internal/sanitize.
func TestInputLineBidiOverride_Break(t *testing.T) {
	t.Skip("issue #117")
	for _, in := range []string{"/join \u202e", "see \u202elareneg# and \u2066x\u2069", "@bo\u202e"} {
		m, _ := newTestModelFake(t, 80, 24)
		paste(m, in)
		for i, l := range strings.Split(m.View(), "\n") {
			if strings.ContainsAny(l, "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069") {
				t.Errorf("paste %q: bidi control drawn raw on line %d: %q", in, i, l)
			}
		}
		m2, _ := newTestModelFake(t, 80, 24)
		typeKeys(m2, in)
		for i, l := range strings.Split(m2.View(), "\n") {
			if strings.ContainsAny(l, "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069") {
				t.Errorf("typed %q: bidi control drawn raw on line %d: %q", in, i, l)
			}
		}
	}
}
