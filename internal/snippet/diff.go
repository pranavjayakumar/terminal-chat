// Package snippet builds code-sharing messages: fenced blocks from files
// (with line ranges and permalinks) and unified-diff detection. It does no
// terminal I/O; callers sanitize at render time as usual.
package snippet

import "strings"

// LooksLikeDiff reports whether s is a unified diff: it starts with a
// "diff --git" line, or has "--- "/"+++ " header lines followed by an "@@ -"
// hunk. CRLF is accepted. Prose that merely contains "---" does not match.
func LooksLikeDiff(s string) bool {
	first := true
	var prevMinus, sawHeader bool
	for len(s) > 0 {
		var ln string
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			ln, s = s[:i], s[i+1:]
		} else {
			ln, s = s, ""
		}
		ln = strings.TrimSuffix(ln, "\r")
		if first {
			if ln == "" {
				continue // leading blank lines
			}
			first = false
			if strings.HasPrefix(ln, "diff --git ") {
				return true
			}
		}
		switch {
		case sawHeader && strings.HasPrefix(ln, "@@ -"):
			return true
		case prevMinus && strings.HasPrefix(ln, "+++ "):
			sawHeader = true
			prevMinus = false
		case strings.HasPrefix(ln, "--- "):
			prevMinus = true
		default:
			prevMinus = false
		}
	}
	return false
}
