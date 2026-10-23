package tui

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
)

// rgb is a 24-bit color, 0xRRGGBB.
type rgb uint32

func hexColor(s string) rgb {
	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil || len(s) != 7 {
		panic("bad color " + s)
	}
	return rgb(v)
}

func (c rgb) parts() (r, g, b float64) {
	return float64(c >> 16 & 0xff), float64(c >> 8 & 0xff), float64(c & 0xff)
}

func (c rgb) String() string { return fmt.Sprintf("#%06x", uint32(c)) }

// mix is a + (b-a)*p per channel, p clamped to [0, 1].
func mix(a, b rgb, p float64) rgb {
	p = min(max(p, 0), 1)
	ar, ag, ab := a.parts()
	br, bg, bb := b.parts()
	ch := func(x, y float64) uint32 { return uint32(math.Round(x + (y-x)*p)) }
	return rgb(ch(ar, br)<<16 | ch(ag, bg)<<8 | ch(ab, bb))
}

// luminance is the WCAG relative luminance.
func (c rgb) luminance() float64 {
	lin := func(v float64) float64 {
		v /= 255
		if v <= 0.03928 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	r, g, b := c.parts()
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// hue is c's hue in degrees (0–360).
func (c rgb) hue() float64 {
	r, g, b := c.parts()
	mx, mn := max(r, g, b), min(r, g, b)
	if mx == mn {
		return 0
	}
	d := mx - mn
	var h float64
	switch mx {
	case r:
		h = math.Mod((g-b)/d, 6)
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	h *= 60
	if h < 0 {
		h += 360
	}
	return h
}

// contrast is the WCAG contrast ratio of two colors (1 to 21).
func contrast(a, b rgb) float64 {
	la, lb := a.luminance(), b.luminance()
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// theme is one color scheme. Every color the UI draws comes from here.
type theme struct {
	name, desc                   string
	bg, panel, element, line     rgb
	fg, dim, faint               rgb
	accent, accentFg, accentBg   rgb
	info, ok, err, warn          rgb
	addBg, delBg, add, del, hunk rgb
	nicks                        [8]rgb
	light                        bool
	// dialog, when set, recolors dialogs: QBasic's gray boxes on blue.
	dialog *theme
}

// nick is the muted name color for a user id: stable across runs.
func (t *theme) nick(id uint64) rgb {
	h := fnv.New32a()
	h.Write([]byte(strconv.FormatUint(id, 10)))
	return t.nicks[h.Sum32()%uint32(len(t.nicks))]
}

const defaultTheme = "graphite"

// themeOrder is how the palette and Settings list the built-in themes.
var themeOrder = []string{"graphite", "sky", "mint", "rose", "lilac", "ember", "phosphor", "paper", "teletype", "macintosh",
	"turbo", "norton", "qbasic", "night", "paper-retro"}

// retroNicks reads on every dark retro blue: eight hues at least 22° apart.
var retroNicks = [8]string{"#ffe14d", "#5ff0f0", "#ff9ce6", "#9ff07a", "#ffa860", "#ff8a8a", "#b8a8ff", "#6cf0b8"}

var themes = buildThemes()

type themeSpec struct {
	name, desc                                      string
	bg, panel, element, line, fg, dim, faint        string
	accent, accentFg, accentBg, info, ok, err, warn string
	addBg, delBg, add, del, hunk                    string
	nicks                                           [8]string
	light                                           bool
}

func (s themeSpec) build() *theme {
	t := &theme{name: s.name, desc: s.desc, light: s.light,
		bg: hexColor(s.bg), panel: hexColor(s.panel), element: hexColor(s.element), line: hexColor(s.line),
		fg: hexColor(s.fg), dim: hexColor(s.dim), faint: hexColor(s.faint),
		accent: hexColor(s.accent), accentFg: hexColor(s.accentFg), accentBg: hexColor(s.accentBg),
		info: hexColor(s.info), ok: hexColor(s.ok), err: hexColor(s.err), warn: hexColor(s.warn),
		addBg: hexColor(s.addBg), delBg: hexColor(s.delBg), add: hexColor(s.add), del: hexColor(s.del), hunk: hexColor(s.hunk)}
	for i, n := range s.nicks {
		t.nicks[i] = hexColor(n)
	}
	return t
}

func buildThemes() map[string]*theme {
	type spec = themeSpec
	graphite := spec{name: "graphite", desc: "neutral grays, peach accent",
		bg: "#0b0b0c", panel: "#141416", element: "#1c1c1f", line: "#2a2a2e", fg: "#ececec", dim: "#8b8b92", faint: "#5d5d64",
		accent: "#fab283", accentFg: "#1a1208", accentBg: "#2a2018", info: "#7fb4f0", ok: "#7fd88f", err: "#e06c75", warn: "#e5c07b",
		addBg: "#122019", delBg: "#24151a", add: "#7fd88f", del: "#e06c75", hunk: "#7fb4f0",
		nicks: [8]string{"#f2a07b", "#e3c46e", "#a9d47a", "#74d4a8", "#6fc8d8", "#86a8ec", "#b49cf0", "#e89cc8"}}
	variant := func(name, desc, accent, accentFg, accentBg string, f func(*spec)) spec {
		s := graphite
		s.name, s.desc, s.accent, s.accentFg, s.accentBg = name, desc, accent, accentFg, accentBg
		if f != nil {
			f(&s)
		}
		return s
	}
	specs := []spec{
		graphite,
		variant("sky", "graphite, sky blue accent", "#7fb4f0", "#08121c", "#16202c", func(s *spec) {
			s.info, s.hunk = "#b9a2f0", "#b9a2f0"
		}),
		variant("mint", "graphite, mint accent", "#7fd8a8", "#06140d", "#13241c", nil),
		variant("rose", "graphite, rose accent", "#f0a0b8", "#1c0a10", "#2a1820", nil),
		variant("lilac", "graphite, lilac accent", "#b9a2f0", "#120c1e", "#1f1a2c", nil),
		{name: "ember", desc: "warm charcoal, amber accent",
			bg: "#120f0d", panel: "#1a1613", element: "#231e1a", line: "#342c26", fg: "#efe6dc", dim: "#a2978b", faint: "#6d645b",
			accent: "#f0a35e", accentFg: "#1a0f05", accentBg: "#2d2116", info: "#86b6c9", ok: "#a3c585", err: "#e27d6f", warn: "#e6c07b",
			addBg: "#1b2216", delBg: "#2a1714", add: "#a3c585", del: "#e27d6f", hunk: "#86b6c9",
			nicks: [8]string{"#e8946a", "#d6c27a", "#a9c585", "#7fbfa5", "#86b6c9", "#9aa4dc", "#c49ad6", "#e08f9a"}},
		{name: "phosphor", desc: "black glass, green CRT accent",
			bg: "#070b08", panel: "#0d130f", element: "#141c16", line: "#1f2b22", fg: "#d6f5d0", dim: "#86a883", faint: "#55705a",
			accent: "#7cf29a", accentFg: "#04120a", accentBg: "#13261a", info: "#7fd8e8", ok: "#7cf29a", err: "#ff8a7a", warn: "#e8e07a",
			addBg: "#10261a", delBg: "#2a1513", add: "#7cf29a", del: "#ff8a7a", hunk: "#7fd8e8",
			nicks: [8]string{"#7cf29a", "#e8e07a", "#f0b070", "#f08aa8", "#c3a0f0", "#8ab4f8", "#7fd8e8", "#b0f070"}},
		{name: "paper", desc: "cream and ink, rust accent", light: true,
			bg: "#f4efe4", panel: "#ebe4d6", element: "#e0d8c6", line: "#cfc5b0", fg: "#25211b", dim: "#5f574b", faint: "#857a69",
			accent: "#b4441c", accentFg: "#fff8ef", accentBg: "#f3dccd", info: "#1f5f8b", ok: "#2d7a3a", err: "#b42318", warn: "#8a5a00",
			addBg: "#dcefd6", delBg: "#f6dad4", add: "#2d7a3a", del: "#b42318", hunk: "#1f5f8b",
			nicks: [8]string{"#b4441c", "#8a6100", "#3f6b12", "#146a4c", "#1f6f8b", "#3a52b0", "#7a3e9d", "#a3306b"}},
		{name: "teletype", desc: "typewriter ink, red ribbon", light: true,
			bg: "#efece3", panel: "#e6e2d6", element: "#dbd6c7", line: "#c9c3b2", fg: "#1c1c1a", dim: "#55534c", faint: "#7c786d",
			accent: "#c0392b", accentFg: "#fff6f0", accentBg: "#f2d6cf", info: "#335c99", ok: "#2f6b3a", err: "#c0392b", warn: "#8a6100",
			addBg: "#d9ead2", delBg: "#f2d6cf", add: "#2f6b3a", del: "#c0392b", hunk: "#335c99",
			nicks: [8]string{"#b4441c", "#8a6100", "#3f6b12", "#146a4c", "#1f6f8b", "#3a52b0", "#7a3e9d", "#a3306b"}},
		{name: "macintosh", desc: "1-bit black on white", light: true,
			bg: "#fbfbfb", panel: "#f0f0f0", element: "#e2e2e2", line: "#bdbdbd", fg: "#111111", dim: "#4f4f4f", faint: "#767676",
			accent: "#111111", accentFg: "#fbfbfb", accentBg: "#e6e6e6", info: "#2a4fb5", ok: "#2e7d32", err: "#c62828", warn: "#8d6e00",
			addBg: "#e3f1e3", delBg: "#f6e0e0", add: "#2e7d32", del: "#c62828", hunk: "#2a4fb5",
			nicks: [8]string{"#b4441c", "#8a6100", "#3f6b12", "#146a4c", "#1f6f8b", "#3a52b0", "#7a3e9d", "#a3306b"}},
		// DOS-era palettes, made for the Retro skin and fine in any.
		{name: "turbo", desc: "Turbo Vision blue, yellow accent",
			bg: "#10237a", panel: "#0c1b63", element: "#2a43a8", line: "#7d93e6", fg: "#f2f4ff", dim: "#b9c4f0", faint: "#8494d4",
			accent: "#ffe14d", accentFg: "#10237a", accentBg: "#26389a", info: "#5ff0f0", ok: "#7cf08a", err: "#ff6e6e", warn: "#ffe14d",
			addBg: "#163a6e", delBg: "#3a1f5e", add: "#7fd88f", del: "#e06c75", hunk: "#5ff0f0",
			nicks: retroNicks},
		{name: "norton", desc: "Norton Commander panels, cyan accent",
			bg: "#0b2a8a", panel: "#082070", element: "#1d40a8", line: "#5f8ae0", fg: "#e6f6ff", dim: "#9fd0ee", faint: "#7398cc",
			accent: "#55ffff", accentFg: "#000000", accentBg: "#14409e", info: "#ffff55", ok: "#55ff55", err: "#ff6b6b", warn: "#ffff55",
			addBg: "#0f4a6e", delBg: "#3a1f5e", add: "#55ff55", del: "#ff7f7f", hunk: "#ffff55",
			nicks: [8]string{"#55ffff", "#ffff55", "#55ff55", "#ff8af0", "#ffaa55", "#ff7f7f", "#b0b0ff", "#7fffbf"}},
		{name: "qbasic", desc: "MS-DOS Edit blue, gray dialogs",
			bg: "#0000a8", panel: "#000088", element: "#2020c8", line: "#6a6af0", fg: "#ffffff", dim: "#b4b4b4", faint: "#8484d8",
			accent: "#c0c0c0", accentFg: "#000000", accentBg: "#1c1cb8", info: "#55ffff", ok: "#55ff55", err: "#ff6b6b", warn: "#ffff55",
			addBg: "#00589a", delBg: "#58208a", add: "#55ff55", del: "#ff8080", hunk: "#55ffff",
			nicks: [8]string{"#ffff55", "#55ffff", "#ff9ce6", "#55ff55", "#ffaa55", "#ff8a8a", "#c0b0ff", "#7fffbf"}},
		{name: "night", desc: "near-black, yellow and cyan",
			bg: "#0a0d18", panel: "#0f1322", element: "#1c2338", line: "#3c4870", fg: "#e8ecf8", dim: "#a2aacb", faint: "#6c7699",
			accent: "#ffe14d", accentFg: "#0a0d18", accentBg: "#2a2812", info: "#5ff0f0", ok: "#7cf08a", err: "#ff6e6e", warn: "#ffb86c",
			addBg: "#10261a", delBg: "#2a1513", add: "#7fd88f", del: "#e06c75", hunk: "#5ff0f0",
			nicks: retroNicks},
		{name: "paper-retro", desc: "Windows 3.1 gray, navy text", light: true,
			bg: "#c0c0c0", panel: "#b4b4b4", element: "#a2a2a2", line: "#7a7a7a", fg: "#000050", dim: "#1c1c60", faint: "#46467a",
			accent: "#0000c0", accentFg: "#ffffff", accentBg: "#a8a8d8", info: "#005a6e", ok: "#005c00", err: "#a00000", warn: "#6e5000",
			addBg: "#a8d0a8", delBg: "#d8a8a8", add: "#005c00", del: "#a00000", hunk: "#00008b",
			nicks: [8]string{"#8a0000", "#7a3a00", "#4e4e00", "#1e5a00", "#00583a", "#004e6e", "#3030b0", "#7a007a"}},
	}
	out := make(map[string]*theme, len(specs))
	for _, s := range specs {
		out[s.name] = s.build()
	}
	out["qbasic"].dialog = spec{name: "qbasic", desc: "dialogs", light: true,
		bg: "#0000a8", panel: "#aaaaaa", element: "#c4c4c4", line: "#555555", fg: "#000000", dim: "#2a2a2a", faint: "#484848",
		accent: "#0000a8", accentFg: "#ffffff", accentBg: "#c8c8e8", info: "#0000a8", ok: "#005c00", err: "#a00000", warn: "#5c4400",
		addBg: "#a8d0a8", delBg: "#d8a8a8", add: "#005c00", del: "#a00000", hunk: "#00008b",
		nicks: [8]string{"#7a0000", "#6a3200", "#424200", "#184a00", "#004a30", "#00405a", "#28289a", "#6a006a"}}.build()
	return out
}

// themeNamed is the built-in theme called name, or the default.
func themeNamed(name string) *theme {
	if t, ok := themes[name]; ok {
		return t
	}
	return themes[defaultTheme]
}
