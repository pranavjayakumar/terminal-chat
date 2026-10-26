package tui

import (
	"errors"
	"maps"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/iconfont"
	"github.com/skailama/tc/internal/proto"
)

// Icons: every icon the UI draws for these roles comes from one table, in
// three packs. tc Symbols is tc's own font (internal/iconfont), drawn where
// the terminal's font fallback finds it; Unicode and DOS are plain
// characters that every terminal has.

type iconRole uint8

const (
	icTC iconRole = iota
	icChannel
	icPrivate
	icPerson
	icHuddle
	icMic
	icMuted
	icAgent
	icE2E
	icSearch
	icPin
	icSettings
	icReact
	icReply
	icCopy
	icMore
	icThread
	icFile
	icImage
	icCode
	icBell
	icAway
	icOnline
	icNotes
	icTalking
	icEdit
	icDelete
	nIcons
)

// iconTable is role → its tc Symbols code point and its Unicode and DOS
// fallbacks.
var iconTable = [nIcons]struct {
	name     string
	sym      rune
	uni, dos string
}{
	icTC:       {"tc", 0xE400, "≡", "≡"},
	icChannel:  {"channel", 0xE401, "#", "#"},
	icPrivate:  {"private", 0xE402, "◆", "♦"},
	icPerson:   {"person", 0xE403, "◉", "☻"},
	icHuddle:   {"huddle", 0xE404, "♪", "♫"},
	icMic:      {"mic", 0xE405, "●", "•"},
	icMuted:    {"muted", 0xE406, "⊘", "x"},
	icAgent:    {"agent", 0xE407, "◇", "☼"},
	icE2E:      {"e2e", 0xE408, "◆", "♦"},
	icSearch:   {"search", 0xE409, "⌕", "►"},
	icPin:      {"pin", 0xE40A, "¶", "¶"},
	icSettings: {"settings", 0xE40C, "⚙", "☼"},
	icReact:    {"react", 0xE40D, "☺", "☺"},
	icReply:    {"reply", 0xE40E, "↩", "◄"},
	icCopy:     {"copy", 0xE40F, "⧉", "◘"},
	icMore:     {"more", 0xE410, "⋯", "…"},
	icThread:   {"thread", 0xE411, "↳", "►"},
	icFile:     {"file", 0xE412, "▤", "≡"},
	icImage:    {"image", 0xE413, "▣", "■"},
	icCode:     {"code", 0xE414, "‹›", "«»"},
	icBell:     {"bell", 0xE415, "♪", "♪"},
	icAway:     {"away", 0xE416, "◐", "◑"}, // offline DMs are ○: away must differ without color
	icOnline:   {"online", 0xE417, "●", "☻"},
	icNotes:    {"notes", 0xE418, "▪", "■"},
	icTalking:  {"talking", 0xE419, "▄", "▬"},
	icEdit:     {"edit", 0xE41A, "✎", "»"},
	icDelete:   {"delete", 0xE41B, "✕", "x"},
}

// Icon packs, as Settings › Appearance › Icons names them; "" is Auto.
const (
	packAuto    = ""
	packSymbols = "symbols"
	packUnicode = "unicode"
	packDOS     = "dos"
)

var packOrder = []string{packAuto, packSymbols, packUnicode, packDOS}

func packLabel(p string) string {
	return map[string]string{packAuto: "Auto", packSymbols: "tc Symbols", packUnicode: "Unicode", packDOS: "DOS"}[p]
}

// IconRow is one icon in every pack, for tc fonts test.
type IconRow struct{ Name, Symbol, Unicode, DOS string }

// IconRows lists every icon.
func IconRows() []IconRow {
	out := make([]IconRow, 0, nIcons)
	for _, ic := range iconTable {
		out = append(out, IconRow{ic.name, string(ic.sym), ic.uni, ic.dos})
	}
	return out
}

func iconIn(pack string, r iconRole) string {
	ic := iconTable[r]
	switch pack {
	case packSymbols:
		return string(ic.sym)
	case packDOS:
		return ic.dos
	}
	return ic.uni
}

// icon is role r in the pack in use.
func (m *Model) icon(r iconRole) string { return iconIn(m.pack, r) }

// resolvePack works out the pack in use: the chosen one, or for Auto, tc
// Symbols when the font is installed here, the terminal finds it per glyph
// (symbolsFallback), we aren't over SSH (the font would have to be on the
// other end) and the locale doesn't make ambiguous characters wide;
// otherwise Unicode, or DOS with the Retro skin.
func (m *Model) resolvePack() {
	pack := m.ui.Icons
	if pack == packAuto {
		pack = packUnicode
		switch {
		case m.fontInstalled() && m.opt.FontFallback && !m.opt.SSH && !cjkLocale():
			pack = packSymbols
		case m.skin().id == "retro":
			pack = packDOS
		}
	}
	if pack != m.pack {
		m.pack = pack
		m.blocks.reset()
		m.main.ver++
		m.thread.ver++
	}
}

// fontInstalled is whether the font was on this machine when tc started
// (checked once: View must not touch the disk). A font installed since
// shows from the next start, when the terminal has had a chance to load it.
func (m *Model) fontInstalled() bool { return m.fontHave }

// checkFont looks for the font once, at start.
func (m *Model) checkFont() {
	m.fontHave = m.opt.Font.Installed != nil && m.opt.Font.Installed()
}

// refreshFontCmd replaces an out-of-date installed font with this build's,
// quietly, so new icons never show as boxes (tc update does the same for
// installs it manages; Homebrew's and the TUI's need it here).
func (m *Model) refreshFontCmd() tea.Cmd {
	refresh := m.opt.Font.Refresh
	if refresh == nil || !m.fontHave {
		return nil
	}
	return func() tea.Msg {
		refresh()
		return nil
	}
}

// cjkLocale reports a CJK locale, where terminals often draw East Asian
// ambiguous characters two columns wide: Private Use glyphs then misalign.
func cjkLocale() bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" {
			return strings.HasPrefix(v, "ja") || strings.HasPrefix(v, "ko") || strings.HasPrefix(v, "zh")
		}
	}
	return os.Getenv("RUNEWIDTH_EASTASIAN") == "1"
}

// FontHooks lets the UI check for and install the icon font.
type FontHooks struct {
	Installed func() bool
	Install   func() error
	Refresh   func() // replace an out-of-date font; best effort
}

// osFont is the real font on this machine.
var osFont = FontHooks{
	Installed: iconfont.Installed,
	Install:   func() error { _, err := iconfont.Install(); return err },
	Refresh:   func() { iconfont.Refresh() },
}

// setIcons picks the icon pack.
func (m *Model) setIcons(pack string) {
	m.ui.Icons = pack
	m.savePrefs()
	m.resolvePack()
}

// iconFontInstalledMsg reports an install from Settings.
type iconFontInstalledMsg struct{ err error }

// installIconFont installs the font from Settings, off the event loop.
func (m *Model) installIconFont() tea.Cmd {
	install := m.opt.Font.Install
	if install == nil {
		m.setErr("", errors.New("tc can't install fonts from here: run tc fonts install"))
		return nil
	}
	return func() tea.Msg { return iconFontInstalledMsg{install()} }
}

// iconFontInstalled reports an install. The icons in use stay as they are
// for this session: a running terminal often can't draw a font installed
// after it started, and switching now would turn every icon into a box.
func (m *Model) iconFontInstalled(msg iconFontInstalledMsg) {
	if msg.err != nil {
		m.setErr("install tc icons", msg.err)
		return
	}
	m.fontHave = true
	if m.mode == modeSettings {
		for i, c := range m.setCtls() {
			if c.label == "Icons" {
				m.set.row = i // the button's row is gone: keep the focus on Icons
			}
		}
	}
	if !m.opt.FontFallback {
		m.setInfo("installed · if your terminal shows them, pick tc Symbols in Settings › Icons")
		return
	}
	m.setInfo("installed · restart your terminal, then tc uses its icons")
}

// iconsHint tells someone without the font, once, that it exists.
func (m *Model) iconsHint() {
	if m.ui.IconsHint || m.opt.Font.Installed == nil || m.fontInstalled() || m.opt.SSH || !m.opt.FontFallback {
		return
	}
	m.ui.IconsHint = true
	m.savePrefs()
	m.setInfo("New: tc icons · Settings › Icons")
}

// symbolsFallback reports whether the terminal finds a glyph missing from
// its own font in another installed font, the tc Symbols one included.
// Only terminals known to, and never inside a multiplexer: herdr, tmux and
// the like (and terminals embedded in editors, such as Zed's) draw tc
// Symbols' private-use code points as boxes.
func symbolsFallback(env func(string) string) bool {
	if env("TMUX") != "" || env("STY") != "" || env("HERDR_PANE_ID") != "" || env("ZELLIJ") != "" {
		return false
	}
	switch env("TERM_PROGRAM") {
	case "iTerm.app", "Apple_Terminal", "ghostty", "WezTerm", "kitty":
		return true
	case "":
	default:
		return false // a terminal that names itself, but not one of those
	}
	switch env("TERM") {
	case "xterm-kitty", "xterm-ghostty", "wezterm":
		return true
	}
	return false
}

// underlineStyles reports whether the terminal draws curly and dotted
// underlines in their own color. Only terminals known to, and never inside
// a multiplexer that may drop or garble the sequences.
func underlineStyles(env func(string) string) bool {
	if env("TMUX") != "" || env("STY") != "" || env("HERDR_PANE_ID") != "" {
		return false
	}
	switch env("TERM_PROGRAM") {
	case "WezTerm", "ghostty", "iTerm.app", "kitty":
		return true
	}
	switch env("TERM") {
	case "xterm-kitty", "xterm-ghostty", "wezterm":
		return true
	}
	return env("KITTY_WINDOW_ID") != ""
}

// touches reports whether mentions and channel links get styled
// underlines: Settings › Appearance › Underlines, Auto by detection.
func (m *Model) touches() bool {
	switch m.ui.Touches {
	case "on":
		return m.st.color
	case "off":
		return false
	}
	return m.opt.Touches && m.st.color
}

// iconsCtl is Settings › Appearance › Icons.
func (m *Model) iconsCtl() setCtl {
	opts := make([]string, len(packOrder))
	cur := 0
	for i, p := range packOrder {
		opts[i] = packLabel(p)
		if p == m.ui.Icons {
			cur = i
		}
	}
	sub := "Auto: tc Symbols in terminals that can show them"
	if m.opt.SSH {
		sub = "over SSH: run tc fonts install on your own computer"
	}
	return setCtl{label: "Icons", sub: sub, opts: opts, cur: cur, preview: true,
		pick: func(i int) tea.Cmd { m.setIcons(packOrder[i]); return nil }}
}

// previewRoles are the icons Settings shows as the pack's preview.
var previewRoles = []iconRole{icChannel, icPrivate, icPerson, icHuddle, icMic, icMuted, icSearch, icPin,
	icSettings, icReact, icReply, icCopy, icMore, icThread, icCode, icFile, icAway, icOnline, icTalking}

// drawIconPreview draws the pack in use as a row of icons and what Auto
// picked.
func (m *Model) drawIconPreview(c *canvas, x, y, w int) {
	th := m.st.th
	end := x + w
	for _, r := range previewRoles {
		g := m.icon(r)
		if x+strWidth(g)+1 > end-16 {
			break
		}
		x = c.put(x, y, g, fgc(th.fg), "") + 1
	}
	note := packLabel(m.pack)
	if m.ui.Icons == packAuto {
		note = "auto → " + note
	}
	if x+2+strWidth(note) <= end {
		c.put(x+1, y, note, fgc(th.faint), "")
	}
}

// setTouches picks the styled underlines: "" auto, "on", "off".
func (m *Model) setTouches(v string) {
	m.ui.Touches = v
	m.savePrefs()
	m.blocks.reset()
	m.main.ver++
	m.thread.ver++
}

// chanNamed is the id of the non-DM channel called name, or 0.
func (m *Model) chanNamed(name string) uint64 {
	return m.chanIDs[name]
}

// noteChanNames indexes channel names for #links; messages are redrawn only
// when the set of names changes (a channel created, renamed or gone).
func (m *Model) noteChanNames() {
	ids := make(map[string]uint64, len(m.chans))
	for _, v := range m.chans {
		if v.Kind != proto.ChanDM && v.Name != "" {
			ids[v.Name] = v.ID
		}
	}
	if maps.Equal(ids, m.chanIDs) {
		return
	}
	m.chanIDs = ids
	m.blocks.reset()
	m.main.ver++
	m.thread.ver++
}
