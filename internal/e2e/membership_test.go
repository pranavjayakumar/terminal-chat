package e2e

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
)

// team logs in alice (admin) and others, and waits until everyone's
// devices are verified by everyone.
func team(t *testing.T, names ...string) (string, e2ecrypto.ServerID, []*user) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	dir := t.TempDir()
	s, addr := startServer(t, dir, "127.0.0.1:0")
	var us []*user
	for i, n := range names {
		us = append(us, login(t, s, addr, n, i == 0))
	}
	eventually(t, "online", func() bool {
		for _, u := range us {
			if !u.online() {
				return false
			}
		}
		return true
	})
	sid := us[0].sid(t)
	eventually(t, "devices verified", func() bool {
		for _, a := range us {
			for _, b := range us {
				if a.verifiedDevices(sid, b.Me().ID) != 1 {
					return false
				}
			}
		}
		return true
	})
	return dir, sid, us
}

// rawEncs are the encs the server stores for ch, by message id: what a
// removed member could get from a leaked database or a malicious server.
func rawEncs(t *testing.T, dir string, ch uint64) map[uint64][]byte {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "tc.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, enc FROM messages WHERE chan = ? AND enc IS NOT NULL`, ch)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[uint64][]byte{}
	for rows.Next() {
		var id uint64
		var enc []byte
		if err := rows.Scan(&id, &enc); err != nil {
			t.Fatal(err)
		}
		out[id] = enc
	}
	return out
}

// canDecrypt reports whether any epoch key u holds for ch opens enc.
func (u *user) canDecrypt(t *testing.T, sid e2ecrypto.ServerID, ch uint64, enc []byte) bool {
	t.Helper()
	h, err := e2ecrypto.ParseEnc(enc)
	if err != nil {
		t.Fatal(err)
	}
	d, ok, _ := u.Trust().Device(sid, h.Dev)
	if !ok {
		t.Fatal("unknown sending device")
	}
	head, _, _ := u.Trust().ChainHead(sid, ch)
	for e := uint32(1); e <= head.Epoch+8; e++ {
		if k := u.epochKey(sid, ch, e); k != nil {
			if _, err := e2ecrypto.Decrypt(*k, d.Ed25519[:], sid, ch, enc); err == nil {
				return true
			}
		}
	}
	return false
}

func (u *user) send(t *testing.T, ctx context.Context, ch uint64, body string) {
	t.Helper()
	if err := u.Send(ch, body, 0); err != nil {
		t.Fatal(err)
	}
	if err := u.Flush(ctx); err != nil {
		t.Fatal(err)
	}
}

func (u *user) e2eAt(ch uint64, epoch uint32) func() bool {
	return func() bool { st := u.status(ch); return st.State == client.E2EOK && st.Epoch == epoch }
}

func (u *user) reads(ch uint64, body string) func() bool {
	return func() bool { sec, ok := u.sec(ch, body); return ok && sec == client.SecE2E }
}

// Adding members carries the next epoch, whose backlink lets a joiner read
// the whole history; removing carol carries an epoch without her: she
// keeps what she had, and can't open anything sent after, even given the
// server's raw rows.
func TestMembershipAddRemoveHistory(t *testing.T) {
	dir, sid, us := team(t, "alice", "bob", "carol")
	alice, bob, carol := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	alice.send(t, ctx, ch.ID, "before bob")

	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	if st := alice.status(ch.ID); st.Epoch != 2 {
		t.Fatalf("after adding bob: %+v", st)
	}
	eventually(t, "bob at epoch 2", bob.e2eAt(ch.ID, 2))
	if _, err := bob.History(ctx, ch.ID, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob reads the history", bob.reads(ch.ID, "before bob"))
	alice.send(t, ctx, ch.ID, "after bob")
	eventually(t, "bob reads", bob.reads(ch.ID, "after bob"))

	if err := bob.AddMember(ctx, ch.ID, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol at epoch 3", carol.e2eAt(ch.ID, 3))
	if _, err := carol.History(ctx, ch.ID, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol reads two epochs back", func() bool {
		return carol.reads(ch.ID, "before bob")() && carol.reads(ch.ID, "after bob")()
	})

	// bob isn't the creator: he can't remove carol.
	if err := bob.RemoveMember(ctx, ch.ID, carol.Me().ID); err == nil {
		t.Fatal("bob removed carol")
	}
	if err := alice.RemoveMember(ctx, ch.ID, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol sees she was removed", func() bool { v, ok := carol.Channel(ch.ID); return ok && v.Removed && !v.Member })
	eventually(t, "bob at epoch 4", bob.e2eAt(ch.ID, 4))
	alice.send(t, ctx, ch.ID, "after carol")
	eventually(t, "bob reads", bob.reads(ch.ID, "after carol"))
	if !carol.reads(ch.ID, "before bob")() {
		t.Fatal("carol lost the history she had")
	}
	if carol.epochKey(sid, ch.ID, 4) != nil {
		t.Fatal("carol holds the epoch-4 key")
	}
	after := alice.msgID(ch.ID, "after carol")
	encs := rawEncs(t, dir, ch.ID)
	if len(encs) != 3 || encs[after] == nil {
		t.Fatalf("server rows: %d", len(encs))
	}
	for id, enc := range encs {
		if got := carol.canDecrypt(t, sid, ch.ID, enc); got != (id != after) {
			t.Fatalf("carol can decrypt message %d: %v", id, got)
		}
	}
	// Closing the channel forgets it.
	if err := carol.Leave(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := carol.Channel(ch.ID); ok {
		t.Fatal("closed channel still listed")
	}
}

// A leaver can't rotate for the others: the next sender rotates first, so
// nothing after the leave is sealed to a key the leaver holds.
func TestMembershipLeaveRotatesBeforeNextSend(t *testing.T) {
	dir, sid, us := team(t, "alice", "bob")
	alice, bob := us[0], us[1]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob at epoch 2", bob.e2eAt(ch.ID, 2))
	bob.send(t, ctx, ch.ID, "bye")
	if err := bob.Leave(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "alice applied the leave", func() bool {
		v, _ := alice.Channel(ch.ID)
		return !slices.Contains(v.Members, bob.Me().ID)
	})
	alice.send(t, ctx, ch.ID, "after bob left")
	if st := alice.status(ch.ID); st.Epoch != 3 {
		t.Fatalf("alice didn't rotate before sending: %+v", st)
	}
	id := alice.msgID(ch.ID, "after bob left")
	enc := rawEncs(t, dir, ch.ID)[id]
	if h, err := e2ecrypto.ParseEnc(enc); err != nil || h.Epoch != 3 {
		t.Fatalf("sent under epoch %d (%v)", h.Epoch, err)
	}
	if bob.epochKey(sid, ch.ID, 3) != nil || bob.canDecrypt(t, sid, ch.ID, enc) {
		t.Fatal("bob can read what was sent after he left")
	}
}

// chan.encrypt converts a plaintext private channel: the old messages
// become legacy (for a member who saw them, and for one added later, by
// the server's Channel.legacy), new ones are encrypted.
func TestMembershipEncryptChannel(t *testing.T) {
	_, _, us := team(t, "alice", "bob", "carol")
	alice, bob, carol := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateChannel(ctx, "team", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	alice.send(t, ctx, ch.ID, "plain old")
	eventually(t, "bob has it", func() bool { sec, ok := bob.sec(ch.ID, "plain old"); return ok && sec == client.SecPlain })
	if err := bob.EncryptChannel(ctx, ch.ID); err == nil {
		t.Fatal("bob (not the creator) encrypted the channel")
	}
	if err := alice.EncryptChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob at epoch 1", bob.e2eAt(ch.ID, 1))
	if sec, _ := bob.sec(ch.ID, "plain old"); sec != client.SecLegacy {
		t.Fatalf("bob's old message is %s", sec)
	}
	if sec, _ := alice.sec(ch.ID, "plain old"); sec != client.SecLegacy {
		t.Fatalf("alice's old message is %s", sec)
	}
	bob.send(t, ctx, ch.ID, "secret now")
	eventually(t, "alice reads it", alice.reads(ch.ID, "secret now"))

	if err := alice.AddMember(ctx, ch.ID, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol at epoch 2", carol.e2eAt(ch.ID, 2))
	if _, err := carol.History(ctx, ch.ID, 0); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol reads", carol.reads(ch.ID, "secret now"))
	if sec, ok := carol.sec(ch.ID, "plain old"); !ok || sec != client.SecLegacyClaimed {
		// sec() skips messages whose body isn't shown; legacy bodies are.
		t.Fatalf("carol's view of the old message: %s %v", sec, ok)
	}
}

// Disabling a user makes the admin's client rotate the encrypted channels
// they shared.
func TestMembershipDisableRotates(t *testing.T) {
	_, sid, us := team(t, "alice", "bob", "carol")
	alice, bob, carol := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	for _, u := range []*user{bob, carol} {
		if err := alice.AddMember(ctx, ch.ID, u.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "bob at epoch 3", bob.e2eAt(ch.ID, 3))
	if err := alice.DisableUser(ctx, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	if st := alice.status(ch.ID); st.Epoch != 4 {
		t.Fatalf("not rotated: %+v", st)
	}
	ep, _, _ := alice.Trust().Epoch(sid, ch.ID, 4)
	if want := []uint64{alice.Me().ID, bob.Me().ID}; !slices.Equal(ep.Roster, want) {
		t.Fatalf("epoch 4 roster %v, want %v", ep.Roster, want)
	}
	eventually(t, "bob at epoch 4", bob.e2eAt(ch.ID, 4))
}

// Sends racing an add: every message lands once and decrypts, and a
// message sent after the add was applied uses the new epoch.
func TestMembershipAddSendRace(t *testing.T) {
	dir, sid, us := team(t, "alice", "bob", "carol")
	alice, bob, carol := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob at epoch 2", bob.e2eAt(ch.ID, 2))
	bodies := []string{"r0", "r1", "r2", "r3", "r4", "r5", "r6", "r7"}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, b := range bodies {
			if err := bob.Send(ch.ID, b, 0); err != nil {
				t.Error(err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	if err := alice.AddMember(ctx, ch.ID, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := bob.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob at epoch 3", bob.e2eAt(ch.ID, 3))
	bob.send(t, ctx, ch.ID, "after the add")
	for _, b := range append(bodies, "after the add") {
		eventually(t, "alice reads "+b, alice.reads(ch.ID, b))
		if n := alice.count(ch.ID, b); n != 1 {
			t.Fatalf("%s arrived %d times", b, n)
		}
	}
	ep3, _, _ := alice.Trust().Epoch(sid, ch.ID, 3)
	for id, enc := range rawEncs(t, dir, ch.ID) {
		h, _ := e2ecrypto.ParseEnc(enc)
		if id > ep3.Seq && h.Epoch < 3 {
			t.Fatalf("message %d stored after epoch 3 under epoch %d", id, h.Epoch)
		}
	}
	if h, _ := e2ecrypto.ParseEnc(rawEncs(t, dir, ch.ID)[bob.msgID(ch.ID, "after the add")]); h.Epoch != 3 {
		t.Fatalf("sent after the add under epoch %d", h.Epoch)
	}
}

// An admin outside an encrypted channel removes bob without a bundle: bob
// sees his removal, alice's next send rotates first, and bob can't open it
// even from the server's raw rows.
func TestMembershipAdminRemovesWithoutBundle(t *testing.T) {
	dir, sid, us := team(t, "admin", "alice", "bob")
	admin, alice, bob := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateEncryptedChannel(ctx, "sec", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", alice.e2eAt(ch.ID, 1))
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob at epoch 2", bob.e2eAt(ch.ID, 2))
	// The admin isn't a member: the private channel isn't even in their cache.
	if err := admin.RemoveMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob sees his removal", func() bool { v, ok := bob.Channel(ch.ID); return ok && v.Removed })
	eventually(t, "alice applied it", func() bool {
		v, _ := alice.Channel(ch.ID)
		return !slices.Contains(v.Members, bob.Me().ID)
	})
	if st := alice.status(ch.ID); st.Epoch != 2 {
		t.Fatalf("someone rotated already: %+v", st)
	}
	alice.send(t, ctx, ch.ID, "after the admin removed bob")
	if st := alice.status(ch.ID); st.Epoch != 3 {
		t.Fatalf("alice didn't rotate before sending: %+v", st)
	}
	enc := rawEncs(t, dir, ch.ID)[alice.msgID(ch.ID, "after the admin removed bob")]
	if h, err := e2ecrypto.ParseEnc(enc); err != nil || h.Epoch != 3 {
		t.Fatalf("sent under epoch %d (%v)", h.Epoch, err)
	}
	if bob.epochKey(sid, ch.ID, 3) != nil || bob.canDecrypt(t, sid, ch.ID, enc) {
		t.Fatal("bob can read what was sent after his removal")
	}
}

// Converting a channel with a webhook: the hook goes, its retired bot
// stays a member, and client and server agree on the roster with it, so
// the conversion, later adds and sends all work.
func TestMembershipEncryptChannelWithHookBot(t *testing.T) {
	_, _, us := team(t, "alice", "bob", "carol")
	alice, bob, carol := us[0], us[1], us[2]
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ch, err := alice.CreateChannel(ctx, "team", true, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := alice.AddMember(ctx, ch.ID, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.CreateHook(ctx, ch.ID, "ci"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "bot joined", func() bool { v, _ := alice.Channel(ch.ID); return len(v.Members) == 3 })
	if err := alice.EncryptChannel(ctx, ch.ID); err != nil {
		t.Fatal(err)
	}
	if hooks, err := alice.ListHooks(ctx); err != nil || len(hooks) != 0 {
		t.Fatalf("hooks after encrypt: %+v %v", hooks, err)
	}
	eventually(t, "bob at epoch 1", bob.e2eAt(ch.ID, 1))
	alice.send(t, ctx, ch.ID, "encrypted with a bot around")
	eventually(t, "bob reads", bob.reads(ch.ID, "encrypted with a bot around"))
	if err := bob.AddMember(ctx, ch.ID, carol.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol at epoch 2", carol.e2eAt(ch.ID, 2))
	carol.send(t, ctx, ch.ID, "hi from carol")
	eventually(t, "alice reads carol", alice.reads(ch.ID, "hi from carol"))
	if st := alice.status(ch.ID); st.State != client.E2EOK {
		t.Fatalf("alice's status %+v", st)
	}
}
