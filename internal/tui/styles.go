package tui

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// styles is the whole palette, built from one theme. Every style comes from
// one renderer that always writes truecolor: the frame is re-encoded cell by
// cell for the real terminal (canvas.go), so tests pin the output profile in
// prof, and nothing ever queries the terminal (AdaptiveColor would send an
// OSC 11 query from inside the alt screen).
type styles struct {
	color bool
	th    *theme
	prof  termenv.Profile // the terminal's; the canvas degrades colors to it
	enc   *encoder
	r     *lipgloss.Renderer

	base, dim, bold, italic, faint  lipgloss.Style
	time, sep, edited, deleted      lipgloss.Style
	code, codeBar, url, mention     lipgloss.Style
	mentionMark, selMark            lipgloss.Style
	reaction, reactionMine, replies lipgloss.Style
	failed, pending                 lipgloss.Style
	header, topic, status           lipgloss.Style
	sideTitle, sideSection          lipgloss.Style
	sideItem, sideActive, sideUnrd  lipgloss.Style
	badge, mentionBadge             lipgloss.Style
	border, typing, pill            lipgloss.Style
	noticeErr, noticeInfo           lipgloss.Style
	prompt, placeholder             lipgloss.Style
	overlayTitle, overlaySel        lipgloss.Style
	diffAdd, diffDel, diffHunk      lipgloss.Style
	diffHdr                         lipgloss.Style
}

// noColor reports whether NO_COLOR is set (https://no-color.org: any non-empty value).
func noColor() bool { return os.Getenv("NO_COLOR") != "" }

// defaultStyles builds styles for the real terminal on w.
func defaultStyles(w io.Writer, th *theme) *styles {
	prof := lipgloss.NewRenderer(w).ColorProfile() // from the environment only
	color := !noColor() && prof != termenv.Ascii
	return newStyles(color, prof, th)
}

// testStyles builds styles with a fixed profile, independent of the
// environment the tests run in.
func testStyles(color bool) *styles {
	return newStyles(color, termenv.ANSI256, themes[defaultTheme])
}

func newStyles(color bool, prof termenv.Profile, th *theme) *styles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	n := r.NewStyle
	fg := func(s lipgloss.Style, c rgb) lipgloss.Style {
		if !color {
			return s
		}
		return s.Foreground(lipgloss.Color(c.String()))
	}
	s := &styles{color: color, th: th, prof: prof, enc: newEncoder(prof, color), r: r}
	// quiet colors become faint text without color.
	q := func(s lipgloss.Style, c rgb) lipgloss.Style {
		if !color {
			return s.Faint(true)
		}
		return s.Foreground(lipgloss.Color(c.String()))
	}
	s.base = fg(n(), th.fg)
	s.dim = q(n(), th.dim)
	s.bold = fg(n().Bold(true), th.fg)
	s.italic = fg(n().Italic(true), th.fg)
	s.faint = q(n(), th.faint)
	s.time = q(n(), th.faint)
	s.sep = q(n(), th.faint)
	s.edited = q(n(), th.faint)
	s.deleted = q(n().Italic(true), th.faint)
	s.code = fg(n(), th.warn)
	s.codeBar = q(n(), th.line)
	s.diffAdd = fg(n(), th.add)
	s.diffDel = fg(n(), th.del)
	s.diffHunk = q(n(), th.hunk)
	s.diffHdr = q(n().Bold(true), th.dim)
	s.url = fg(n().Underline(true), th.info)
	s.mention = fg(n().Bold(true), th.accent)
	s.mentionMark = fg(n().Bold(true), th.accent)
	s.selMark = fg(n().Bold(true), th.accent)
	s.reaction = q(n(), th.dim)
	s.reactionMine = fg(n().Bold(true), th.accent)
	s.replies = fg(n(), th.info)
	s.failed = fg(n().Bold(true), th.err)
	s.pending = q(n(), th.faint)
	s.header = fg(n().Bold(true), th.fg)
	s.topic = q(n(), th.dim)
	s.status = q(n(), th.dim)
	s.sideTitle = fg(n().Bold(true), th.fg)
	s.sideSection = q(n(), th.faint)
	s.sideItem = q(n(), th.dim)
	s.sideActive = fg(n().Bold(true), th.fg)
	s.sideUnrd = fg(n().Bold(true), th.fg)
	s.badge = q(n().Bold(true), th.dim)
	s.mentionBadge = fg(n().Bold(true), th.accent)
	s.border = q(n(), th.line)
	s.typing = q(n(), th.faint)
	s.pill = n().Bold(true)
	if color {
		s.pill = s.pill.Foreground(lipgloss.Color(th.accentFg.String())).Background(lipgloss.Color(th.accent.String()))
	} else {
		s.pill = s.pill.Reverse(true)
	}
	s.noticeErr = fg(n().Bold(true), th.err)
	s.noticeInfo = fg(n(), th.info)
	s.prompt = fg(n().Bold(true), th.accent)
	s.placeholder = q(n(), th.faint)
	s.overlayTitle = fg(n().Bold(true), th.fg)
	s.overlaySel = n().Bold(true)
	if color {
		s.overlaySel = s.overlaySel.Foreground(lipgloss.Color(th.accentFg.String())).Background(lipgloss.Color(th.accent.String()))
	} else {
		s.overlaySel = s.overlaySel.Reverse(true)
	}
	return s
}

// author is the stable name color for a user id.
func (s *styles) author(id uint64) lipgloss.Style {
	st := s.r.NewStyle().Bold(true)
	if s.color {
		st = st.Foreground(lipgloss.Color(s.th.nick(id).String()))
	}
	return st
}
