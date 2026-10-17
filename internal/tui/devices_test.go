package tui

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
)

type fakeDevs struct {
	list       map[uint64][]client.DeviceInfo
	refreshed  []uint64
	refreshErr error
	revoked    []e2ecrypto.DevID
	revokeErr  error
	devErr     error
}

func (f *fakeClient) DeviceError() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dv.devErr
}

func (f *fakeClient) Devices(user uint64) []client.DeviceInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]client.DeviceInfo(nil), f.dv.list[user]...)
}

func (f *fakeClient) FindDevice(user uint64, p string) (client.DeviceInfo, error) {
	for _, d := range f.Devices(user) {
		if p != "" && strings.HasPrefix(hex.EncodeToString(d.ID[:]), p) {
			return d, nil
		}
	}
	return client.DeviceInfo{}, client.ErrNoDevice
}

func (f *fakeClient) RefreshDevices(_ context.Context, user uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dv.refreshed = append(f.dv.refreshed, user)
	return f.dv.refreshErr
}

func (f *fakeClient) RevokeDevice(_ context.Context, id e2ecrypto.DevID, allowSelf bool) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if allowSelf {
		panic("the TUI never revokes this device")
	}
	if f.dv.revokeErr != nil {
		return 0, f.dv.revokeErr
	}
	f.dv.revoked = append(f.dv.revoked, id)
	for i, d := range f.dv.list[f.me.ID] {
		if d.ID == id {
			f.dv.list[f.me.ID][i].Revoked = true
			f.dv.list[f.me.ID][i].RevokedAt = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
		}
	}
	return 2, nil
}

func tdev(b byte, this, revoked, verified bool) client.DeviceInfo {
	return client.DeviceInfo{ID: e2ecrypto.DevID{b, b, b, b, 1}, Created: time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		KeyFP: "SHA256:" + strings.Repeat("K", 43), This: this, Revoked: revoked, Verified: verified}
}

func devFake(f *fakeClient) {
	f.dv.list = map[uint64][]client.DeviceInfo{
		1: {tdev(0xaa, true, false, true), tdev(0xbb, false, false, true), tdev(0xcc, false, true, true), tdev(0xdd, false, false, false)},
		2: {tdev(0x22, false, false, true)},
	}
}

func pastDevTypeAhead(m *Model) { m.devs.opened = m.devs.opened.Add(-time.Second) }

// /devices lists my devices; d on another active one asks, y revokes it
// (and the list shows it revoked); Esc or any other key cancels; this
// device, revoked and unverified ones can't be picked.
func TestDevicesOverlay(t *testing.T) {
	for _, sz := range [][2]int{{40, 12}, {80, 24}, {200, 60}, {24, 8}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		devFake(f)
		runInput(t, m, "/devices")
		if m.mode != modeDevices {
			t.Fatalf("%v: mode %v notice %q", sz, m.mode, m.notice)
		}
		v := checkView(t, m, "devices")
		if sz[1] >= 12 {
			for _, want := range []string{"aaaaaaaa", "this device", "bbbbbbbb", "active", "revoked", "unverified"} {
				if !strings.Contains(v, want) {
					t.Errorf("%v: missing %q in\n%s", sz, want, v)
				}
			}
		}
		if len(f.dv.refreshed) != 1 || f.dv.refreshed[0] != 1 {
			t.Fatalf("refreshed %v", f.dv.refreshed)
		}
		press(m, "down", "d") // typed ahead: ignored
		if m.devs.confirm || m.devs.sel != 0 {
			t.Fatalf("%v: type-ahead acted", sz)
		}
		pastDevTypeAhead(m)
		press(m, "d") // this device
		if m.devs.confirm || !m.noticeErr || !strings.Contains(m.notice, "this device") {
			t.Fatalf("%v: d on this device: confirm %v notice %q", sz, m.devs.confirm, m.notice)
		}
		press(m, "down", "d")
		if !m.devs.confirm {
			t.Fatalf("%v: no confirmation", sz)
		}
		v = checkView(t, m, "confirm")
		if !strings.Contains(v, "bbbbbbbb? y") && !strings.Contains(v, "bbbbbbbb for good? y") && sz[0] >= 40 {
			t.Errorf("%v: confirm title missing:\n%s", sz, v)
		}
		press(m, "n")
		if m.devs.confirm || len(f.dv.revoked) != 0 || m.mode != modeDevices {
			t.Fatalf("%v: n revoked %v", sz, f.dv.revoked)
		}
		press(m, "d", "y")
		if len(f.dv.revoked) != 1 || f.dv.revoked[0] != f.dv.list[1][1].ID {
			t.Fatalf("%v: revoked %v", sz, f.dv.revoked)
		}
		// The SSH-key advice always shows; the channel count only if it fits.
		if !strings.HasPrefix(m.notice, "✓ revoked device bbbbbbbb") ||
			!strings.HasSuffix(m.notice, "had your SSH key? an admin must disable your account") ||
			(sz[0] >= 200) != strings.Contains(m.notice, "new keys in 2 encrypted channels") {
			t.Errorf("%v: notice %q", sz, m.notice)
		}
		if m.mode != modeDevices || !m.devs.devs[1].Revoked {
			t.Fatalf("%v: list not refreshed: %+v", sz, m.devs.devs)
		}
		checkView(t, m, "after revoke")
		for _, i := range []int{2, 3} { // revoked, unverified
			m.devs.sel = i
			press(m, "d")
			if m.devs.confirm {
				t.Fatalf("%v: d on device %d asked", sz, i)
			}
		}
		for range 10 {
			press(m, "down")
			checkView(t, m, "scrolled")
		}
		press(m, "enter")
		if m.mode != modeNormal {
			t.Fatalf("%v: Enter left mode %v", sz, m.mode)
		}
	}
}

// A server's error text never reaches the terminal raw; the title says the
// list is cached.
func TestDevicesStaleSanitized(t *testing.T) {
	for _, devs := range []bool{false, true} {
		m, f := newTestModelFake(t, 80, 24)
		if devs {
			devFake(f)
		}
		f.dv.refreshErr = errors.New("bad\x1b]0;pwned\x07\x1b[31mred")
		runInput(t, m, "/devices")
		v := m.View()
		if strings.Contains(v, "\x1b]0;") || strings.Contains(v, "\a") {
			t.Fatalf("escape sequence leaked: %q", v)
		}
		if p := ansi.Strip(v); (!devs && !strings.Contains(p, "can't load the list: bad")) || (devs && !strings.Contains(p, "cached")) {
			t.Fatalf("devs %v: no stale marker:\n%s", devs, p)
		}
		checkView(t, m, "stale")
	}
}

// B11: when the server refused this device (too many devices), /devices
// says so (sanitized), and only in our own list.
func TestDevicesUnregisteredNotice(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	f.dv.devErr = fmt.Errorf("%w: server said no\x1b]0;x\x07", client.ErrDeviceNotRegistered)
	runInput(t, m, "/devices")
	v := checkView(t, m, "unregistered")
	if strings.Contains(v, "\x1b]0;") || !strings.Contains(ansi.Strip(v), "not registered for encryption") || !m.noticeErr {
		t.Fatalf("no notice:\n%s", ansi.Strip(v))
	}
	m.clearNotice()
	press(m, kEsc)
	runInput(t, m, "/devices @bob")
	if m.notice != "" {
		t.Fatalf("notice in bob's list: %q", m.notice)
	}

	// Narrow: the cause stays visible.
	m, f = newTestModelFake(t, 40, 12)
	devFake(f)
	f.dv.devErr = fmt.Errorf("%w: %w", client.ErrDeviceNotRegistered, client.ErrTooManyDevices)
	runInput(t, m, "/devices")
	if v := ansi.Strip(checkView(t, m, "unregistered narrow")); !strings.Contains(v, "too many devices") {
		t.Fatalf("cause cut off:\n%s", v)
	}

	// A failed revoke's error isn't replaced by the refresh after it.
	pastDevTypeAhead(m)
	f.dv.revokeErr = errors.New("forbidden")
	press(m, "down", "d", "y")
	if !strings.Contains(m.notice, "forbidden") {
		t.Fatalf("revoke error replaced: %q", m.notice)
	}
	// A revoke that went through keeps its ✓ (and says we're still waiting).
	f.dv.revokeErr = nil
	m.clearNotice()
	press(m, "d", "y")
	if !strings.Contains(m.notice, "✓ revoked") || !strings.Contains(m.notice, "isn't registered yet") {
		t.Fatalf("after revoke: %q", m.notice)
	}
	f.dv.devErr = nil
	press(m, "r")
	if strings.Contains(m.notice, "not registered") {
		t.Fatalf("registered now, still warned: %q", m.notice)
	}
}

// /device-revoke's question doesn't come back after the user declined it
// while the refresh was still running.
func TestDeviceRevokeDeclinedBeforeRefresh(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	m.input.SetValue("/device-revoke bbbb")
	_, cmd := m.Update(keyMsg("enter")) // hold the refresh
	if !m.devs.confirm {
		t.Fatal("no question")
	}
	pastDevTypeAhead(m)
	press(m, "n", "up")
	run(m, cmd)
	if m.devs.confirm || m.devs.sel != 0 {
		t.Fatalf("question came back: confirm %v sel %d", m.devs.confirm, m.devs.sel)
	}
}

// /devices @bob is read-only; d does nothing.
func TestDevicesOtherUser(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	runInput(t, m, "/devices @bob")
	v := checkView(t, m, "bob's devices")
	if !strings.Contains(v, "devices of @bob") || !strings.Contains(v, "22222222") || strings.Contains(v, "d: revoke") {
		t.Fatalf("bob's list:\n%s", v)
	}
	pastDevTypeAhead(m)
	press(m, "d", "y")
	if len(f.dv.revoked) != 0 {
		t.Fatal("revoked someone else's device")
	}
	for line, want := range map[string]string{
		"/devices @nobody":      "unknown user",
		"/devices @bob @carol":  "usage",
		"/device-revoke":        "usage",
		"/device-revoke ee":     "no such device",
		"/device-revoke aa":     "this device",
		"/device-revoke cc":     "already revoked",
		"/device-revoke dd":     "isn't signed",
		"/device-revoke 22":     "no such device", // bob's
		"/device-revoke aa bb ": "usage",
	} {
		press(m, "esc")
		runInput(t, m, line)
		if !m.noticeErr || !strings.Contains(m.notice, want) || m.mode != modeNormal {
			t.Errorf("%q: notice %q (err %v) mode %v", line, m.notice, m.noticeErr, m.mode)
		}
		m.clearInput()
	}
}

// /device-revoke id opens the list with that device's question up; y
// revokes, and a failure is shown.
func TestDeviceRevokeCommand(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	f.dv.refreshErr = client.ErrOffline
	runInput(t, m, "/device-revoke bbbb")
	if m.mode != modeDevices || !m.devs.confirm || m.devs.sel != 1 {
		t.Fatalf("mode %v confirm %v sel %d notice %q", m.mode, m.devs.confirm, m.devs.sel, m.notice)
	}
	v := checkView(t, m, "device-revoke")
	if !strings.Contains(v, "revoke device bbbbbbbb for good? y") {
		t.Fatalf("no question:\n%s", v)
	}
	press(m, "y") // typed ahead
	if len(f.dv.revoked) != 0 || !m.devs.confirm {
		t.Fatal("type-ahead y revoked")
	}
	pastDevTypeAhead(m)
	f.dv.revokeErr = errors.New("forbidden")
	press(m, "y")
	if !m.noticeErr || !strings.Contains(m.notice, "forbidden") {
		t.Fatalf("failure notice %q", m.notice)
	}
	f.dv.revokeErr = nil
	press(m, "d", "y")
	if len(f.dv.revoked) != 1 {
		t.Fatalf("revoked %v", f.dv.revoked)
	}
	if v := checkView(t, m, "stale list"); !strings.Contains(v, "bbbbbbbb") {
		t.Fatalf("list:\n%s", v)
	}
}

// Update.Revoked shows who revoked what; for this device, a red notice.
func TestDeviceRevokedNotice(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 2, Dev: f.dv.list[2][0].ID}}}))
	if m.noticeErr || !strings.Contains(m.notice, "@bob revoked a device (22222222)") {
		t.Fatalf("notice %q", m.notice)
	}
	checkView(t, m, "notice")
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 1, Dev: f.dv.list[1][1].ID}}}))
	if !strings.Contains(m.notice, "your device bbbbbbbb was revoked") {
		t.Fatalf("notice %q", m.notice)
	}
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 1, Dev: f.dv.list[1][0].ID}}}))
	if !m.noticeErr || !strings.Contains(m.notice, "this device was revoked") {
		t.Fatalf("notice %q", m.notice)
	}
	if v := ansi.Strip(m.View()); strings.Contains(v, "\x1b") {
		t.Fatal("escape leaked")
	}

	// Several at once: this device wins; mine before a new device before others.
	m, f = newTestModelFake(t, 80, 24)
	devFake(f)
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 1, Dev: f.dv.list[1][0].ID}, {User: 2, Dev: f.dv.list[2][0].ID}}}))
	if !strings.Contains(m.notice, "this device was revoked") {
		t.Fatalf("notice %q", m.notice)
	}
	run(m, m.applyUpdate(client.Update{NewOwn: []client.DeviceRef{{User: 1, Dev: f.dv.list[1][3].ID}, {User: 1, Dev: f.dv.list[1][1].ID}}}))
	if !m.noticeErr || !strings.Contains(m.notice, "new device of yours: dddddddd (+1 more)") {
		t.Fatalf("notice %q", m.notice)
	}
	// An error notice isn't replaced by news about someone else.
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 2, Dev: f.dv.list[2][0].ID}}}))
	if !strings.Contains(m.notice, "new device of yours") {
		t.Fatalf("notice %q", m.notice)
	}
	checkView(t, m, "notices")
}

// B2: this device's former id (revoked while it was offline, then
// re-registered) reads as this device, not "your device"; a new device that
// appeared meanwhile leads the notice. The point survives 80 columns.
func TestFormerDeviceRevokedNotice(t *testing.T) {
	old := tdev(0xee, false, true, true)
	old.Former = true
	m, f := newTestModelFake(t, 80, 24)
	devFake(f)
	f.dv.list[1] = append(f.dv.list[1], old)
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 1, Dev: old.ID}},
		NewOwn: []client.DeviceRef{{User: 1, Dev: f.dv.list[1][3].ID}}}))
	if !m.noticeErr || !strings.HasPrefix(m.notice, "new device of yours: dddddddd — not you? /devices to revoke it · this device re-registered (was eeeeeeee)") {
		t.Fatalf("notice %q", m.notice)
	}
	if v := checkView(t, m, "former+new"); !strings.Contains(v, "dddddddd — not you? /devices") {
		t.Fatalf("new device cut off:\n%s", v)
	}

	m, f = newTestModelFake(t, 80, 24)
	devFake(f)
	f.dv.list[1] = append(f.dv.list[1], old)
	run(m, m.applyUpdate(client.Update{Revoked: []client.DeviceRef{{User: 1, Dev: old.ID}, {User: 1, Dev: f.dv.list[1][1].ID}}}))
	if !m.noticeErr || m.notice != "this device re-registered — /devices (was eeeeeeee) (+1 more)" {
		t.Fatalf("notice %q", m.notice)
	}
	if v := checkView(t, m, "former"); !strings.Contains(v, "/devices") {
		t.Fatalf("cut off:\n%s", v)
	}

	// The list says old id, and whether it is still active.
	live := tdev(0xff, false, false, true)
	live.Former = true
	f.dv.list[1] = append(f.dv.list[1], live)
	for _, w := range []int{80, 40} {
		m.w = w
		m.devs.devs = f.dv.list[1]
		m.mode = modeDevices
		rows := strings.Join(m.deviceRows(w), "\n")
		want := []string{"eeeeeeee  old id of this device, revoked", "ffffffff  old id of this device, active"}
		if w < 50 {
			want = []string{"eeeeeeee  old id, revoked", "ffffffff  old id, active"}
		}
		for _, s := range want {
			if !strings.Contains(rows, s) {
				t.Fatalf("%d cols: missing %q in\n%s", w, s, rows)
			}
		}
	}
}
