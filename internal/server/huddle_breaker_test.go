package server

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

func sessionOf(t *testing.T, ts *testServer, user uint64) *session {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for s := range ts.sessions {
		if s.user.ID == user {
			return s
		}
	}
	t.Fatal("no session")
	return nil
}

// opHuddleJoin now checks the device outside Server.mu ("a revoke that races
// it ends the participation through huddleEventLocked"). That only holds if
// the revoke lands after the join is in the hub. If the whole device.revoke
// write (commit + huddleEventLocked under Server.mu) runs between the
// unlocked device query and the join taking Server.mu, the KDeviceRevoke
// sweep finds nothing to drop and the join then admits the revoked device,
// which stays in the roster and relayed until it leaves (§14.1: dev must be
// non-revoked).
func TestHuddle_BreakJoinRacesDeviceRevoke(t *testing.T) {
	ts := startServer(t, "quic")
	a := ts.voiceClient(t, "ana")
	ch := a.createE2E("sec").ID
	da, _ := a.registerDevice(ts)
	stream := make([]byte, 16)
	rand.Read(stream)
	as := sessionOf(t, ts, a.user.ID)

	type result struct {
		res any
		err error
	}
	done := make(chan result, 1)
	ts.mu.Lock() // a device.revoke write holds the hub lock
	go func() {
		r, err := ts.opHuddleJoin(context.Background(), as, proto.HuddleJoinArgs{Chan: ch, Dev: da.ID[:], Stream: stream})
		done <- result{r, err}
	}()
	time.Sleep(300 * time.Millisecond) // join has run its device query and waits for Server.mu
	// Exactly what opDeviceRevoke's write does under Server.mu: commit the
	// revocation, then sweep huddles.
	if _, err := ts.st.db.Exec(`UPDATE devices SET revoked = ?, revoked_seq = 1 WHERE dev = ?`, []byte{1}, da.ID[:]); err != nil {
		ts.mu.Unlock()
		t.Fatal(err)
	}
	ts.huddleEventLocked(proto.Event{Kind: proto.KDeviceRevoke})
	ts.mu.Unlock()

	r := <-done
	if r.err == nil {
		ts.hud.mu.RLock()
		p := ts.hud.bySess[as]
		ts.hud.mu.RUnlock()
		t.Fatalf("revoked device admitted to the encrypted huddle (res %+v, participant %v)", r.res, p != nil)
	}
}

// dispatch checks s.disabled under Server.mu, releases it, and opHuddleJoin
// re-takes it without re-checking. A user.disable that commits in that gap
// (its KUserUpdate sweep finds no participation yet) is followed by the
// join admitting the disabled user: their entry is broadcast in the roster
// and datagrams are relayed to and from them, contrary to §14.1 ("in the same
// critical section that updates membership"). opHuddleJoin is called
// directly here, as it runs after dispatch's check passed.
func TestHuddle_BreakJoinAfterDisableCheck(t *testing.T) {
	ts := startServer(t, "quic")
	adm := ts.registerCaps(t, "adm", true, voiceCaps)
	adm.syncFresh()
	b := ts.voiceClient(t, "bo")
	bs := sessionOf(t, ts, b.user.ID)

	if _, err := ts.opUserDisable(context.Background(), adm.user, proto.UserArgs{User: b.user.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := ts.opHuddleJoin(context.Background(), bs, proto.HuddleJoinArgs{Chan: b.gen}); err == nil {
		t.Fatal("disabled user joined a huddle")
	}
}
