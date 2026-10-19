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
	if c.name == "highlight" || c.name == "unhighlight" {
		m.refreshHighlights() // another pane may have changed them
	}
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
	m.reloadChans()
}

// refreshHighlights re-reads the keywords from the cache, which another tc
// on the same cache may have changed; the log's markers are laid out again
// when they differ.
func (m *Model) refreshHighlights() {
	words := m.c.Highlights()
	if slices.Equal(words, m.highlights) {
		return
	}
	m.highlights = words
	m.main.ver++
	m.thread.ver++
}

// concernsMe is true for a message that @mentions me or contains one of my
// highlight words. The caller has already excluded my own, deleted and bot
// messages.
func (m *Model) concernsMe(body string) bool {
	return client.Mentioned(outsideCode(body), m.mentionAs) || client.Highlighted(body, m.highlights)
}

// concernMark is the log gutter for a message by someone else: "▎" for an
// @mention, "*" when only a highlight word matched, blank otherwise.
func (m *Model) concernMark(body string) string {
	switch {
	case client.Mentioned(outsideCode(body), m.mentionAs):
		return m.st.mentionMark.Render("▎")
	case client.Highlighted(body, m.highlights):
		return m.st.mentionMark.Render("*")
	}
	return " "
}

func joinWords(ss []string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitize.Line(s)
	}
	return strings.Join(out, ", ")
}

// outsideCode is body without its fenced code blocks (the renderer's fence
// rule): a pasted package.json full of "@alice/utils" isn't a mention, just
// as highlight words don't match inside code.
func outsideCode(body string) string {
	if !strings.Contains(body, "```") {
		return body
	}
	var keep []string
	in := false
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			in = !in
			continue
		}
		if !in {
			keep = append(keep, ln)
		}
	}
	return strings.Join(keep, "\n")
}
