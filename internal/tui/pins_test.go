package tui

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// findMsg locates a message in the fake's channels and applies fn to it.
func (f *fakeClient) findMsg(id uint64, fn func(ch uint64, x *client.Message)) bool {
	for ch, ms := range f.msgs {
		for i := range ms {
			if ms[i].ID == id {
				fn(ch, &f.msgs[ch][i])
				return true
			}
		}
	}
	return false
}

func (f *fakeClient) setChanPins(ch uint64, ids []uint64) {
	for i := range f.chans {
		if f.chans[i].ID == ch {
			f.chans[i].Pins = slices.Clone(ids)
		}
	}
}

func (f *fakeClient) Pin(ctx context.Context, msg uint64) error {
	f.record("pin")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findMsg(msg, func(ch uint64, x *client.Message) {
		x.Pinned = true
		f.pinOrder = append([]uint64{msg}, slices.DeleteFunc(f.pinOrder, func(id uint64) bool { return id == msg })...)
		f.setChanPins(ch, f.pinOrder)
	})
	return nil
}

func (f *fakeClient) Unpin(ctx context.Context, msg uint64) error {
	f.record("unpin")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findMsg(msg, func(ch uint64, x *client.Message) {
		x.Pinned = false
		f.pinOrder = slices.DeleteFunc(f.pinOrder, func(id uint64) bool { return id == msg })
		f.setChanPins(ch, f.pinOrder)
	})
	return nil
}

func (f *fakeClient) Pins(ch uint64) []client.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []client.Message
	for _, id := range f.pinOrder {
		f.findMsg(id, func(c uint64, x *client.Message) {
			if c == ch {
				out = append(out, *x)
			}
		})
	}
	return out
}

func (f *fakeClient) FetchPins(ctx context.Context, ch uint64) error {
	f.record("fetchpins")
	return nil
}

func TestPinKeyTogglesSelected(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.openChannel(10)
	press(m, "up") // empty input: select the newest message
	if m.mode != modeSelect {
		t.Fatalf("mode %v", m.mode)
	}
	sel, _ := m.selected()
	for sel.ID == 0 { // skip the pending/failed entries at the end
		press(m, "up")
		sel, _ = m.selected()
	}
	run(m, m.handleKey(keyMsg("p")))
	if !f.called("pin") {
		t.Fatalf("calls: %v", f.calls)
	}
	if !strings.Contains(m.notice, "pinned") || strings.Contains(m.notice, "unpinned") {
		t.Fatalf("notice %q", m.notice)
	}
	m.reloadPane(&m.main)
	m.reloadChans()
	if v, _ := m.chanView(10); len(v.Pins) != 1 || v.Pins[0] != sel.ID {
		t.Fatalf("channel pins %v, want [%d]", v.Pins, sel.ID)
	}
	if h := ansi.Strip(m.headerLine(80)); !strings.Contains(h, "📌 1") {
		t.Fatalf("header lacks pin count: %q", h)
	}
	if v := ansi.Strip(checkView(t, m, "pinned")); !strings.Contains(v, "📌") {
		t.Fatalf("pinned message has no marker:\n%s", v)
	}

	// p again unpins.
	m.mode = modeSelect
	run(m, m.handleKey(keyMsg("p")))
	if !f.called("unpin") || !strings.Contains(m.notice, "unpinned") {
		t.Fatalf("calls %v notice %q", f.calls, m.notice)
	}
	m.reloadChans()
	if h := ansi.Strip(m.headerLine(80)); strings.Contains(h, "📌") {
		t.Fatalf("header still shows pins: %q", h)
	}
}

func TestPinKeyIgnoresDeletedAndTypedWord(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.openChannel(10)
	press(m, "up")
	// Select the deleted message (107 is before it; 106 is deleted).
	for i := 0; i < 60; i++ {
		if sel, ok := m.selected(); ok && sel.ID == 106 {
			break
		}
		press(m, "up")
	}
	if sel, _ := m.selected(); sel.ID != 106 {
		t.Fatal("couldn't select the deleted message")
	}
	run(m, m.handleKey(keyMsg("p")))
	if f.called("pin") {
		t.Fatal("pinned a deleted message")
	}
	// A pasted/typed word starting with p is not the key.
	run(m, m.handleKey(keyMsg("pin")))
	if f.called("pin") {
		t.Fatal("typed word acted as a key")
	}
}

func TestPinsOverlay(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		m.openChannel(10)
		f.mu.Lock()
		f.findMsg(100, func(ch uint64, x *client.Message) { x.Pinned = true })
		f.findMsg(104, func(ch uint64, x *client.Message) { x.Pinned = true })
		f.pinOrder = []uint64{104, 100}
		f.setChanPins(10, f.pinOrder)
		f.mu.Unlock()
		m.reloadChans()

		m.input.SetValue("/pins")
		run(m, m.submit())
		if m.mode != modeSearch || m.search.pins != 10 || len(m.search.res) != 2 {
			t.Fatalf("%v: mode %v pins %d res %d", sz, m.mode, m.search.pins, len(m.search.res))
		}
		if !f.called("fetchpins") {
			t.Fatal("/pins didn't refresh from the server")
		}
		if m.search.res[0].ID != 104 {
			t.Fatalf("newest pin first: %v", m.search.res[0].ID)
		}
		v := ansi.Strip(checkView(t, m, "pins overlay"))
		if !strings.Contains(v, "pinned in #general") || !strings.Contains(v, "bob") {
			t.Fatalf("%v: overlay:\n%s", sz, v)
		}
		if strings.ContainsAny(v, "\x1b\a") {
			t.Fatalf("raw control chars in overlay")
		}

		// Esc closes.
		m.searchKey(keyMsg("esc"))
		if m.mode != modeNormal {
			t.Fatalf("esc left mode %v", m.mode)
		}
		checkView(t, m, "pins closed")
	}
}

func TestPinsOverlayEmpty(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}} {
		m := newTestModel(t, sz[0], sz[1])
		m.openChannel(10)
		m.input.SetValue("/pins")
		run(m, m.submit())
		if m.mode != modeSearch {
			t.Fatalf("mode %v", m.mode)
		}
		if v := ansi.Strip(checkView(t, m, "empty pins")); !strings.Contains(v, "nothing pinned") {
			t.Fatalf("%v:\n%s", sz, v)
		}
	}
}

func TestPinsEnterJumps(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	m.openChannel(11) // somewhere else
	f.mu.Lock()
	f.findMsg(105, func(ch uint64, x *client.Message) { x.Pinned = true })
	f.pinOrder = []uint64{105}
	f.setChanPins(10, f.pinOrder)
	f.mu.Unlock()
	m.reloadChans()
	// Pins of a channel are listed from the channel they belong to.
	m.search = searchState{pins: 10, res: []proto.Message{f.msgs[10][5].Message}}
	m.mode = modeSearch
	run(m, m.searchKey(keyMsg("enter")))
	if m.mode == modeSearch {
		t.Fatal("Enter didn't close the overlay")
	}
	if m.active != 10 {
		t.Fatalf("active %d, want 10", m.active)
	}
	if sel, ok := m.selected(); !ok || sel.ID != 105 {
		t.Fatalf("selected %+v ok=%v, want 105", sel, ok)
	}
	checkView(t, m, "after jump")
}

func TestHelpMentionsPins(t *testing.T) {
	h := strings.Join(sheetPlain(100), "\n")
	if !strings.Contains(h, "/pins") || !strings.Contains(h, "pin") {
		t.Fatal("help lacks pins")
	}
}
