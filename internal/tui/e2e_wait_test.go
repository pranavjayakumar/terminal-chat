package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A message we can't open says why, and names /accept-key when only that
// helps (#94); once accepted it shows its text.
func TestWaitingMessagesSayWhy(t *testing.T) {
	mk := func(id, author uint64, w client.WaitReason) client.Message {
		return client.Message{Message: proto.Message{ID: id, Chan: 20, Author: author, Time: 1}, Sec: client.SecWaiting, Wait: w}
	}
	for _, sz := range [][2]int{{34, 12}, {60, 20}, {120, 40}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		e2eFake(f, client.E2EStatus{State: client.E2EOK, Epoch: 1})
		f.msgs[20] = []client.Message{mk(401, 2, client.WaitKey), mk(402, 2, client.WaitDevice), mk(403, 2, client.WaitKeyChange), mk(404, 1, client.WaitKeyChange)}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		v := ansi.Strip(checkView(t, m, "waiting"))
		// Narrow: the reason may be cut, the command not.
		for _, want := range []string{"🔒 /accept-key @bob", "🔒 /endorse-key"} {
			if !strings.Contains(v, want) {
				t.Errorf("%d cols: missing %q in\n%s", sz[0], want, v)
			}
		}
		if sz[0] < 120 {
			continue
		}
		for _, want := range []string{"🔒 waiting for key", "🔒 waiting for the sender's device",
			"🔒 /accept-key @bob to read: @bob's key changed", "🔒 /endorse-key to read: sent under another SSH key of yours"} {
			if !strings.Contains(v, want) {
				t.Errorf("missing %q in\n%s", want, v)
			}
		}

		// Only the reason changes (the cert arrived under a changed key):
		// the row is redrawn, not served from the render cache.
		f.mu.Lock()
		f.msgs[20][1].Wait = client.WaitKeyChange
		f.mu.Unlock()
		run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{20: true}}))
		v = ansi.Strip(checkView(t, m, "relabelled"))
		if strings.Contains(v, "sender's device") || strings.Count(v, "/accept-key @bob") != 2 {
			t.Errorf("reason change not redrawn:\n%s", v)
		}

		// Accepted: the client opens it, the view follows.
		f.mu.Lock()
		f.msgs[20][2] = client.Message{Message: proto.Message{ID: 403, Chan: 20, Author: 2, Time: 1, Body: "from the new laptop"}, Sec: client.SecE2E}
		f.mu.Unlock()
		run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{20: true}}))
		v = ansi.Strip(checkView(t, m, "accepted"))
		if !strings.Contains(v, "from the new laptop") || strings.Count(v, "@bob's key changed") != 1 {
			t.Errorf("after accept:\n%s", v)
		}
	}
}

// #198: under a rollback or fork alarm a message sealed under the restored
// chain says /alarm-recover opens it; a genuinely invalid one still says
// invalid. Every frame fits, down to 20x6.
func TestWaitingForRestore(t *testing.T) {
	for _, sz := range [][2]int{{20, 6}, {34, 12}, {40, 12}, {60, 16}, {80, 24}, {200, 60}} {
		m, f := newTestModelFake(t, sz[0], sz[1])
		e2eFake(f, client.E2EStatus{State: client.E2EAlarm, Epoch: 4, Detail: "fork: a second bundle for epoch 4"})
		f.msgs[20] = []client.Message{
			{Message: proto.Message{ID: 501, Chan: 20, Author: 2, Time: 1}, Sec: client.SecWaiting, Wait: client.WaitRestore},
			{Message: proto.Message{ID: 502, Chan: 20, Author: 2, Time: 2}, Sec: client.SecInvalid},
		}
		run(m, m.applyUpdate(client.Update{Channels: true}))
		run(m, m.openChannel(20))
		v := ansi.Strip(checkView(t, m, "restore"))
		if sz[0] >= 40 && !strings.Contains(v, "🔒 /alarm-recover") && !strings.Contains(v, "with /alarm-recover") {
			t.Errorf("%dx%d: the command is cut off:\n%s", sz[0], sz[1], v)
		}
		if sz[0] < 80 {
			continue
		}
		for _, want := range []string{"🔒 waiting: accept the restore with /alarm-recover", "⚠ invalid encrypted message (hidden)"} {
			if !strings.Contains(v, want) {
				t.Errorf("%dx%d: missing %q in\n%s", sz[0], sz[1], want, v)
			}
		}
	}
}
