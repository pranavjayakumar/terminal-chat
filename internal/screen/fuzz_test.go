package screen

import (
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/skailama/tc/internal/proto"
)

// checkGrid: within limits, every wide grapheme followed by its
// continuation and no continuation without one, no control characters.
func checkGrid(t *testing.T, g *Grid) {
	t.Helper()
	if g.W < 1 || g.H < 1 || g.W > MaxCols || g.H > MaxRows || len(g.C) != g.W*g.H {
		t.Fatalf("grid %dx%d with %d cells", g.W, g.H, len(g.C))
	}
	for y := range g.H {
		row := g.Row(y)
		for x, c := range row {
			switch c.W {
			case 0:
				if x == 0 || row[x-1].W != 2 || c.S != "" {
					t.Fatalf("row %d: orphan continuation at %d", y, x)
				}
			case 1:
			case 2:
				if x+1 >= len(row) || row[x+1].W != 0 {
					t.Fatalf("row %d: wide cell at %d without continuation", y, x)
				}
			default:
				t.Fatalf("width %d", c.W)
			}
			for _, r := range c.S {
				if unicode.IsControl(r) || r == 0x1b {
					t.Fatalf("control %U in a cell", r)
				}
			}
		}
	}
}

// FuzzDecode: the viewer's decoder on hostile updates never panics, never
// grows past the limits and never yields a malformed grid.
func FuzzDecode(f *testing.F) {
	var e Encoder
	g := Parse("\x1b[1;31mred\x1b[0m 世界 ok\r\nsecond line\r\n", 30, 4)
	k, _, _ := e.Encode(g, "seed", false)
	g2 := g.Clone()
	g2.Row(1)[0] = Cell{S: "S", W: 1}
	d, _, _ := e.Encode(g2, "seed", false)
	f.Add(k, d)
	f.Add(k, []byte{})
	f.Add([]byte{0x01}, k)
	f.Fuzz(func(t *testing.T, a, b []byte) {
		var dec Decoder
		for _, u := range [][]byte{a, b, a} {
			if _, err := dec.Apply(u); err != nil {
				continue
			}
			if dec.Grid() != nil {
				checkGrid(t, dec.Grid())
			}
		}
	})
}

// FuzzRoundTrip: whatever a pane shows, the viewer rebuilds exactly the
// grid the streamer encoded, by keyframe and by diff.
func FuzzRoundTrip(f *testing.F) {
	f.Add("\x1b[32mok\x1b[0m\r\nb", "\x1b[32mOK\x1b[0m\r\nb世", uint8(20), uint8(3))
	f.Add("a\tb\x1b]8;;x\x07c", "", uint8(5), uint8(1))
	f.Fuzz(func(t *testing.T, s1, s2 string, w, h uint8) {
		cols, rows := int(w)%120+1, int(h)%30+1
		g1 := Parse(s1, cols, rows)
		g2 := Parse(s2, cols, rows)
		checkGrid(t, g1)
		checkGrid(t, g2)
		if g2.W != g1.W || g2.H != g1.H {
			g2 = resize(g2, g1.W, g1.H)
		}
		var e Encoder
		var d Decoder
		for _, g := range []*Grid{g1, g2, g1} {
			u, _, err := e.Encode(g, "p", false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Apply(u); err != nil {
				t.Fatal(err)
			}
			if !d.Grid().Equal(g) {
				t.Fatalf("round trip:\n%q\n%q", d.Grid().Text(), g.Text())
			}
		}
	})
}

func resize(g *Grid, w, h int) *Grid {
	o := NewGrid(w, h)
	for y := range min(h, g.H) {
		row := g.Row(y)[:min(w, g.W)]
		if n := len(row); n > 0 && row[n-1].W == 2 {
			row = row[:n-1]
		}
		copy(o.Row(y), row)
	}
	return o
}

// FuzzReassemble: arbitrary chunk payloads never panic or hold more than
// maxPartials updates.
func FuzzReassemble(f *testing.F) {
	cs, _ := Chunks(3, []byte(strings.Repeat("x", proto.ScreenChunkData+5)))
	f.Add(cs[0], cs[1], cs[0])
	f.Fuzz(func(t *testing.T, a, b, c []byte) {
		var r Reassembler
		now := time.Unix(0, 0)
		for _, p := range [][]byte{a, b, c, b, a} {
			if u, ok := r.Add(p, now); ok && len(u) > MaxUpdate {
				t.Fatalf("update of %d bytes", len(u))
			}
			if len(r.parts) > maxPartials {
				t.Fatal("too many partials")
			}
		}
	})
}
