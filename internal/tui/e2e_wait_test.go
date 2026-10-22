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
		if sz[0] < 120 {
			continue // narrow: they must fit, and may be cut
		}
		for _, want := range []string{"🔒 waiting for key", "🔒 waiting for the sender's device",
			"🔒 @bob's key changed: /accept-key @bob", "🔒 from your other key: /accept-key @alice"} {
			if !strings.Contains(v, want) {
				t.Errorf("missing %q in\n%s", want, v)
			}
		}

		// Accepted: the client opens it, the view follows.
		f.mu.Lock()
		f.msgs[20][2] = client.Message{Message: proto.Message{ID: 403, Chan: 20, Author: 2, Time: 1, Body: "from the new laptop"}, Sec: client.SecE2E}
		f.mu.Unlock()
		run(m, m.applyUpdate(client.Update{Msgs: map[uint64]bool{20: true}}))
		v = ansi.Strip(checkView(t, m, "accepted"))
		if !strings.Contains(v, "from the new laptop") || strings.Contains(v, "@bob's key changed") {
			t.Errorf("after accept:\n%s", v)
		}
	}
}
