package screen

import (
	"regexp"
	"strings"
)

// Secret scrubbing: a streamed pane is shown live to other people, so
// anything that looks like a credential is masked on the streamer's side,
// before encoding. Best effort (patterns, not certainty): it catches the
// common token formats and `password=…`-style assignments, not every
// secret a pane can show.

// Mask replaces each scrubbed cell's text.
const Mask = "•"

// secretRes match a whole secret; group 1, when present, is the part to
// mask (the rest is context such as "password=").
var secretRes = []*regexp.Regexp{
	regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`),                                     // AWS access key id
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{30,}|github_pat_[A-Za-z0-9_]{22,})`),     // GitHub
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}`),                                        // GitLab
	regexp.MustCompile(`\bxox[abposr]-[A-Za-z0-9-]{10,}`),                                   // Slack
	regexp.MustCompile(`hooks\.slack\.com/services/[A-Za-z0-9/]+`),                          // Slack webhook
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),                                           // OpenAI, Anthropic (sk-ant-…)
	regexp.MustCompile(`\b[rsp]k_(?:live|test)_[A-Za-z0-9]{16,}`),                           // Stripe
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),                                           // Google API key
	regexp.MustCompile(`\bnpm_[A-Za-z0-9]{36}`),                                             // npm
	regexp.MustCompile(`\bey[JI][A-Za-z0-9_-]{8,}\.ey[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), // JWT
	regexp.MustCompile(`/hooks/([a-z2-7]{26})\b`),                                           // tc webhook URL
	regexp.MustCompile(`\b[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s:@]+:([^/\s@]+)@`),                 // URL user:password@
	regexp.MustCompile(`(?i)\b(?:bearer|basic|token)\s+([A-Za-z0-9._~+/=-]{16,})`),          // Authorization headers
	regexp.MustCompile(`(?i)(?:password|passwd|passphrase|pwd|secret|token|api[_-]?key|access[_-]?key|auth[_-]?key|client[_-]?secret|private[_-]?key|credentials?)["']?\s*[:=]\s*["']?([^\s"',;]{4,})`),
}

var (
	keyBody  = regexp.MustCompile(`^[A-Za-z0-9+/=]{16,}$`)
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
)

// Scrub masks what looks like a secret in g, in place, and returns how many
// it masked. A private key block is masked whole, rows from its BEGIN line
// to its END line (or the bottom of the pane).
func Scrub(g *Grid) int {
	var s Scrubber
	return s.Scrub(g)
}

// Scrubber is Scrub for a stream of pictures: the patterns run only on
// rows whose text changed since the last picture (a pane mostly scrolls or
// sits still, and matching every row ten times a second costs).
type Scrubber struct {
	seen, next map[string][][2]int // row text → byte ranges to mask
}

// Scrub is the package Scrub, remembering rows for the next picture.
func (sc *Scrubber) Scrub(g *Grid) int {
	n := 0
	inKey := false
	if sc.next == nil {
		sc.next = map[string][][2]int{}
	}
	// Private key blocks first, whole rows.
	done := make([]bool, g.H)
	for y := range g.H {
		text, _ := rowText(g.Row(y))
		hasKey := strings.Contains(text, "PRIVATE KEY")
		switch {
		case inKey || hasKey && pemBegin.MatchString(text):
			if !inKey {
				n++
			}
			inKey = !pemEnd.MatchString(text)
			maskCells(g.Row(y), 0, g.W)
			done[y] = true
		case hasKey && pemEnd.MatchString(text):
			// The END of a key whose BEGIN scrolled off: its body is the
			// base64 rows just above.
			n++
			maskCells(g.Row(y), 0, g.W)
			done[y] = true
			for k := y - 1; k >= 0 && !done[k]; k-- {
				t, _ := rowText(g.Row(k))
				if !keyBody.MatchString(strings.TrimSpace(t)) {
					break
				}
				maskCells(g.Row(k), 0, g.W)
				done[k] = true
			}
		}
	}
	// Then each logical line: rows joined while a row is full to its last
	// cell (a soft wrap), so a token wrapped over several rows still
	// matches and is masked on all of them.
	for y := 0; y < g.H; {
		if done[y] {
			y++
			continue
		}
		var b strings.Builder
		var at [][2]int // byte → (row, cell)
		k := y
		for ; k < g.H && !done[k]; k++ {
			row := g.Row(k)
			for x, c := range row {
				if c.W == 0 {
					continue
				}
				b.WriteString(c.S)
				for range len(c.S) {
					at = append(at, [2]int{k, x})
				}
			}
			if row[len(row)-1].S == " " && row[len(row)-1].W == 1 {
				k++
				break // not full: the line ends here
			}
		}
		text := b.String()
		ranges, ok := sc.seen[text]
		if !ok {
			ranges = secretRanges(text)
		}
		sc.next[text] = ranges
		for _, r := range ranges {
			s, e := at[r[0]], at[r[1]-1]
			fresh := false
			for row := s[0]; row <= e[0]; row++ {
				x0, x1 := 0, g.W
				if row == s[0] {
					x0 = s[1]
				}
				if row == e[0] {
					x1 = e[1] + 1
				}
				if maskCells(g.Row(row), x0, x1) {
					fresh = true
				}
			}
			if fresh {
				n++ // overlapping patterns count once
			}
		}
		y = k
	}
	sc.seen, sc.next = sc.next, sc.seen
	clear(sc.next)
	return n
}

// secretRanges lists the byte ranges of text to mask.
func secretRanges(text string) [][2]int {
	var out [][2]int
	for _, re := range secretRes {
		for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[0], m[1]
			if len(m) >= 4 && m[2] >= 0 {
				s, e = m[2], m[3]
			}
			if e > s {
				out = append(out, [2]int{s, e})
			}
		}
	}
	return out
}

// rowText is row's text with, for every byte, the cell it came from.
func rowText(row []Cell) (string, []int) {
	var b strings.Builder
	at := make([]int, 0, len(row))
	for x, c := range row {
		if c.W == 0 {
			continue
		}
		b.WriteString(c.S)
		for range len(c.S) {
			at = append(at, x)
		}
	}
	return b.String(), at
}

// maskCells masks cells [x0, x1) of row, wide ones included whole, and
// reports whether any was not masked already.
func maskCells(row []Cell, x0, x1 int) bool {
	if x0 > 0 && row[x0].W == 0 {
		x0--
	}
	if x1 < len(row) && row[x1].W == 0 {
		x1++
	}
	fresh := false
	for x := x0; x < x1; x++ {
		if row[x].S == " " && row[x].W == 1 || row[x].S == Mask {
			continue // keep the layout readable: spaces stay spaces
		}
		row[x].S, row[x].W = Mask, 1
		fresh = true
	}
	return fresh
}

// ScrubText masks what looks like a secret in one line of text (a pane's
// label), rune by rune.
func ScrubText(s string) string {
	ranges := secretRanges(s)
	if len(ranges) == 0 {
		return s
	}
	mask := make([]bool, len(s))
	for _, r := range ranges {
		for i := r[0]; i < r[1]; i++ {
			mask[i] = true
		}
	}
	var b strings.Builder
	for i, r := range s {
		if mask[i] && r != ' ' {
			b.WriteString(Mask)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
