//go:build tctamper

package e2e

import (
	"sync/atomic"
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// A plaintext send is stored but its RES is lost; the channel is encrypted
// before alice reconnects. Her outbox then resends the entry encrypted under
// the same nonce and the server answers conflict: the message was delivered
// (it synced, in plaintext), so it must not also show as failed.
func TestPlainSendThenEncrypted_BreakConflict(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	a := alice.Me().ID
	chv, err := bob.CreateChannel(ctx, "secret", true, "")
	if err != nil {
		t.Fatal(err)
	}
	ch := chv.ID
	if err := bob.AddMember(ctx, ch, a); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice is in", func() bool {
		for _, cv := range alice.Channels() {
			if cv.ID == ch {
				return true
			}
		}
		return false
	})

	var dropRes, bye atomic.Bool
	dropRes.Store(true)
	env.tp.setOut(func(user uint64, f frame) []frame {
		if user != a {
			return []frame{f}
		}
		if bye.Load() {
			bye.Store(false)
			return []frame{{Type: proto.TBye, Payload: proto.MustRaw(proto.Bye{Code: proto.ByeServerShutdown})}}
		}
		if f.Type == proto.TRes && dropRes.Load() {
			var r proto.Res
			var sr proto.SendRes
			if proto.Unmarshal(f.Payload, &r) == nil && r.OK && proto.Unmarshal(r.Data, &sr) == nil && sr.Msg != 0 {
				dropRes.Store(false)
				return nil
			}
		}
		return []frame{f}
	})
	const body = "sent in the clear, RES lost"
	if err := alice.Send(ch, body, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has it", func() bool { return bob.has(ch, body) })
	eventually(t, "the RES dropped", func() bool { return !dropRes.Load() })

	if err := bob.EncryptChannel(ctx, ch); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice sees the channel encrypted", alice.e2eAt(ch, 1))
	bye.Store(true)
	bob.send(t, ctx, bob.channel("general"), "anything for alice")

	pending := func() (client.Message, bool) {
		for _, m := range alice.Messages(ch, 50) {
			if m.ID == 0 && m.Local != 0 {
				return m, true
			}
		}
		return client.Message{}, false
	}
	if err := alice.Flush(ctx); err != nil {
		t.Errorf("flush: %v", err)
	}
	if m, ok := pending(); ok {
		t.Fatalf("delivered message also shows as failed: %q", m.Failed)
	}
	n := 0
	for _, m := range alice.Messages(ch, 50) {
		if m.Body == body && m.ID != 0 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("alice shows %d delivered copies", n)
	}
}
