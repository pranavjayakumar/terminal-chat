package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
)

// Devices (docs/TUI.md "Devices"): /devices [@user] lists a user's devices,
// d on one of your own asks to revoke it, y confirms; /device-revoke id
// opens the list with that device's confirmation up.

type devicesState struct {
	user    uint64
	devs    []client.DeviceInfo
	sel     int
	loading bool
	stale   string // why the list may be out of date ("" if fresh)
	confirm bool   // the revoke question for devs[sel] is up
	busy    bool   // a revoke is in flight
	opened  time.Time
}

// devicesMsg delivers a refreshed list (err: shown cached instead) or the
// result of a revoke (revoked != nil).
type devicesMsg struct {
	user    uint64
	devs    []client.DeviceInfo
	err     error
	revoked *revokeResult
	pick    e2ecrypto.DevID // select (and confirm) this device once loaded
	ask     bool
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
				return nil, true, fmt.Errorf("unknown user %s", c.args[0])
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
	m.devs = devicesState{user: user, devs: m.c.Devices(user), loading: true, opened: m.now()}
	m.devPick(pick, ask)
	m.mode = modeDevices
	return m.refreshDevices(user, pick, ask)
}

func (m *Model) refreshDevices(user uint64, pick e2ecrypto.DevID, ask bool) tea.Cmd {
	be := m.c
	ctx := m.ctx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(ctx, reqTimeout)
		defer cancel()
		err := be.RefreshDevices(ctx, user)
		return devicesMsg{user: user, devs: be.Devices(user), err: err, pick: pick, ask: ask}
	}
}

// devPick selects device id (if listed) and, with ask, puts up its revoke
// question.
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

func (m *Model) gotDevices(msg devicesMsg) tea.Cmd {
	if r := msg.revoked; r != nil {
		m.devs.busy = false
		if r.err != nil {
			m.setErr("revoke", r.err)
		} else {
			s := "✓ revoked device " + r.short
			if r.rotated > 0 {
				s += fmt.Sprintf(" — new keys in %d encrypted %s", r.rotated, plural(r.rotated, "channel", "channels"))
			}
			m.setInfo(s)
		}
		if m.mode == modeDevices && m.devs.user == msg.user {
			m.devs.loading = true
			return m.refreshDevices(msg.user, e2ecrypto.DevID{}, false)
		}
		return nil
	}
	if m.mode != modeDevices || m.devs.user != msg.user {
		return nil
	}
	var cur e2ecrypto.DevID
	if m.devs.sel < len(m.devs.devs) {
		cur = m.devs.devs[m.devs.sel].ID
	}
	confirm := m.devs.confirm
	m.devs.devs, m.devs.loading, m.devs.stale = msg.devs, false, ""
	if msg.err != nil {
		m.devs.stale = "cached: " + msg.err.Error()
	}
	m.devs.sel, m.devs.confirm = 0, false
	m.devPick(cur, confirm)
	if !confirm {
		m.devPick(msg.pick, msg.ask)
	}
	m.devs.sel = min(m.devs.sel, max(len(m.devs.devs)-1, 0))
	return nil
}

func (m *Model) devicesKey(key string) tea.Cmd {
	s := &m.devs
	if m.now().Sub(s.opened) < typeAhead && key != kEsc {
		return nil // a keystroke typed before the list appeared mustn't act on it
	}
	if s.confirm {
		s.confirm = false
		if key != kConfirmYes || s.sel >= len(s.devs) || s.busy {
			m.setInfo("not revoked")
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
	switch key {
	case kEsc, "q":
		m.mode = modeNormal
	case kUp, "k", "ctrl+p":
		s.sel = max(s.sel-1, 0)
	case kDown, "j", "ctrl+n":
		s.sel = min(s.sel+1, max(len(s.devs)-1, 0))
	case kHome, "g":
		s.sel = 0
	case kEnd, "G":
		s.sel = max(len(s.devs)-1, 0)
	case "r":
		if !s.loading {
			s.loading = true
			return m.refreshDevices(s.user, e2ecrypto.DevID{}, false)
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

func (m *Model) devicesLines(w, h int) []string {
	s := &m.devs
	mine := s.user == m.me.ID
	var title string
	switch {
	case s.confirm && s.sel < len(s.devs):
		title = fmt.Sprintf("revoke device %s for good? y: revoke · any other key: cancel", s.devs[s.sel].Short())
	case mine:
		title = "your devices — d: revoke · r: refresh · Esc closes"
	default:
		title = "devices of " + m.atName(s.user) + " — r: refresh · Esc closes"
	}
	out := []string{fit(m.st.overlayTitle.Render(truncate(title, w)), w)}
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

// deviceRows is one line per device ("" lines are never selectable: the
// list has none, so sel indexes devs and rows alike).
func (m *Model) deviceRows(w int) []string {
	s := &m.devs
	if len(s.devs) == 0 {
		switch {
		case s.loading:
			return []string{" loading…"}
		case s.stale != "":
			return []string{" " + truncate(s.stale, max(w-1, 1))}
		}
		return []string{" no devices registered for encryption"}
	}
	rows := make([]string, len(s.devs))
	for i, d := range s.devs {
		var tags []string
		switch {
		case d.This && !d.Revoked:
			tags = append(tags, "this device")
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
		row := fmt.Sprintf("%s %s  %s  %s", mark, d.Short(), strings.Join(tags, ", "), "added "+d.Created.Local().Format("2006-01-02"))
		if w >= 70 && d.KeyFP != "" {
			row += "  " + d.KeyFP
		}
		rows[i] = row
	}
	return rows
}

// revokedNotice says which devices were just revoked (Update.Revoked).
func (m *Model) revokedNotice(rs []client.DeviceRevoked) {
	if len(rs) == 0 {
		return
	}
	r := rs[len(rs)-1]
	this := false
	if r.User == m.me.ID {
		for _, d := range m.c.Devices(m.me.ID) {
			if d.ID == r.Dev && d.This {
				this = true
			}
		}
	}
	switch {
	case this:
		m.setNotice("this device was revoked — it re-registers with new keys when it reconnects", true, false, 0)
	case r.User == m.me.ID:
		m.setInfo("your device " + r.Short() + " was revoked — it gets no new encryption keys")
	default:
		m.setInfo(m.atName(r.User) + " revoked a device (" + r.Short() + ") — encrypted channels get a new key before the next message")
	}
	if m.mode == modeDevices && !m.devs.loading && !m.devs.confirm {
		m.devs.devs = m.c.Devices(m.devs.user)
		m.devs.sel = min(m.devs.sel, max(len(m.devs.devs)-1, 0))
	}
}
