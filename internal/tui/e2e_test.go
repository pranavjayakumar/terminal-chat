package tui

import (
	"strings"
	"testing"
	"time"

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
		mk(299, "claimed old", client.SecLegacyClaimed),
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
	for _, want := range []string{"@bob 🔒", "⚠ unauthenticated (legacy)", "⚠ unauthenticated (legacy, server-claimed)", "🔒 waiting for key", "⚠ invalid encrypted message",
		"⚠ inserted by server", "a real secret"} {
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

// Encrypted edits, deletes and reactions (step 8): "(edited)" for a
// verified edit, "removed (not by author)" for a delete its author didn't
// sign, verified reactions; all fit. Only verified messages can be edited.
func TestE2EEditDeleteReactRender(t *testing.T) {
	mk := func(id, author uint64, body string, sec client.Sec) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 20, Author: author, Body: body, Time: 1}, Sec: sec}
	}
	setup := func(f *fakeClient) {
		e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
		removed, deleted := mk(305, 2, "", client.SecRemoved), mk(306, 2, "", client.SecE2E)
		unverified := mk(309, 2, "", client.SecWaiting)
		removed.Deleted, deleted.Deleted, unverified.Deleted = true, true, true
		edited := mk(307, 1, "fixed "+strings.Repeat("typo ", 20), client.SecE2E)
		edited.Edited, edited.Reactions = 5, map[string][]uint64{"👍": {1, 2}, "🎉": {2}}
		f.msgs[20] = append(f.msgs[20], removed, deleted, edited, mk(308, 1, "", client.SecInvalid), unverified)
	}
	for _, sz := range [][2]int{{20, 8}, {34, 10}, {60, 16}, {80, 24}, {140, 40}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		setup(f)
		run(m, m.applyUpdate(client.Update{Channels: true, Msgs: map[uint64]bool{20: true}}))
		run(m, m.openChannel(20))
		checkView(t, m, "e2e ops")
	}
	m, f := newTestModelFake(t, 140, 60)
	setup(f)
	run(m, m.applyUpdate(client.Update{Channels: true}))
	run(m, m.openChannel(20))
	v := ansi.Strip(checkView(t, m, "e2e ops"))
	for _, want := range []string{"removed (not by author)", "message deleted", "deleted (unverified)", "(edited)", "👍 2", "🎉 1"} {
		if !strings.Contains(v, want) {
			t.Errorf("missing %q in\n%s", want, v)
		}
	}
	sel := func(id uint64) {
		m.enterSelect()
		for i, x := range m.main.msgs {
			if x.ID == id {
				m.main.sel = i
			}
		}
	}
	sel(308)
	run(m, m.handleKey(keyMsg("e")))
	if m.editing != 0 {
		t.Fatal("an invalid message went into edit")
	}
	sel(307)
	run(m, m.handleKey(keyMsg("e")))
	if m.editing != 307 {
		t.Fatal("a verified message can't be edited")
	}
}

// Plaintext in an encrypted channel, legacy or inserted, never notifies;
// a verified message does.
func TestE2EUnauthenticatedNeverNotifies(t *testing.T) {
	m, f := newTestModelFake(t, 80, 24)
	e2eFake(f, client.E2EStatus{State: client.E2EOK})
	run(m, m.applyUpdate(client.Update{Channels: true}))
	nt := notes(m)
	x := func(id uint64, sec client.Sec) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 20, Author: 2, Body: "@alice look"}, Sec: sec}
	}
	run(m, m.applyUpdate(client.Update{New: []client.Message{x(400, client.SecLegacy), x(401, client.SecInserted), x(402, client.SecInvalid)}}))
	if got := nt.got(); len(got) != 0 {
		t.Fatalf("notified: %v", got)
	}
	run(m, m.applyUpdate(client.Update{New: []client.Message{x(403, client.SecE2E)}}))
	if got := nt.got(); len(got) != 1 {
		t.Fatalf("verified message: %v", got)
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

// "message deleted" only for a delete we can attribute.
func TestDeletedText(t *testing.T) {
	for sec, want := range map[client.Sec]string{
		client.SecPlain: "message deleted", client.SecE2E: "message deleted",
		client.SecWaiting: "deleted (unverified)", client.SecRemoved: "removed (not by author)",
		client.SecLegacy: "removed (not by author)", client.SecInserted: "removed (not by author)",
		client.SecInvalid: "removed (not by author)",
	} {
		m := client.Message{Sec: sec}
		m.Deleted = true
		if got := deletedText(m); got != want {
			t.Errorf("%s: %q, want %q", sec, got, want)
		}
	}
}

// A verified message whose signed time is more than client.MaxTimeDrift
// from the server's shows the signed time and a dim "⚠ server time
// differs"; it never groups under the previous message (its time would be
// hidden); one within the skew shows the server's time, unmarked. All fit.
func TestE2ETimeDriftMarker(t *testing.T) {
	server := time.Date(2026, 10, 10, 14, 0, 0, 0, time.Local).UnixMilli()
	signed := time.Date(2026, 10, 10, 9, 30, 0, 0, time.Local).UnixMilli()
	mk := func(id uint64, body string, sts int64) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 20, Author: 2, Body: body, Time: server},
			Sec: client.SecE2E, SignedTime: sts}
	}
	first, drift, near := mk(500, "first", server), mk(501, "rewritten", signed), mk(502, "skewed", server-4*60*1000)
	if !drift.TimeDrift() || near.TimeDrift() {
		t.Fatal("TimeDrift")
	}
	r := testRenderer(false)
	lines := ansi.Strip(strings.Join(r.message(drift, 80, true), "\n"))
	if !strings.Contains(lines, "09:30") || strings.Contains(lines, "14:00") || !strings.Contains(lines, driftMark) {
		t.Fatalf("drifting message:\n%s", lines)
	}
	if l := ansi.Strip(strings.Join(r.message(near, 80, true), "\n")); !strings.Contains(l, "14:00") || strings.Contains(l, driftMark) {
		t.Fatalf("message within the skew:\n%s", l)
	}
	if grouped(first, drift) || grouped(drift, near) {
		t.Fatal("a drifting message was grouped")
	}
	if !grouped(first, near) {
		t.Fatal("ordinary follow-up not grouped")
	}
	for _, sz := range [][2]int{{20, 8}, {34, 10}, {80, 24}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
		f.msgs[20] = []client.Message{first, drift, near}
		run(m, m.applyUpdate(client.Update{Channels: true, Msgs: map[uint64]bool{20: true}}))
		run(m, m.openChannel(20))
		v := ansi.Strip(checkView(t, m, "drift"))
		if sz[0] == 80 && (!strings.Contains(v, driftMark) || !strings.Contains(v, "09:30")) {
			t.Fatalf("view:\n%s", v)
		}
	}
	m, _ := newTestModelFake(t, 80, 24)
	m.openViewer(drift)
	if !strings.Contains(m.view.title, "09:30") || !strings.Contains(m.view.title, driftMark) {
		t.Fatalf("viewer title %q", m.view.title)
	}
}
