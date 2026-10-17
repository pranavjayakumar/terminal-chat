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
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Devices (docs/TUI.md "Devices"): /devices [@user] lists a user's devices,
// d on one of your own asks to revoke it, y confirms; /device-revoke id
// opens the list with that device's confirmation up.

type devicesState struct {
	user    uint64
	devs    []client.DeviceInfo
	sel     int
	loading bool
	stale   string // why the list may be out of date ("" if fresh), sanitized
	confirm bool   // the revoke question for devs[sel] is up
	opened  time.Time
	// pendPick/pendAsk: /device-revoke's device, to select (and ask about)
	// once the background refresh lands, unless a key was handled first.
	pendPick e2ecrypto.DevID
	pendAsk  bool
	// busy: a revoke is in flight. It survives closing and reopening the
	// overlay, so the same device can't be revoked twice meanwhile.
	busy bool
}

// devicesMsg delivers a refreshed list (err: shown cached instead) or the
// result of a revoke (revoked != nil).
type devicesMsg struct {
	user    uint64
	devs    []client.DeviceInfo
	err     error
	revoked *revokeResult
}

type revokeResult struct {
	short   string
	rotated int
	err     error
}

func (m *Model) devicesCommand(c command) (tea.Cmd, bool, error) {
	switch c.name {
	case "devices":
		user := m.me.ID
		switch len(c.args) {
		case 0:
		case 1:
			u, ok := m.findUser(c.args[0])
			if !ok {
				return nil, true, fmt.Errorf("unknown user %s", sanitize.Line(c.args[0]))
			}
			user = u
		default:
			return nil, true, errors.New("usage: /devices [@user]")
		}
		return m.openDevices(user, e2ecrypto.DevID{}, false), true, nil
	case "device-revoke":
		if len(c.args) != 1 {
			return nil, true, errors.New("usage: /device-revoke id (the id from /devices)")
		}
		d, err := m.c.FindDevice(m.me.ID, c.args[0])
		if errors.Is(err, client.ErrNoDevice) {
			return nil, true, errors.New("you have no such device — /devices lists them")
		}
		if err != nil {
			return nil, true, err
		}
		if err := revocable(d); err != nil {
			return nil, true, err
		}
		return m.openDevices(m.me.ID, d.ID, true), true, nil
	}
	return nil, false, nil
}

// revocable says why d can't be revoked from here (nil: it can).
func revocable(d client.DeviceInfo) error {
	switch {
	case d.Revoked:
		return fmt.Errorf("device %s is already revoked", d.Short())
	case d.This:
		return fmt.Errorf("device %s is this device — revoke it from another one (or tc devices revoke %s --self)", d.Short(), d.Short())
	case !d.Verified:
		return fmt.Errorf("device %s isn't signed by a key this device trusts for you", d.Short())
	}
	return nil
}

// openDevices shows the cached list at once and refreshes it in the background.
func (m *Model) openDevices(user uint64, pick e2ecrypto.DevID, ask bool) tea.Cmd {
	m.leaveSelect()
	m.devs = devicesState{user: user, devs: m.c.Devices(user), loading: true, opened: m.now(),
		pendPick: pick, pendAsk: ask, busy: m.devs.busy}
	m.devPick(pick, ask)
	m.mode = modeDevices
	return m.refreshDevices(user)
}

func (m *Model) refreshDevices(user uint64) tea.Cmd {
	be := m.c
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		err := be.RefreshDevices(ctx, user)
		return devicesMsg{user: user, devs: be.Devices(user), err: err}
	}
}

// devPick selects device id (if listed) and, with ask, puts up its revoke
// question (only if it can still be revoked).
func (m *Model) devPick(id e2ecrypto.DevID, ask bool) {
	if id == (e2ecrypto.DevID{}) {
		return
	}
	for i, d := range m.devs.devs {
		if d.ID == id {
			m.devs.sel = i
			m.devs.confirm = ask && revocable(d) == nil
			return
		}
	}
}

// setDevs replaces the list, keeping the selected device (and its question,
// while it can still be revoked).
func (m *Model) setDevs(devs []client.DeviceInfo) {
	s := &m.devs
	var cur e2ecrypto.DevID
	if s.sel < len(s.devs) {
		cur = s.devs[s.sel].ID
	}
	confirm := s.confirm
	s.devs, s.sel, s.confirm = devs, 0, false
	m.devPick(cur, confirm)
	s.sel = min(s.sel, max(len(s.devs)-1, 0))
}

// errText is err for showing: the server's code and message, sanitized.
func errText(err error) string {
	msg := err.Error()
	var pe *proto.Error
	switch {
	case errors.Is(err, client.ErrOffline):
		msg = "offline"
	case errors.As(err, &pe):
		msg = pe.Code
		if pe.Msg != "" {
			msg += ": " + pe.Msg
		}
	}
	return sanitize.Line(msg)
}

func (m *Model) gotDevices(msg devicesMsg) tea.Cmd {
	s := &m.devs
	if r := msg.revoked; r != nil {
		s.busy = false
		if r.err != nil {
			m.setErr("revoke", r.err)
		} else {
			t := "✓ revoked device " + r.short
			if r.rotated > 0 {
				t += fmt.Sprintf(" — new keys in %d encrypted %s", r.rotated, plural(r.rotated, "channel", "channels"))
			}
			// The revoke doesn't remove an SSH key; say what does.
			t += " · if it had your SSH key, ask an admin to disable your account"
			m.setInfo(t)
		}
		if m.mode == modeDevices && s.user == msg.user {
			s.loading = true
			return m.refreshDevices(msg.user)
		}
		return nil
	}
	if m.mode != modeDevices || s.user != msg.user {
		return nil
	}
	s.loading, s.stale = false, ""
	if msg.err != nil {
		s.stale = errText(msg.err)
	}
	m.setDevs(msg.devs)
	if s.pendPick != (e2ecrypto.DevID{}) && !s.confirm {
		m.devPick(s.pendPick, s.pendAsk)
	}
	s.pendPick, s.pendAsk = e2ecrypto.DevID{}, false
	return nil
}

func (m *Model) devicesKey(key string) tea.Cmd {
	s := &m.devs
	if m.now().Sub(s.opened) < typeAhead && key != kEsc {
		return nil // a keystroke typed before the list appeared mustn't act on it
	}
	s.pendPick, s.pendAsk = e2ecrypto.DevID{}, false // the user has taken over
	if s.confirm {
		s.confirm = false
		if key != kConfirmYes || s.sel >= len(s.devs) {
			m.setInfo("not revoked")
			return nil
		}
		if s.busy {
			m.setInfo("a revoke is still running")
			return nil
		}
		d := s.devs[s.sel]
		if err := revocable(d); err != nil {
			m.setErr("revoke", err)
			return nil
		}
		s.busy = true
		be, user, short, id := m.c, s.user, d.Short(), d.ID
		m.setInfo("revoking device " + short + "…")
		ctx := m.ctx
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(ctx, 2*reqTimeout)
			defer cancel()
			n, err := be.RevokeDevice(ctx, id, false)
			return devicesMsg{user: user, revoked: &revokeResult{short: short, rotated: n, err: err}}
		}
	}
	last := max(len(s.devs)-1, 0)
	switch key {
	case kEsc, "q", kEnter:
		m.mode = modeNormal
	case kUp, "k", "ctrl+p":
		s.sel = max(s.sel-1, 0)
	case kDown, "j", "ctrl+n":
		s.sel = min(s.sel+1, last)
	case kPgUp:
		s.sel = max(s.sel-10, 0)
	case kPgDown:
		s.sel = min(s.sel+10, last)
	case kHome, "g":
		s.sel = 0
	case kEnd, "G":
		s.sel = last
	case "r":
		if !s.loading {
			s.loading = true
			return m.refreshDevices(s.user)
		}
	case kSelDelete:
		if s.user != m.me.ID || s.sel >= len(s.devs) {
			return nil
		}
		if s.busy {
			m.setInfo("a revoke is still running")
			return nil
		}
		if err := revocable(s.devs[s.sel]); err != nil {
			m.setErr("revoke", err)
			return nil
		}
		s.confirm = true
	}
	return nil
}

func (m *Model) devicesTitle(w int) string {
	s := &m.devs
	narrow := w < 60
	if s.confirm && s.sel < len(s.devs) {
		if narrow {
			return fmt.Sprintf("revoke %s? y: yes · other: no", s.devs[s.sel].Short())
		}
		return fmt.Sprintf("revoke device %s for good? y: revoke · any other key: cancel", s.devs[s.sel].Short())
	}
	var t string
	switch {
	case s.user == m.me.ID && narrow:
		t = "devices · d revoke · Esc"
	case s.user == m.me.ID:
		t = "your devices — d: revoke · r: refresh · Esc closes"
	case narrow:
		t = m.atName(s.user) + " · Esc"
	default:
		t = "devices of " + m.atName(s.user) + " — r: refresh · Esc closes"
	}
	switch {
	case s.loading:
		t += " · refreshing…"
	case s.stale != "":
		t += " · cached (" + s.stale + ")"
	}
	return t
}

func (m *Model) devicesLines(w, h int) []string {
	s := &m.devs
	title := m.st.overlayTitle.Render(truncate(m.devicesTitle(w), w))
	if s.confirm {
		title = m.st.noticeErr.Render(truncate(m.devicesTitle(w), w))
	}
	out := []string{fit(title, w)}
	rows := m.deviceRows(w)
	start, end := window(len(rows), s.sel, max(h-1, 0))
	for i := start; i < end; i++ {
		l := truncate(rows[i], w)
		if i == s.sel && len(s.devs) > 0 {
			out = append(out, fit(m.st.overlaySel.Render(fit(l, w)), w))
		} else {
			out = append(out, fit(l, w))
		}
	}
	return padLines(out, w, h)
}

// deviceRows is one line per device, so sel indexes devs and rows alike.
func (m *Model) deviceRows(w int) []string {
	s := &m.devs
	if len(s.devs) == 0 {
		switch {
		case s.loading:
			return []string{" loading…"}
		case s.stale != "":
			return []string{" can't load the list: " + s.stale}
		}
		return []string{" no devices registered for encryption"}
	}
	rows := make([]string, len(s.devs))
	for i, d := range s.devs {
		var tags []string
		switch {
		case d.This && !d.Revoked:
			tags = append(tags, "this device")
		case d.Former:
			tags = append(tags, "this device's old keys")
		case d.Revoked && !d.RevokedAt.IsZero():
			tags = append(tags, "revoked "+d.RevokedAt.Local().Format("2006-01-02"))
		case d.Revoked:
			tags = append(tags, "revoked")
		default:
			tags = append(tags, "active")
		}
		if !d.Verified {
			tags = append(tags, "unverified")
		}
		mark := " "
		if d.This {
			mark = "*"
		}
		added := "added " + d.Created.Local().Format("2006-01-02")
		if w < 50 {
			added = d.Created.Local().Format("2006-01-02")
		}
		row := fmt.Sprintf("%s %s  %s  %s", mark, d.Short(), strings.Join(tags, ", "), added)
		// A fingerprint is compared by eye: whole or not at all.
		if fp := sanitize.Line(d.KeyFP); fp != "" && strWidth(row)+2+strWidth(fp) <= w {
			row += "  " + fp
		}
		rows[i] = row
	}
	return rows
}

// devicesNotice says which devices were just revoked or newly appeared for
// us (Update.Revoked, Update.NewOwn): this device's revoke first (by its
// current id, or a former one when it was revoked while offline and has
// re-registered already), then ours, then other people's. It doesn't hide
// an error notice unless this device was revoked.
func (m *Model) devicesNotice(revoked, newOwn []client.DeviceRef) {
	if len(revoked) == 0 && len(newOwn) == 0 {
		return
	}
	var this e2ecrypto.DevID
	var former []e2ecrypto.DevID
	for _, d := range m.c.Devices(m.me.ID) {
		switch {
		case d.This:
			this = d.ID
		case d.Former:
			former = append(former, d.ID)
		}
	}
	var mine, others []client.DeviceRef
	thisGone := false
	var formerGone *client.DeviceRef
	for _, r := range revoked {
		switch {
		case r.User == m.me.ID && r.Dev == this:
			thisGone = true
		case r.User == m.me.ID && slices.Contains(former, r.Dev):
			formerGone = &r
		case r.User == m.me.ID:
			mine = append(mine, r)
		default:
			others = append(others, r)
		}
	}
	more := func(n int) string {
		if n > 1 {
			return fmt.Sprintf(" (+%d more)", n-1)
		}
		return ""
	}
	switch {
	case thisGone:
		m.setNotice("this device was revoked — it re-registers with new keys when it reconnects", true, false, 0)
	case formerGone != nil && len(newOwn) > 0:
		m.setNotice(fmt.Sprintf("this device was revoked (as %s) and re-registered; new device of yours: %s%s — not you? /devices",
			formerGone.Short(), newOwn[0].Short(), more(len(newOwn))), true, false, 0)
	case formerGone != nil:
		m.setNotice(fmt.Sprintf("this device was revoked (as %s) and re-registered with new keys — not you? /devices",
			formerGone.Short()), true, false, 0)
	case len(mine) > 0:
		m.setNotice(fmt.Sprintf("your device %s was revoked%s — /devices", mine[0].Short(), more(len(mine))), true, false, 0)
	case len(newOwn) > 0:
		m.setNotice(fmt.Sprintf("new device of yours: %s%s — not you? /devices to revoke it", newOwn[0].Short(), more(len(newOwn))), true, false, 0)
	case m.noticeErr && m.notice != "":
	case len(others) > 0:
		m.setInfo(fmt.Sprintf("%s revoked a device (%s)%s — encrypted channels get a new key before the next message",
			m.atName(others[0].User), others[0].Short(), more(len(others))))
	}
	if m.mode == modeDevices && !m.devs.loading {
		m.setDevs(m.c.Devices(m.devs.user))
	}
}
