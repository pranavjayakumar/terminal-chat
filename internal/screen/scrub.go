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
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY[A-Z ]*-----`)
)

// Scrub masks what looks like a secret in g, in place, and returns how many
// it masked. A private key block is masked whole, rows from its BEGIN line
// to its END line (or the bottom of the pane).
func Scrub(g *Grid) int {
	n := 0
	inKey := false
	for y := range g.H {
		row := g.Row(y)
		text, at := rowText(row)
		if inKey || pemBegin.MatchString(text) {
			if !inKey {
				n++
			}
			inKey = !pemEnd.MatchString(text)
			maskCells(row, 0, len(row))
			continue
		}
		for _, re := range secretRes {
			for _, m := range re.FindAllStringSubmatchIndex(text, -1) {
				s, e := m[0], m[1]
				if len(m) >= 4 && m[2] >= 0 {
					s, e = m[2], m[3]
				}
				if e <= s {
					continue
				}
				maskCells(row, at[s], at[e-1]+1)
				n++
			}
		}
	}
	return n
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

// maskCells masks cells [x0, x1) of row, wide ones included whole.
func maskCells(row []Cell, x0, x1 int) {
	if x0 > 0 && row[x0].W == 0 {
		x0--
	}
	if x1 < len(row) && row[x1].W == 0 {
		x1++
	}
	for x := x0; x < x1; x++ {
		if row[x].S == " " && row[x].W == 1 {
			continue // keep the layout readable: spaces stay spaces
		}
		row[x].S, row[x].W = Mask, 1
	}
}
