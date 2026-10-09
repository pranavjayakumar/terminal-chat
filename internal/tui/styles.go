package tui

import (
	"hash/fnv"
	"io"
	"os"
	"strconv"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// authorPalette holds mid-tone 256-color indexes that stay readable on both
// dark and light backgrounds. lipgloss degrades them on 16-color terminals.
var authorPalette = [8]string{"167", "71", "68", "133", "37", "172", "98", "131"}

// styles is the whole palette. Every style comes from one renderer so tests
// can pin the color profile, and so nothing ever queries the terminal
// (AdaptiveColor would send an OSC 11 query from inside the alt screen).
type styles struct {
	color bool

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
	authors                         [len(authorPalette)]lipgloss.Style
}

// noColor reports whether NO_COLOR is set (https://no-color.org: any non-empty value).
func noColor() bool { return os.Getenv("NO_COLOR") != "" }

// defaultStyles builds styles for the real terminal on w.
func defaultStyles(w io.Writer) *styles {
	r := lipgloss.NewRenderer(w)
	color := !noColor()
	if !color {
		// termenv's Ascii profile would also drop bold/dim; NO_COLOR only
		// asks us to drop color.
		r.SetColorProfile(termenv.ANSI)
	}
	return newStyles(r, color)
}

// testStyles builds styles with a fixed profile, independent of the
// environment the tests run in.
func testStyles(color bool) *styles {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.ANSI256)
	return newStyles(r, color)
}

func newStyles(r *lipgloss.Renderer, color bool) *styles {
	n := r.NewStyle
	fg := func(s lipgloss.Style, c string) lipgloss.Style {
		if !color {
			return s
		}
		return s.Foreground(lipgloss.Color(c))
	}
	s := &styles{color: color}
	s.base = n()
	s.dim = n().Faint(true)
	s.bold = n().Bold(true)
	s.italic = n().Italic(true)
	s.faint = n().Faint(true)
	s.time = n().Faint(true)
	s.sep = n().Faint(true)
	s.edited = n().Faint(true)
	s.deleted = n().Faint(true).Italic(true)
	s.code = fg(n(), "173")
	s.codeBar = fg(n().Faint(true), "8")
	s.diffAdd = fg(n(), "34")
	s.diffDel = fg(n(), "160")
	s.diffHunk = fg(n().Faint(true), "37")
	s.diffHdr = n().Bold(true).Faint(true)
	s.url = fg(n().Underline(true), "39")
	s.mention = fg(n().Bold(true), "214")
	s.mentionMark = fg(n().Bold(true), "214")
	s.selMark = fg(n().Bold(true), "39")
	s.reaction = n()
	s.reactionMine = fg(n().Bold(true), "39")
	s.replies = fg(n(), "39")
	s.failed = fg(n().Bold(true), "160")
	s.pending = n().Faint(true)
	s.header = n().Bold(true)
	s.topic = n().Faint(true)
	s.status = n().Faint(true)
	s.sideTitle = n().Bold(true)
	s.sideSection = n().Faint(true)
	s.sideItem = n()
	s.sideActive = n().Bold(true).Reverse(true)
	s.sideUnrd = n().Bold(true)
	s.badge = fg(n().Bold(true), "39")
	s.mentionBadge = fg(n().Bold(true), "214")
	s.border = n().Faint(true)
	s.typing = n().Faint(true).Italic(true)
	s.pill = n().Reverse(true).Bold(true)
	s.noticeErr = fg(n().Bold(true), "160")
	s.noticeInfo = fg(n(), "39")
	s.prompt = n().Bold(true)
	s.placeholder = n().Faint(true)
	s.overlayTitle = n().Bold(true)
	s.overlaySel = n().Reverse(true)
	for i, c := range authorPalette {
		s.authors[i] = fg(n().Bold(true), c)
	}
	return s
}

// author is the stable color for a user id.
func (s *styles) author(id uint64) lipgloss.Style {
	h := fnv.New32a()
	h.Write([]byte(strconv.FormatUint(id, 10)))
	return s.authors[h.Sum32()%uint32(len(s.authors))]
}
