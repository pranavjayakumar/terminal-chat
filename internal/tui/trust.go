package tui

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Trust UX (docs/E2E.md step 10, docs/TUI.md "Verifying people"):
// /verify (safety numbers, --github), the key-change banner and
// /accept-key, /alarm, /alarm-clear and /alarm-recover, "?" for members trusted only on
// first use, /members.

// verifyState is the trust overlay: a safety number to compare (/verify),
// the result of a GitHub check (gh != nil), or pending keys to accept
// (/accept-key, accept != nil). Keys are ignored for typeAhead after it
// opens, so a keystroke typed before it appeared can't confirm anything.
type verifyState struct {
	user   uint64
	sn     client.SafetyNumber
	gh     *client.GitHubCheck
	accept []string // the fingerprints shown, exactly what y acknowledges
	only   bool     // accept just these (one named key), not all pending
	opened time.Time
	scroll int
}

// typeAhead is how long the trust overlays ignore keys after opening.
const typeAhead = 500 * time.Millisecond

func (m *Model) openTrustOverlay(v verifyState) {
	m.leaveSelect()
	v.opened = m.now()
	m.verify = v
	m.mode = modeVerify
}

type verifyGHMsg struct {
	res client.GitHubCheck
	err error
}

// name is "@name" for user, sanitized.
func (m *Model) atName(u uint64) string {
	n := sanitize.Line(m.users[u].Name)
	if n == "" {
		n = "user" + fmt.Sprint(u)
	}
	return "@" + n
}

// refreshTrust reloads who is unverified and which e2e channels have a
// member with an unacknowledged key change (with m.e2e, in reloadChans).
func (m *Model) refreshTrust() {
	m.unverified = map[uint64]bool{}
	for _, u := range m.c.UnverifiedUsers() {
		m.unverified[u] = true
	}
	m.keyChg = map[uint64][]uint64{}
	for ch := range m.e2e {
		if us := m.c.KeyChangedMembers(ch); len(us) > 0 {
			m.keyChg[ch] = us
		}
	}
}

// bannerText is the sticky key-change banner for the active channel ("" if
// none). One line, however many members changed keys.
func (m *Model) bannerText() string {
	us := m.keyChg[m.active]
	switch len(us) {
	case 0:
		return ""
	case 1:
		// The command first: it must survive truncation in a narrow pane.
		n := m.atName(us[0])
		return fmt.Sprintf("⚠ /accept-key %s, then /verify %s — %s's key changed", n, n, n)
	}
	names := make([]string, len(us))
	for i, u := range us {
		names[i] = m.atName(u)
	}
	return "⚠ /accept-key, then /verify each — keys changed: " + strings.Join(names, ", ")
}

// unverifiedIn lists ch's other members trusted only on first use (e2e
// channels only).
func (m *Model) unverifiedIn(ch uint64) []uint64 {
	if _, ok := m.e2e[ch]; !ok {
		return nil
	}
	v, _ := m.chanView(ch)
	var out []uint64
	for _, u := range v.Members {
		if u != m.me.ID && m.unverified[u] {
			out = append(out, u)
		}
	}
	return out
}

// unverifiedHint shows, once per channel per run, a dim hint that "?" marks
// members not verified yet.
func (m *Model) unverifiedHint() {
	ch := m.active
	if ch == 0 || m.hinted[ch] || m.notice != "" {
		return
	}
	us := m.unverifiedIn(ch)
	if len(us) == 0 {
		return
	}
	if m.hinted == nil {
		m.hinted = map[uint64]bool{}
	}
	m.hinted[ch] = true
	m.setNotice("? = not verified yet — compare safety numbers with /verify "+m.atName(us[0]), false, false, errTTL)
	m.noticeDim = true
	m.noticeCh = ch // about this channel: switching away drops it
}

// ---- commands ----

func (m *Model) trustCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "verify":
		cmd, err := m.verifyCommand(c)
		return cmd, true, err
	case "accept-key":
		if len(c.args) < 1 || len(c.args) > 2 {
			return nil, true, errors.New("usage: /accept-key @user [fingerprint]")
		}
		u, ok := m.findUser(c.args[0])
		if !ok {
			return nil, true, fmt.Errorf("unknown user %s", sanitize.Line(c.args[0]))
		}
		pend := m.c.PendingKeys(u)
		if len(pend) == 0 {
			return nil, true, fmt.Errorf("no pending key change for %s", m.atName(u))
		}
		v := verifyState{user: u, accept: pend}
		if len(c.args) == 2 {
			if !slices.Contains(pend, c.args[1]) {
				return nil, true, fmt.Errorf("%s has no pending key %s (/accept-key %s lists them)",
					m.atName(u), sanitize.Line(c.args[1]), m.atName(u))
			}
			v.accept, v.only = []string{c.args[1]}, true
		}
		m.openTrustOverlay(v)
		return nil, true, nil
	case "alarm":
		ch, err := m.needActive()
		if err != nil {
			return nil, true, err
		}
		info, ok := m.c.Alarm(ch)
		if !ok {
			return nil, true, fmt.Errorf("no alarm in %s", m.chanTitle(ch))
		}
		m.list = listState{title: "alarm in " + m.chanTitle(ch), rows: m.alarmRows(ch, info)}
		m.mode = modeList
		return nil, true, nil
	case "alarm-clear":
		return nil, true, m.alarmClear(c)
	case "alarm-recover":
		cmd, err := m.alarmRecover(c)
		return cmd, true, err
	case "members":
		ch, err := m.needActive()
		if err != nil {
			return nil, true, err
		}
		m.list = listState{title: "members of " + m.chanTitle(ch), rows: m.memberRows(ch)}
		m.mode = modeList
		return nil, true, nil
	}
	return nil, false, nil
}

func (m *Model) needActive() (uint64, error) {
	if m.active == 0 {
		return 0, errors.New("no channel open")
	}
	return m.active, nil
}

func (m *Model) verifyCommand(c command) (tea.Cmd, error) {
	const usage = "usage: /verify @user [--github login]"
	var who, login string
	for i := 0; i < len(c.args); i++ {
		switch a := c.args[i]; {
		case a == "--github":
			if i+1 >= len(c.args) || login != "" {
				return nil, errors.New(usage)
			}
			login = c.args[i+1]
			i++
		case who == "":
			who = a
		default:
			return nil, errors.New(usage)
		}
	}
	if who == "" {
		return nil, errors.New(usage)
	}
	u, ok := m.findUser(who)
	if !ok {
		return nil, fmt.Errorf("unknown user %s", sanitize.Line(who))
	}
	if u == m.me.ID {
		return nil, errors.New("that's you — verify someone else")
	}
	if login != "" {
		be := m.c
		ctx := m.ctx
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, reqTimeout)
			defer cancel()
			res, err := be.VerifyGitHub(ctx, u, login)
			return verifyGHMsg{res, err}
		}, nil
	}
	sn, err := m.c.SafetyNumber(u)
	if err != nil {
		return nil, err
	}
	m.openTrustOverlay(verifyState{user: u, sn: sn})
	return nil, nil
}

func (m *Model) gotVerifyGH(msg verifyGHMsg) {
	if msg.err != nil {
		m.setErr("verify", msg.err)
		return
	}
	res := msg.res
	m.openTrustOverlay(verifyState{user: res.User, gh: &res})
}

func (m *Model) verifyKey(key string) {
	v := &m.verify
	if m.now().Sub(v.opened) < typeAhead && key != kEsc {
		return // only cancelling is safe to act on before it could be read
	}
	switch key {
	case kEsc, "q", kEnter:
		m.mode = modeNormal
		switch {
		case v.accept != nil:
			m.setInfo("key change not accepted")
		case v.gh == nil || v.gh.Match:
			m.setInfo("not verified")
		}
	case "y":
		if v.accept == nil {
			return
		}
		err := m.c.AcceptKeyChanges(v.user, v.accept, v.only)
		if errors.Is(err, client.ErrPendingChanged) {
			// Something changed while the overlay was up: show what's pending now.
			pend := m.c.PendingKeys(v.user)
			if len(pend) == 0 || v.only {
				m.mode = modeNormal
				m.setErr("accept-key", err)
				return
			}
			m.openTrustOverlay(verifyState{user: v.user, accept: pend})
			m.setErr("accept-key", err)
			return
		}
		m.mode = modeNormal
		if err != nil {
			m.setErr("accept-key", err)
			return
		}
		m.reloadChans()
		n := m.atName(v.user)
		m.setInfo(fmt.Sprintf("accepted %s's new key — unverified until /verify %s", n, n))
	case "v":
		var err error
		switch {
		case v.accept != nil:
			return
		case v.gh != nil:
			if !v.gh.Match {
				return
			}
			err = m.c.ConfirmGitHub(*v.gh)
		default:
			err = m.c.MarkVerified(v.user, v.sn.Digits)
		}
		m.mode = modeNormal
		if err != nil {
			m.setErr("verify", err)
			return
		}
		m.reloadChans()
		m.setInfo(fmt.Sprintf("✓ %s verified", m.atName(v.user)))
	case kUp, "k":
		v.scroll = max(v.scroll-1, 0)
	case kDown, "j":
		v.scroll++
	case kPgUp:
		v.scroll = max(v.scroll-max(m.dims().bodyH-2, 1), 0)
	case kPgDown, " ":
		v.scroll += max(m.dims().bodyH-2, 1)
	case kHome, "g":
		v.scroll = 0
	}
}

// wrapPlain wraps plain (unstyled) text to width w at spaces, cutting
// words longer than w.
func wrapPlain(s string, w int) []string {
	w = max(w, 1)
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		for strWidth(word) > w {
			if line != "" {
				out = append(out, line)
				line = ""
			}
			var head string
			head, word = splitWidth(word, w)
			if head == "" {
				head, word = word, ""
			}
			out = append(out, head)
		}
		switch {
		case word == "":
		case line == "":
			line = word
		case strWidth(line)+1+strWidth(word) <= w:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" || len(out) == 0 {
		out = append(out, line)
	}
	return out
}

type styledRow struct {
	text string
	st   *lipgloss.Style
}

// verifyRows is the overlay's content at width w (title excluded).
func (m *Model) verifyRows(w int) []string {
	v := &m.verify
	var rows []styledRow
	add := func(s string, st *lipgloss.Style) {
		for _, l := range wrapPlain(s, w-1) {
			rows = append(rows, styledRow{l, st})
		}
	}
	gap := func() { rows = append(rows, styledRow{}) }
	them := m.atName(v.user)
	switch {
	case v.accept != nil:
		bad := m.st.noticeErr
		add(fmt.Sprintf("%s presented SSH keys this device hasn't pinned:", them), &bad)
		for _, fp := range v.accept {
			add("  "+sanitize.Line(fp), nil)
		}
		gap()
		add("Accepting trusts exactly these keys on first use: "+them+" stays unverified. Then compare "+
			"safety numbers with /verify "+them+". If you don't expect a new key (a new laptop, a new SSH key), "+
			"ask "+them+" before accepting.", nil)
		gap()
		add("Press y to accept, Esc to cancel.", nil)
	case v.gh != nil:
		g := v.gh
		login := sanitize.Line(g.Login)
		ok := m.st.noticeInfo
		bad := m.st.noticeErr
		if g.Match {
			add(fmt.Sprintf("✓ every key this device pinned for %s is on github.com/%s", them, login), &ok)
			gap()
			add(fmt.Sprintf("Press v to verify %s because you confirmed github.com/%s is theirs; Esc to cancel.", them, login), nil)
		} else {
			add(fmt.Sprintf("✗ MISMATCH: %d of %s's pinned keys aren't on github.com/%s — nothing was verified", len(g.Missing), them, login), &bad)
		}
		gap()
		add("Keys this device pinned for "+them+":", nil)
		for _, fp := range g.Pinned {
			mark, st := "  ✓ ", (*lipgloss.Style)(nil)
			for _, x := range g.Missing {
				if x == fp {
					mark, st = "  ✗ ", &bad
				}
			}
			add(mark+sanitize.Line(fp), st)
		}
		gap()
		add("Keys on github.com/"+login+":", nil)
		if len(g.GitHub) == 0 {
			add("  (none)", nil)
		}
		for _, fp := range g.GitHub {
			add("  "+sanitize.Line(fp), nil)
		}
		if !g.Match {
			gap()
			add("Ask "+them+" which account holds their tc key, or compare safety numbers: /verify "+them, nil)
		}
	default:
		dim := m.st.dim
		add("Safety number with "+them+":", nil)
		per := 4
		if w < 26 {
			per = 2
		}
		gs := v.sn.Groups()
		for i := 0; i < len(gs); i += per {
			rows = append(rows, styledRow{"  " + strings.Join(gs[i:min(i+per, len(gs))], " "), &m.st.overlayTitle})
		}
		gap()
		add("Compare it with "+them+" over another channel (in person or a call). Press v if every digit matches, Esc to cancel.", nil)
		gap()
		for _, p := range []client.SafetyParty{v.sn.Me, v.sn.Them} {
			label := m.atName(p.User)
			if p.User == m.me.ID {
				label = "you (" + label + ")"
			}
			add(label+" — SSH keys:", nil)
			for _, fp := range p.Fingerprints {
				add("  "+sanitize.Line(fp), &dim)
			}
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		l := " " + r.text
		if r.st != nil {
			l = " " + r.st.Render(r.text)
		}
		out[i] = fit(truncate(l, w), w)
	}
	return out
}

func (m *Model) verifyLines(w, h int) []string {
	v := &m.verify
	title := fmt.Sprintf("verify %s — v: they match · Esc: cancel", m.atName(v.user))
	switch {
	case v.accept != nil:
		title = fmt.Sprintf("accept %s's new key? y: accept · Esc: cancel", m.atName(v.user))
	case v.gh != nil && v.gh.Match:
		title = fmt.Sprintf("GitHub check for %s — v: verify · Esc: cancel", m.atName(v.user))
	case v.gh != nil:
		title = fmt.Sprintf("GitHub check for %s — Esc closes", m.atName(v.user))
	}
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
	rows := m.verifyRows(w)
	n := max(h-1, 0)
	v.scroll = min(max(v.scroll, 0), max(len(rows)-n, 0))
	end := min(v.scroll+n, len(rows))
	out = append(out, rows[v.scroll:end]...)
	if end < len(rows) && n > 0 {
		out[len(out)-1] = fit(m.st.dim.Render(truncate(" ↓ more", w)), w)
	}
	return padLines(out, w, h)
}

// ---- alarms ----

func (m *Model) alarmRows(ch uint64, a client.AlarmInfo) []string {
	w := max(m.dims().mainW-2, 10)
	var rows []string
	add := func(s string) { rows = append(rows, wrapPlain(s, w)...) }
	add("⚠ " + a.Detail)
	if !a.Since.IsZero() {
		add("raised " + a.Since.Local().Format("2006-01-02 15:04:05"))
	}
	if a.Server {
		add("(" + serverWide + ")")
	}
	rows = append(rows, "")
	add(a.Why)
	rows = append(rows, "")
	if a.Computed {
		add("It clears by itself when its cause is gone.")
	} else {
		scope := ""
		if a.Server {
			scope = " (" + serverWide + ")"
		}
		add("Nothing is sent here while it lasts.")
		if a.Chain {
			add("Clearing doesn't help while the server still shows this: it comes back at once. If the server " +
				"was restored from a backup, /alarm-recover shows whether this device can accept the server's older " +
				"key chain and exactly what that discards; it never does anything without your confirmation.")
			add("If you know the cause is gone, /alarm-clear " + m.chanConfirmName(ch) + " clears it (recorded in the " +
				"trust audit log).")
		} else {
			add("Clear it only once you know why it happened: /alarm-clear " + m.chanConfirmName(ch) + scope +
				". It comes back if the server still presents what raised it. Clearing is recorded in the trust audit log.")
		}
	}
	return rows
}

// recoverMsg is the result of /alarm-recover: a preview (done false) or
// the accepted recovery (done true).
type recoverMsg struct {
	ch   uint64
	plan client.ChainRecovery
	done bool
	err  error
}

// alarmRecover is /alarm-recover: with no name it previews (online) what
// accepting the server's chain would do and keeps that preview; with the
// channel's name typed it accepts exactly the kept preview — the client
// refuses if the server's chain isn't the same any more.
func (m *Model) alarmRecover(c command) (tea.Cmd, error) {
	ch, err := m.needActive()
	if err != nil {
		return nil, err
	}
	if _, ok := m.c.Alarm(ch); !ok {
		return nil, fmt.Errorf("no alarm in %s", m.chanTitle(ch))
	}
	be, ctx := m.c, m.ctx
	if c.rest == "" {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, reqTimeout)
			defer cancel()
			plan, err := be.ChainRecoveryPlan(ctx, ch)
			return recoverMsg{ch: ch, plan: plan, err: err}
		}, nil
	}
	want := m.chanConfirmName(ch)
	if !confirmMatches(c.rest, want) {
		return nil, fmt.Errorf("that's not this channel's name — /alarm-recover previews first, then /alarm-recover %s", want)
	}
	plan, ok := m.recoverPl[ch]
	if !ok {
		return nil, fmt.Errorf("preview first: /alarm-recover (shows what accepting discards), then /alarm-recover %s", want)
	}
	delete(m.recoverPl, ch) // one confirmation per preview
	m.setInfo("accepting the server's key chain…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		return recoverMsg{ch: ch, plan: plan, done: true, err: be.RecoverChain(ctx, plan)}
	}, nil
}

func (m *Model) gotRecover(msg recoverMsg) {
	if msg.err != nil {
		m.setErr("alarm-recover", msg.err)
		return
	}
	name := m.chanConfirmName(msg.ch)
	if msg.done {
		m.reloadChans()
		p := msg.plan
		if p.Fork {
			m.setInfo(fmt.Sprintf("%s follows the server's key chain again (epoch %d) — recorded in the trust audit log", name, p.Top))
		} else {
			m.setInfo(fmt.Sprintf("%s rewound to epoch %d and a fresh key started — recorded in the trust audit log", name, p.Ancestor))
		}
		return
	}
	if m.recoverPl == nil {
		m.recoverPl = map[uint64]client.ChainRecovery{}
	}
	m.recoverPl[msg.ch] = msg.plan
	m.list = listState{title: "accept the server's key chain for " + name + "?", rows: m.recoverRows(msg.plan)}
	m.mode = modeList
}

func (m *Model) names(us []uint64) string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = m.atName(u)
	}
	if len(out) == 0 {
		return "nobody"
	}
	return strings.Join(out, " ")
}

// recoverRows says what confirming p accepts and discards.
func (m *Model) recoverRows(p client.ChainRecovery) []string {
	w := max(m.dims().mainW-2, 10)
	var rows []string
	add := func(s string) { rows = append(rows, wrapPlain(s, w)...) }
	name := m.chanConfirmName(p.Chan)
	lost := fmt.Sprintf("epoch %d", p.Head)
	if p.Discarded() > 1 {
		lost = fmt.Sprintf("epochs %d–%d", p.Ancestor+1, p.Head)
	}
	if p.Fork {
		add(fmt.Sprintf("The server's key chain is the one this device verified up to epoch %d, then goes on with "+
			"different epochs %d–%d, the first made by %s. That is what another member recovering from a server "+
			"restored from a backup looks like — and also what that member and the server splitting the channel "+
			"would look like.", p.Ancestor, p.Ancestor+1, p.Top, m.atName(p.Rotator)))
		rows = append(rows, "")
		add(fmt.Sprintf("Accepting discards %s on this device and follows the server's chain. Its key goes to: %s.",
			lost, m.names(p.Roster)))
	} else {
		add(fmt.Sprintf("The server's key chain ends at epoch %d; this device verified up to epoch %d. Epochs up to "+
			"%d are the very ones this device verified, so the server is missing %s. That is what a server "+
			"restored from a backup taken before them looks like — and also what a server hiding them on purpose "+
			"(for example to undo a removal) would look like.", p.Ancestor, p.Head, p.Ancestor, lost))
		rows = append(rows, "")
		add(fmt.Sprintf("Accepting discards %s on this device and whatever changed in it, then starts a fresh key "+
			"(epoch %d) for: %s.", lost, p.Ancestor+1, m.names(p.Roster)))
	}
	for _, u := range p.Readded {
		rows = append(rows, "")
		add(fmt.Sprintf("⚠ %s wasn't in epoch %d (removed after the backup?) and gets the key again. /remove "+
			"them afterwards if that removal should stand.", m.atName(u), p.Head))
	}
	if len(p.Dropped) > 0 {
		rows = append(rows, "")
		add(fmt.Sprintf("No longer get the key (not members on the server, or not in its chain): %s.", m.names(p.Dropped)))
	}
	rows = append(rows, "")
	add("Accept only if you know the server was restored from a backup. Accepting is recorded in the trust " +
		"audit log; other members' devices that verified the discarded epochs have to accept too.")
	rows = append(rows, "")
	add("To accept: /alarm-recover " + name)
	return rows
}

// serverWide says what clearing the server-wide alarm does.
const serverWide = "server-wide — clears the alarm for all encrypted channels"

// chanConfirmName is what /alarm-clear wants typed: the channel's name,
// "#name" or "@name" for a DM.
func (m *Model) chanConfirmName(ch uint64) string { return m.chanTitle(ch) }

func (m *Model) alarmClear(c command) error {
	ch, err := m.needActive()
	if err != nil {
		return err
	}
	info, ok := m.c.Alarm(ch)
	if !ok {
		return fmt.Errorf("no alarm in %s", m.chanTitle(ch))
	}
	if info.Computed {
		return errors.New("this alarm can't be cleared: it ends when its cause does (/alarm explains)")
	}
	want := m.chanConfirmName(ch)
	scope := ""
	if info.Server {
		scope = " — " + serverWide
	}
	if c.rest == "" {
		return fmt.Errorf("to confirm, type the channel's name: /alarm-clear %s (read /alarm first)%s", want, scope)
	}
	if !confirmMatches(c.rest, want) {
		return fmt.Errorf("that's not this channel's name — type /alarm-clear %s%s", want, scope)
	}
	if err := m.c.ClearAlarm(ch); err != nil {
		return err
	}
	m.reloadChans()
	if info.Server {
		m.setInfo("server-wide alarm cleared for all encrypted channels and recorded — it comes back if the server still presents what raised it")
		return nil
	}
	m.setInfo("alarm cleared and recorded — it comes back if the server still presents what raised it")
	return nil
}

// confirmMatches: typed is want, with or without its #/@ prefix.
func confirmMatches(typed, want string) bool {
	typed = strings.TrimSpace(typed)
	bare := strings.TrimLeft(want, "#@")
	return bare != "" && (typed == want || typed == bare)
}

// ---- members ----

func (m *Model) memberRows(ch uint64) []string {
	v, _ := m.chanView(ch)
	changed := map[uint64]bool{}
	for _, u := range m.keyChg[ch] {
		changed[u] = true
	}
	_, e2e := m.e2e[ch]
	var rows []string
	for _, u := range v.Members {
		n := m.atName(u)
		switch {
		case u == m.me.ID:
			rows = append(rows, "  "+n+" (you)")
		case changed[u]:
			rows = append(rows, "! "+n+" — key changed: /verify "+n+" or /accept-key "+n)
		case e2e && m.unverified[u]:
			rows = append(rows, "? "+n+" — not verified: /verify "+n)
		default:
			rows = append(rows, "  "+n)
		}
	}
	if len(rows) == 0 {
		rows = []string{"  (no members known)"}
	}
	return rows
}

// dmUnverified reports whether ch is a DM whose single other member is
// unverified.
func (m *Model) dmUnverified(v client.ChannelView) bool {
	if v.Kind != proto.ChanDM {
		return false
	}
	us := m.unverifiedIn(v.ID)
	return len(us) == 1 && len(m.dmOthers(v)) == 1
}
