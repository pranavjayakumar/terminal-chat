package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// helloBye sends HELLO and returns the BYE (zero if WELCOME came instead).
func helloBye(t *testing.T, ts *testServer, key ssh.Signer, invite, name string, caps []string) (proto.Bye, proto.Welcome) {
	t.Helper()
	conn, codec := ts.dial(t)
	defer conn.Close()
	exp, err := conn.Exporter()
	if err != nil {
		t.Fatal(err)
	}
	sig, err := identity.Sign(key, exp)
	if err != nil {
		t.Fatal(err)
	}
	if err := codec.Write(proto.THello, proto.Hello{Client: "test", Caps: caps, Auth: proto.Auth{
		Pubkey: key.PublicKey().Marshal(), Sig: sig, Invite: invite, Name: name}}); err != nil {
		t.Fatal(err)
	}
	f, err := codec.Read()
	if err != nil {
		t.Fatal(err)
	}
	var b proto.Bye
	var w proto.Welcome
	if f.Type == proto.TBye {
		f.Decode(&b)
	} else if f.Type != proto.TWelcome || f.Decode(&w) != nil {
		t.Fatalf("frame %#x", f.Type)
	}
	return b, w
}

// A join retried after the WELCOME was lost (B10): the same key with the
// same, now spent, single-use invite logs in as the user it created, and
// nothing more is spent or created. The same for an expired or revoked
// invite, or one for someone else: a registered key is a login (§4).
func TestRedeemRetrySameKeyLogsIn(t *testing.T) {
	ts := startServer(t, "tcp")
	key := newKey(t)
	inv := teamInvite(t, ts, InviteOpts{Keys: [][]byte{key.PublicKey().Marshal()}, KeySrc: "github:carol"})
	c, bye := ts.connect(t, key, inv.Token, "carol")
	if bye != "" {
		t.Fatal(bye)
	}
	c.conn.Close()
	for _, name := range []string{"carol", "other"} {
		b, w := helloBye(t, ts, key, inv.Token, name, nil)
		if b.Code != "" || w.User.ID != c.user.ID || w.User.Name != "carol" {
			t.Fatalf("retry as %s: bye %+v welcome %+v", name, b, w.User)
		}
	}
	other := teamInvite(t, ts, InviteOpts{Uses: 3})
	if b, w := helloBye(t, ts, key, other.Token, "dave", nil); b.Code != "" || w.User.ID != c.user.ID {
		t.Fatalf("another invite with a known key: %+v", b)
	}
	if b, w := helloBye(t, ts, key, "nonsense", "dave", nil); b.Code != "" || w.User.ID != c.user.ID {
		t.Fatalf("garbage invite with a known key: %+v", b)
	}
	if ts.UserCount() != 1 {
		t.Fatalf("users = %d", ts.UserCount())
	}
	// The 3-use invite still has all three uses.
	for i, n := range []string{"e1", "e2", "e3"} {
		if _, bye := ts.connect(t, newKey(t), other.Token, n); bye != "" {
			t.Fatalf("use %d: %s", i+1, bye)
		}
	}
	// A disabled user's key is revoked, invite or not.
	if err := disableUser(ts, c.user.ID); err != nil {
		t.Fatal(err)
	}
	if b, _ := helloBye(t, ts, key, inv.Token, "carol", nil); b.Code != proto.ByeRevoked {
		t.Fatalf("disabled: %+v", b)
	}
}

// key_hint: a HELLO with the wrong key for a key-bound invite gets the
// accepted fingerprints in the BYE when (and only when) it asked, and
// nothing is spent, so the client can retry with the right key.
func TestKeyBoundInviteHint(t *testing.T) {
	ts := startServer(t, "tcp")
	k1, k2, wrong := newKey(t), newKey(t), newKey(t)
	inv := teamInvite(t, ts, InviteOpts{Keys: [][]byte{k1.PublicKey().Marshal(), k2.PublicKey().Marshal()}, KeySrc: "github:x"})
	b, _ := helloBye(t, ts, wrong, inv.Token, "x", nil)
	if b.Code != proto.ByeInviteInvalid || b.Keys != nil {
		t.Fatalf("no cap: %+v", b)
	}
	b, _ = helloBye(t, ts, wrong, inv.Token, "x", []string{proto.CapKeyHint})
	want := []string{identity.Fingerprint(k1.PublicKey()), identity.Fingerprint(k2.PublicKey())}
	slices.Sort(want)
	if b.Code != proto.ByeInviteInvalid || !slices.Equal(b.Keys, want) {
		t.Fatalf("hint: %+v want %v", b, want)
	}
	// An unbound invite, or a spent/unknown token, gives no hint.
	plain := teamInvite(t, ts, InviteOpts{})
	if _, bye := ts.connect(t, newKey(t), plain.Token, "p"); bye != "" {
		t.Fatal(bye)
	}
	if b, _ := helloBye(t, ts, wrong, plain.Token, "q", []string{proto.CapKeyHint}); b.Code != proto.ByeInviteInvalid || b.Keys != nil {
		t.Fatalf("spent: %+v", b)
	}
	if c, bye := ts.connect(t, k2, inv.Token, "x"); bye != "" || c.user.Name != "x" {
		t.Fatalf("right key after wrong ones: %q", bye)
	}
}

func disableUser(ts *testServer, id uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := ts.st.db.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE id = ?`, id)
	return err
}
