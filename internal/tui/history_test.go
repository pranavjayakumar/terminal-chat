package tui

import (
	"testing"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// #74: leaving a channel drops its cached messages, so after a rejoin its
// history must be fetched again, not skipped as "already complete".
func TestRejoinRefetchesHistory(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	f.status.State = client.Online
	m.status = f.status
	run(m, m.openChannel(11))
	m.histDone[11] = true // the earlier fetch reached the start
	// Leave: the channel is no longer ours and its cache is gone.
	f.chans[0].Member = false
	f.msgs[11] = nil
	m.Update(updateMsg(client.Update{Channels: true}))
	run(m, m.openChannel(10)) // /leave moves on
	// Rejoin.
	f.chans[0].Member = true
	m.Update(updateMsg(client.Update{Channels: true}))
	before := f.history
	run(m, m.openChannel(11))
	if f.history == before {
		t.Fatal("rejoined channel's history not fetched")
	}
}

// #93: a private channel I was removed from doesn't try to load history
// (the server would answer forbidden).
func TestRemovedChannelNoHistoryError(t *testing.T) {
	m, f := newTestModelFake(t, 100, 30)
	f.status.State = client.Online
	m.status = f.status
	f.histErr = &proto.Error{Code: proto.ErrForbidden}
	for i := range f.chans {
		if f.chans[i].ID == 13 {
			f.chans[i].Member, f.chans[i].Removed = false, true
		}
	}
	f.msgs[13] = nil
	m.reloadChans()
	run(m, m.openChannel(13))
	if m.noticeErr {
		t.Fatalf("error notice %q", m.notice)
	}
	checkView(t, m, "removed")
}
