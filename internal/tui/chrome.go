package tui

import (
	"fmt"
	"math"
	"time"

	"github.com/skailama/tc/internal/sanitize"
)

// ---- huddle bar ----

// huddleBarShown: a huddle runs in the open channel.
func (m *Model) huddleBarShown() bool {
	if m.active == 0 {
		return false
	}
	if m.hud.in {
		return m.hud.cur == m.active
	}
	return m.hud.here > 0
}

// meter is a 4-cell level meter. Voice reports who is talking, not how
// loud, so a talking person's meter moves and a quiet one's lies flat.
func (m *Model) meter(talking bool, seed int) string {
	const bars = "▁▂▃▄▅▆▇"
	rs := []rune(bars)
	out := make([]rune, 4)
	t := float64(m.now().UnixMilli()) / 1000
	for q := range out {
		lvl := 0.0
		if talking && !m.fx.off {
			lvl = math.Abs(math.Sin(t*6+float64(q)*1.4+float64(seed)))*0.8 + 0.2
		} else if talking {
			lvl = 0.6
		}
		out[q] = rs[min(int(lvl*float64(len(rs))), len(rs)-1)]
	}
	return string(out)
}

func (m *Model) drawHuddleBar(c *canvas, g geo) {
	th := m.st.th
	h := &m.hud
	y, x0, w := g.hud, g.CX, g.CW
	c.fill(x0, y, w, 1, th.element, "")
	old := c.setClip(rect{x0, y, x0 + w, y + 1})
	defer c.setClip(old)
	// controls on the right, sized first so they always fit
	type ctl struct {
		t, act string
		st     cstyle
	}
	var ctls []ctl
	if h.in {
		if h.talking {
			ctls = append(ctls, ctl{"mute", "cmd:/mute", fgc(th.dim)})
		} else {
			ctls = append(ctls, ctl{"talk", "cmd:/talk", fgc(th.dim)})
		}
		ctls = append(ctls, ctl{"leave", "cmd:/leave-huddle", fgc(th.err)})
	} else {
		ctls = append(ctls, ctl{"join", "cmd:/huddle", fgb(th.info)})
	}
	leftW := 4 // ▎ ♪
	if h.in && h.talking {
		leftW += 6 // ● mic
	}
	right := 1
	for _, k := range ctls {
		right += strWidth(k.t) + 2
	}
	for len(ctls) > 0 && leftW+right > w {
		right -= strWidth(ctls[0].t) + 2 // talk/mute goes first, then leave
		ctls = ctls[1:]
	}
	rx := x0 + w - right + 2
	for _, k := range ctls {
		rx = c.put(rx, y, k.t, k.st, k.act) + 2
	}
	limit := x0 + w - right
	c.put(x0, y, "▎", fgc(th.info), "")
	x := c.put(x0+2, y, "♪", fgc(th.info), "")
	if w >= 34 {
		x = c.put(x+1, y, "huddle", fgb(th.fg), "")
		if h.in && !h.joinedAt.IsZero() {
			s := int(m.now().Sub(h.joinedAt) / time.Second)
			x = c.put(x+1, y, fmt.Sprintf("%d:%02d", s/60, s%60), fgc(th.faint), "")
		}
	}
	if h.in && h.talking {
		// we are broadcasting: this must never be cut
		x = c.put(x+1, y, "● mic", fgb(th.err), "")
	}
	x += 2
	who := h.hereWho
	if h.in {
		who = h.roster
	}
	shown := 0
	for i, u := range who {
		if u == m.me.ID {
			continue // our state is the ● mic above
		}
		talking := h.speaking[u]
		name := sanitize.Line(m.users[u].Name)
		if name == "" {
			name = fmt.Sprint(u)
		}
		need := strWidth(name) + 1 + 4 + 3
		if x+need > limit {
			if rest := len(who) - i; x+3 <= limit && rest > 0 {
				c.put(x, y, fmt.Sprintf("+%d", rest), fgc(th.faint), "")
			}
			break
		}
		st := fgc(th.dim)
		if talking {
			st = fgb(th.nick(u))
		}
		x = c.put(x, y, name, st, "") + 1
		mc := th.faint
		if talking {
			mc = th.info
		}
		x = c.put(x, y, m.meter(talking, i*2), fgc(mc), "") + 3
		shown++
	}
	if shown == 0 && len(who) <= 1 && h.in && x+8 <= limit {
		c.put(x, y, "just you", fgc(th.faint), "")
	}
}

// ---- toasts ----

// toastShown: the notice is a passing one, shown as a toast pill.
func (m *Model) toastShown() bool {
	return m.notice != "" && !m.noticeErr && !m.noticeSticky
}

func (m *Model) toastTTL() time.Duration {
	if m.noticeTTL != 0 {
		return m.noticeTTL
	}
	return infoTTL
}

// toastMoving: the toast is fading in or out.
func (m *Model) toastMoving() bool {
	if !m.toastShown() || m.fx.off {
		return false
	}
	end := m.noticeAt.Add(m.toastTTL())
	return m.running(m.noticeAt, dToast) || !m.now().Before(end.Add(-dToast))
}

// toastWake is when the toast starts fading out, if that's still ahead.
func (m *Model) toastWake() time.Duration {
	if !m.toastShown() || m.fx.off {
		return 0
	}
	d := m.noticeAt.Add(m.toastTTL() - dToast).Sub(m.now())
	if d <= 0 {
		return 0
	}
	return d
}

func (m *Model) drawToast(c *canvas, g geo) {
	if m.overlayShown() {
		m.drawDialogNotice(c)
		return
	}
	if !m.toastShown() || g.noteY < 0 {
		return
	}
	th := m.st.th
	v := 1.0
	if !m.fx.off {
		vin := ease(m.progress(m.noticeAt, dToast))
		left := m.noticeAt.Add(m.toastTTL()).Sub(m.now())
		vout := ease(float64(left) / float64(dToast))
		v = min(vin, vout)
	}
	text := truncate(m.notice, g.CW-6)
	tw := strWidth(text) + 4
	x := g.CX + (g.CW-tw)/2
	y := g.noteY // the row under it is the input's frame, so it only fades
	if strWidth(m.statusLine(g.CW)) > 0 && g.noteY > g.msgTop {
		y-- // the row holds an error, a ring or typing: float above it
	}
	bg := mix(th.bg, th.element, v)
	c.fill(x, y, tw, 1, bg, "toast:close")
	c.put(x, y, "▐", fgc(bg), "toast:close")
	c.put(x+2, y, text, cstyle{fg: mix(th.bg, th.fg, v), hasFg: true, attr: aMonoRev}.on(bg), "toast:close")
	c.put(x+tw-1, y, "▌", fgc(bg), "toast:close")
}

func absSin(x float64) float64 { return math.Abs(math.Sin(x)) }

// drawDialogNotice shows the notice row over a dialog's bottom edge, so an
// error or confirmation from inside a dialog is never hidden by it.
func (m *Model) drawDialogNotice(c *canvas) {
	th := m.st.th
	line := m.statusLine(m.w - 4)
	if m.toastShown() {
		line = m.st.noticeInfo.Render(truncate(m.notice, m.w-4))
		if t := m.statusLine(m.w - 4); t != "" {
			line = t + m.st.dim.Render(" · ") + line
		}
	}
	if strWidth(line) == 0 {
		return
	}
	line = truncate(line, m.w-4)
	w := strWidth(line) + 2
	x := max((m.w-w)/2, 0)
	y := max(m.h-2, 0)
	c.fill(x, y, w, 1, th.element, "toast:close")
	c.putANSI(x+1, y, line, w-2, fgc(th.fg).on(th.element), "toast:close")
}
