package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/sanitize"
)

// Mouse: every drawn cell may name an action (canvas.go); a click runs the
// action under it, motion only moves the hover highlight. A motion that
// doesn't change what is hovered reuses the last frame, so all-motion
// reporting stays cheap.

// hoverMsg is the message under the pointer: its pane key and index.
func (m *Model) hoverMsg() (string, int, bool) {
	kind, rest, ok := strings.Cut(m.hover, ":")
	if !ok {
		return "", 0, false
	}
	switch kind {
	case "msg", "tb", "react", "addreact", "thread", "copy":
	default:
		return "", 0, false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) < 2 {
		return "", 0, false
	}
	is, _, _ := strings.Cut(parts[1], "~")
	i, err := strconv.Atoi(is)
	if err != nil {
		return "", 0, false
	}
	return parts[0], i, true
}

// dragKind is the edge the mouse is dragging.
type dragKind uint8

const (
	dragNone dragKind = iota
	dragSidebar
	dragThread
)

func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	a := ""
	if m.lastCanvas != nil {
		a = m.lastCanvas.actionAt(msg.X, msg.Y)
	}
	if msg.Action == tea.MouseActionPress && m.skipIntro() {
		return nil // a click during the intro only ends it
	}
	switch msg.Action {
	case tea.MouseActionMotion:
		if m.drag != dragNone && msg.Button == tea.MouseButtonNone {
			m.endDrag(msg.X, false) // the release went to another pane
		}
		switch m.drag {
		case dragSidebar:
			m.dragTo(msg.X)
			return nil
		case dragThread:
			m.resizeThread(m.w-msg.X, false)
			return nil
		}
		if a == m.hover {
			m.reuseFrame = true
			return nil
		}
		m.hover = a
		if i, ok := strings.CutPrefix(a, "emo:"); ok && m.mode == modeEmoji {
			if n, err := strconv.Atoi(i); err == nil && n >= 0 && n < len(m.emo.items) {
				m.emo.sel = n // one highlight for keys and pointer
			}
		}
		return nil
	case tea.MouseActionRelease:
		m.endDrag(msg.X, true)
		return nil
	}
	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelDown:
		return m.wheel(msg)
	case tea.MouseButtonLeft:
	default:
		return nil
	}
	switch a {
	case "sbresize":
		m.drag, m.dragX = dragSidebar, msg.X
		return nil
	case "thrresize":
		m.drag, m.dragX = dragThread, msg.X
		return nil
	}
	return m.click(a)
}

// endDrag finishes a drag at x: a sidebar edge click (press and release
// in place) cycles its size, a thread pane's new width is saved.
func (m *Model) endDrag(x int, release bool) {
	switch m.drag {
	case dragSidebar:
		if release && abs(float64(x-m.dragX)) < 2 {
			m.cycleSidebar()
		}
	case dragThread:
		m.savePrefs() // the width the drag left
	}
	m.drag = dragNone
}

// dragTo sizes the sidebar from where its edge is dragged.
func (m *Model) dragTo(x int) {
	mode := sbNormal
	switch {
	case x < (sbWIcons+sbWNormal)/2:
		mode = sbIcons
	case x > (sbWNormal+sbWWide)/2:
		mode = sbWide
	}
	if abs(float64(x-m.dragX)) >= 2 {
		m.dragX = -100 // a drag, not a click
		if m.w < sbIconsBelow {
			m.sbForce = mode // automatic size here: this session only
			m.layoutInput()
			return
		}
		m.setSidebar(mode)
	}
}

func (m *Model) wheel(msg tea.MouseMsg) tea.Cmd {
	up := msg.Button == tea.MouseButtonWheelUp
	switch m.mode {
	case modeActivity:
		m.activityWheel(msg.Button)
		return nil
	case modeHelp:
		if up {
			m.helpScroll = max(m.helpScroll-3, 0)
		} else {
			m.helpScroll = min(m.helpScroll+3, m.helpMaxScroll())
		}
		return nil
	case modeEmoji:
		m.pickWheel(up)
		return nil
	case modeSwitcher:
		if up && m.pal.sel > 0 {
			m.pal.sel--
		} else if !up && m.pal.sel < len(m.pal.shown)-1 {
			m.pal.sel++
		}
		return nil
	case modeNormal, modeSelect:
	default:
		// other dialogs: the wheel moves like the arrow keys
		k := tea.KeyMsg{Type: tea.KeyDown}
		if up {
			k.Type = tea.KeyUp
		}
		return m.handleKey(k)
	}
	p := m.focus()
	g := m.geo()
	if g.split && msg.X < g.RX {
		p = &m.main
	}
	if up {
		return m.scrollBy(p, 3)
	}
	return m.scrollBy(p, -3)
}

// click runs the action a.
func (m *Model) click(a string) tea.Cmd {
	kind, rest, _ := strings.Cut(a, ":")
	if m.mode == modeEmoji {
		switch kind {
		case "emo", "emotab", "emoclose":
			return m.pickMouse(kind, rest)
		case "noop":
			return nil
		}
		m.closePicker(true) // a click anywhere else closes it and still acts
	}
	switch kind {
	case "":
		return nil
	case "close":
		if m.overlayShown() {
			return m.handleKey(tea.KeyMsg{Type: tea.KeyEscape})
		}
		return nil
	case "noop":
		return nil
	case "toast":
		m.clearNotice()
		return nil
	case "chan":
		id, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			return nil
		}
		m.leaveSelect()
		return m.openChannel(id)
	case "palette":
		m.openPalette(rest)
		return nil
	case "settings":
		m.openSettings(rest)
		return nil
	case "keys":
		m.openHelp()
		return nil
	case "menu":
		return m.menuAction(rest)
	case "app":
		return m.appAction(a)
	case "status":
		m.setInfo("status · /away [status] · /back")
		return nil
	case "cmd":
		return m.appAction(rest)
	case "send":
		if m.mode == modeNormal {
			return m.submit()
		}
		return nil
	case "scroll":
		m.focus().scroll = 0
		return m.markRead()
	case "pal":
		i, _ := strconv.Atoi(rest)
		if i >= 0 && i < len(m.pal.shown) {
			m.pal.sel = i
			return m.palRun(m.pal.shown[i])
		}
		return nil
	case "paltab":
		i, _ := strconv.Atoi(rest)
		m.pal.tab = min(max(i, 0), len(palTabs)-1)
		m.filterPalette()
		return nil
	case "setpage", "set":
		return m.settingsClick(a)
	case "thread":
		if rest == "close" {
			m.closeThread()
			m.layoutInput()
			return nil
		}
	case "comp":
		i, err := strconv.Atoi(rest)
		if err != nil || !m.compShown() || i < 0 || i >= len(m.comp.items) {
			return nil
		}
		m.comp.sel = i
		cmd, _ := m.compKey(kTab)
		return cmd
	case "focus":
		if m.mode == modeSelect {
			m.leaveSelect()
		}
		return nil
	}
	// The rest act on a message: kind:pane:index[:arg].
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) < 2 {
		return nil
	}
	p := &m.main
	if parts[0] == "thr" {
		p = &m.thread
	}
	i := findRef(p.msgs, parts[1])
	if i < 0 {
		return nil // that message is gone since the frame was drawn
	}
	x := p.msgs[i]
	arg := ""
	if len(parts) == 3 {
		arg = parts[2]
	}
	switch kind {
	case "msg":
		if m.mode == modeSelect && m.focus() == p && p.sel == i {
			m.leaveSelect()
			return nil
		}
		m.selectAt(p, i)
		return nil
	case "thread":
		return m.openThreadOf(x)
	case "copy":
		return m.copyCmd(sanitize.Text(x.Body))
	case "react":
		if x.ID == 0 || x.Deleted || m.mode != modeNormal && m.mode != modeSelect {
			return nil
		}
		return m.reactWith(x, arg)
	case "addreact":
		m.selectAt(p, i)
		if m.mode == modeSelect {
			m.openPicker(true)
		}
		return nil
	case "tb":
		switch arg {
		case "reply":
			return m.openThreadOf(x)
		case "copy":
			return m.copyCmd(sanitize.Text(x.Body))
		case "react":
			m.selectAt(p, i)
			cmd, _ := m.selectKey(keyMsgRune('+'))
			return cmd
		default:
			m.selectAt(p, i)
		}
	}
	return nil
}

func keyMsgRune(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

// selectAt enters select mode on message i of p.
func (m *Model) selectAt(p *pane, i int) {
	if p == &m.main && m.thread.open() {
		return // the input and selection belong to the thread while it's open
	}
	if m.mode != modeNormal && m.mode != modeSelect {
		return
	}
	m.mode = modeSelect
	p.sel = i
	m.ensureVisible(p)
	m.updatePlaceholder()
}

func (m *Model) openThreadOf(x client.Message) tea.Cmd {
	root := x.ID
	if x.Thread != 0 {
		root = x.Thread
	}
	if root == 0 {
		return nil
	}
	m.leaveSelect()
	cmd := m.openThread(root)
	m.layoutInput()
	return cmd
}
