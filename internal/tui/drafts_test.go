package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/proto"
)

// press sends keys through Update (not handleKey) so the draft bookkeeping
// that runs after every Update is exercised, as in the real program.
func press(m *Model, keys ...string) {
	for _, k := range keys {
		_, cmd := m.Update(keyMsg(k))
		run(m, cmd)
	}
}

func typeVia(m *Model, s string) {
	for _, r := range s {
		press(m, string(r))
	}
}

func sidebarRow(t *testing.T, view, name string) string {
	t.Helper()
	for _, l := range strings.Split(view, "\n") {
		if i := strings.Index(l, "│"); i > 0 {
			if side := l[:i]; strings.Contains(side, name) {
				return side
			}
		}
	}
	t.Fatalf("no sidebar row for %q in\n%s", name, view)
	return ""
}

func TestDraftsKeptPerChannelAcrossSwitch(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeVia(m, "hello wor")
	if m.active != 10 {
		t.Fatalf("active = %d", m.active)
	}
	press(m, "ctrl+n") // general → secret
	if m.active != 13 {
		t.Fatalf("ctrl+n went to %d", m.active)
	}
	if m.input.Value() != "" {
		t.Fatalf("draft leaked into the next channel: %q", m.input.Value())
	}
	if got := f.draft(10, 0); got != "hello wor" {
		t.Fatalf("draft not saved on switch: %q", got)
	}
	if row := sidebarRow(t, checkView(t, m, "other channel"), "general"); !strings.Contains(row, "✎") {
		t.Fatalf("general has a draft but no mark: %q", row)
	}
	typeVia(m, "secret stuff")
	press(m, "ctrl+p") // back to general
	if m.active != 10 || m.input.Value() != "hello wor" {
		t.Fatalf("restored %q in %d", m.input.Value(), m.active)
	}
	if row := sidebarRow(t, checkView(t, m, "back"), "general"); strings.Contains(row, "✎") {
		t.Fatalf("active channel shows its own draft mark: %q", row)
	}
	if row := sidebarRow(t, checkView(t, m, "back"), "secret"); !strings.Contains(row, "✎") {
		t.Fatalf("secret lost its mark: %q", row)
	}
	typeVia(m, "ld")
	press(m, "ctrl+n")
	press(m, "ctrl+p")
	if m.input.Value() != "hello world" {
		t.Fatalf("draft = %q", m.input.Value())
	}
}

func TestDraftsPerThread(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeVia(m, "main draft")
	m.openThread(103) // as a jump from search or the activity view does
	m.Update(noopMsg{})
	if !m.thread.open() || m.input.Value() != "" {
		t.Fatalf("thread open=%v input=%q", m.thread.open(), m.input.Value())
	}
	if got := f.draft(10, 0); got != "main draft" {
		t.Fatalf("channel draft = %q", got)
	}
	typeVia(m, "thread draft")
	if row := sidebarRow(t, checkView(t, m, "thread"), "general"); !strings.Contains(row, "✎") {
		t.Fatalf("the channel draft is hidden while the thread is open but unmarked: %q", row)
	}
	press(m, "esc") // close the thread
	if m.thread.open() || m.input.Value() != "main draft" {
		t.Fatalf("after closing: thread open=%v input=%q", m.thread.open(), m.input.Value())
	}
	if got := f.draft(10, 103); got != "thread draft" {
		t.Fatalf("thread draft = %q", got)
	}
	m.openThread(103)
	m.Update(noopMsg{})
	if m.input.Value() != "thread draft" {
		t.Fatalf("thread draft not restored: %q", m.input.Value())
	}
}

func TestDraftClearedOnSend(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeVia(m, "ship it")
	press(m, "ctrl+n", "ctrl+p") // saved by the switch
	if f.draft(10, 0) != "ship it" {
		t.Fatalf("draft = %q", f.draft(10, 0))
	}
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "ship it" {
		t.Fatalf("sent = %+v", f.sent)
	}
	if got := f.draft(10, 0); got != "" {
		t.Fatalf("draft survived the send: %q", got)
	}
	press(m, "ctrl+n", "ctrl+p")
	if m.input.Value() != "" {
		t.Fatalf("sent text came back: %q", m.input.Value())
	}
	if row := sidebarRow(t, checkView(t, m, "sent"), "secret"); strings.Contains(row, "✎") {
		t.Fatalf("mark left after send: %q", row)
	}
}

func TestDraftsRestoredOnStart(t *testing.T) {
	f := newFake()
	f.SetDraft(10, 0, "from last time")
	f.SetDraft(13, 0, "another")
	m := New(t.Context(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.input.Value() != "from last time" {
		t.Fatalf("input at start = %q", m.input.Value())
	}
	if row := sidebarRow(t, checkView(t, m, "start"), "secret"); !strings.Contains(row, "✎") {
		t.Fatalf("no mark for the other channel's draft: %q", row)
	}
	// Restarting must not turn a restored draft into something else.
	m.Close()
	if f.draft(10, 0) != "from last time" || f.draft(13, 0) != "another" {
		t.Fatalf("drafts after close: %v", f.drafts)
	}
}

func TestDraftSavedAfterTypingPausesAndOnClose(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	typeVia(m, "abc")
	if got := f.draft(10, 0); got != "" {
		t.Fatalf("written on every keystroke: %q", got)
	}
	if !m.draftTimer {
		t.Fatal("no save scheduled")
	}
	m.Update(draftSaveMsg{})
	if got := f.draft(10, 0); got != "abc" {
		t.Fatalf("after the pause: %q", got)
	}
	typeVia(m, "d")
	m.Close()
	if got := f.draft(10, 0); got != "abcd" {
		t.Fatalf("after close: %q", got)
	}
}

func TestEditTextIsNeverADraft(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	press(m, "up") // select mode on the newest message of mine…
	for i, x := range m.main.msgs {
		if x.ID == 103 {
			m.main.sel = i
		}
	}
	press(m, "e")
	if m.editing != 103 || m.input.Value() == "" {
		t.Fatalf("not editing: %d %q", m.editing, m.input.Value())
	}
	press(m, "ctrl+n") // switching away abandons the edit
	if m.editing != 0 || m.input.Value() != "" {
		t.Fatalf("edit survived the switch: %d %q", m.editing, m.input.Value())
	}
	press(m, "ctrl+p")
	if m.input.Value() != "" || len(f.drafts) != 0 {
		t.Fatalf("edit text became a draft: %q %v", m.input.Value(), f.drafts)
	}
}

func TestDraftMarksFitAllSizes(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}, {69, 20}, {20, 6}} {
		m := newTestModel(t, sz[0], sz[1])
		typeVia(m, "draft here")
		press(m, "ctrl+n")
		checkView(t, m, "switch")
		press(m, "ctrl+k")
		v := checkView(t, m, "switcher")
		if !strings.Contains(v, "✎") && sz[1] > 8 {
			t.Errorf("%v: switcher doesn't mark the channel with a draft\n%s", sz, v)
		}
		press(m, "esc", "ctrl+p")
		checkView(t, m, "back")
	}
}

// A jump from the activity view lands in select mode with the channel's
// draft still in the input; editing a message must not eat that draft.
func TestEditAfterJumpKeepsTheDraft(t *testing.T) {
	jumpToMine := func(t *testing.T) (*Model, *fakeClient) {
		m, f := newTestModelFake(t, 100, 30)
		activityFake(f)
		typeVia(m, "half typed")
		press(m, "alt+a", "enter") // the reaction on my message 103, same channel
		if m.mode != modeSelect || m.focus().msgs[m.focus().sel].ID != 103 || m.input.Value() != "half typed" {
			t.Fatalf("mode %v input %q", m.mode, m.input.Value())
		}
		return m, f
	}
	t.Run("cancelled", func(t *testing.T) {
		m, f := jumpToMine(t)
		press(m, "e")
		if m.editing != 103 || m.input.Value() == "half typed" {
			t.Fatalf("not editing: %d %q", m.editing, m.input.Value())
		}
		press(m, "esc")
		if m.editing != 0 || m.input.Value() != "half typed" {
			t.Fatalf("after Esc: editing %d input %q", m.editing, m.input.Value())
		}
		m.Close()
		if got := f.draft(10, 0); got != "half typed" {
			t.Fatalf("saved draft = %q", got)
		}
	})
	t.Run("saved", func(t *testing.T) {
		m, f := jumpToMine(t)
		press(m, "e", "enter")
		if !f.called("edit") {
			t.Fatal("edit wasn't sent")
		}
		if m.editing != 0 || m.input.Value() != "half typed" {
			t.Fatalf("after saving: editing %d input %q", m.editing, m.input.Value())
		}
	})
	t.Run("switching away and back", func(t *testing.T) {
		m, f := jumpToMine(t)
		press(m, "e", "ctrl+n")
		if m.editing != 0 || m.input.Value() != "" {
			t.Fatalf("edit survived: %d %q", m.editing, m.input.Value())
		}
		press(m, "ctrl+p")
		if m.active != 10 || m.input.Value() != "half typed" {
			t.Fatalf("back in %d: input %q", m.active, m.input.Value())
		}
		m.Close()
		if got := f.draft(10, 0); got != "half typed" {
			t.Fatalf("saved draft = %q", got)
		}
	})
	t.Run("the overlay ends the edit", func(t *testing.T) {
		m, _ := jumpToMine(t)
		press(m, "e", "alt+a")
		if m.editing != 0 || m.input.Value() != "half typed" || m.mode != modeActivity {
			t.Fatalf("overlay: editing %d input %q mode %v", m.editing, m.input.Value(), m.mode)
		}
	})
	t.Run("a jump ends the edit", func(t *testing.T) {
		m, _ := jumpToMine(t)
		press(m, "e")
		m.jumpTo(proto.Message{ID: 104, Chan: 10})
		if m.editing != 0 || m.input.Value() != "half typed" {
			t.Fatalf("jumpTo: editing %d input %q", m.editing, m.input.Value())
		}
	})
}
