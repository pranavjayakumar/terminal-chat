package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
)

var altM = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}, Alt: true}

// #132: ⌥M mutes and unmutes the open channel, and in the palette the
// highlighted channel, which stays open with the row marked; the sidebar
// can't take keys, so this is its keyboard mute. Every frame fits.
func TestMuteKey(t *testing.T) {
	for _, sz := range writeSizes {
		m, f := newTestModelFake(t, sz[0], sz[1])
		ch := m.active
		if ch == 0 {
			t.Fatal("no open channel")
		}
		run(m, m.handleKey(altM))
		if !m.muted(ch) || f.nchans[ch].Level != client.LevelOff {
			t.Fatalf("%v: ⌥M didn't mute the open channel", sz)
		}
		checkView(t, m, "muted here")
		run(m, m.handleKey(altM))
		if m.muted(ch) {
			t.Fatalf("%v: ⌥M again didn't unmute", sz)
		}

		press(m, "alt+k")
		typeVia(m, "backend")
		if got := m.palChan(); got != 11 {
			t.Fatalf("%v: highlighted channel %d, want 11", sz, got)
		}
		v := strings.Join(strings.Fields(ansi.Strip(checkView(t, m, "palette"))), " ")
		if sz[0] >= 80 && !strings.Contains(v, "⌥M mute") {
			t.Errorf("%v: footer lacks ⌥M mute:\n%s", sz, v)
		}
		run(m, m.handleKey(altM))
		if !m.muted(11) || m.mode != modeSwitcher || m.line.Value() != "backend" {
			t.Fatalf("%v: palette ⌥M: muted %v mode %v query %q", sz, m.muted(11), m.mode, m.line.Value())
		}
		if m.muted(ch) && ch != 11 {
			t.Fatalf("%v: the open channel was muted instead", sz)
		}
		v = strings.Join(strings.Fields(ansi.Strip(checkView(t, m, "palette muted"))), " ")
		if sz[0] >= 80 && (!strings.Contains(v, "backend · muted") && !strings.Contains(v, "backend muted") || !strings.Contains(v, "⌥M unmute")) {
			t.Errorf("%v: muted row not marked:\n%s", sz, v)
		}
		run(m, m.handleKey(altM))
		if m.muted(11) {
			t.Fatalf("%v: palette ⌥M again didn't unmute", sz)
		}
		press(m, "esc")
		checkView(t, m, "closed")
	}
}

// ⌥M on a palette row that isn't a channel (a command) does nothing.
func TestMuteKeyPaletteCommandRow(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	press(m, "alt+k")
	typeVia(m, "settings")
	if m.palChan() != 0 {
		t.Skip("top row is a channel")
	}
	run(m, m.handleKey(altM))
	for id, n := range f.nchans {
		if n.Level == client.LevelOff {
			t.Fatalf("channel %d muted from a command row", id)
		}
	}
	checkView(t, m, "command row")

	// The Mute #x row (meta ⌥M) acts on the open channel.
	press(m, "esc")
	press(m, "alt+k")
	typeVia(m, "mute")
	for m.palChan() == 0 && m.pal.sel < len(m.pal.shown)-1 {
		m.pal.sel++
	}
	if it := m.pal.shown[m.pal.sel]; !strings.HasPrefix(it.label, "Mute ") {
		t.Fatalf("highlighted %q, want the Mute row", it.label)
	}
	run(m, m.handleKey(altM))
	if !m.muted(m.active) {
		t.Fatal("⌥M on the Mute row didn't mute the open channel")
	}
	checkView(t, m, "mute row")
}
