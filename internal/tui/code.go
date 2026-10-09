package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/skailama/tc/internal/snippet"
)

// seg is a run of message lines: prose, or one fenced block.
type seg struct {
	code   bool
	info   string // fence language, "" when none or unusable
	diff   bool   // block is a unified diff
	open   string // the fence lines as written (code only)
	close  string // "" when the block is never closed
	closed bool
	lines  []string
}

// splitFences splits sanitized message lines into prose and fenced blocks.
// An unclosed fence runs to the end, as the renderer has always treated it.
func splitFences(lines []string) []seg {
	var out []seg
	for i := 0; i < len(lines); i++ {
		ln := lines[i]
		if !isFence(ln) {
			if n := len(out); n > 0 && !out[n-1].code {
				out[n-1].lines = append(out[n-1].lines, ln)
			} else {
				out = append(out, seg{lines: []string{ln}})
			}
			continue
		}
		s := seg{code: true, open: ln, info: fenceLang(ln)}
		for i++; i < len(lines); i++ {
			if isFence(lines[i]) {
				s.closed, s.close = true, lines[i]
				break
			}
			s.lines = append(s.lines, lines[i])
		}
		s.diff = strings.EqualFold(s.info, "diff") ||
			(s.info == "" && strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s.open), "```")) == "" &&
				snippet.LooksLikeDiff(strings.Join(s.lines, "\n")))
		out = append(out, s)
	}
	return out
}

func isFence(ln string) bool { return strings.HasPrefix(strings.TrimSpace(ln), "```") }

const maxLabel = 16

// fenceLang is the first word of a fence's info string when it looks like a
// language name (letters, digits and a few symbols), else "".
func fenceLang(ln string) string {
	info := strings.TrimPrefix(strings.TrimSpace(ln), "```")
	if i := strings.IndexAny(info, " \t"); i >= 0 {
		info = info[:i]
	}
	if info == "" || len(info) > maxLabel {
		return ""
	}
	for _, r := range info {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("+#._-", r)) {
			return ""
		}
	}
	return info
}

type diffKind uint8

const (
	dCtx diffKind = iota
	dAdd
	dDel
	dHunk
	dHdr
)

// classifyDiff labels each line of a unified diff. Outside a hunk "--- " and
// "+++ " are file headers; inside one a "--- " line is a deletion unless the
// next line is "+++ " (the next file of a plain diff).
func classifyDiff(lines []string) []diffKind {
	ks := make([]diffKind, len(lines))
	inHunk := false
	for i, ln := range lines {
		ln = strings.TrimSuffix(ln, "\r")
		switch {
		case strings.HasPrefix(ln, "@@"):
			ks[i], inHunk = dHunk, true
		case strings.HasPrefix(ln, "diff "):
			ks[i], inHunk = dHdr, false
		case strings.HasPrefix(ln, "--- ") && (!inHunk || i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")):
			ks[i], inHunk = dHdr, false
		case strings.HasPrefix(ln, "+++ ") && !inHunk, strings.HasPrefix(ln, "+++ ") && i > 0 && ks[i-1] == dHdr:
			ks[i] = dHdr
		case !inHunk && ln != "" && !strings.ContainsRune(" +-", rune(ln[0])):
			ks[i] = dHdr // index, new file mode, rename from, Binary files, ...
		case strings.HasPrefix(ln, "+"):
			ks[i] = dAdd
		case strings.HasPrefix(ln, "-"):
			ks[i] = dDel
		}
	}
	return ks
}

func (s *styles) diffStyle(k diffKind) lipgloss.Style {
	switch k {
	case dAdd:
		return s.diffAdd
	case dDel:
		return s.diffDel
	case dHunk:
		return s.diffHunk
	case dHdr:
		return s.diffHdr
	}
	return s.code
}

// codeLines renders a block's lines (without the bar), each cut to w cells,
// styled as code or diff. The label, if any, is right-aligned on the first
// line when it fits next to that line's text.
func (r *renderer) codeLines(sg seg, w int, pending bool) []string {
	var ks []diffKind
	if sg.diff {
		ks = classifyDiff(sg.lines)
	}
	out := make([]string, len(sg.lines))
	for i, ln := range sg.lines {
		st := r.style(fCode, pending)
		if sg.diff {
			st = r.st.diffStyle(ks[i])
			if pending {
				st = st.Faint(true)
			}
		}
		ln = truncate(ln, w)
		out[i] = st.Render(ln)
		if i == 0 && sg.info != "" {
			if gap := w - strWidth(ln) - strWidth(sg.info); gap >= 2 {
				out[i] += spaces(gap) + r.st.dim.Render(sg.info)
			}
		}
	}
	return out
}
