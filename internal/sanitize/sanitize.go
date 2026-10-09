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

// Text returns s with everything that could drive a terminal removed:
// C0/C1 controls (except \n and \t), DEL, invalid UTF-8, and bidi
// override/isolate characters that can visually reorder text.
// Tabs become 4 spaces so width calculations stay predictable.
func Text(s string) string {
	if isClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		switch {
		case r == utf8.RuneError && size <= 1:
			b.WriteRune('�')
		case r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteString("    ")
		case unsafe(r):
			// drop
		default:
			b.WriteRune(r)
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
	}
	return unicode.Is(unicode.Co, r) // private use: can be font/terminal specific
}
