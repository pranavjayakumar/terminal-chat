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

// Huddle meters are a braille line, like a scope trace: each cell holds
// two samples, each one dot at its level 0–3 (left column dots 7,3,2,1
// bottom up, right column 8,6,5,4).
var (
	brailleLeft  = [4]rune{0x40, 0x04, 0x02, 0x01}
	brailleRight = [4]rune{0x80, 0x20, 0x10, 0x08}
)

// braille draws levels (0–3, oldest first) two per cell.
func braille(levels []uint8) string {
	out := make([]rune, 0, (len(levels)+1)/2)
	for i := 0; i < len(levels); i += 2 {
		r := rune(0x2800) + brailleLeft[min(levels[i], 3)]
		if i+1 < len(levels) {
			r += brailleRight[min(levels[i+1], 3)]
		} else {
			r += brailleRight[0]
		}
		out = append(out, r)
	}
	return string(out)
}

// Meter sample counts: ours rolls over 10 samples, others' over 4.
const (
	meMeterN    = 10
	otherMeterN = 4
)

// meterLevel is one sample for a meter. Voice reports who is talking, not
// how loud, so a talking person's samples move and a quiet one's lie on
// the floor.
func (m *Model) meterLevel(talking bool, seed int) uint8 {
	switch {
	case !talking:
		return 0
	case m.fx.off:
		return 2
	}
	t := float64(m.now().UnixMilli()) / 1000
	return uint8(1 + math.Round(math.Abs(math.Sin(t*5.3+float64(seed)*1.7))*2))
}

// sampleMeters adds one sample to every meter in our huddle. It runs on the
// animation tick, which only ticks while we are in a huddle.
func (m *Model) sampleMeters() {
	h := &m.hud
	if !h.in {
		h.meters = nil
		return
	}
	if h.meters == nil {
		h.meters = map[uint64][]uint8{}
	}
	for i, u := range h.roster {
		n, talking := otherMeterN, h.speaking[u]
		if u == m.me.ID {
			n, talking = meMeterN, h.talking
		}
		s := append(h.meters[u], m.meterLevel(talking, i*3+len(h.meters[u])))
		if len(s) > n {
			s = s[len(s)-n:]
		}
		h.meters[u] = s
	}
}

// meter is u's braille meter of n samples, newest on the right.
func (m *Model) meter(u uint64, n int, talking bool) string {
	s := m.hud.meters[u]
	lv := make([]uint8, n)
	if len(s) == 0 && talking {
		for i := range lv {
			lv[i] = m.meterLevel(true, i)
		}
	}
	copy(lv[n-min(len(s), n):], s[max(len(s)-n, 0):])
	return braille(lv)
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
	if h.in {
		leftW += 8 // ● live / ⊘ muted
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
	if h.in {
		// our own voice: always shown, and whether it is going out
		if w >= 50 {
			x = c.put(x+2, y, "you", fgc(th.fg), "") + 1
			mc := th.info
			if h.talking {
				mc = th.accent
			}
			x = c.put(x, y, m.meter(m.me.ID, meMeterN, h.talking), fgc(mc), "")
		}
		if h.talking {
			c.put(x+1, y, "● live", fgb(th.accent), "") // we are broadcasting: never cut
		} else {
			c.put(x+1, y, "⊘ muted", fgc(th.faint), "")
		}
		x += 1 + strWidth("⊘ muted") // the same width either way, so nobody's meter shifts on a toggle
	}
	x += 2
	who := h.hereWho
	if h.in {
		who = h.roster
	}
	shown := 0
	for i, u := range who {
		if u == m.me.ID {
			continue // drawn above as "you"
		}
		talking := h.speaking[u]
		name := sanitize.Line(m.users[u].Name)
		if name == "" {
			name = fmt.Sprint(u)
		}
		need := strWidth(name) + 1 + otherMeterN/2 + 3
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
		mc := th.info
		if talking {
			mc = th.accent
		}
		x = c.put(x, y, m.meter(u, otherMeterN, talking), fgc(mc), "") + 3
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
	if sw := strWidth(m.statusLine(g.CW)); sw > 0 {
		// The row holds an error, a ring or typing: share it on the right
		// when both fit, else float above it, clear of the text there.
		if right := g.CX + g.CW - tw; sw+3 <= right-g.CX {
			x = right
		} else if g.noteY > g.msgTop {
			y--
			c.fill(max(x-1, g.CX), y, min(tw+2, g.CW), 1, th.bg, "")
		}
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
