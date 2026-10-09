package sanitize

import "strings"

// StripANSI removes CSI and OSC escape sequences (colored logs, hyperlinks,
// titles) and carriage returns, then applies Text, so the result keeps the
// readable text and is always acceptable as message text (Valid).
func StripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch s[i+1] {
			case '[': // CSI: ends at a byte in 0x40–0x7E
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				i = j
				continue
			case ']': // OSC: ends at BEL or ESC \
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j++
				}
				i = j
				continue
			}
		}
		if c == '\r' {
			continue
		}
		b.WriteByte(c)
	}
	return Text(b.String())
}
