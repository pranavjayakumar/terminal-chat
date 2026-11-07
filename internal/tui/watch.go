package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/screen"
)

// Pane sharing in a huddle (docs/VOICE.md "Pane sharing", PROTOCOL.md
// §14.5): /pair streams a herdr pane, read-only, to the huddle; /watch,
// ⌥W or the huddle bar's "watch" shows the pane someone shares, full
// screen. While we stream, a ● LIVE banner is drawn over everything.

// WatchBackend is the part of client.Client pane sharing uses (older
// fakes lack it). The client posts Update{Watch} when the picture or our
// stream changes.
type WatchBackend interface {
	StartShare(ctx context.Context, label string, capture client.Capture) error
	StopShare()
	Sharing() (client.ShareInfo, bool)
	ShareStopped() error
	LiveIn(ch uint64) (uint64, bool)
	Watch(ctx context.Context, user uint64) error
	StopWatch()
	Watching(ver uint64) (client.WatchFrame, bool)
}

// paneCapturer reads what a pane shows now, with colors
// (*notify.HerdrCLI); a Herdr without it streams plain text.
type paneCapturer interface {
	CapturePane(ctx context.Context, id string) (string, error)
}

type watchUI struct {
	// our stream
	sharing  bool
	live     uint64 // who shares a pane in our huddle (0: nobody, or us)
	label    string
	watchers int
	scrubbed int
	enc      bool
	// the pane we watch (modeWatch)
	frame client.WatchFrame
	grid  *screen.Grid
	x, y  int // pan
	// pending: tc watch @user, until they are live somewhere we can join.
	pending   string
	pendingAt time.Time
	joining   bool
}

type (
	pairPanesMsg struct {
		arg   string
		panes []notify.Pane
		err   error
	}
	pairedMsg  struct{ label string }
	watchedMsg struct{ user uint64 }
)

// pendingWait is how long tc watch @user waits for them to go live.
const pendingWait = 30 * time.Second

func (m *Model) watchBackend() (WatchBackend, bool) {
	wb, ok := m.c.(WatchBackend)
	return wb, ok
}

// SetWatchTarget makes the UI watch @name's pane as soon as they share
// one in a huddle we can join (tc watch @name).
func (m *Model) SetWatchTarget(name string) {
	m.wt.pending = strings.TrimPrefix(strings.ToLower(name), "@")
	m.wt.pendingAt = m.now()
}

func (m *Model) watchCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "pair", "unpair", "watch":
	default:
		return nil, false, nil
	}
	wb, ok := m.watchBackend()
	hb, ok2 := m.huddleBackend()
	if !ok || !ok2 {
		return nil, true, errors.New("pane sharing isn't available")
	}
	switch c.name {
	case "unpair":
		if !m.unpair() {
			return nil, true, errors.New("not streaming a pane")
		}
		return nil, true, nil
	case "watch":
		if len(c.args) > 1 {
			return nil, true, errors.New("usage: /watch [@user]")
		}
		return m.watchUser(wb, hb, c.rest)
	}
	if len(c.args) > 1 {
		return nil, true, errors.New("usage: /pair [pane id or label]")
	}
	if _, in := hb.InHuddle(); !in {
		return nil, true, errors.New("/pair streams a pane to a huddle: /huddle first")
	}
	if m.opt.Herdr == nil {
		return nil, true, errors.New("/pair needs herdr: run tc inside a herdr pane (HERDR_PANE_ID isn't set here)")
	}
	h, ctx, arg := m.opt.Herdr, m.ctx, c.rest
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		ps, err := h.Panes(ctx)
		return pairPanesMsg{arg: arg, panes: ps, err: err}
	}, true, nil
}

// watchUser watches name's pane (or, with no name, whoever shares in our
// huddle), joining the huddle it is in when that's not ours.
func (m *Model) watchUser(wb WatchBackend, hb HuddleBackend, name string) (tea.Cmd, bool, error) {
	cur, in := hb.InHuddle()
	var who uint64
	if name == "" {
		if !in {
			return nil, true, errors.New("not in a huddle (/huddle to join)")
		}
		u, ok := wb.LiveIn(cur)
		if !ok {
			return nil, true, errors.New("nobody is sharing a pane in your huddle")
		}
		who = u
	} else {
		u, ok := m.findUser(name)
		if !ok {
			return nil, true, fmt.Errorf("no user %s", sanitize.Line(name))
		}
		who = u
	}
	if who == m.me.ID {
		return nil, true, errors.New("that's your own pane")
	}
	ch, ok := m.liveWhere(wb, who)
	if !ok {
		return nil, true, fmt.Errorf("%s isn't sharing a pane", m.userName(who))
	}
	if in && ch == cur {
		return m.callThen("watch", func(ctx context.Context) (tea.Msg, error) {
			return watchedMsg{who}, wb.Watch(ctx, who)
		}), true, nil
	}
	// Watching means being in that huddle: join it (leaving ours), then watch.
	return m.callThen("watch", func(ctx context.Context) (tea.Msg, error) {
		if err := hb.JoinHuddle(ctx, ch); err != nil {
			return nil, err
		}
		return watchedMsg{who}, wb.Watch(ctx, who)
	}), true, nil
}

// liveWhere finds the huddle user shares a pane in, among ours.
func (m *Model) liveWhere(wb WatchBackend, user uint64) (uint64, bool) {
	chans := make([]uint64, 0, len(m.hud.counts))
	for ch := range m.hud.counts {
		chans = append(chans, ch)
	}
	slices.Sort(chans)
	if m.hud.in {
		chans = append([]uint64{m.hud.cur}, chans...)
	}
	for _, ch := range chans {
		if u, ok := wb.LiveIn(ch); ok && u == user {
			return ch, true
		}
	}
	return 0, false
}

func (m *Model) userName(u uint64) string {
	if n := sanitize.Line(m.users[u].Name); n != "" {
		return "@" + n
	}
	return fmt.Sprint(u)
}

// watchKeyNormal is ⌥W outside the viewer: watch the pane shared in our
// huddle.
func (m *Model) watchKeyNormal() tea.Cmd {
	wb, ok := m.watchBackend()
	hb, ok2 := m.huddleBackend()
	if !ok || !ok2 {
		return nil
	}
	cmd, _, err := m.watchUser(wb, hb, "")
	if err != nil {
		m.setErr("watch", err)
	}
	return cmd
}

func (m *Model) startPair(p notify.Pane) tea.Cmd {
	wb, ok := m.watchBackend()
	if !ok {
		return nil
	}
	h, id, label := m.opt.Herdr, p.ID, p.Name()
	capture := func(ctx context.Context) (string, error) {
		if c, ok := h.(paneCapturer); ok {
			return c.CapturePane(ctx, id)
		}
		return h.ReadPane(ctx, id, screen.MaxRows)
	}
	return m.callThen("pair", func(ctx context.Context) (tea.Msg, error) {
		return pairedMsg{label}, wb.StartShare(ctx, label, capture)
	})
}

// unpair stops our pane stream at once; false if none ran.
func (m *Model) unpair() bool {
	wb, ok := m.watchBackend()
	if !ok || !m.wt.sharing {
		return false
	}
	wb.StopShare()
	m.wt.sharing = false
	m.setNotice("stopped streaming "+m.wt.label, false, false, 0)
	return true
}

// watchMsg handles pane sharing's own messages.
func (m *Model) watchMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case pairPanesMsg:
		if msg.err != nil {
			m.setErr("pair", msg.err)
			return nil, true
		}
		ps := m.otherPanes(msg.panes)
		if msg.arg != "" {
			p, err := matchPane(ps, msg.arg)
			if err != nil {
				m.setErr("pair", err)
				return nil, true
			}
			return m.startPair(p), true
		}
		// The obvious one: the only other pane in our tab.
		var mine []notify.Pane
		for _, p := range ps {
			if p.Tab == m.opt.Herdr.SelfTab() {
				mine = append(mine, p)
			}
		}
		switch {
		case len(mine) == 1:
			return m.startPair(mine[0]), true
		case len(ps) == 0:
			m.setErr("pair", errors.New("no other pane to stream: open one in herdr first"))
			return nil, true
		}
		m.leaveSelect()
		m.share = shareState{panes: ps, pair: true}
		m.mode = modePanePick
		return nil, true
	case pairedMsg:
		m.refreshWatch()
		m.setNotice(m.icon(icMic)+" you're live: "+sanitize.Line(msg.label)+" · /unpair or Esc stops", false, false, errTTL)
		return nil, true
	case watchedMsg:
		m.wt.pending, m.wt.joining = "", false
		m.wt.x, m.wt.y = 0, 0
		m.refreshWatch()
		m.leaveSelect()
		m.mode = modeWatch
		m.clearNotice()
		return nil, true
	}
	return nil, false
}

// refreshWatch re-reads our stream and the picture we watch.
func (m *Model) refreshWatch() tea.Cmd {
	wb, ok := m.watchBackend()
	if !ok {
		return nil
	}
	w := &m.wt
	was := w.sharing
	info, sharing := wb.Sharing()
	w.sharing, w.watchers, w.scrubbed, w.enc = sharing, info.Watchers, info.Scrubbed, info.Enc
	if sharing {
		w.label = sanitize.Line(info.Label) // kept after a stop, to say what stopped
	}
	if was && !sharing {
		if err := wb.ShareStopped(); err != nil {
			m.setErr("pair", fmt.Errorf("stopped streaming %s: %w", w.label, err))
		}
	}
	w.live = 0
	if hb, ok := m.huddleBackend(); ok {
		if ch, in := hb.InHuddle(); in {
			if u, ok := wb.LiveIn(ch); ok && u != m.me.ID {
				w.live = u
			}
		}
	}
	f, ok := wb.Watching(w.frame.Ver)
	switch {
	case ok:
		g := f.Grid
		if g == nil && f.Ver == w.frame.Ver {
			g = w.grid // unchanged: keep our copy
		}
		w.frame, w.grid = f, g
	default:
		w.frame, w.grid = client.WatchFrame{}, nil // nothing kept while not watching
		if m.mode == modeWatch {
			m.mode = modeNormal
		}
	}
	return m.watchPending(wb)
}

// watchPending does tc watch @user's waiting part.
func (m *Model) watchPending(wb WatchBackend) tea.Cmd {
	w := &m.wt
	if w.pending == "" || w.joining {
		return nil
	}
	if m.now().Sub(w.pendingAt) > pendingWait {
		m.setErr("watch", fmt.Errorf("@%s isn't sharing a pane in any of your huddles", sanitize.Line(w.pending)))
		w.pending = ""
		return nil
	}
	u, ok := m.findUser(w.pending)
	if !ok {
		return nil // users may still be loading
	}
	if _, ok := m.liveWhere(wb, u); !ok {
		return nil
	}
	hb, ok := m.huddleBackend()
	if !ok {
		return nil
	}
	w.joining = true
	cmd, _, err := m.watchUser(wb, hb, w.pending)
	if err != nil {
		w.joining, w.pending = false, ""
		m.setErr("watch", err)
	}
	return cmd
}

func (m *Model) watchKey(key string) tea.Cmd {
	w := &m.wt
	page := max(m.h-3, 1)
	switch key {
	case kEsc, "q", kWatch:
		if wb, ok := m.watchBackend(); ok {
			wb.StopWatch()
		}
		w.frame, w.grid = client.WatchFrame{}, nil
		m.mode = modeNormal
		return nil
	case kSelCopy:
		if w.grid == nil {
			return nil
		}
		return m.copyCmd(w.grid.Text())
	case kLeft, "h":
		w.x -= 8
	case kRight, "l":
		w.x += 8
	case kUp, "k":
		w.y--
	case kDown, "j":
		w.y++
	case kPgUp:
		w.y -= page
	case kPgDown, " ":
		w.y += page
	case kHome:
		w.x, w.y = 0, 0
	}
	m.clampWatch()
	return nil
}

func (m *Model) clampWatch() {
	w := &m.wt
	if w.grid == nil {
		w.x, w.y = 0, 0
		return
	}
	rows := max(m.geo().statusY-1, 1)
	w.x = min(max(w.x, 0), max(w.grid.W-m.w, 0))
	w.y = min(max(w.y, 0), max(w.grid.H-rows, 0))
}

// drawWatch draws the shared pane over the chat area: a title row, then
// the cells as the streamer's pane shows them (sanitized when decoded),
// cut to the screen and panned with the arrows.
func (m *Model) drawWatch(c *canvas, g geo) {
	th := m.st.th
	w := &m.wt
	m.clampWatch()
	bottom := g.statusY
	if bottom <= 0 {
		bottom = m.h
	}
	c.fill(0, 0, m.w, 1, th.element, "")
	who := m.userName(w.frame.User)
	state, sc := "live", th.err
	if w.frame.Ended {
		state, sc = "ended", th.faint
	}
	x := c.put(1, 0, "▎", fgc(th.info), "")
	if w.frame.Enc {
		x = c.put(x+1, 0, "🔒", cstyle{}, "")
	}
	x = c.put(x+1, 0, who, fgb(th.fg), "")
	if w.frame.Label != "" {
		x = c.put(x, 0, " · "+truncate(w.frame.Label, max(m.w/3, 8)), fgc(th.dim), "")
	}
	x = c.put(x, 0, " · ", fgc(th.dim), "")
	x = c.put(x, 0, state, fgb(sc), "")
	hint := "Esc leaves · y copy · ←→↑↓ pan"
	if m.w-x < strWidth(hint)+3 {
		hint = "Esc · y"
	}
	if m.w-x >= strWidth(hint)+3 {
		c.put(m.w-strWidth(hint)-1, 0, hint, fgc(th.faint), "")
	}
	top := 1
	if w.grid == nil {
		msg := "waiting for " + who + "'s pane…"
		if w.frame.Ended {
			msg = who + " stopped sharing"
		}
		if y := (top + bottom) / 2; y < bottom {
			c.put(max((m.w-strWidth(msg))/2, 0), y, truncate(msg, m.w), fgc(th.faint), "")
		}
		return
	}
	old := c.setClip(rect{0, top, m.w, bottom})
	defer c.setClip(old)
	gr := w.grid
	for sy := top; sy < bottom; sy++ {
		gy := w.y + sy - top
		if gy >= gr.H {
			break
		}
		row := gr.Row(gy)
		for gx := w.x; gx < gr.W && gx-w.x < m.w; gx++ {
			cl := row[gx]
			if cl.W == 0 {
				if gx == w.x { // the left edge cuts a wide grapheme: show a space
					c.put(0, sy, " ", cellStyle(cl.St), "")
				}
				continue
			}
			c.put(gx-w.x, sy, cl.S, cellStyle(cl.St), "")
		}
	}
}

func cellStyle(s screen.Style) cstyle {
	st := cstyle{fg: rgb(s.FG), bg: rgb(s.BG), hasFg: s.A&screen.HasFG != 0, hasBg: s.A&screen.HasBG != 0}
	for _, a := range [...]struct {
		from screen.Attr
		to   attr
	}{{screen.Bold, aBold}, {screen.Faint, aFaint}, {screen.Italic, aItalic}, {screen.Underline, aUnderline}, {screen.Reverse, aReverse}} {
		if s.A&a.from != 0 {
			st.attr |= a.to
		}
	}
	return st
}

// liveText is the ● LIVE banner's text, longest that fits w.
func (m *Model) liveText(w int) string {
	wt := &m.wt
	full := m.icon(icMic) + " LIVE " + wt.label
	switch wt.watchers {
	case 0:
		full += " · nobody watching"
	case 1:
		full += " · 1 watching"
	default:
		full += fmt.Sprintf(" · %d watching", wt.watchers)
	}
	if wt.scrubbed > 0 {
		full += fmt.Sprintf(" · %d hidden", wt.scrubbed)
	}
	full += " · Esc stops"
	for _, s := range []string{full, m.icon(icMic) + " LIVE " + wt.label + " · Esc stops", m.icon(icMic) + " LIVE " + wt.label, m.icon(icMic) + " LIVE"} {
		if strWidth(s)+2 <= w {
			return s
		}
	}
	return truncate(m.icon(icMic)+" LIVE", max(w-2, 1))
}

// drawLive draws the ● LIVE banner at the top right, over everything,
// while we stream a pane.
func (m *Model) drawLive(c *canvas) {
	if !m.wt.sharing || m.w < 4 || m.h < 1 {
		return
	}
	th := m.st.th
	text := m.liveText(min(m.w, 72))
	tw := strWidth(text) + 2
	x := m.w - tw
	old := c.setClip(rect{0, 0, m.w, m.h})
	defer c.setClip(old)
	c.fill(x, 0, tw, 1, th.err, "cmd:/unpair")
	c.put(x+1, 0, text, cstyle{fg: th.bg, hasFg: true, bg: th.err, hasBg: true, attr: aBold | aMonoRev}, "cmd:/unpair")
}
