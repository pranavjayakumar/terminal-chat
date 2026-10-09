package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

func (f *fakeClient) E2EStatus(ch uint64) client.E2EStatus {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.e2eSt[ch]
}

// e2eFake makes the DM with bob (20) encrypted, holding one message of
// each kind an encrypted channel can show.
func e2eFake(f *fakeClient, st client.E2EStatus) {
	f.chans[4].E2E = true
	f.e2eSt = map[uint64]client.E2EStatus{20: st}
	mk := func(id uint64, body string, sec client.Sec) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 20, Author: 2, Body: body, Time: 1}, Sec: sec}
	}
	f.msgs[20] = []client.Message{
		mk(300, "from before encryption", client.SecLegacy),
		mk(301, "", client.SecWaiting),
		mk(302, "", client.SecInvalid),
		mk(303, "click https://evil.example "+strings.Repeat("now ", 30), client.SecInserted),
		mk(304, "a real secret "+strings.Repeat("word ", 30), client.SecE2E),
	}
}

func setE2E(m *Model, f *fakeClient, ch uint64, st client.E2EStatus) {
	f.mu.Lock()
	f.e2eSt[ch] = st
	f.mu.Unlock()
	run(m, m.applyUpdate(client.Update{Channels: true}))
}

// The 🔒 header, key-state markers and per-message markers fit every
// size, and say what they should.
func TestE2EMarkersFit(t *testing.T) {
	for _, sz := range [][2]int{{20, 8}, {34, 10}, {60, 16}, {80, 24}, {140, 40}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
		run(m, m.applyUpdate(client.Update{Channels: true, Msgs: map[uint64]bool{20: true}}))
		run(m, m.openChannel(20))
		checkView(t, m, "e2e dm")
		for _, st := range []client.E2EStatus{
			{State: client.E2EWaitingKey, Detail: "waiting for the key of epoch 2"},
			{State: client.E2EUnverified, Detail: "epoch 2: can't verify the rotating device"},
			{State: client.E2EAlarm, Detail: "fork: a second bundle for epoch 2 " + strings.Repeat("x", 80)},
		} {
			setE2E(m, f, 20, st)
			checkView(t, m, st.State.String())
		}
	}

	m, f := newTestModelFake(t, 120, 40)
	e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	v := ansi.Strip(checkView(t, m, "e2e dm"))
	for _, want := range []string{"@bob 🔒", "unencrypted (legacy)", "🔒 waiting for key", "⚠ invalid encrypted message",
		"⚠ unencrypted, inserted by server", "a real secret"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	if strings.Contains(v, "alarm") || strings.Contains(v, "unverified") {
		t.Errorf("ok channel shows a warning:\n%s", v)
	}
	setE2E(m, f, 20, client.E2EStatus{State: client.E2EWaitingKey})
	if v := ansi.Strip(m.headerLine(m.w)); !strings.Contains(v, "🔒 waiting for key") {
		t.Errorf("header %q", v)
	}
	setE2E(m, f, 20, client.E2EStatus{State: client.E2EUnverified})
	if v := ansi.Strip(m.headerLine(m.w)); !strings.Contains(v, "🔒 unverified") {
		t.Errorf("header %q", v)
	}
	// A plaintext channel has no lock.
	run(m, m.openChannel(10))
	if v := ansi.Strip(m.headerLine(m.w)); strings.Contains(v, "🔒") {
		t.Errorf("plaintext channel header %q", v)
	}
}

// In alarm the header says so and Enter doesn't send: the text stays in
// the input with a notice why.
func TestE2EAlarmBlocksSend(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	e2eFake(f, client.E2EStatus{State: client.E2EAlarm, Detail: "rollback: the server shows epoch 1, this device verified 3"})
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	if v := ansi.Strip(m.headerLine(m.w)); !strings.Contains(v, "⚠ alarm: rollback") {
		t.Errorf("header %q", v)
	}
	typeVia(m, "psst")
	press(m, "enter")
	if len(f.sent) != 0 {
		t.Fatalf("sent %+v", f.sent)
	}
	v := ansi.Strip(checkView(t, m, "alarm send"))
	if !strings.Contains(v, "encryption is in alarm") || m.input.Value() != "psst" {
		t.Fatalf("no notice or input lost (%q):\n%s", m.input.Value(), v)
	}
	// Once it's no longer in alarm, the same Enter sends.
	setE2E(m, f, 20, client.E2EStatus{State: client.E2EOK})
	press(m, "enter")
	if len(f.sent) != 1 || f.sent[0].Body != "psst" {
		t.Fatalf("sent %+v", f.sent)
	}
}
