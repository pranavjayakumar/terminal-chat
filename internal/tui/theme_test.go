package tui

import "testing"

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
