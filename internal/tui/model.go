package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

type mode int

const (
	modeNormal mode = iota
	modeSelect
	modeSwitcher
	modeHelp
	modeSearch
	modeConfirmDelete
	modeReact
	modePanePick
	modeSharePreview
	modeActivity
	modeViewer
	modeList
	modeVerify
	modeDevices // devices.go
	modeDisable // disable.go
)

// pane is a scrollable message list: a channel (root 0) or a thread.
type pane struct {
	ch, root uint64
	msgs     []client.Message
	scroll   int // lines up from the bottom; 0 = following new messages
	sel      int // selected message index in select mode, -1 none

	// memo of the last layout, owned by View/Update on the event loop.
	ver                     int
	memoVer, memoW, memoSel int
	memoMode                mode
	memoLines               []string
	memoOwner               []int
}

func (p *pane) open() bool { return p.ch != 0 }

// Model is the Bubble Tea model.
type Model struct {
	hud    huddleUI // voice huddles (huddle.go)
	nt     notifyUI // notification levels, rings (notifypolicy.go)
	opened bool     // a channel has been opened; stops the first-start auto-open
	c      Backend
	ctx    context.Context
	st     *styles
	r      renderer
	rc     renderCache
	opt    Options

	w, h      int
	me        proto.User
	status    client.Status
	chans     []client.ChannelView
	e2e       map[uint64]client.E2EStatus // non-public channels we're in, refreshed with chans
	users     map[uint64]proto.User
	userList  []proto.User
	presence  map[uint64]client.Presence
	typing    []uint64
	typingRun bool

	active uint64
	main   pane
	thread pane

	limits   map[uint64]int
	histBusy map[uint64]bool
	histDone map[uint64]bool

	input   textarea.Model
	line    textinput.Model // switcher query and react prompt
	mode    mode
	editing uint64 // message id whose edit is in the input
	target  client.Message
	sidebar int // 0 auto, 1 forced on, -1 forced off

	notice       string
	noticeErr    bool
	noticeSticky bool          // hints that stay until their state ends
	noticeTTL    time.Duration // 0: the default for the kind
	noticeGen    int           // bumped on every change; expiry ticks carry it
	noticeTimed  int           // generation that has an expiry tick scheduled
	ctrlC        bool
	escAt        time.Time // first Esc on a non-empty draft
	helpScroll   int
	view         viewer

	sw     switcher
	search searchState
	act    activityState

	focused    bool
	lastTyping time.Time
	now        func() time.Time

	notes      map[uint64]*pendingNote
	noteGen    int
	lastNotify time.Time

	badge                *badgeWriter
	badgeWant, badgeSent string
	badgeGen             int

	share shareState

	lastKey              time.Time
	autoAway, manualAway bool
	awayRun              bool

	drafts     map[draftKey]string // unsent input per place to type, mirrored in the cache
	draftDirty map[draftKey]bool   // changed since the last write to the cache
	draftTimer bool                // a debounced save is scheduled
	draftMark  map[uint64]bool     // channels with a draft not shown in the input
	dk         draftKey            // where the input's text belongs

	highlights []string      // keywords that count like an @mention (lowercase)
	mentionAs  []string      // names that @mention me: mine, then my groups
	groups     []proto.Group // cached for @completion; refreshed with mentionAs
	comp       completer     // the / command and @/# completion popup (complete.go)
	list       listState

	jump jumpState

	unverified map[uint64]bool     // users trusted only on first use (e2e channels)
	keyChg     map[uint64][]uint64 // e2e channel → members with an unacknowledged key change
	hinted     map[uint64]bool     // channels that showed the "?" hint this run
	verify     verifyState
	devs       devicesState
	dis        disableState
	noticeDim  bool   // the notice is a quiet hint
	newRelease string // newer release tag, shown in the sidebar title
}

// ---- tea messages ----

type (
	updateMsg        client.Update
	updatesClosedMsg struct{}
	errMsg           struct {
		what string
		err  error
	}
	infoMsg    string
	historyMsg struct {
		ch   uint64
		more bool
		err  error
	}
	openChanMsg struct{ ch uint64 }
	listMsg     listState
	pinsMsg     struct {
		ch  uint64
		res []proto.Message
		err error
	}
	searchMsg struct {
		q   string
		res []proto.Message
		err error
	}
	typingTickMsg   struct{}
	noopMsg         struct{}
	okMsg           struct{} // a request succeeded
	noticeExpireMsg struct{ gen int }
)

const (
	pageLimit  = 100
	maxInputH  = 6
	reqTimeout = 30 * time.Second
	infoTTL    = 4 * time.Second
	errTTL     = 8 * time.Second
	escWindow  = 2 * time.Second
)

// New builds the model and loads the cache.
func New(ctx context.Context, c Backend, o Options) *Model {
	st := o.styles
	if st == nil {
		if o.Out != nil {
			st = defaultStyles(o.Out)
		} else {
			st = defaultStyles(os.Stdout)
		}
	}
	m := &Model{
		c: c, ctx: ctx, st: st, opt: o,
		limits: map[uint64]int{}, histBusy: map[uint64]bool{}, histDone: map[uint64]bool{},
		focused: true, now: time.Now,
		main: pane{sel: -1}, thread: pane{sel: -1},
		notes: map[uint64]*pendingNote{}, badge: &badgeWriter{},
	}
	m.lastKey = m.now()
	m.r = renderer{st: st, name: func(id uint64) string { return m.users[id].Name },
		bot: func(id uint64) bool { return m.users[id].Bot }}
	m.input = newInput(st)
	m.line = newLine(st)
	m.loadDrafts()
	m.refreshPrefs()
	m.reloadAll()
	if ch := m.defaultChannel(); ch != 0 {
		m.openChannel(ch)
	}
	m.syncDraft()
	return m
}

// openDefault selects the default channel when none is active. A fresh
// client's cache is empty when New runs; the first snapshot arrives later.
func (m *Model) openDefault() tea.Cmd {
	if m.active != 0 || m.opened {
		return nil
	}
	return m.openChannel(m.defaultChannel())
}

func (m *Model) defaultChannel() uint64 { return m.defaultChannelExcept(0) }

func (m *Model) defaultChannelExcept(skip uint64) uint64 {
	var first uint64
	for _, v := range m.chans {
		if !v.Member || v.ID == skip {
			continue
		}
		if v.Kind != proto.ChanDM && v.Name == "general" {
			return v.ID
		}
		if first == 0 {
			first = v.ID
		}
	}
	return first
}

// Init starts listening for client updates.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.waitUpdate(), textarea.Blink, m.awayTick(), m.updateCheckCmd()}
	if m.active != 0 && len(m.main.msgs) < 30 {
		cmds = append(cmds, m.fetchHistory(m.active))
	}
	return tea.Batch(cmds...)
}

func (m *Model) waitUpdate() tea.Cmd {
	ch := m.c.Updates()
	return func() tea.Msg {
		u, ok := <-ch
		if !ok {
			return updatesClosedMsg{}
		}
		return updateMsg(u)
	}
}

// call runs a network request in a Cmd and reports failures as a notice.
func (m *Model) call(what string, f func(ctx context.Context) error) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		if err := f(ctx); err != nil {
			return errMsg{what, err}
		}
		return okMsg{}
	}
}

// ---- loading from the cache ----

func (m *Model) reloadAll() {
	m.me = m.c.Me()
	m.r.me = m.me
	m.status = m.c.Status()
	m.reloadUsers()
	m.reloadChans()
	m.reloadPane(&m.main)
	m.reloadPane(&m.thread)
	m.reloadTyping()
}

func (m *Model) reloadUsers() {
	m.userList = m.c.Users()
	m.users = make(map[uint64]proto.User, len(m.userList))
	for _, u := range m.userList {
		m.users[u.ID] = u
	}
	if u, ok := m.users[m.me.ID]; ok {
		m.me = u
		m.r.me = u
	}
	m.reloadPresence()
	m.refreshMentionNames()
	m.reloadComp()
	m.rc.reset()
	m.main.ver++
	m.thread.ver++
}

func (m *Model) reloadPresence() {
	m.presence = make(map[uint64]client.Presence, len(m.userList))
	for _, u := range m.userList {
		m.presence[u.ID] = m.c.Presence(u.ID)
	}
}

func (m *Model) reloadChans() {
	m.chans = m.c.Channels()
	prev, hadPrev := m.e2e[m.active]
	m.e2e = map[uint64]client.E2EStatus{}
	for _, v := range m.chans {
		if v.Member && v.Kind != proto.ChanPublic {
			if st := m.c.E2EStatus(v.ID); st.State != client.E2EOff {
				m.e2e[v.ID] = st
			}
		}
	}
	if cur, ok := m.e2e[m.active]; ok && hadPrev && prev.Epoch > 0 && cur.Epoch > prev.Epoch && cur.State == client.E2EOK {
		m.setInfo(fmt.Sprintf("🔒 new channel key (epoch %d)", cur.Epoch))
	}
	m.refreshTrust()
	m.refreshHighlights()
	m.refreshMentionNames()
	m.comp.here = nil // members may have changed
	m.reloadComp()
	defer m.unverifiedHint()
	if m.active != 0 {
		if v, ok := m.chanView(m.active); !ok || (!v.Member && !v.Removed && v.Kind != proto.ChanPublic) {
			// Left or removed: fall back to something we're in.
			m.active = 0
			m.main = pane{sel: -1}
			m.closeThread()
			if ch := m.defaultChannel(); ch != 0 {
				m.openChannel(ch)
			}
		}
	}
}

func (m *Model) chanView(id uint64) (client.ChannelView, bool) {
	for _, v := range m.chans {
		if v.ID == id {
			return v, true
		}
	}
	return client.ChannelView{}, false
}

func (m *Model) limit(ch uint64) int {
	if l := m.limits[ch]; l > 0 {
		return l
	}
	return pageLimit
}

func msgKey(x client.Message) (uint64, int64) { return x.ID, x.Local }

func lastID(ms []client.Message) uint64 {
	var id uint64
	for _, x := range ms {
		id = max(id, x.ID)
	}
	return id
}

// reloadPane re-reads p's messages, keeping the selection on the same
// message and the viewport still when new lines arrive while scrolled up.
func (m *Model) reloadPane(p *pane) {
	if !p.open() {
		return
	}
	var selID uint64
	var selLocal int64
	if p.sel >= 0 && p.sel < len(p.msgs) {
		selID, selLocal = msgKey(p.msgs[p.sel])
	}
	old := lastID(p.msgs)
	w := m.paneWidth(p)
	var before int
	if p.scroll > 0 && w > 0 {
		before = m.suffixLines(p, w, old)
	}
	if p.root != 0 {
		p.msgs = m.c.Thread(p.root)
	} else {
		p.msgs = m.c.Messages(p.ch, m.limit(p.ch))
	}
	p.ver++
	if p.scroll > 0 && w > 0 {
		p.scroll += m.suffixLines(p, w, old) - before
		p.scroll = max(p.scroll, 0)
	}
	if p.sel >= 0 {
		p.sel = -1
		for i, x := range p.msgs {
			if id, l := msgKey(x); (selID != 0 && id == selID) || (selID == 0 && l == selLocal) {
				p.sel = i
				break
			}
		}
		if p.sel < 0 && len(p.msgs) > 0 {
			p.sel = len(p.msgs) - 1
		}
	}
}

// suffixLines counts lines of messages newer than old (and unsent ones):
// the part of the pane that grows at the bottom.
func (m *Model) suffixLines(p *pane, w int, old uint64) int {
	_, owner := m.paneLines(p, w)
	n := 0
	for _, o := range owner {
		if o >= 0 && (p.msgs[o].ID > old || p.msgs[o].ID == 0) {
			n++
		}
	}
	return n
}

func (m *Model) reloadTyping() {
	m.typing = m.typing[:0]
	if m.active == 0 {
		return
	}
	for _, u := range m.c.TypingUsers(m.active) {
		if u != m.me.ID {
			m.typing = append(m.typing, u)
		}
	}
}

func (m *Model) typingTick() tea.Cmd {
	if len(m.typing) == 0 || m.typingRun {
		return nil
	}
	m.typingRun = true
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return typingTickMsg{} })
}

// ---- channels ----

func (m *Model) openChannel(ch uint64) tea.Cmd {
	if ch == 0 {
		return nil
	}
	m.opened = true
	if m.jump.wide != 0 && m.jump.wide != ch {
		m.endWiden(false) // leaving the channel a jump widened
	}
	if ch == m.active && m.main.ch == ch {
		m.closeThread()
		m.main.scroll = 0
		return m.markRead()
	}
	m.active = ch
	m.seen(ch)
	m.main = pane{ch: ch, sel: -1}
	m.closeThread()
	if m.mode == modeSelect || m.mode == modeConfirmDelete || m.mode == modeReact {
		m.mode = modeNormal
	}
	m.cancelEdit()
	m.reloadPane(&m.main)
	m.reloadTyping()
	m.updatePlaceholder()
	var cmds []tea.Cmd
	if v, ok := m.chanView(ch); ok && !v.Member && v.Kind == proto.ChanPublic {
		cmds = append(cmds, m.call("join", func(ctx context.Context) error { return m.c.Join(ctx, ch) }))
	}
	cmds = append(cmds, m.markRead(), m.typingTick())
	m.unverifiedHint()
	if len(m.main.msgs) < 30 {
		cmds = append(cmds, m.fetchHistory(ch))
	}
	return tea.Batch(cmds...)
}

// markRead moves the active channel's read marker to its newest message
// while the view is at the bottom. Local and cheap.
func (m *Model) markRead() tea.Cmd {
	// Not gated on terminal focus: a visible but unfocused pane (a herdr
	// split next to an editor) is still being read.
	if m.active == 0 || m.main.scroll > 0 {
		return nil
	}
	if m.status.State == client.Syncing {
		// Read state is incomplete mid-sync; marking now would clobber
		// unread/mentions on every device. Online transition retries.
		return nil
	}
	for i := range m.chans {
		v := &m.chans[i]
		if v.ID != m.active {
			continue
		}
		last := max(v.Last, lastID(m.main.msgs))
		if !v.Member || last <= v.Read {
			return nil
		}
		if err := m.c.MarkRead(v.ID, last); err != nil {
			m.setErr("mark read", err)
			return nil
		}
		v.Read, v.Unread, v.Mention = last, 0, false
	}
	return nil
}

// sidebarOrder is the channel cycling order: what the sidebar shows.
func (m *Model) sidebarOrder() []client.ChannelView {
	var out []client.ChannelView
	for _, v := range m.chans {
		if v.Member || v.Removed || v.ID == m.active {
			out = append(out, v)
		}
	}
	return out
}

func (m *Model) cycle(dir int, unreadOnly bool) tea.Cmd {
	list := m.sidebarOrder()
	if len(list) == 0 {
		return nil
	}
	cur := slices.IndexFunc(list, func(v client.ChannelView) bool { return v.ID == m.active })
	for step := 1; step <= len(list); step++ {
		i := ((cur+dir*step)%len(list) + len(list)) % len(list)
		if cur < 0 {
			i = 0
			if dir < 0 {
				i = len(list) - 1
			}
		}
		if !unreadOnly || list[i].Unread > 0 || list[i].Mention {
			if list[i].ID == m.active {
				return nil
			}
			return m.openChannel(list[i].ID)
		}
	}
	return nil
}

// ---- threads ----

func (m *Model) openThread(root uint64) tea.Cmd {
	if root == 0 || m.active == 0 {
		return nil
	}
	m.thread = pane{ch: m.active, root: root, sel: -1}
	m.reloadPane(&m.thread)
	m.updatePlaceholder()
	// The cache may hold only some of the replies; fetch the newest page
	// from the server (results arrive as a normal Msgs update).
	ch, c, ctx := m.thread.ch, m.c, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		_, err := c.ThreadHistory(ctx, ch, root, 0)
		return threadLoadedMsg{ch, root, err}
	}
}

func (m *Model) closeThread() {
	m.jump.waiting = false
	m.thread = pane{sel: -1}
	m.updatePlaceholder()
}

// focus is the pane the input and selection act on.
func (m *Model) focus() *pane {
	if m.thread.open() {
		return &m.thread
	}
	return &m.main
}

// ---- history ----

func (m *Model) fetchHistory(ch uint64) tea.Cmd {
	if ch == 0 || m.histBusy[ch] || m.histDone[ch] || m.status.State != client.Online {
		return nil
	}
	var before uint64
	for _, x := range m.main.msgs {
		if x.ID != 0 && x.Chan == ch {
			before = x.ID
			break
		}
	}
	m.histBusy[ch] = true
	c, ctx := m.c, m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		more, err := c.History(ctx, ch, before)
		return historyMsg{ch, more, err}
	}
}

// loadOlder runs when the channel pane is scrolled to the top: show more of
// the cache first, then ask the server.
func (m *Model) loadOlder() tea.Cmd {
	p := &m.main
	if !p.open() || m.histBusy[p.ch] {
		return nil
	}
	count := func() int {
		n := 0
		for _, x := range p.msgs {
			if x.ID != 0 {
				n++
			}
		}
		return n
	}
	n := count()
	if n >= m.limit(p.ch) {
		m.limits[p.ch] = m.limit(p.ch) + pageLimit
		m.reloadPane(p)
		if count() > n {
			return nil
		}
	}
	return m.fetchHistory(p.ch)
}

// ---- notices ----

func (m *Model) setErr(what string, err error) {
	msg := err.Error()
	if errors.Is(err, client.ErrOffline) {
		msg = "offline — try again when connected"
	}
	var pe *proto.Error
	if errors.As(err, &pe) {
		msg = pe.Code
		if pe.Msg != "" {
			msg += ": " + pe.Msg
		}
	}
	if what != "" {
		msg = what + ": " + msg
	}
	m.setNotice(msg, true, false, 0)
}

// setInfo shows a notice that clears itself after infoTTL.
func (m *Model) setInfo(s string) { m.setNotice(s, false, false, 0) }

// setHint shows a notice that stays until clearNotice.
func (m *Model) setHint(s string) { m.setNotice(s, false, true, 0) }

func (m *Model) setNotice(s string, isErr, sticky bool, ttl time.Duration) {
	m.notice, m.noticeErr, m.noticeSticky, m.noticeTTL = sanitize.Line(s), isErr, sticky, ttl
	m.noticeDim = false
	m.noticeGen++
}

func (m *Model) clearNotice() {
	if m.notice == "" {
		return
	}
	m.notice, m.noticeErr, m.noticeSticky, m.noticeDim = "", false, false, false
	m.noticeGen++
}

// clearErr drops an error notice after something succeeded.
func (m *Model) clearErr() {
	if m.noticeErr {
		m.clearNotice()
	}
}

// noticeTick schedules expiry for the current notice once per generation.
func (m *Model) noticeTick() tea.Cmd {
	if m.notice == "" || m.noticeSticky || m.noticeTimed == m.noticeGen {
		return nil
	}
	m.noticeTimed = m.noticeGen
	d := m.noticeTTL
	if d == 0 {
		d = infoTTL
		if m.noticeErr {
			d = errTTL
		}
	}
	gen := m.noticeGen
	return tea.Tick(d, func(time.Time) tea.Msg { return noticeExpireMsg{gen} })
}

// ---- clipboard ----

// copyCmd puts text on the clipboard (see clip). It only says "copied" when
// a method was actually attempted.
func (m *Model) copyCmd(text string) tea.Cmd {
	ctx, opt := m.ctx, m.opt
	return func() tea.Msg {
		// May run a clipboard tool: never on the UI goroutine.
		how, err := clip(ctx, opt, text)
		switch {
		case err != nil:
			return errMsg{"copy", err}
		case how == "":
			return errMsg{"copy", errors.New("no clipboard available")}
		}
		return infoMsg("copied")
	}
}

// ---- Update ----

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.update(msg)
	if m.active != m.hud.seenActive {
		m.hud.seenActive = m.active
		m.refreshHuddle()
	}
	m.checkRing()
	if t := m.schedulePrefsTick(); t != nil {
		cmd = tea.Batch(cmd, t)
	}
	m.syncDraft()
	m.updatePlaceholder()
	if t := m.draftTick(); t != nil {
		cmd = tea.Batch(cmd, t)
	}
	if t := m.noticeTick(); t != nil {
		cmd = tea.Batch(cmd, t)
	}
	if t := m.badgeTick(); t != nil {
		cmd = tea.Batch(cmd, t)
	}
	return m, cmd
}

func (m *Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if cmd, ok := m.huddleMsg(msg); ok {
		return m, cmd
	}
	switch msg := msg.(type) {
	case noticeExpireMsg:
		if msg.gen == m.noticeGen {
			m.clearNotice()
		}
		return m, nil
	case okMsg:
		m.clearErr()
		return m, nil
	case threadLoadedMsg:
		if msg.err != nil {
			if !errors.Is(msg.err, client.ErrOffline) {
				m.setErr("thread history", msg.err)
			}
		} else {
			m.clearErr()
		}
		if m.thread.open() && m.thread.ch == msg.ch && m.thread.root == msg.root {
			m.reloadPane(&m.thread)
		}
		m.finishJump(true)
		return m, nil
	case activityRefreshMsg:
		m.act.timer = false
		if m.act.pending && m.mode == modeActivity {
			m.refreshActivity()
		}
		return m, nil
	case draftSaveMsg:
		m.draftTimer = false
		m.flushDrafts()
		return m, nil
	case notifyFlushMsg:
		return m, m.flushNote(msg)
	case ringMsg:
		return m, m.ringMsg(msg)
	case prefsTickMsg:
		return m, m.prefsTick(msg)
	case notifyTestMsg:
		return m, m.notifyTestNext()
	case badgeMsg:
		return m, m.reportBadge(msg)
	case awayTickMsg:
		return m, m.checkAway()
	case panesMsg:
		return m, m.gotPanes(msg)
	case verifyGHMsg:
		m.gotVerifyGH(msg)
		return m, nil
	case devicesMsg:
		return m, m.gotDevices(msg)
	case snippetMsg:
		m.gotSnippet(msg)
		return m, nil
	case paneReadMsg:
		m.gotPaneText(msg)
		return m, nil
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		m.layoutInput()
		m.clampScroll(&m.main)
		m.clampScroll(&m.thread)
		return m, nil
	case tea.FocusMsg:
		m.focused = true
		m.seen(m.active)
		return m, m.markRead()
	case tea.BlurMsg:
		m.focused = false
		return m, nil
	case updateMsg:
		if msg.Prefs || msg.Reset {
			m.refreshPrefs()
		}
		if msg.Huddle || msg.Reset || msg.Channels {
			m.refreshHuddle()
		}
		return m, tea.Batch(m.applyUpdate(client.Update(msg)), m.huddleStarted(msg.HuddleStarted), m.waitUpdate())
	case updatesClosedMsg:
		return m, nil
	case errMsg:
		if msg.what == "huddle" && m.nt.answering {
			m.stopRing(true) // answering failed: let the error show
		}
		m.setErr(msg.what, msg.err)
		return m, nil
	case infoMsg:
		m.setInfo(string(msg))
		return m, nil
	case inviteShownMsg:
		m.showInvite(msg)
		return m, nil
	case updateAvailMsg:
		m.newRelease = string(msg)
		return m, nil
	case historyMsg:
		m.histBusy[msg.ch] = false
		if msg.err != nil {
			if !errors.Is(msg.err, client.ErrOffline) {
				m.setErr("history", msg.err)
			}
			return m, nil
		}
		if !msg.more {
			m.histDone[msg.ch] = true
		}
		if msg.ch == m.main.ch {
			n := 0
			for _, x := range m.main.msgs {
				if x.ID != 0 {
					n++
				}
			}
			m.limits[msg.ch] = max(m.limit(msg.ch), n+pageLimit)
			m.reloadPane(&m.main)
		}
		return m, nil
	case openChanMsg:
		m.reloadChans()
		ch := msg.ch
		if ch == 0 { // left the active channel
			ch = m.defaultChannelExcept(m.active)
		}
		return m, m.openChannel(ch)
	case searchMsg:
		if msg.err != nil {
			m.setErr("search", msg.err)
			return m, nil
		}
		m.search = searchState{q: msg.q, res: msg.res}
		m.mode = modeSearch
		return m, nil
	case listMsg:
		m.list = listState(msg)
		m.mode = modeList
		return m, nil
	case pinsMsg:
		if msg.err != nil {
			m.setErr("pins", msg.err)
			return m, nil
		}
		if msg.ch != m.active {
			return m, nil // switched channels while it loaded
		}
		m.search = searchState{pins: msg.ch, res: msg.res}
		m.mode = modeSearch
		return m, nil
	case typingTickMsg:
		m.typingRun = false
		m.reloadTyping()
		return m, m.typingTick()
	case tea.MouseMsg:
		if msg.Action != tea.MouseActionPress {
			return m, nil
		}
		if m.mode == modeActivity { // the overlay covers the log: scroll its list
			m.activityWheel(msg.Button)
			return m, nil
		}
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			return m, m.scrollBy(m.focus(), 3)
		case tea.MouseButtonWheelDown:
			return m, m.scrollBy(m.focus(), -3)
		}
		return m, nil
	case tea.KeyMsg:
		return m, m.handleKey(msg)
	}
	// Cursor blink and other component messages.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

func (m *Model) applyUpdate(u client.Update) tea.Cmd {
	var cmds []tea.Cmd
	if u.Status || u.Reset {
		prev := m.status
		m.status = m.c.Status()
		if m.status.Stopped && m.status.Err != "" && !prev.Stopped {
			m.setNotice("disconnected: "+m.status.Err, true, false, 0)
		}
		if m.status.State == client.Online && prev.State != client.Online {
			if len(m.main.msgs) < 30 {
				cmds = append(cmds, m.fetchHistory(m.active))
			}
			cmds = append(cmds, m.markRead())
		}
	}
	if u.Reset || u.Channels {
		m.refreshDrafts()
	}
	if u.Reset {
		m.reloadAll()
		cmds = append(cmds, m.openDefault())
		if m.mode == modeActivity {
			m.refreshActivity()
		}
		cmds = append(cmds, m.markRead())
		return tea.Batch(cmds...)
	}
	if u.Users {
		m.reloadUsers()
	} else if u.Presence {
		m.reloadPresence()
	}
	if u.Channels || u.Users {
		m.reloadChans()
		cmds = append(cmds, m.openDefault())
	}
	if u.Msgs[m.main.ch] {
		m.reloadPane(&m.main)
	}
	if m.thread.open() && u.Msgs[m.thread.ch] {
		m.reloadPane(&m.thread)
		m.finishJump(false)
	}
	if u.Typing[m.active] {
		m.reloadTyping()
		cmds = append(cmds, m.typingTick())
	}
	if len(u.Failed) > 0 {
		f := u.Failed[len(u.Failed)-1]
		m.setNotice("message not sent: "+f.Failed, true, false, 0)
	}
	m.devicesNotice(u.Revoked, u.NewOwn)
	cmds = append(cmds, m.notifyFor(u.New))
	if m.mode == modeActivity && m.activityAffected(u) {
		cmds = append(cmds, m.queueActivityRefresh())
	}
	if u.Msgs[m.active] || u.Channels {
		cmds = append(cmds, m.markRead())
	}
	return tea.Batch(cmds...)
}

// ---- keys ----

func (m *Model) handleKey(k tea.KeyMsg) tea.Cmd {
	if cmd := m.keyActivity(); cmd != nil {
		return tea.Batch(cmd, m.handleKey1(k))
	}
	return m.handleKey1(k)
}

func (m *Model) handleKey1(k tea.KeyMsg) tea.Cmd {
	key := keyName(k)
	if key == kQuit {
		if m.ctrlC {
			return tea.Quit
		}
		m.ctrlC = true
		m.setInfo("press Ctrl-C again to quit")
		return nil
	}
	if m.ctrlC {
		m.ctrlC = false
		if !m.noticeErr {
			m.clearNotice()
		}
	}
	if key != kEsc {
		m.escAt = time.Time{}
	}
	if key == kTalk {
		if cmd, ok := m.talkKey(); ok {
			return cmd
		}
	}

	switch m.mode {
	case modeSwitcher:
		return m.switcherKey(k)
	case modeSearch:
		return m.searchKey(k)
	case modeActivity:
		return m.activityKey(k)
	case modeList:
		m.listKey(keyName(k))
		return nil
	case modeHelp:
		m.helpKey(key)
		return nil
	case modeVerify:
		m.verifyKey(key)
		return nil
	case modeDevices:
		return m.devicesKey(key)
	case modeDisable:
		return m.disableKey(key)
	case modeViewer:
		if key == kSelCopy {
			if m.view.copy != "" {
				return m.copyCmd(m.view.copy)
			}
			return m.copyCmd(strings.Join(m.view.lines, "\n"))
		}
		m.viewerKey(key)
		return nil
	case modeConfirmDelete:
		m.mode = modeSelect
		m.clearNotice()
		if key == kConfirmYes {
			id := m.target.ID
			return m.call("delete", func(ctx context.Context) error { return m.c.Delete(ctx, id) })
		}
		return nil
	case modeReact:
		return m.reactKey(k)
	case modePanePick:
		return m.panePickKey(key)
	case modeSharePreview:
		return m.sharePreviewKey(key)
	case modeSelect:
		if cmd, handled := m.selectKey(k); handled {
			m.trimWiden()
			return cmd
		}
		m.leaveSelect()
	}
	return m.normalKey(k)
}

func (m *Model) normalKey(k tea.KeyMsg) tea.Cmd {
	key := keyName(k)
	empty := m.input.Value() == ""
	if m.compShown() {
		// The popup's keys come first, Esc included (before the mic or a ring).
		if cmd, ok := m.compKey(key); ok {
			return cmd
		}
	}
	switch {
	case key == kTab && m.tabComplete():
		return nil
	case key == kSwitcher:
		m.openSwitcher()
		return nil
	case key == kActivity:
		m.openActivity()
		return nil
	case key == kSidebar || key == kSidebarAlt:
		switch {
		case m.sidebarShown():
			m.sidebar = -1
		case m.w < minSidebarCols:
			m.setInfo("too narrow for sidebar")
			return nil
		default:
			m.sidebar = 1
		}
		m.layoutInput()
		m.clampScroll(&m.main)
		m.clampScroll(&m.thread)
		return nil
	case key == kHelpAlt:
		m.openHelp()
		return nil
	case key == kCtrlEnd || (key == kEnd && empty):
		p := m.focus()
		p.scroll = 0
		if p == &m.main {
			return m.markRead()
		}
		return nil
	case key == kClearLine:
		m.clearInput()
		return nil
	case isPrevChan(key):
		return m.cycle(-1, false)
	case isNextChan(key):
		return m.cycle(1, false)
	case isPrevUnread(key):
		return m.cycle(-1, true)
	case isNextUnread(key):
		return m.cycle(1, true)
	case key == kPgUp:
		return m.scrollBy(m.focus(), max(m.dims().bodyH-1, 1))
	case key == kPgDown:
		return m.scrollBy(m.focus(), -max(m.dims().bodyH-1, 1))
	case key == kEsc:
		// One Esc backs out one thing: an open mic first (we're
		// broadcasting), then a ring, then the usual chain.
		if m.micOff() {
			return nil
		}
		if m.nt.ring.ch != 0 {
			m.stopRing(true) // dismissed
			return nil
		}
		switch {
		case m.editing != 0:
			m.cancelEdit()
		case m.thread.open():
			m.closeThread()
			m.layoutInput()
		case !empty:
			if !m.escAt.IsZero() && m.now().Sub(m.escAt) <= escWindow {
				m.escAt = time.Time{}
				m.clearInput()
				m.clearNotice()
				return nil
			}
			m.escAt = m.now()
			m.setNotice("Esc again to clear draft", false, false, escWindow)
		default:
			m.clearNotice()
		}
		return nil
	case key == kEnter:
		return m.submit()
	case key == kUp && empty:
		m.enterSelect()
		return nil
	}
	before := m.input.Value()
	var cmd tea.Cmd
	m.input.SetHeight(m.inputMax()) // see layoutInput
	if keyName(k) == "" && k.Type == tea.KeyRunes {
		// Multi-rune input is text: the textarea's own keymap would read
		// a typed "down" as cursor-down. InsertString sanitizes like typing.
		m.input.InsertString(string(k.Runes))
		// A paste (or a burst that may be one) never opens the popup, so a
		// pasted line followed by Enter is sent, not completed.
		m.closeComp()
	} else {
		m.input, cmd = m.input.Update(k)
		if m.input.Value() != before {
			m.refreshComp()
		} else {
			m.hideComp() // a cursor move never opens it, so ↑/↓ and Enter stay theirs
		}
	}
	m.layoutInput()
	if v := m.input.Value(); v != before && v != "" && !strings.HasPrefix(v, "/") && m.editing == 0 {
		cmd = tea.Batch(cmd, m.sendTyping())
	}
	return cmd
}

func (m *Model) sendTyping() tea.Cmd {
	if m.active == 0 || m.status.State != client.Online || m.now().Sub(m.lastTyping) < 3*time.Second {
		return nil
	}
	m.lastTyping = m.now()
	ch, c := m.active, m.c
	return func() tea.Msg {
		_ = c.SendTyping(ch) // best effort
		return noopMsg{}
	}
}

// ---- scrolling ----

func (m *Model) paneWidth(p *pane) int {
	d := m.dims()
	if p == &m.thread {
		return d.thrW
	}
	return d.msgW
}

func (m *Model) clampScroll(p *pane) {
	if !p.open() {
		return
	}
	w, h := m.paneWidth(p), m.dims().bodyH
	if w <= 0 || h <= 0 {
		return
	}
	lines, _ := m.paneLines(p, w)
	p.scroll = min(max(p.scroll, 0), max(len(lines)-h, 0))
}

// scrollBy scrolls p up by n lines (down if negative); reaching the top of
// the channel pane loads older messages.
func (m *Model) scrollBy(p *pane, n int) tea.Cmd {
	if !p.open() {
		return nil
	}
	w, h := m.paneWidth(p), m.dims().bodyH
	if w <= 0 || h <= 0 {
		return nil
	}
	lines, _ := m.paneLines(p, w)
	top := max(len(lines)-h, 0)
	p.scroll = min(max(p.scroll+n, 0), top)
	if p == &m.main {
		if n > 0 && p.scroll == top {
			return m.loadOlder()
		}
		if p.scroll == 0 {
			return m.markRead()
		}
	}
	return nil
}

// ---- select mode ----

func (m *Model) enterSelect() {
	p := m.focus()
	if len(p.msgs) == 0 {
		return
	}
	m.mode = modeSelect
	p.sel = len(p.msgs) - 1
	m.ensureVisible(p)
	m.updatePlaceholder()
}

func (m *Model) leaveSelect() {
	m.mode = modeNormal
	m.main.sel, m.thread.sel = -1, -1
	if m.jump.wide != 0 {
		m.endWiden(true)
		m.main.scroll = 0
	}
	m.updatePlaceholder()
}

func (m *Model) selected() (client.Message, bool) {
	p := m.focus()
	if p.sel < 0 || p.sel >= len(p.msgs) {
		return client.Message{}, false
	}
	return p.msgs[p.sel], true
}

// ensureVisible scrolls so the selected message is on screen.
func (m *Model) ensureVisible(p *pane) {
	w, h := m.paneWidth(p), m.dims().bodyH
	if w <= 0 || h <= 0 || p.sel < 0 {
		return
	}
	lines, owner := m.paneLines(p, w)
	first, last := -1, -1
	for i, o := range owner {
		if o == p.sel {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return
	}
	total := len(lines)
	// Visible range is [total-h-scroll, total-scroll).
	if bottom := total - p.scroll; last >= bottom {
		p.scroll = total - last - 1
	}
	if top := total - h - p.scroll; first < top {
		p.scroll = total - h - first
	}
	p.scroll = min(max(p.scroll, 0), max(total-h, 0))
}

// selectByPage moves the selection about one screen of lines up (dir -1)
// or down (dir 1).
func (m *Model) selectByPage(p *pane, dir int) {
	w, h := m.paneWidth(p), m.dims().bodyH
	if w <= 0 || p.sel < 0 {
		return
	}
	_, owner := m.paneLines(p, w)
	cur := -1
	for i, o := range owner {
		if o == p.sel {
			cur = i
			if dir < 0 {
				break
			}
		}
	}
	if cur < 0 {
		return
	}
	target := min(max(cur+dir*max(h-1, 1), 0), len(owner)-1)
	// Land on the nearest message line in the direction of travel.
	for target >= 0 && target < len(owner) && owner[target] < 0 {
		target -= dir
	}
	if target >= 0 && target < len(owner) && owner[target] >= 0 && owner[target] != p.sel {
		p.sel = owner[target]
	} else if dir < 0 {
		p.sel = 0
	} else {
		p.sel = len(p.msgs) - 1
	}
	m.ensureVisible(p)
}

// ---- help ----

func (m *Model) openHelp() {
	m.leaveSelect()
	m.mode = modeHelp
	m.helpScroll = 0
}

func (m *Model) helpKey(key string) {
	page := max(m.dims().bodyH-2, 1)
	switch key {
	case kEsc, kHelp, kHelpAlt, "q", kEnter:
		m.mode = modeNormal
	case kUp, "k":
		m.helpScroll--
	case kDown, "j":
		m.helpScroll++
	case kPgUp:
		m.helpScroll -= page
	case kPgDown, " ":
		m.helpScroll += page
	case kHome, "g":
		m.helpScroll = 0
	case kEnd, "G":
		m.helpScroll = len(helpLines)
	}
	m.helpScroll = min(max(m.helpScroll, 0), m.helpMaxScroll())
}

func (m *Model) selectKey(k tea.KeyMsg) (tea.Cmd, bool) {
	p := m.focus()
	key := keyName(k)
	switch key {
	case kEsc:
		m.leaveSelect()
		return nil, true
	case kUp:
		if p.sel > 0 {
			p.sel--
			m.ensureVisible(p)
		} else if p == &m.main {
			return m.loadOlder(), true
		}
		return nil, true
	case kDown:
		if p.sel < len(p.msgs)-1 {
			p.sel++
			m.ensureVisible(p)
		} else {
			m.leaveSelect()
			p.scroll = 0
			return m.markRead(), true
		}
		return nil, true
	case kPgUp:
		m.selectByPage(p, -1)
		return nil, true
	case kPgDown:
		m.selectByPage(p, 1)
		return nil, true
	case kHome, "g":
		p.sel = 0
		m.ensureVisible(p)
		return nil, true
	case kEnd, kCtrlEnd, "G":
		p.sel = len(p.msgs) - 1
		m.ensureVisible(p)
		return nil, true
	}
	sel, ok := m.selected()
	if !ok {
		return nil, false
	}
	mine := sel.Author == m.me.ID
	switch key {
	case kSelEdit:
		if !mine || sel.ID == 0 || sel.Deleted {
			m.setInfo("you can only edit your own sent messages")
			return nil, true
		}
		if !sel.Sec.Trusted() {
			// Encrypted channels: only verified messages carry the id an
			// encrypted edit refers to.
			m.setInfo("this message can't be edited")
			return nil, true
		}
		m.leaveSelect()
		// A jump can leave select mode with a draft in the input: keep it
		// in its slot; cancelEdit puts it back.
		m.setDraft(m.dk, m.input.Value())
		m.editing = sel.ID
		m.input.SetValue(sanitize.Text(sel.Body))
		m.layoutInput()
		m.setHint("editing — Enter to save, Esc to cancel")
		return nil, true
	case kSelDelete:
		if sel.Failed != "" {
			if err := m.c.DiscardFailed(sel.Local); err != nil {
				m.setErr("discard", err)
			}
			return nil, true
		}
		if !mine || sel.ID == 0 || sel.Deleted {
			m.setInfo("you can only delete your own sent messages")
			return nil, true
		}
		m.target = sel
		m.mode = modeConfirmDelete
		m.setHint("delete this message? y to confirm")
		return nil, true
	case kSelReply:
		if sel.Failed != "" {
			if err := m.c.RetryFailed(sel.Local); err != nil {
				m.setErr("retry", err)
			}
			return nil, true
		}
		root := sel.ID
		if sel.Thread != 0 {
			root = sel.Thread
		}
		if root == 0 {
			return nil, true
		}
		m.leaveSelect()
		cmd := m.openThread(root)
		m.layoutInput()
		return cmd, true
	case kSelReact:
		if sel.ID == 0 || sel.Deleted {
			return nil, true
		}
		m.target = sel
		m.mode = modeReact
		m.line.Reset()
		m.line.Prompt = "react: "
		m.line.Placeholder = "+1, :tada:, or an emoji"
		m.line.Focus()
		return nil, true
	case kSelCopy:
		return m.copyCmd(sanitize.Text(sel.Body)), true
	case kSelPin:
		if sel.ID == 0 || sel.Deleted {
			return nil, true
		}
		id, pinned := sel.ID, sel.Pinned
		return m.callThen("pin", func(ctx context.Context) (tea.Msg, error) {
			if pinned {
				return infoMsg("unpinned"), m.c.Unpin(ctx, id)
			}
			return infoMsg("pinned"), m.c.Pin(ctx, id)
		}), true
	case kSelView:
		m.openViewer(sel)
		return nil, true
	}
	return nil, false
}

func (m *Model) reactKey(k tea.KeyMsg) tea.Cmd {
	switch keyName(k) {
	case kEsc:
		m.mode = modeSelect
		return nil
	case kEnter:
		m.mode = modeSelect
		emoji, ok := resolveEmoji(m.line.Value())
		if !ok {
			m.setErr("", fmt.Errorf("unknown emoji %q", m.line.Value()))
			return nil
		}
		id := m.target.ID
		mine := slices.Contains(m.target.Reactions[emoji], m.me.ID)
		return m.call("react", func(ctx context.Context) error {
			if mine {
				return m.c.Unreact(ctx, id, emoji)
			}
			return m.c.React(ctx, id, emoji)
		})
	}
	var cmd tea.Cmd
	m.line, cmd = m.line.Update(k)
	return cmd
}

// cancelEdit ends an edit: its text leaves the input and the draft that was
// there when the edit began (held in m.drafts meanwhile) comes back.
func (m *Model) cancelEdit() {
	if m.editing == 0 {
		return
	}
	m.editing = 0
	m.input.SetValue(m.drafts[m.dk])
	m.hideComp()
	m.layoutInput()
	m.clearNotice()
}
