// Package sanitize makes untrusted text safe to write to a terminal.
//
// Message bodies, names, topics and anything else a remote user controls must
// pass through Text (multi-line) or Line (single line) before rendering.
package sanitize

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxMarks is how many combining marks one character keeps; more only
// stack into unreadable "Zalgo" text that spills over neighbouring lines.
// Three covers real scripts (Arabic and Hebrew pointing, Thai, Vietnamese).
const MaxMarks = 3

const (
	zwnj      = 0x200C
	zwj       = 0x200D
	blackFlag = 0x1F3F4 // starts an emoji tag sequence (🏴 + tags + cancel tag)
	tagFirst  = 0xE0020
	tagCancel = 0xE007F
)

// Text returns s with everything that could drive a terminal or disguise
// text removed: C0/C1 controls (except \n and \t), DEL, invalid UTF-8,
// bidi overrides and isolates, private-use characters, invisible format
// characters (zero-width spaces, word joiners, BOM, tag characters...),
// and combining marks past MaxMarks on one character. A zero-width joiner
// stays inside an emoji sequence (👩‍💻), a non-joiner between letters of a
// script that needs it (Persian, Indic), and tag characters in an emoji
// flag sequence (🏴󠁧󠁢󠁳󠁣󠁴󠁿). Tabs become 4 spaces so width calculations stay
// predictable.
func Text(s string) string {
	if isClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	var prev rune // last rune written, 0 at the start or after a newline
	marks := 0    // combining marks written on prev's character
	vs := false   // a variation selector follows the current character
	inTags := false
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if inTags {
			if r >= tagFirst && r <= tagCancel {
				b.WriteRune(r)
				inTags = r != tagCancel
				continue
			}
			inTags = false
		}
		switch {
		case r == utf8.RuneError && size <= 1:
			r = '�'
		case r == '\n':
			b.WriteByte('\n')
			prev, marks, vs = 0, 0, false
			continue
		case r == '\t':
			b.WriteString("    ")
			prev, marks, vs = ' ', 0, false
			continue
		case r == zwj:
			next, _ := utf8.DecodeRuneInString(s[i:])
			if !isEmoji(prev) || !isEmoji(next) {
				continue
			}
		case r == zwnj:
			next, _ := utf8.DecodeRuneInString(s[i:])
			if !joinsScript(prev) || !joinsScript(next) {
				continue
			}
		case unsafe(r):
			continue
		case unicode.Is(unicode.Variation_Selector, r):
			// One per character: more change nothing visible, only the bytes
			// (two names that look the same).
			if vs {
				continue
			}
			vs = true
			b.WriteRune(r)
			prev = r
			continue
		case isMark(r):
			if marks >= MaxMarks {
				continue
			}
			marks++
			b.WriteRune(r)
			continue
		}
		b.WriteRune(r)
		prev, marks, vs = r, 0, false
		if r == blackFlag {
			inTags = true
		}
	}
	return b.String()
}

// Line is Text for single-line fields (names, topics, notification titles):
// newlines become spaces.
func Line(s string) string {
	return strings.ReplaceAll(Text(s), "\n", " ")
}

// Valid reports whether s is acceptable as stored message text: valid UTF-8
// with no control characters other than \n and \t. The server rejects
// anything else (PROTOCOL.md §10).
func Valid(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\n' && r != '\t' && isControl(r) {
			return false
		}
	}
	return true
}

// ValidName reports whether s is acceptable as a display name: one line,
// unchanged by Line (so nothing invisible or disguising), not blank, and
// without leading or trailing space. Names are how people tell each other
// apart, so they get the strict rule; message text is only cleaned on
// render.
func ValidName(s string) bool {
	if s == "" {
		return true // no display name
	}
	if !utf8.ValidString(s) || strings.ContainsAny(s, "\n\t") || Line(s) != s {
		return false
	}
	return strings.TrimSpace(s) == s && strings.IndexFunc(s, func(r rune) bool {
		return !unicode.IsSpace(r) && !isMark(r) && r != zwj && r != zwnj
	}) >= 0
}

func isClean(s string) bool {
	for i := 0; i < len(s); i++ {
		// Anything other than printable ASCII and \n takes the slow path.
		if c := s[i]; c != '\n' && (c < 0x20 || c >= 0x7f) {
			return false
		}
	}
	return true
}

func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func unsafe(r rune) bool {
	if isControl(r) {
		return true
	}
	switch {
	case r >= 0x202A && r <= 0x202E: // LRE RLE PDF LRO RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI RLI FSI PDI
		return true
	case r == 0x200E || r == 0x200F || r == 0x061C: // LRM RLM ALM
		return true
	case r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0: // Hangul fillers: blank "letters"
		return true
	}
	// Cf: zero-width space, word joiner, BOM, soft hyphen, invisible math
	// operators, tag characters... (ZWJ and ZWNJ are Cf too: handled above).
	// Co: private use, font/terminal specific.
	return unicode.In(r, unicode.Cf, unicode.Co)
}

// isMark is a combining mark that stacks on its base character. Variation
// selectors (emoji vs text style) are marks too but never stack visibly,
// so they don't count.
func isMark(r rune) bool {
	if unicode.Is(unicode.Variation_Selector, r) {
		return false
	}
	return unicode.In(r, unicode.Mn, unicode.Me)
}

// isEmoji approximates Extended_Pictographic (plus what may end an emoji
// before a ZWJ: a variation selector or skin tone), which Go's unicode
// tables lack.
func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, skin tones, symbols
		r >= 0x2600 && r <= 0x27BF, // misc symbols, dingbats (♀ ♂ ⚕ ❤)
		r >= 0x2300 && r <= 0x23FF,
		r >= 0x2B00 && r <= 0x2BFF,
		r == 0xFE0F, r == 0x2640, r == 0x2642, r == 0x2695:
		return true
	}
	return false
}

// joinsScript: scripts whose letters use ZWNJ to control joining or
// conjuncts (Arabic, Syriac, Indic and so on), not Latin, Greek or Cyrillic.
func joinsScript(r rune) bool {
	return r >= 0x0590 && (unicode.IsLetter(r) || unicode.In(r, unicode.Mn, unicode.Mc))
}
