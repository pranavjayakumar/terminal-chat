package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// The highlighted person must stay the highlighted person when the list
// changes under the open popup; otherwise Enter inserts someone else.
func TestCompSelectionFollowsItem_BreakUserLeaves(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "hi @")
	press(m, "down") // @carol
	if got := m.comp.items[m.comp.sel].text; got != "@carol" {
		t.Fatalf("setup: highlighted %s, items %v", got, compTexts(m))
	}
	// bob's account is disabled while the popup is open.
	for i := range f.users {
		if f.users[i].Name == "bob" {
			f.users[i].Disabled = true
		}
	}
	m.reloadUsers()
	if !m.compShown() {
		t.Fatal("popup closed by the reload")
	}
	if got := m.comp.items[m.comp.sel].text; got != "@carol" {
		t.Errorf("highlight moved from @carol to %s after bob left (items %v)", got, compTexts(m))
	}
	press(m, "enter")
	if v := m.input.Value(); v != "hi @carol " {
		t.Errorf("Enter inserted the wrong person: %q, want %q", v, "hi @carol ")
	}
}

func TestCompSelectionFollowsItem_BreakChannelAppears(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "see #")
	press(m, "down") // #general
	if got := m.comp.items[m.comp.sel].text; got != "#general" {
		t.Fatalf("setup: highlighted %s, items %v", got, compTexts(m))
	}
	// A new joined channel that sorts first arrives (another device joined it).
	f.chans = append([]client.ChannelView{{Channel: proto.Channel{ID: 40, Name: "alpha", Kind: proto.ChanPublic, Members: []uint64{1}}, Display: "alpha", Member: true}}, f.chans...)
	m.reloadChans()
	if got := m.comp.items[m.comp.sel].text; got != "#general" {
		t.Errorf("highlight moved from #general to %s after a channel appeared (items %v)", got, compTexts(m))
	}
	press(m, "enter")
	if v := m.input.Value(); v != "see #general " {
		t.Errorf("Enter inserted the wrong channel: %q, want %q", v, "see #general ")
	}
}

// Names are case-insensitive (findUser/findChan lowercase); an exact name
// typed with capitals ranks first as exact, yet Enter rewrites instead of
// sending as TUI.md promises for a word that already equals the item.
func TestExactMentionCaseInsensitive_BreakEnterDoesntSend(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeKeys(m, "thanks @Bob")
	if !m.compShown() || m.comp.items[m.comp.sel].text != "@bob" || m.comp.items[m.comp.sel].rank != exactRank {
		t.Fatalf("setup: %+v", m.comp.items)
	}
	press(m, "enter")
	if len(f.sent) != 1 {
		t.Errorf("Enter on exact (case-insensitive) @Bob didn't send: input %q sent %+v", m.input.Value(), f.sent)
	}
}

// Hostile display names, topics and odd names in the popup at many sizes:
// every frame fits, nothing leaks, no panic.
func TestCompHostileNamesFit(t *testing.T) {
	hostile := []string{
		"\x1b[31mred\x1b]0;pwned\a",
		"\u202eevil\u202c name",
		"z​w‍j\ufeff",
		"你好世界你好世界你好世界你好世界你好世界",
		"👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧",
		"é́́́́́́́́́́́́́́́́́́́́ combining",
		"\u0085c1\u009bcontrols\u0090",
		strings.Repeat("W", 500),
		"line1\nline2\rline3\ttab",
	}
	for _, sz := range [][2]int{{80, 24}, {40, 12}, {30, 8}, {20, 6}, {12, 5}, {8, 4}, {69, 7}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		for i, d := range hostile {
			f.users = append(f.users, proto.User{ID: uint64(50 + i), Name: "u" + strings.Repeat("x", i*4), Display: d})
		}
		for i, d := range hostile {
			f.chans = append(f.chans, client.ChannelView{Channel: proto.Channel{ID: uint64(60 + i), Name: "c" + strings.Repeat("y", i*4), Kind: proto.ChanPublic, Topic: d}, Display: "c", Member: true})
		}
		m.reloadUsers()
		m.reloadChans()
		for _, in := range []string{"@", "@u", "@x", "#", "#c", "/", "/re"} {
			press(m, "ctrl+u")
			typeKeys(m, in)
			checkView(t, m, "typed "+in)
			for range 12 {
				press(m, "down")
				checkView(t, m, "down "+in)
			}
			press(m, "up")
			checkView(t, m, "up "+in)
		}
	}
}

// Enter on the / menu must never send text to the channel, and a typo'd
// command is never sent as a message.
func TestCompSlashNeverSends(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	for _, in := range []string{"/", "/x", "/hepl", "/q", "/HELP", "/join", "/pins"} {
		press(m, "ctrl+u")
		typeKeys(m, in)
		press(m, "enter")
		if m.mode != modeNormal {
			press(m, "esc")
		}
		if len(f.sent) != 0 {
			t.Fatalf("%q + Enter sent a message: %+v", in, f.sent)
		}
	}
}

// Tab/Enter/Esc/arrow storms with cursor moves and multi-line drafts never
// panic and every frame fits.
func TestCompKeyStorm(t *testing.T) {
	keys := []string{"@", "b", "tab", "#", "g", "enter", "esc", "tab", "up", "down", "ctrl+p", "ctrl+n",
		"alt+enter", "/", "h", "backspace", "backspace", "home", "end", "ctrl+u", "@", "c", "a", "esc", "tab", "tab"}
	for _, sz := range [][2]int{{80, 24}, {20, 6}, {40, 12}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		for i := 0; i < 150; i++ {
			k := keys[(i*7+i/3)%len(keys)]
			if k == "home" || k == "end" {
				send(m, tea.KeyMsg{Type: map[string]tea.KeyType{"home": tea.KeyHome, "end": tea.KeyEnd}[k]})
			} else {
				press(m, k)
			}
			if m.mode != modeNormal {
				press(m, "esc")
				m.mode = modeNormal
			}
			checkView(t, m, k)
		}
		for _, s := range f.sent {
			if strings.HasPrefix(s.Body, "/") && !strings.HasPrefix(s.Body, "//") {
				t.Fatalf("a /command was sent as text: %q", s.Body)
			}
		}
	}
}
