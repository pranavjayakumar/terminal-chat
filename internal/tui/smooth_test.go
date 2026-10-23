package tui

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// alphaOf is how far c has come from bg toward full, 0..1, on the channel
// that moves most.
func alphaOf(c, bg, full rgb) float64 {
	cr, cg, cb := c.parts()
	br, bgg, bb := bg.parts()
	fr, fg, fb := full.parts()
	best, a := 0.0, 0.0
	for _, ch := range [][3]float64{{cr, br, fr}, {cg, bgg, fg}, {cb, bb, fb}} {
		if d := math.Abs(ch[2] - ch[1]); d > best {
			best, a = d, (ch[0]-ch[1])/(ch[2]-ch[1])
		}
	}
	return a
}

// steps runs the intro on a ~30 fps clock (33 ms frames, as the frame tick
// does) from its first frame to after its end, calling see on each.
func steps(t *testing.T, m *Model, clk *clock, see func(i int)) {
	t.Helper()
	for i := 0; i < 80 && m.introShown(); i++ {
		m.View()
		see(i)
		clk.add(33 * time.Millisecond)
		m.fx.running = false
		m.Update(frameMsg{gen: m.fx.gen})
	}
	if m.introShown() {
		t.Fatal("the intro never ended")
	}
}

// The Modern logo fades in, holds, fades out, and the chat fades in: no
// frame jumps more than a third of the way, and each phase only moves one
// way.
func TestModernIntroSmooth(t *testing.T) {
	m, _, clk := introModelSkin(t, "")
	th := m.st.th
	var logo, chat []float64
	lx, ly := -1, -1
	steps(t, m, clk, func(int) {
		c := m.lastCanvas
		if lx < 0 {
			for i := range c.cells {
				if c.cells[i].s == "█" {
					lx, ly = i%c.w, i/c.w
					break
				}
			}
		}
		cl := c.at(lx, ly)
		if cl.s == "█" {
			logo = append(logo, alphaOf(cl.fg, th.bg, th.accent))
		} else {
			chat = append(chat, alphaOf(c.at(2, c.h-1).bg, th.bg, th.accent)) // the status bar's tc pill
		}
	})
	if len(logo) < 8 || len(chat) < 6 {
		t.Fatalf("too few frames: logo %d, chat %d", len(logo), len(chat))
	}
	even(t, "logo", append([]float64{0}, logo...))
	even(t, "chat", append(chat, 1))
	t.Logf("logo alpha per 33 ms frame: %.2f", logo)
	t.Logf("chat alpha per 33 ms frame: %.2f", chat)
}

// even fails on a jump (more than a third of the range in one frame) or a
// reversal within a rise or a fall.
func even(t *testing.T, what string, a []float64) {
	t.Helper()
	dir := 0.0
	for i := 1; i < len(a); i++ {
		d := a[i] - a[i-1]
		if math.Abs(d) > 0.34 {
			t.Fatalf("%s jumps %.2f at frame %d: %.2f", what, d, i, a)
		}
		if math.Abs(d) > 0.01 {
			if dir != 0 && math.Signbit(d) != math.Signbit(dir) && !(dir > 0 && d < 0 && a[i-1] > 0.98) {
				t.Fatalf("%s reverses at frame %d: %.2f", what, i, a)
			}
			dir = d
		}
	}
}

// The Retro zoom grows every frame, without a jump.
func TestRetroZoomSmooth(t *testing.T) {
	m, _, clk := introModelSkin(t, "retro")
	var widths []float64
	steps(t, m, clk, func(int) {
		if m.fx.zoomAt.IsZero() {
			return
		}
		c := m.lastCanvas
		row := c.h / 2
		n := 0
		for x := 0; x < c.w; x++ {
			if c.at(x, row).s != "░" {
				n++
			}
		}
		widths = append(widths, float64(n)/float64(c.w))
	})
	if len(widths) < 10 {
		t.Fatalf("zoom has %d frames", len(widths))
	}
	even(t, "zoom", append(widths, 1))
	t.Logf("zoom open share per 33 ms frame: %.2f", widths)
}

func introModelSkin(t *testing.T, skin string) (*Model, *fakeClient, *clock) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ui.json")
	os.WriteFile(p, []byte(`{"skin":"`+skin+`"}`), 0o600)
	f := newFake()
	m := New(context.Background(), f, Options{Out: &syncBuf{}, Notifier: &fakeNotifier{}, styles: testStyles(true), motion: true, UIPrefs: p})
	clk := newClock(m)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.introShown() {
		t.Fatal("no intro")
	}
	_ = strings.TrimSpace
	return m, f, clk
}
