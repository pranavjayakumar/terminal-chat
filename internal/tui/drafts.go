package tui

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// draftDebounce is how long typing must pause before the draft is written
// to the cache. Switching channel, clearing the input and quitting write at
// once.
const draftDebounce = time.Second

// draftKey says where the input's text belongs: a channel (root 0) or a
// thread of it.
type draftKey struct{ ch, root uint64 }

type draftSaveMsg struct{}

// focusKey is the draft slot of the pane the input currently acts on.
func (m *Model) focusKey() draftKey {
	p := m.focus()
	return draftKey{p.ch, p.root}
}

// loadDrafts reads the saved drafts once; from then on m.drafts is the
// authority and the cache just follows it.
func (m *Model) loadDrafts() {
	m.drafts = map[draftKey]string{}
	m.draftDirty = map[draftKey]bool{}
	for _, d := range m.c.Drafts() {
		if strings.TrimSpace(d.Body) != "" {
			m.drafts[draftKey{d.Chan, d.Thread}] = d.Body
		}
	}
}

// syncDraft keeps the input and the drafts in step. It runs after every
// Update: when the pane the input acts on changed (channel switch, thread
// opened or closed) the text moves to the slot it was typed in and the new
// slot's draft is restored; otherwise the draft just follows the input.
func (m *Model) syncDraft() {
	if m.editing != 0 {
		return // an edit's text is not a draft
	}
	key, val := m.focusKey(), m.input.Value()
	if key == m.dk {
		m.setDraft(key, val)
		if strings.TrimSpace(val) == "" {
			m.flushDrafts() // a sent or cleared draft must not come back after a crash
		}
	} else {
		prev := m.dk
		m.setDraft(prev, val)
		m.dk = key
		if d := m.drafts[key]; d != "" || prev.ch != 0 {
			m.input.SetValue(d)
			m.layoutInput()
		} else {
			// Text typed with no channel open has no slot of its own: it
			// becomes the draft of the first one that has none.
			m.setDraft(key, val)
		}
		m.flushDrafts()
	}
	m.draftMark = map[uint64]bool{}
	for k, v := range m.drafts {
		if k != m.dk && v != "" {
			m.draftMark[k.ch] = true
		}
	}
}

// setDraft records val as key's draft in memory and marks it for saving.
func (m *Model) setDraft(key draftKey, val string) {
	if key.ch == 0 {
		return
	}
	if strings.TrimSpace(val) == "" {
		val = ""
	}
	if m.drafts[key] == val {
		return
	}
	if val == "" {
		delete(m.drafts, key)
	} else {
		m.drafts[key] = val
	}
	m.draftDirty[key] = true
}

// flushDrafts writes the changed drafts. A draft stays dirty until its
// write succeeds, so the next flush retries it.
func (m *Model) flushDrafts() {
	for key := range m.draftDirty {
		if err := m.c.SetDraft(key.ch, key.root, m.drafts[key]); err != nil {
			m.setErr("", err)
			continue
		}
		delete(m.draftDirty, key)
	}
}

// draftTick schedules a save for drafts that are still dirty.
func (m *Model) draftTick() tea.Cmd {
	if len(m.draftDirty) == 0 || m.draftTimer {
		return nil
	}
	m.draftTimer = true
	return tea.Tick(draftDebounce, func(time.Time) tea.Msg { return draftSaveMsg{} })
}

func (m *Model) hasDraft(ch uint64) bool { return m.draftMark[ch] }
