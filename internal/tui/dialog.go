package tui

// Dialogs: one shell (dimmed screen, centered panel) for the palette,
// settings, the keys sheet and every older overlay (search, activity,
// lists, the viewer, verify, devices).

// overlayShown reports whether a dialog covers the chat.
func (m *Model) overlayShown() bool {
	switch m.mode {
	case modeNormal, modeSelect, modeConfirmDelete, modeEmoji:
		return false
	case modeViewer:
		return !m.view.wide
	}
	return true
}

// dialogBox is a dialog's panel for a wanted size, shrunk to the screen.
func (m *Model) dialogBox(dw, dh int) rect {
	dw = min(dw, m.w-2)
	dh = min(dh, m.h-2)
	if m.w < 50 {
		dw = m.w
	}
	if m.h < 16 {
		dh = m.h - 1
	}
	dw, dh = max(dw, 1), max(dh, 1)
	x := (m.w - dw) / 2
	y := (m.h - dh) / 2
	if m.h-dh >= 4 {
		y-- // optically centered; a near-full panel stays balanced instead
	}
	return rect{x, y, x + dw, y + dh}
}

// dialog dims the screen and draws an empty panel; it returns the panel.
func (m *Model) dialog(c *canvas, dw, dh int, title string) rect {
	th := m.st.th
	op := ease(m.progress(m.fx.dialogAt, dDialog))
	c.each(0, 0, c.w, c.h, func(cl *cell) {
		cl.fg = mix(cl.fg, th.bg, 0.6*op)
		cl.ul = mix(cl.ul, th.bg, 0.6*op)
		cl.bg = mix(cl.bg, 0x000000, 0.35*op)
		cl.act, cl.hover = c.act("close"), false
		if !m.st.color {
			cl.attr |= aFaint
		}
	})
	r := m.dialogBox(dw, dh)
	if op < 0.7 && r.y0+1 < r.y1 && r.y1 < m.h-1 { // the rise never covers the status bar
		r.y0++
		r.y1++
	}
	c.fill(r.x0, r.y0, r.x1-r.x0, r.y1-r.y0, mix(th.bg, th.panel, op), "noop")
	if m.skin().framed {
		m.box(c, r.x0, r.y0, r.x1-r.x0, r.y1-r.y0, mix(th.panel, th.line, op), mix(th.bg, th.panel, op))
		c.tag(r.x0, r.y0, r.x1-r.x0, r.y1-r.y0, "noop", false, true)
		m.shade(c, r)
	}
	if title != "" && r.y1-r.y0 > 2 {
		c.put(r.x0+3, r.y0+1, truncate(title, r.x1-r.x0-10), fgb(mix(th.panel, th.fg, op)), "noop")
		if r.x1-r.x0 >= 12 {
			c.put(r.x1-6, r.y0+1, "esc", fgc(th.faint), "close")
		}
	}
	return r
}

func (m *Model) drawOverlay(c *canvas, g geo) {
	if !m.overlayShown() {
		return
	}
	if d := m.dst; d != nil {
		// The theme recolors its dialogs: draw them with its dialog styles.
		st, th := m.st, c.th
		m.st, c.th = d, d.th
		styleLine(&m.line, d)
		defer func() {
			m.st, c.th = st, th
			styleLine(&m.line, st)
		}()
	}
	switch m.mode {
	case modeSwitcher:
		m.drawPalette(c)
		return
	case modeSettings:
		m.drawSettings(c)
		return
	case modeHelp:
		m.drawKeys(c)
		return
	case modeAgent:
		m.drawAgentPanel(c)
		return
	}
	r := m.dialog(c, 110, m.h-2, "")
	w, h := r.x1-r.x0-4, r.y1-r.y0-2
	if w < 10 || m.w < 50 {
		r = m.inside(r)
		w, h = r.x1-r.x0, r.y1-r.y0
		m.drawLines(c, r.x0, r.y0, w, h, m.overlayLines(w, h))
		return
	}
	m.drawLines(c, r.x0+2, r.y0+1, w, h, m.overlayLines(w, h))
}

// overlaySize is the text area the older overlays get, for their paging.
func (m *Model) overlaySize() (int, int) {
	r := m.dialogBox(110, m.h-2)
	if r.x1-r.x0-4 < 10 || m.w < 50 {
		r = m.inside(r)
		return r.x1 - r.x0, r.y1 - r.y0
	}
	return r.x1 - r.x0 - 4, r.y1 - r.y0 - 2
}

func (m *Model) overlayLines(w, h int) []string {
	switch m.mode {
	case modeVerify:
		return m.verifyLines(w, h)
	case modeDevices:
		return m.devicesLines(w, h)
	case modeDisable:
		return m.disableLines(w, h)
	case modeSearch:
		return m.searchLines(w, h)
	case modeActivity:
		return m.activityLines(w, h)
	case modeList:
		return m.listLines(w, h)
	case modeViewer:
		return m.viewerLines(w, h)
	case modePanePick:
		return m.panePickLines(w, h)
	case modeSharePreview:
		return m.sharePreviewLines(w, h)
	}
	return nil
}

// inside is the part of a dialog panel its frame leaves free.
func (m *Model) inside(r rect) rect {
	if !m.skin().framed || r.x1-r.x0 < 3 || r.y1-r.y0 < 3 {
		return r
	}
	return rect{r.x0 + 1, r.y0 + 1, r.x1 - 1, r.y1 - 1}
}

// keycap draws " k " as a key.
func (m *Model) keycap(c *canvas, x, y int, k string) int {
	th := m.st.th
	return c.put(x, y, " "+k+" ", fgb(th.fg).on(th.element).with(aMonoRev), "")
}
