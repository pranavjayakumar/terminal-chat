// Package screen is a shared terminal pane as cells (PROTOCOL.md §14.5,
// docs/VOICE.md "Pane sharing"): parsing a captured pane, scrubbing secrets from it, and
// the key/diff update codec and chunking that carry it to viewers.
//
// Everything a viewer decodes is hostile input: the codec bounds every
// size, and cell text is sanitized on both ends, so no escape sequence or
// control character from a pane ever reaches a terminal.
package screen

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/rivo/uniseg"

	"github.com/skailama/tc/internal/sanitize"
)

// Grid limits: a pane bigger than this is cut (rows from the top, columns
// on the right).
const (
	MaxCols = 400
	MaxRows = 200
)

// Attr is a cell's text attributes.
type Attr uint8

const (
	Bold Attr = 1 << iota
	Faint
	Italic
	Underline
	Reverse
	HasFG // FG is set (else the viewer's default)
	HasBG // BG is set
)

// attrMask is every bit an update may carry.
const attrMask = Bold | Faint | Italic | Underline | Reverse | HasFG | HasBG

// Style is a cell's look; colors are 0xRRGGBB.
type Style struct {
	FG, BG uint32
	A      Attr
}

// Cell is one terminal cell. A wide grapheme fills its cell (W 2) and
// leaves W 0 and S "" in the next.
type Cell struct {
	S  string
	W  uint8
	St Style
}

var blank = Cell{S: " ", W: 1}

// Grid is a pane's visible cells, row-major.
type Grid struct {
	W, H int
	C    []Cell
}

// NewGrid is a blank w×h grid.
func NewGrid(w, h int) *Grid {
	g := &Grid{W: w, H: h, C: make([]Cell, w*h)}
	for i := range g.C {
		g.C[i] = blank
	}
	return g
}

// Row is row y's cells (aliasing g).
func (g *Grid) Row(y int) []Cell { return g.C[y*g.W : (y+1)*g.W] }

// Clone copies g.
func (g *Grid) Clone() *Grid {
	return &Grid{W: g.W, H: g.H, C: append([]Cell(nil), g.C...)}
}

// Equal reports whether g and o show the same thing.
func (g *Grid) Equal(o *Grid) bool {
	if g == nil || o == nil || g.W != o.W || g.H != o.H {
		return false
	}
	for i := range g.C {
		if g.C[i] != o.C[i] {
			return false
		}
	}
	return true
}

// Text is the grid as plain text (for copying): wide graphemes once,
// trailing spaces and blank last rows trimmed.
func (g *Grid) Text() string {
	lines := make([]string, g.H)
	for y := range g.H {
		var b strings.Builder
		for _, c := range g.Row(y) {
			if c.W > 0 {
				b.WriteString(c.S)
			}
		}
		lines[y] = strings.TrimRight(b.String(), " ")
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

// Parse turns a captured pane (`herdr pane read --format ansi`: text with
// SGR color sequences, lines ending in \r\n) into a grid of at most
// maxCols×maxRows (the last rows are kept). SGR sets styles; every other
// escape sequence and control character is dropped, and text is
// sanitized, so a grid never holds anything that could drive a terminal.
func Parse(s string, maxCols, maxRows int) *Grid {
	maxCols = min(max(maxCols, 1), MaxCols)
	maxRows = min(max(maxRows, 1), MaxRows)
	s = strings.TrimSuffix(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > maxRows {
		lines = lines[len(lines)-maxRows:]
	}
	var st Style
	rows := make([][]Cell, len(lines))
	w := 1
	for i, l := range lines {
		rows[i], st = parseLine(l, st, maxCols)
		w = max(w, len(rows[i]))
	}
	g := NewGrid(w, len(rows))
	for y, r := range rows {
		copy(g.Row(y), r)
	}
	return g
}

// parseLine lays one line's text into cells; st carries SGR state across
// lines, as a terminal does.
func parseLine(s string, st Style, maxCols int) ([]Cell, Style) {
	var row []Cell
	text := func(t string) {
		for len(t) > 0 && len(row) < maxCols {
			i := strings.IndexByte(t, '\t')
			seg := t
			if i >= 0 {
				seg, t = t[:i], t[i+1:]
			} else {
				t = ""
			}
			row = layText(row, sanitize.Line(seg), st, maxCols)
			if i >= 0 {
				for n := 8 - len(row)%8; n > 0 && len(row) < maxCols; n-- {
					row = append(row, Cell{S: " ", W: 1, St: st})
				}
			}
		}
	}
	for len(s) > 0 {
		j := strings.IndexByte(s, 0x1b)
		if j < 0 {
			text(s)
			break
		}
		text(s[:j])
		s = s[j:]
		n, params, sgr := escape(s)
		if sgr {
			st = applySGR(st, params)
		}
		s = s[n:]
	}
	return row, st
}

// escape measures the escape sequence at the start of s (s[0] is ESC):
// its length, and its parameters when it is SGR (CSI … m).
func escape(s string) (n int, params string, sgr bool) {
	if len(s) < 2 {
		return len(s), "", false
	}
	switch c := s[1]; {
	case c == '[': // CSI: parameters, intermediates, a final byte 0x40–0x7E
		j := 2
		for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
			j++
		}
		if j < len(s) && s[j] == 'm' {
			return j + 1, s[2:j], true
		}
		return min(j+1, len(s)), "", false
	case c == ']' || c == 'P' || c == '_' || c == '^' || c == 'X': // OSC/DCS/APC/PM/SOS: to BEL or ST
		j := 2
		for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
			j++
		}
		if j < len(s) && s[j] == 0x1b {
			j++
		}
		return min(j+1, len(s)), "", false
	case c >= 0x20 && c <= 0x2f: // intermediates, then a final byte
		j := 2
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		return min(j+1, len(s)), "", false
	case c < 0x80:
		return 2, "", false
	}
	_, size := utf8.DecodeRuneInString(s[1:]) // never split a character
	return 1 + size, "", false
}

// layText appends t's graphemes (already sanitized) as cells, up to
// maxCols. A wide grapheme that wouldn't fit is dropped.
func layText(row []Cell, t string, st Style, maxCols int) []Cell {
	state := -1
	for t != "" && len(row) < maxCols {
		var g string
		var w int
		g, t, w, state = uniseg.FirstGraphemeClusterInString(t, state)
		switch {
		case w == 0:
			continue
		case w > 2:
			// Wider than a wide character: one rune at a time, as
			// terminals measure it.
			for _, r := range g {
				row = layText(row, string(r), st, maxCols)
			}
			continue
		case len(row)+w > maxCols:
			return row
		}
		row = append(row, Cell{S: g, W: uint8(w), St: st})
		if w == 2 {
			row = append(row, Cell{St: st})
		}
	}
	return row
}

func applySGR(st Style, params string) Style {
	if params == "" {
		return Style{}
	}
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	if len(ps) > 32 {
		ps = ps[:32]
	}
	num := func(i int) int {
		if i >= len(ps) {
			return 0
		}
		n, _ := strconv.Atoi(ps[i])
		return n
	}
	for i := 0; i < len(ps); i++ {
		switch n := num(i); {
		case n == 0:
			st = Style{}
		case n == 1:
			st.A |= Bold
		case n == 2:
			st.A |= Faint
		case n == 3:
			st.A |= Italic
		case n == 4:
			st.A |= Underline
		case n == 7:
			st.A |= Reverse
		case n == 22:
			st.A &^= Bold | Faint
		case n == 23:
			st.A &^= Italic
		case n == 24:
			st.A &^= Underline
		case n == 27:
			st.A &^= Reverse
		case n == 39:
			st.FG, st.A = 0, st.A&^HasFG
		case n == 49:
			st.BG, st.A = 0, st.A&^HasBG
		case n == 38 || n == 48:
			var col uint32
			switch num(i + 1) {
			case 2:
				col = uint32(clamp8(num(i+2))<<16 | clamp8(num(i+3))<<8 | clamp8(num(i+4)))
				i += 4
			case 5:
				col = xterm256(num(i + 2))
				i += 2
			default:
				continue
			}
			if n == 38 {
				st.FG, st.A = col, st.A|HasFG
			} else {
				st.BG, st.A = col, st.A|HasBG
			}
		case n >= 30 && n <= 37:
			st.FG, st.A = xterm256(n-30), st.A|HasFG
		case n >= 90 && n <= 97:
			st.FG, st.A = xterm256(n-90+8), st.A|HasFG
		case n >= 40 && n <= 47:
			st.BG, st.A = xterm256(n-40), st.A|HasBG
		case n >= 100 && n <= 107:
			st.BG, st.A = xterm256(n-100+8), st.A|HasBG
		}
	}
	return st
}

func clamp8(n int) int { return min(max(n, 0), 255) }

var ansi16 = [16]uint32{0x000000, 0xcd3131, 0x0dbc79, 0xe5e510, 0x2472c8, 0xbc3fbc, 0x11a8cd, 0xe5e5e5,
	0x666666, 0xf14c4c, 0x23d18b, 0xf5f543, 0x3b8eea, 0xd670d6, 0x29b8db, 0xffffff}

func xterm256(n int) uint32 {
	switch {
	case n < 16:
		return ansi16[max(n, 0)]
	case n < 232:
		n -= 16
		lv := func(v int) uint32 {
			if v == 0 {
				return 0
			}
			return uint32(55 + v*40)
		}
		return lv(n/36)<<16 | lv(n/6%6)<<8 | lv(n%6)
	case n < 256:
		v := uint32(8 + (n-232)*10)
		return v<<16 | v<<8 | v
	}
	return 0
}
