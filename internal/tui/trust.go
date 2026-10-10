package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Trust UX (docs/E2E.md step 10, docs/TUI.md "Verifying people"):
// /verify (safety numbers, --github), the key-change banner and
// /accept-key, /alarm and /alarm-clear, "?" for members trusted only on
// first use, /members.

// verifyState is the /verify overlay: a safety number to compare, or the
// result of a GitHub check (gh != nil).
type verifyState struct {
	user   uint64
	sn     client.SafetyNumber
	gh     *client.GitHubCheck
	scroll int
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
		n := m.atName(us[0])
		return fmt.Sprintf("⚠ %s's key changed — verify with /verify %s or accept with /accept-key %s", n, n, n)
	}
	names := make([]string, len(us))
	for i, u := range us {
		names[i] = m.atName(u)
	}
	return "⚠ keys changed: " + strings.Join(names, ", ") + " — /verify or /accept-key each"
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
}

// ---- commands ----

func (m *Model) trustCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "verify":
		cmd, err := m.verifyCommand(c)
		return cmd, true, err
	case "accept-key":
		if len(c.args) != 1 {
			return nil, true, errors.New("usage: /accept-key @user")
		}
		u, ok := m.findUser(c.args[0])
		if !ok {
			return nil, true, fmt.Errorf("unknown user %s", sanitize.Line(c.args[0]))
		}
		if err := m.c.AcceptKeyChange(u); err != nil {
			return nil, true, err
		}
		m.reloadChans()
		m.setInfo(fmt.Sprintf("accepted %s's new key — unverified until /verify %s", m.atName(u), m.atName(u)))
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
	m.leaveSelect()
	m.verify = verifyState{user: u, sn: sn}
	m.mode = modeVerify
	return nil, nil
}

func (m *Model) gotVerifyGH(msg verifyGHMsg) {
	if msg.err != nil {
		m.setErr("verify", msg.err)
		return
	}
	m.leaveSelect()
	res := msg.res
	m.verify = verifyState{user: res.User, gh: &res}
	m.mode = modeVerify
	if res.Match {
		m.reloadChans()
	}
}

func (m *Model) verifyKey(key string) {
	v := &m.verify
	switch key {
	case kEsc, "q", kEnter:
		m.mode = modeNormal
		if v.gh == nil && key != kEnter {
			m.setInfo("not verified")
		}
	case "v":
		if v.gh != nil {
			return
		}
		m.mode = modeNormal
		if err := m.c.MarkVerified(v.user, v.sn.Digits); err != nil {
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
	if v.gh != nil {
		g := v.gh
		login := sanitize.Line(g.Login)
		ok := m.st.noticeInfo
		bad := m.st.noticeErr
		if g.Match {
			add(fmt.Sprintf("✓ every key this device pinned for %s is on github.com/%s — %s is now verified", them, login, them), &ok)
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
	} else {
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
	if v.gh != nil {
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
		add("(server-wide: every encrypted channel is in alarm)")
	}
	rows = append(rows, "")
	add(a.Why)
	rows = append(rows, "")
	if a.Computed {
		add("It clears by itself when its cause is gone.")
	} else {
		add("Nothing is sent here while it lasts. Clear it only once you know why it happened (e.g. a server " +
			"restored from a backup): /alarm-clear " + m.chanConfirmName(ch) + ". It comes back if the server " +
			"still presents what raised it. Clearing is recorded in the trust audit log.")
	}
	return rows
}

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
	if c.rest == "" {
		return fmt.Errorf("to confirm, type the channel's name: /alarm-clear %s (read /alarm first)", want)
	}
	if !confirmMatches(c.rest, want) {
		return fmt.Errorf("that's not this channel's name — type /alarm-clear %s", want)
	}
	if err := m.c.ClearAlarm(ch); err != nil {
		return err
	}
	m.reloadChans()
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
