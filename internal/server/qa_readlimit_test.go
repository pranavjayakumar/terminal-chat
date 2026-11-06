package server

import (
	"testing"

	"github.com/skailama/tc/internal/proto"
)

// QA v0.6.4: with a busy channel open, the TUI sends a read marker for
// every message that arrives. Those shared the write budget (60, 5/s) with
// edits, reactions, invites and webhooks, so a reader in a busy channel got
// "slow down" on their first reaction or /webhook.
func TestReadMarkersDontStarveWrites(t *testing.T) {
	ts := startServer(t, "tcp")
	alice := ts.register(t, "alice", true)
	snap, _ := alice.syncFresh()
	gen := generalID(t, snap)
	m := alice.send(gen, "busy channel")
	for range 80 {
		alice.req(proto.OpRead, proto.ReadArgs{Chan: gen, Seq: m.Msg})
	}
	alice.ok(proto.OpReact, proto.ReactArgs{Msg: m.Msg, Emoji: "👍"}, nil)
	alice.ok(proto.OpChanTopic, proto.ChanTopicArgs{Chan: gen, Topic: "still works"}, nil)
}
