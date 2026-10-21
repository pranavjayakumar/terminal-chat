package tui

import (
	"math"
	"testing"

	"github.com/muesli/termenv"
)

// Dim text must stay readable and faint text visible on every theme's
// backgrounds, the light ones included.
func TestThemeContrast(t *testing.T) {
	if len(themes) != len(themeOrder) || themeNamed("nope").name != "graphite" {
		t.Fatal("theme table and order disagree, or the default is not graphite")
	}
	for _, name := range themeOrder {
		th := themes[name]
		for _, bg := range []rgb{th.bg, th.panel} {
			if c := contrast(th.fg, bg); c < 7 {
				t.Errorf("%s: fg on %v is %.2f", name, bg, c)
			}
			if c := contrast(th.dim, bg); c < 4.5 {
				t.Errorf("%s: dim on %v is %.2f", name, bg, c)
			}
			minFaint := 2.6
			if th.light {
				minFaint = 3 // light terminals wash out thin glyphs more
			}
			if c := contrast(th.faint, bg); c < minFaint {
				t.Errorf("%s: faint on %v is %.2f", name, bg, c)
			}
		}
		if c := contrast(th.accentFg, th.accent); c < 4.5 {
			t.Errorf("%s: accent pill text is %.2f", name, c)
		}
	}
}

// On 256-color terminals a dark theme's surfaces stay distinct steps.
func TestSurfacesSurvive256(t *testing.T) {
	for _, name := range themeOrder {
		th := themes[name]
		seen := map[int]string{}
		for _, s := range []struct {
			n string
			c rgb
		}{{"bg", th.bg}, {"panel", th.panel}, {"element", th.element}, {"line", th.line}} {
			e := newEncoder(termenv.ANSI256, true)
			e.pinSurfaces(th)
			i := e.pinned[s.c]
			if prev, dup := seen[i]; dup {
				t.Errorf("%s: %s and %s both become %d", name, prev, s.n, i)
			}
			seen[i] = s.n
		}
	}
}

// Every theme's name colors are clearly different hues and readable on its
// background, so people never look alike (not all green in phosphor).
func TestNamePalettes(t *testing.T) {
	for _, name := range themeOrder {
		th := themes[name]
		min := 3.0
		if th.light {
			min = 4.5
		}
		for i, a := range th.nicks {
			if c := contrast(a, th.bg); c < min {
				t.Errorf("%s: name color %v on bg is %.2f", name, a, c)
			}
			for _, b := range th.nicks[i+1:] {
				d := math.Abs(a.hue() - b.hue())
				d = math.Min(d, 360-d)
				if d < 22 {
					t.Errorf("%s: %v and %v are only %.0f° apart", name, a, b, d)
				}
			}
		}
	}
}
