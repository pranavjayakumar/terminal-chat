package tui

import (
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The message menu: ⋯ on the hover toolbar, or m / Enter in select mode.
// A small floating panel by the button that lists what select mode's keys
// do to the selected message, each with its key; picking a row runs that
// key, so the menu has no behavior of its own.

type msgMenuItem struct {
	icon   iconRole
	label  string
	key    string // the select-mode key it runs
	danger bool
}

type msgMenu struct {
	items []msgMenuItem
	sel   int
	ref   string // the message it acts on, as findRef reads it
	at    time.Time

	// Where the frame drew the selected message's ⋯ button (or, without a
	// toolbar, its first row's right end), set each frame by viewPane.
	anchored     bool
	ax0, ax1, ay int
}

// menuItems lists what the select-mode keys do for x, only those that do
// something, in the menu's order (the same rules as selectFooter).
func (m *Model) menuItems(x msgView) []msgMenuItem {
	if x.failed {
		return []msgMenuItem{
			{icReply, "Retry send", kSelReply, false},
			{icCopy, "Copy text", kSelCopy, false},
			{icDelete, "Discard", kSelDelete, true},
		}
	}
	var out []msgMenuItem
	if x.live {
		out = append(out, msgMenuItem{icReply, "Reply in thread", kSelReply, false}, msgMenuItem{icReact, "React", kSelReact, false})
	}
	if x.live && x.mine && x.trusted {
		out = append(out, msgMenuItem{icEdit, "Edit", kSelEdit, false})
	}
	out = append(out, msgMenuItem{icCopy, "Copy text", kSelCopy, false})
	if x.live {
		pin := "Pin"
		if x.pinned {
			pin = "Unpin"
		}
		out = append(out, msgMenuItem{icPin, pin, kSelPin, false})
	}
	if x.live && x.mine {
		out = append(out, msgMenuItem{icDelete, "Delete", kSelDelete, true})
	}
	return out
}

// msgView is what the menu needs to know about a message.
type msgView struct{ failed, live, mine, trusted, pinned bool }

// openMsgMenu opens the menu on the selected message.
func (m *Model) openMsgMenu() {
	if m.mode != modeSelect {
		return
	}
	if _, ok := m.selected(); !ok {
		return
	}
	if g := m.geo(); m.w-g.L < msgMenuMinW || g.statusY-g.top < 1 {
		m.setInfo("too small for the menu: " + m.selectFooter())
		return
	}
	m.mm = msgMenu{at: m.now()}
	m.fillMsgMenu()
	m.mode = modeMsgMenu
}

// msgMenuMinW is the narrowest the menu draws.
const msgMenuMinW = 12

// fillMsgMenu sets the rows for the selected message as it is now.
func (m *Model) fillMsgMenu() {
	p := m.focus()
	sel, ok := m.selected()
	if !ok {
		return
	}
	m.mm.items = m.menuItems(msgView{
		failed:  sel.Failed != "",
		live:    sel.ID != 0 && !sel.Deleted,
		mine:    sel.Author == m.me.ID,
		trusted: sel.Sec.Trusted(),
		pinned:  sel.Pinned,
	})
	m.mm.sel = min(m.mm.sel, len(m.mm.items)-1)
	_, m.mm.ref, _ = strings.Cut(msgRef("", p.sel, sel), ":")
}

// closeMsgMenu goes back to select mode on the same message.
func (m *Model) closeMsgMenu() {
	if m.mode != modeMsgMenu {
		return
	}
	m.mode = modeSelect
	if _, ok := m.selected(); !ok {
		m.leaveSelect()
	}
}

// runMenuItem closes the menu and runs item i's key on its message, as
// select mode would.
func (m *Model) runMenuItem(i int) tea.Cmd {
	if i < 0 || i >= len(m.mm.items) {
		return nil
	}
	key := m.mm.items[i].key
	m.closeMsgMenu()
	p := m.focus()
	j := findRef(p.msgs, m.mm.ref)
	if m.mode != modeSelect || j < 0 {
		m.leaveSelect() // the message is gone since the menu opened
		return nil
	}
	p.sel = j
	cmd, _ := m.selectKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
	return cmd
}

func (m *Model) msgMenuKey(k tea.KeyMsg) tea.Cmd {
	key := keyName(k)
	switch key {
	case kEsc, kSelMenu:
		m.closeMsgMenu()
	case kUp:
		m.mm.sel = max(m.mm.sel-1, 0)
	case kDown:
		m.mm.sel = min(m.mm.sel+1, len(m.mm.items)-1)
	case kHome:
		m.mm.sel = 0
	case kEnd:
		m.mm.sel = len(m.mm.items) - 1
	case kEnter:
		return m.runMenuItem(m.mm.sel)
	default:
		for i, it := range m.mm.items {
			if it.key == key {
				return m.runMenuItem(i)
			}
		}
		// Any other key closes the menu and does what it does in select mode.
		m.closeMsgMenu()
		return m.handleKey1(k)
	}
	return nil
}

// msgMenuRect places the menu by its anchor: right-aligned under the ⋯
// button, above it when there is no room below, never past the sidebar,
// the right edge or into the status bar. n is how many rows fit.
func (m *Model) msgMenuRect(g geo) (r rect, n int, framed bool) {
	mm := &m.mm
	if !mm.anchored || len(mm.items) == 0 {
		return rect{}, 0, false
	}
	lw, iw, kw := 0, 0, 0
	for _, it := range mm.items {
		lw = max(lw, strWidth(it.label))
		iw = max(iw, strWidth(m.icon(it.icon)))
		kw = max(kw, strWidth(it.key))
	}
	w := 1 + iw + 2 + lw + 3 + kw + 1 + 2 // padding, icon, label, gap, key, padding, frame
	lo, hi := g.L, m.w
	top, bottom := g.top, g.statusY
	room := bottom - top
	if hi-lo < msgMenuMinW || room <= 0 {
		return rect{}, 0, false
	}
	w = min(w, hi-lo)
	x := mm.ax1 - w
	if x < lo {
		x = mm.ax0 // a toolbar on the left: the menu hangs right of it
	}
	x = min(max(x, lo), hi-w)
	// Below the button, else above it; when neither holds every row, the
	// roomier side gets a shorter menu that scrolls, so the button stays
	// in sight. Only with no row free on either side does it cover it.
	ay := mm.ay
	below, above := bottom-ay-1, ay-top
	space, under := below, true
	need := len(mm.items) + 2
	switch {
	case need <= below:
	case need <= above, above > below:
		space, under = above, false
	}
	if space <= 0 {
		space, under, ay = room, false, bottom // over the button, up from the bottom
	}
	h := min(need, space)
	n, framed = h-2, true
	if h < 3 {
		n, framed = min(len(mm.items), h), false
		h = n
	}
	y := ay + 1
	if !under {
		y = ay - h
	}
	return rect{x, y, x + w, y + h}, n, framed
}

func (m *Model) drawMsgMenu(c *canvas, g geo) {
	if m.mode != modeMsgMenu {
		return
	}
	r, n, framed := m.msgMenuRect(g)
	if n <= 0 {
		return
	}
	th := m.st.th
	mm := &m.mm
	k := ease(m.progress(mm.at, dToast))
	if framed {
		m.shade(c, r)
	}
	old := c.setClip(r)
	defer c.setClip(old)
	w := r.x1 - r.x0
	pbg := mix(th.bg, th.panel, k)
	c.fill(r.x0, r.y0, w, r.y1-r.y0, pbg, "noop")
	y, x0, x1 := r.y0, r.x0, r.x1
	if framed {
		m.box(c, r.x0, r.y0, w, r.y1-r.y0, mix(th.bg, th.line, k), pbg)
		c.tag(r.x0, r.y0, w, r.y1-r.y0, "noop", false, true)
		y, x0, x1 = y+1, x0+1, x1-1
	}
	start, end := window(len(mm.items), mm.sel, n)
	for i := start; i < end; i++ {
		it := mm.items[i]
		act := "mm:" + strconv.Itoa(i)
		on := i == mm.sel // the pointer moves sel too: one highlight
		bg := pbg
		if on {
			bg = th.accent
		}
		pick := func(normal rgb, a attr) cstyle {
			col := normal
			if on {
				col, a = th.accentFg, a|aMonoRev
			}
			return cstyle{fg: mix(pbg, col, k), hasFg: true, attr: a, bg: bg, hasBg: true}
		}
		fg := th.fg
		if it.danger {
			fg = th.err
		}
		c.fill(x0, y, x1-x0, 1, bg, act)
		c.tag(x0, y, x1-x0, 1, act, false, true)
		kx := x1 - 1 - strWidth(it.key)
		x := c.put(x0+1, y, m.icon(it.icon), pick(fg, 0), act)
		c.put(x+2, y, truncate(it.label, max(kx-x-3, 0)), pick(fg, 0), act)
		c.put(kx, y, it.key, pick(th.faint, 0), act)
		y++
	}
}
