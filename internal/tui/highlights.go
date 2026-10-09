package tui

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// highlightCommand runs /highlight and /unhighlight; ok is false for other
// commands. Keywords are local to this machine (the client's cache).
func (m *Model) highlightCommand(c command) (ok bool, err error) {
	switch c.name {
	case "highlight":
		if len(c.args) == 0 {
			if len(m.highlights) == 0 {
				m.setInfo("no highlight words — /highlight word… adds some")
			} else {
				m.setNotice("highlight words: "+joinWords(m.highlights), false, false, errTTL)
			}
			return true, nil
		}
		words := make([]string, len(c.args))
		for i, a := range c.args {
			w, err := client.NormalizeHighlight(a)
			if err != nil {
				return true, fmt.Errorf("%s: %w", sanitize.Line(a), err)
			}
			words[i] = w
		}
		var added []string
		for _, w := range words {
			isNew, err := m.c.AddHighlight(w)
			if err != nil {
				m.afterHighlights()
				return true, fmt.Errorf("%s: %w", sanitize.Line(w), err)
			}
			if isNew {
				added = append(added, w)
			}
		}
		m.afterHighlights()
		if len(added) == 0 {
			m.setInfo("already highlighted")
		} else {
			m.setInfo("highlighting " + joinWords(added))
		}
		return true, nil
	case "unhighlight":
		if len(c.args) == 0 {
			return true, errors.New("usage: /unhighlight word…")
		}
		words := make([]string, len(c.args))
		for i, a := range c.args {
			w, err := client.NormalizeHighlight(a)
			if err != nil {
				return true, fmt.Errorf("%s: %w", sanitize.Line(a), err)
			}
			if !slices.Contains(m.highlights, w) {
				return true, fmt.Errorf("%s isn't a highlight word", sanitize.Line(w))
			}
			words[i] = w
		}
		for _, w := range words {
			if _, err := m.c.RemoveHighlight(w); err != nil {
				m.afterHighlights()
				return true, fmt.Errorf("%s: %w", sanitize.Line(w), err)
			}
		}
		m.afterHighlights()
		m.setInfo("no longer highlighting " + joinWords(words))
		return true, nil
	}
	return false, nil
}

// afterHighlights re-reads the keywords and everything derived from them:
// the mention state of channels and the markers in the open panes.
func (m *Model) afterHighlights() {
	m.highlights = m.c.Highlights()
	m.reloadChans()
	m.main.ver++
	m.thread.ver++
}

// concernsMe is true for a message that @mentions me or contains one of my
// highlight words. The caller has already excluded my own, deleted and bot
// messages.
func (m *Model) concernsMe(body string) bool {
	return mentionsMe(body, m.me.Name) || client.Highlighted(body, m.highlights)
}

func joinWords(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitize.Line(s)
	}
	return strings.Join(out, ", ")
}
