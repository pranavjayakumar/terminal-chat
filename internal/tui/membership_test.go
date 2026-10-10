package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
)

func submitLine(m *Model, in string) {
	m.input.SetValue(in)
	run(m, m.submit())
}

// /remove and /encrypt call the backend in private channels and refuse
// (keeping the input) where they make no sense.
func TestMembershipCommands(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	run(m, m.openChannel(13)) // #secret, private
	submitLine(m, "/remove @carol")
	if !f.called("remove 3") || m.input.Value() != "" {
		t.Fatalf("calls %v, input %q", f.calls, m.input.Value())
	}
	submitLine(m, "/encrypt")
	if !f.called("encrypt 13") || !strings.Contains(m.notice, "end-to-end encrypted") {
		t.Fatalf("calls %v, notice %q", f.calls, m.notice)
	}
	checkView(t, m, "encrypt")

	for _, tc := range []struct {
		ch       uint64
		in, want string
	}{
		{10, "/remove @bob", "private channels"},
		{10, "/encrypt", "public"},
		{13, "/remove", "usage"},
		{13, "/remove @ghost", "unknown user"},
	} {
		run(m, m.openChannel(tc.ch))
		submitLine(m, tc.in)
		if m.input.Value() != tc.in || !m.noticeErr || !strings.Contains(m.notice, tc.want) {
			t.Errorf("%q in %d: input %q notice %q", tc.in, tc.ch, m.input.Value(), m.notice)
		}
		m.input.SetValue("")
	}
	f.chans[3].E2E = true
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(13))
	submitLine(m, "/encrypt")
	if !strings.Contains(m.notice, "already") {
		t.Errorf("notice %q", m.notice)
	}
}

// /add in an encrypted channel names the members it can't verify.
func TestAddUnverifiedNotice(t *testing.T) {
	m, f := newTestModelFake(t, 60, 16)
	f.addErr = &client.UnverifiedMembersError{Users: []uint64{4}, Names: []string{"@averyveryverylongusername"}}
	run(m, m.openChannel(13))
	submitLine(m, "/add @averyveryverylongusername")
	if !m.noticeErr || !strings.Contains(m.notice, "no verified device of @averyveryverylongusername") {
		t.Fatalf("notice %q", m.notice)
	}
	checkView(t, m, "add refused")
}

// A channel we were removed from stays open with its history, says so,
// and refuses to send.
func TestRemovedChannelView(t *testing.T) {
	for _, sz := range [][2]int{{20, 8}, {40, 12}, {80, 24}, {140, 40}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		run(m, m.openChannel(13))
		f.mu.Lock()
		f.chans[3].Member, f.chans[3].Removed = false, true
		f.mu.Unlock()
		run(m, m.applyUpdate(client.Update{Channels: true}))
		if m.active != 13 {
			t.Fatalf("%v: switched away from the removed channel", sz)
		}
		v := checkView(t, m, "removed")
		if sz[0] >= 80 && !strings.Contains(v, "you were removed") {
			t.Fatalf("no removal notice:\n%s", v)
		}
		sent := len(f.sent)
		submitLine(m, "hello?")
		if len(f.sent) != sent || !m.noticeErr || !strings.Contains(m.notice, "removed") {
			t.Fatalf("%v: sent into a removed channel (notice %q)", sz, m.notice)
		}
		checkView(t, m, "removed send")
	}
}

// A key rotation in the open channel is announced.
func TestRotationNotice(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	m.clearNotice()
	setE2E(m, f, 20, client.E2EStatus{State: client.E2EOK, Epoch: 2})
	if !strings.Contains(m.notice, "new channel key (epoch 2)") {
		t.Fatalf("notice %q", m.notice)
	}
	if v := ansi.Strip(checkView(t, m, "rotated")); !strings.Contains(v, "epoch 2") {
		t.Fatalf("view:\n%s", v)
	}
}
