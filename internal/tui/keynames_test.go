package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

const macKeySyms = "⌥⌃⇧⏎⌘⇥"

func TestKeyNamesFor(t *testing.T) {
	for _, c := range []struct {
		env, goos string
		pc        bool
	}{
		{"", "darwin", false}, {"", "linux", true}, {"", "freebsd", true},
		{"pc", "darwin", true}, {"PC", "darwin", true}, {"mac", "linux", false}, {" mac ", "linux", false},
		{"bogus", "linux", true}, {"bogus", "darwin", false},
	} {
		if got := keyNamesFor(c.env, c.goos).pc; got != c.pc {
			t.Errorf("TC_KEYS=%q on %s: pc=%v, want %v", c.env, c.goos, got, c.pc)
		}
	}
}

func TestKeyNamesOf(t *testing.T) {
	for in, want := range map[string]string{
		"⌥K":                      "Alt+K",
		"⌥⇧↑":                     "Alt+Shift+↑",
		"⌃P":                      "Ctrl+P",
		"⏎":                       "Enter",
		"⌥⏎":                      "Alt+Enter",
		"⌥↑ ⌥↓":                   "Alt+↑/↓",
		"⌥⇧↑ ⌥⇧↓":                 "Alt+Shift+↑/↓",
		"⌃P ⌃N":                   "Ctrl+P/N",
		"⌃C ⌃C":                   "Ctrl+C Ctrl+C", // press twice: not a pair
		"⌥⏎ ⌃J":                   "Alt+Enter Ctrl+J",
		"⌃B F2":                   "Ctrl+B F2",
		"⌃T hold":                 "Ctrl+T hold",
		" ↑↓ move · ⇥/⏎ insert ":  " ↑↓ move · Tab/Enter insert ",
		"no channel — ⌥K to open": "no channel — Alt+K to open",
		"+":                       "+",
	} {
		if got := (keyNames{pc: true}).of(in); got != want {
			t.Errorf("pc %q = %q, want %q", in, got, want)
		}
		if got := (keyNames{}).of(in); got != in {
			t.Errorf("mac %q = %q, want it unchanged", in, got)
		}
	}
}

func newPCModel(t *testing.T, w, h int) *Model {
	t.Helper()
	m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), PCKeys: true})
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return m
}

// With PC labels every frame still fits, and no Mac symbol is left.
func TestViewFitsPCKeys(t *testing.T) {
	steps := append(append([]string{}, fitSteps...),
		"esc", "f1", "pgdown", "esc", "/", "esc", "esc", ":", "s", "m", "esc", "esc", "ctrl+k", "tab", "tab", "esc", "up", "esc",
		"esc", "alt+e", "right", "alt+e", "alt+k", "tab", "esc",
		"/", "s", "e", "t", "t", "i", "n", "g", "s", "enter", "down", "down", "esc", "esc")
	for _, sz := range [][2]int{{30, 10}, {40, 12}, {50, 16}, {60, 20}, {80, 24}, {120, 36}, {200, 60}} {
		m := newPCModel(t, sz[0], sz[1])
		for _, s := range steps {
			step := "start"
			if s != "" {
				step = s
				run(m, m.handleKey(keyMsg(s)))
			}
			plain := checkView(t, m, step)
			if i := strings.IndexAny(plain, macKeySyms); i >= 0 {
				t.Fatalf("%v after %s: Mac key symbol in a PC frame near %q\n%s", sz, step, plain[max(i-20, 0):min(i+20, len(plain))], plain)
			}
		}
	}
}

func TestPCKeyLabelsShown(t *testing.T) {
	sheet := strings.Join(sheetPlainKeys(100, keyNames{pc: true}), "\n")
	for _, want := range []string{"Alt+K", "Alt+↑/↓", "Ctrl+P/N", "Alt+Shift+↑/↓", "Alt+Enter", "Ctrl+J", "Ctrl+C Ctrl+C", "Alt+E", "Enter"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("PC key sheet lacks %q", want)
		}
	}
	if strings.ContainsAny(sheet, macKeySyms) {
		t.Errorf("PC key sheet has a Mac symbol:\n%s", sheet)
	}
	if !strings.Contains(strings.Join(sheetPlain(100), "\n"), "⌥⇧↑ ⌥⇧↓") {
		t.Error("Mac key sheet lost its symbols")
	}
	for _, pc := range []bool{false, true} {
		m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), PCKeys: pc})
		m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
		plain := checkView(t, m, "start")
		want, other := "⌥K commands · ⌥⏎ newline · ⏎ send", "Alt+K"
		if pc {
			want, other = "Alt+K commands · Alt+Enter newline · Enter send", "⌥"
		}
		if !strings.Contains(plain, want) || strings.Contains(plain, other) {
			t.Errorf("pc=%v: want %q and no %q\n%s", pc, want, other, plain)
		}
	}
}

// A narrow input box drops the newline hint, then send, before dropping
// them all.
func TestInputHintsNarrow(t *testing.T) {
	for _, c := range []struct {
		pc       bool
		w        int
		want, no string
	}{
		{true, 46, "Alt+K commands · Enter send", "newline"},
		{true, 30, "Alt+K commands", "send"},
		{false, 34, "⌥K commands · ⏎ send", "newline"},
		{false, 60, "⌥K commands · ⌥⏎ newline · ⏎ send", "Alt"},
	} {
		m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), PCKeys: c.pc})
		m.Update(tea.WindowSizeMsg{Width: c.w, Height: 14})
		plain := checkView(t, m, "start")
		if !strings.Contains(plain, c.want) || strings.Contains(plain, c.no) {
			t.Errorf("pc=%v at %d: want %q, no %q\n%s", c.pc, c.w, c.want, c.no, plain)
		}
	}
}

// The agent panel's hints and key pills follow the keyboard too, on every
// page and in every skin.
func TestAgentPanelPCKeys(t *testing.T) {
	for _, sk := range []string{"", `,"skin":"retro"`} {
		for _, sz := range [][2]int{{40, 12}, {100, 34}} {
			for row := range apRows {
				m, _ := panelModel(t, sz[0], sz[1], sk)
				m.kn = keyNames{pc: true}
				for range row {
					press(m, "down")
				}
				for _, step := range []string{"row", "enter"} {
					if step == "enter" {
						press(m, "enter")
					}
					plain := frameText(t, m, step)
					if i := strings.IndexAny(plain, macKeySyms); i >= 0 {
						t.Fatalf("%s %v %s on %s: Mac key symbol near %q\n%s", sk, sz, step, apRows[row], plain[max(i-20, 0):min(i+20, len(plain))], plain)
					}
					if sz[0] == 100 && sk == "" && step == "row" && apRows[row] == "Memory" && !strings.Contains(plain, "Enter browse") {
						t.Errorf("memory hint not in words:\n%s", plain)
					}
				}
			}
		}
	}
}

// The palette says Tab moves between tabs, in both spellings.
func TestPaletteTabHint(t *testing.T) {
	// PC: "Tab next tab" is too wide beside the tabs; the footer says it.
	for pc, want := range map[bool]string{false: "⇥ next tab", true: "Tab  next tab"} {
		m := New(context.Background(), newFake(), Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), PCKeys: pc})
		m.Update(tea.WindowSizeMsg{Width: 120, Height: 36})
		run(m, m.handleKey(keyMsg("alt+k")))
		if plain := checkView(t, m, "palette"); !strings.Contains(plain, want) {
			t.Errorf("pc=%v: no %q\n%s", pc, want, plain)
		}
	}
}
