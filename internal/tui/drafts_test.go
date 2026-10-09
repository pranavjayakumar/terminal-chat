package tui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
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

// A failed write must not forget the draft: it stays dirty and is retried.
func TestDraftFlushKeepsDirtyUntilSaved(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.setErr = errors.New("database is locked")
	typeVia(m, "precious words")
	m.Update(draftSaveMsg{})
	if !m.noticeErr || !strings.Contains(m.notice, "database is locked") {
		t.Fatalf("no error shown: %q", m.notice)
	}
	if !m.draftDirty[m.dk] {
		t.Fatal("draft was marked clean although the write failed")
	}
	f.setErr = nil
	m.Update(draftSaveMsg{})
	if got := f.draft(10, 0); got != "precious words" {
		t.Fatalf("retry didn't save: %q", got)
	}
	if len(m.draftDirty) != 0 {
		t.Fatalf("still dirty after a good write: %v", m.draftDirty)
	}
}

// Drafts come from a file other programs can write: no control sequences
// may reach the input.
func TestRestoredDraftIsSanitized(t *testing.T) {
	f := newFake()
	f.SetDraft(10, 0, "safe\x1b]0;pwned\a text\x1b[31m red \u202e")
	f.SetDraft(13, 0, "other\x07\x1b[2J")
	m := New(t.Context(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true)})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got := m.input.Value(); strings.ContainsAny(got, "\x1b\a\u202e") || !strings.HasPrefix(got, "safe") {
		t.Fatalf("input = %q", got)
	}
	press(m, "ctrl+n") // general → secret
	if got := m.input.Value(); strings.ContainsAny(got, "\x1b\a") || !strings.HasPrefix(got, "other") {
		t.Fatalf("second draft = %q", got)
	}
}

// Another tc process on the same cache wrote a draft after this one started.
func TestDraftWrittenByAnotherProcessIsPickedUp(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	f.SetDraft(13, 0, "typed in the other pane\u202e") // plus a bidi override, to be dropped
	press(m, "ctrl+n")                                 // general → secret
	if m.active != 13 || m.input.Value() != "typed in the other pane" {
		t.Fatalf("in %d input %q", m.active, m.input.Value())
	}
}

// A slot with unsaved local changes is not overwritten by what the cache has.
func TestDraftWithUnsavedChangesIsNotReplacedByTheCache(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	key := draftKey{13, 0}
	m.setDraft(key, "local, not saved yet")
	f.SetDraft(13, 0, "remote")
	press(m, "ctrl+n") // into the slot
	if m.input.Value() != "local, not saved yet" {
		t.Fatalf("input = %q", m.input.Value())
	}
	m.setDraft(draftKey{10, 0}, "also local")
	f.SetDraft(10, 0, "remote 2")
	m.Update(updateMsg(client.Update{Channels: true}))
	if got := m.drafts[draftKey{10, 0}]; got != "also local" {
		t.Fatalf("refresh replaced unsaved changes: %q", got)
	}
}

func TestDraftMarksFollowTheCacheOnChannelUpdates(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	if row := sidebarRow(t, checkView(t, m, "start"), "secret"); strings.Contains(row, "✎") {
		t.Fatalf("mark before any draft: %q", row)
	}
	f.SetDraft(13, 0, "from elsewhere")
	m.Update(updateMsg(client.Update{Channels: true}))
	if row := sidebarRow(t, checkView(t, m, "written"), "secret"); !strings.Contains(row, "✎") {
		t.Fatalf("no mark for a draft written by another process: %q", row)
	}
	f.SetDraft(13, 0, "")
	m.Update(updateMsg(client.Update{Channels: true}))
	if row := sidebarRow(t, checkView(t, m, "cleared"), "secret"); strings.Contains(row, "✎") {
		t.Fatalf("mark stays after the draft was removed: %q", row)
	}
	// The draft being typed is never overwritten by a refresh.
	typeVia(m, "typing now")
	f.SetDraft(10, 0, "stale")
	m.Update(updateMsg(client.Update{Channels: true}))
	if m.input.Value() != "typing now" || m.drafts[m.dk] != "typing now" {
		t.Fatalf("input %q draft %q", m.input.Value(), m.drafts[m.dk])
	}
}
