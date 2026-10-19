package tui

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Notifier shows a desktop notification without blocking
// (*notify.Notifier). It may drop it.
type Notifier interface {
	Send(n notify.Note) bool
}

// Herdr is the part of the herdr CLI the UI uses (*notify.HerdrCLI).
type Herdr interface {
	Self() string
	SelfTab() string
	Panes(ctx context.Context) ([]notify.Pane, error)
	ReadPane(ctx context.Context, id string, lines int) (string, error)
	SetTitle(ctx context.Context, title string) error
}

var _ Herdr = (*notify.HerdrCLI)(nil)

const (
	notifyWindow  = 5 * time.Second // per-channel coalescing
	notifyEvery   = 2 * time.Second // global cap
	badgeDebounce = time.Second
	shareLines    = 60
)

type (
	notifyFlushMsg struct {
		ch  uint64
		gen int
	}
	badgeMsg    struct{ gen int }
	awayTickMsg struct{}
	panesMsg    struct {
		arg   string
		panes []notify.Pane
		err   error
	}
	paneReadMsg struct {
		pane notify.Pane
		text string
		err  error
	}
)

// ---- notifications ----

// pendingNote is a channel's coalescing window: the first message notifies
// at once, later ones within the window are counted and summarized when
// it closes. The strongest cue in the window wins.
type pendingNote struct {
	gen         int
	n           int // messages not yet notified
	title, body string
	group       string // title for a summary
	cue         notify.Cue
	kind        noteKind // the strongest in the window
	last        proto.Message // the message title/body show, re-read before showing
}

// shouldNotify is the mention-class trigger (DMs, mentions, highlights,
// reminders, replies in my threads) for a message that passes eligible.
func (m *Model) shouldNotify(x proto.Message) bool {
	return m.eligible(x) && m.mentionClass(x)
}

// noteText is the notification for x: "#chan · alice" or "alice (DM)",
// and the first non-empty line of the body.
func (m *Model) noteText(x proto.Message) (title, body, group string) {
	if m.isReminder(x) {
		title = "⏰ Reminder"
		return title, firstNonEmptyLine(strings.TrimPrefix(x.Body, proto.RemindPrefix)), title
	}
	who := m.r.userName(x.Author)
	if v, ok := m.chanView(x.Chan); ok && v.Kind == proto.ChanDM {
		title = who + " (DM)"
		group = title
	} else {
		group = m.chanTitle(x.Chan)
		title = group + " · " + who
	}
	return title, firstNonEmptyLine(x.Body), group
}

func firstNonEmptyLine(s string) string {
	for _, l := range strings.Split(sanitize.Text(s), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func (m *Model) notifyFor(news []client.Message) tea.Cmd {
	if m.opt.Notifier == nil {
		return nil
	}
	var cmds []tea.Cmd
	now := m.now()
	for _, x := range news {
		// Text that isn't verifiably its author's (inserted by the server,
		// invalid) never notifies.
		if !x.Sec.Trusted() || !m.eligible(x.Message) {
			continue
		}
		kind, window := noteMessage, allWindow
		if m.mentionClass(x.Message) {
			kind, window = noteMention, notifyWindow
		}
		if popup, cue := decide(m.nt.prefs, now, x.Chan, kind); popup {
			cmds = append(cmds, m.queueNote(x.Message, kind, cue, window))
		}
	}
	return tea.Batch(cmds...)
}

func (m *Model) queueNote(x proto.Message, kind noteKind, cue notify.Cue, window time.Duration) tea.Cmd {
	title, body, group := m.noteText(x)
	var soft *pendingNote
	if p := m.notes[x.Chan]; p != nil && kind == noteMention && p.kind < noteMention {
		// A mention never waits out a level-all channel's 30 s window: it
		// opens its own, and the soft messages still unannounced are
		// announced when that one closes.
		delete(m.notes, x.Chan)
		if p.n > 0 {
			soft = p
		}
	}
	if p := m.notes[x.Chan]; p != nil {
		p.n++
		p.title, p.body, p.group, p.last = title, body, group, x
		if cueRank(cue) > cueRank(p.cue) {
			p.cue = cue
		}
		p.kind = max(p.kind, kind)
		return nil
	}
	m.noteGen++
	p := &pendingNote{gen: m.noteGen, title: title, body: body, group: group, cue: cue, kind: kind, last: x}
	m.notes[x.Chan] = p
	if m.now().Sub(m.lastNotify) >= notifyEvery {
		m.fire(x.Chan, title, body, 1, cue)
		if soft != nil {
			p.n = soft.n
			if soft.n == 1 { // the window ends with the soft one, not a repeat
				p.title, p.body, p.last = soft.title, soft.body, soft.last
			}
		}
	} else {
		p.n = 1 // over the global cap: say it when the window closes
		if soft != nil {
			p.n += soft.n
		}
	}
	return m.noteTimer(x.Chan, p.gen, window)
}

// cueRank orders cues so a mention in a window of soft messages wins.
func cueRank(c notify.Cue) int {
	switch c {
	case notify.CueMention:
		return 3
	case notify.CueMessage:
		return 2
	case notify.CueNone:
		return 1
	}
	return 0
}

func (m *Model) noteTimer(ch uint64, gen int, d time.Duration) tea.Cmd {
	return tea.Tick(d, func(time.Time) tea.Msg { return notifyFlushMsg{ch, gen} })
}

func (m *Model) flushNote(msg notifyFlushMsg) tea.Cmd {
	p := m.notes[msg.ch]
	if p == nil || p.gen != msg.gen {
		return nil
	}
	if p.n == 0 {
		delete(m.notes, msg.ch)
		return nil
	}
	if wait := notifyEvery - m.now().Sub(m.lastNotify); wait > 0 {
		return m.noteTimer(msg.ch, msg.gen, wait)
	}
	delete(m.notes, msg.ch)
	// Settings may have changed while it waited (muted, DND).
	popup, cue := decide(m.nt.prefs, m.now(), msg.ch, p.kind)
	if !popup {
		return nil
	}
	if p.n == 1 {
		// It may have been deleted or edited while it waited: never show
		// text its author took back.
		cur, ok := m.currentMessage(p.last)
		if ok && cur.Deleted {
			return nil
		}
		if ok && cur.Body != p.last.Body {
			p.title, p.body, _ = m.noteText(cur)
		}
		m.fire(msg.ch, p.title, p.body, 1, cue)
	} else {
		m.fire(msg.ch, p.group, strconv.Itoa(p.n)+" new messages", p.n, cue)
	}
	return nil
}

func (m *Model) fire(ch uint64, title, body string, n int, cue notify.Cue) {
	m.lastNotify = m.now()
	title, body = m.private(title, body, n)
	m.opt.Notifier.Send(notify.Note{Title: title, Body: body, Cue: cue, Tag: noteTag(ch)})
}

// seen drops pending notifications for a channel the user is now looking at.
func (m *Model) seen(ch uint64) {
	if m.focused {
		delete(m.notes, ch)
	}
}

// ---- herdr unread badge ----

func (m *Model) badgeText() string {
	s := m.unreadSummary()
	if m.hud.in { // in a huddle: the pane shows it even with nothing unread
		h := "tc · 🎧"
		if m.hud.talking && m.hud.talk == talkLatched {
			h += "●" // open mic, even while another pane has focus
		}
		if s == "" {
			return h
		}
		return h + " · " + s
	}
	if s != "" {
		return "tc · " + s
	}
	return ""
}

// badgeTick schedules a report when the summary changed; the report goes
// out badgeDebounce after the last change.
func (m *Model) badgeTick() tea.Cmd {
	if m.opt.Herdr == nil {
		return nil
	}
	t := m.badgeText()
	if t == m.badgeWant {
		return nil
	}
	m.badgeWant = t
	m.badgeGen++
	gen := m.badgeGen
	return tea.Tick(badgeDebounce, func(time.Time) tea.Msg { return badgeMsg{gen} })
}

func (m *Model) reportBadge(msg badgeMsg) tea.Cmd {
	if msg.gen != m.badgeGen || m.badgeWant == m.badgeSent {
		return nil
	}
	m.badgeSent = m.badgeWant
	b, t, ctx := m.badge, m.badgeSent, m.ctx
	seq := b.next()
	h := m.opt.Herdr
	return func() tea.Msg {
		b.set(ctx, h, seq, t)
		return noopMsg{}
	}
}

// badgeWriter keeps badge reports in order: a report runs only if no newer
// one has been issued.
type badgeWriter struct {
	mu   sync.Mutex
	seq  int
	done int
}

func (b *badgeWriter) next() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	return b.seq
}

func (b *badgeWriter) set(ctx context.Context, h Herdr, seq int, title string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if seq <= b.done { // a newer report already went out
		return
	}
	b.done = seq
	_ = h.SetTitle(ctx, title) // display only; best effort
}

// Close undoes what the UI reported to herdr. Call it after the program
// exits.
func (m *Model) Close() {
	m.syncDraft()
	m.flushDrafts()
	if m.opt.Herdr == nil || m.badgeSent == "" {
		return
	}
	m.badgeSent, m.badgeWant = "", ""
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.badge.set(ctx, m.opt.Herdr, m.badge.next(), "")
}

// ---- auto-away ----

func (m *Model) awayTick() tea.Cmd {
	if m.opt.AwayAfter <= 0 || m.awayRun || m.autoAway {
		return nil
	}
	m.awayRun = true
	d := max(m.opt.AwayAfter-m.now().Sub(m.lastKey), time.Second)
	return tea.Tick(d, func(time.Time) tea.Msg { return awayTickMsg{} })
}

func (m *Model) checkAway() tea.Cmd {
	m.awayRun = false
	if m.opt.AwayAfter <= 0 {
		return nil
	}
	if !m.manualAway && !m.autoAway && m.now().Sub(m.lastKey) >= m.opt.AwayAfter {
		m.autoAway = true
		be := m.c
		return m.call("away", func(context.Context) error { return be.SetAway(true, "") })
	}
	return m.awayTick()
}

// keyActivity runs on every key: it ends an automatic away.
func (m *Model) keyActivity() tea.Cmd {
	m.lastKey = m.now()
	if !m.autoAway {
		return nil
	}
	m.autoAway = false
	be := m.c
	return tea.Batch(m.call("back", func(context.Context) error { return be.SetAway(false, "") }), m.awayTick())
}

// ---- /share-pane ----

type shareState struct {
	panes  []notify.Pane
	sel    int
	pane   notify.Pane
	body   string
	lines  []string
	scroll int // lines up from the bottom
	ch     uint64
	root   uint64
}

func (m *Model) sharePane(arg string) (tea.Cmd, error) {
	if m.opt.Herdr == nil {
		return nil, fmt.Errorf("/share-pane needs herdr (run tc inside a herdr pane)")
	}
	if !m.focus().open() {
		return nil, fmt.Errorf("no channel open")
	}
	h, ctx := m.opt.Herdr, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		ps, err := h.Panes(ctx)
		return panesMsg{arg: arg, panes: ps, err: err}
	}, nil
}

// otherPanes is every pane but ours, our tab first.
func (m *Model) otherPanes(ps []notify.Pane) []notify.Pane {
	self, tab := m.opt.Herdr.Self(), m.opt.Herdr.SelfTab()
	var out []notify.Pane
	for _, p := range ps {
		if p.ID != self && p.ID != "" {
			out = append(out, p)
		}
	}
	slices.SortStableFunc(out, func(a, b notify.Pane) int {
		if (a.Tab == tab) != (b.Tab == tab) {
			if a.Tab == tab {
				return -1
			}
			return 1
		}
		return 0
	})
	return out
}

func matchPane(ps []notify.Pane, arg string) (notify.Pane, error) {
	for _, p := range ps {
		if p.ID == arg {
			return p, nil
		}
	}
	var hits []notify.Pane
	for _, p := range ps {
		if strings.EqualFold(p.Label, arg) || strings.EqualFold(p.Name(), arg) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return notify.Pane{}, fmt.Errorf("no pane %q here", sanitize.Line(arg))
	case 1:
		return hits[0], nil
	}
	return notify.Pane{}, fmt.Errorf("%d panes are called %q; use the id", len(hits), sanitize.Line(arg))
}

func (m *Model) gotPanes(msg panesMsg) tea.Cmd {
	if msg.err != nil {
		m.setErr("share-pane", msg.err)
		return nil
	}
	ps := m.otherPanes(msg.panes)
	if msg.arg != "" {
		p, err := matchPane(ps, msg.arg)
		if err != nil {
			m.setErr("share-pane", err)
			return nil
		}
		return m.readPane(p)
	}
	if len(ps) == 0 {
		m.setInfo("no other panes in this workspace")
		return nil
	}
	m.leaveSelect()
	m.share = shareState{panes: ps}
	m.mode = modePanePick
	return nil
}

func (m *Model) readPane(p notify.Pane) tea.Cmd {
	h, ctx := m.opt.Herdr, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		text, err := h.ReadPane(ctx, p.ID, shareLines)
		return paneReadMsg{pane: p, text: text, err: err}
	}
}

func (m *Model) gotPaneText(msg paneReadMsg) {
	if msg.err != nil {
		m.setErr("share-pane", msg.err)
		return
	}
	f := m.focus()
	if !f.open() {
		m.setInfo("no channel open")
		return
	}
	body := shareBody(msg.pane.Name(), msg.text)
	m.leaveSelect()
	m.share = shareState{pane: msg.pane, body: body, lines: strings.Split(body, "\n"), ch: f.ch, root: f.root}
	m.mode = modeSharePreview
}

func (m *Model) panePickKey(key string) tea.Cmd {
	s := &m.share
	switch key {
	case kEsc, "q":
		m.mode = modeNormal
	case kUp, "ctrl+p", "k":
		s.sel = max(s.sel-1, 0)
	case kDown, "ctrl+n", "j":
		s.sel = min(s.sel+1, len(s.panes)-1)
	case kEnter:
		m.mode = modeNormal
		if s.sel < len(s.panes) {
			return m.readPane(s.panes[s.sel])
		}
	}
	return nil
}

func (m *Model) sharePreviewKey(key string) tea.Cmd {
	s := &m.share
	page := max(m.dims().bodyH-2, 1)
	switch key {
	case kEsc, "q":
		m.mode = modeNormal
		m.setInfo("not shared")
	case kUp, "k":
		s.scroll++
	case kDown, "j":
		s.scroll--
	case kPgUp:
		s.scroll += page
	case kPgDown, " ":
		s.scroll -= page
	case kEnter:
		m.mode = modeNormal
		if err := m.c.Send(s.ch, s.body, s.root); err != nil {
			m.setErr("send", err)
			return nil
		}
		p := &m.main
		if m.thread.open() && m.thread.root == s.root && s.root != 0 {
			p = &m.thread
		}
		p.scroll = 0
		m.reloadPane(p)
		m.share = shareState{}
		return nil
	}
	s.scroll = min(max(s.scroll, 0), max(len(s.lines)-(m.dims().bodyH-1), 0))
	return nil
}

// shareBody is the message /share-pane sends: the pane's text, stripped of
// escape sequences, in a code block under a label, keeping the tail when it
// is over MaxBody.
func shareBody(label, text string) string {
	lines := strings.Split(stripANSI(text), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " ")
	}
	for len(lines) > 0 && lines[0] == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	text = strings.ReplaceAll(strings.Join(lines, "\n"), "```", "ʼʼʼ")
	label = strings.ReplaceAll(sanitize.Line(label), "`", "'")
	head, tail := "📋 `"+label+"`\n```\n", "\n```"
	room := proto.MaxBody - len(head) - len(tail)
	if len(text) > room {
		room -= 48 // the truncation note
		cut := len(text) - room
		if nl := strings.IndexByte(text[cut:], '\n'); nl >= 0 && nl < 4096 {
			cut += nl + 1
		}
		for cut < len(text) && text[cut]&0xC0 == 0x80 { // UTF-8 boundary
			cut++
		}
		text = fmt.Sprintf("… (%d bytes truncated)\n", cut) + text[cut:]
	}
	return head + text + tail
}

// stripANSI removes CSI, OSC and other escape sequences and carriage
// returns, then sanitizes what's left. Terminal scrollback is full of them.
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x1b && i+1 < len(s) {
			switch n := s[i+1]; {
			case n == '[': // CSI: parameters, then a final byte 0x40–0x7E
				j := i + 2
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				i = j
				continue
			case n == ']' || n == 'P' || n == '_' || n == '^': // OSC/DCS/APC/PM: to BEL or ST
				j := i + 2
				for j < len(s) && s[j] != 0x07 && !(s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\') {
					j++
				}
				if j < len(s) && s[j] == 0x1b {
					j++
				}
				i = j
				continue
			case n >= 0x20 && n <= 0x2f: // ESC ( B and friends: intermediates, then a final
				j := i + 1
				for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
					j++
				}
				i = j
				continue
			case n >= 0x30 && n <= 0x7e: // two-byte escapes (ESC =, ESC 7, ...)
				i++
				continue
			}
		}
		if c == '\r' {
			continue
		}
		b.WriteByte(c)
	}
	return sanitize.Text(b.String())
}

// currentMessage is x as the cache has it now; ok is false when it's gone.
func (m *Model) currentMessage(x proto.Message) (proto.Message, bool) {
	var list []client.Message
	if x.Thread != 0 {
		list = m.c.Thread(x.Thread)
	} else {
		list = m.c.Messages(x.Chan, pageLimit)
	}
	for _, c := range list {
		if c.ID == x.ID && x.ID != 0 {
			return c.Message, true
		}
	}
	return proto.Message{}, false
}
