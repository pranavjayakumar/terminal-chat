package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

func fpOf(s ssh.Signer) string { return ssh.FingerprintSHA256(s.PublicKey()) }

// The safety number is the same whichever side computes it (party order
// and fingerprint order don't matter), 60 digits, and changes with either
// side's keys, either user id or the server.
func TestSafetyDigitsSymmetric(t *testing.T) {
	sid := testSID()
	a := SafetyParty{User: 1, Fingerprints: []string{"SHA256:aaa", "SHA256:bbb"}}
	b := SafetyParty{User: 2, Fingerprints: []string{"SHA256:ccc"}}
	n := safetyDigits(sid, a, b)
	if !regexp.MustCompile(`^[0-9]{60}$`).MatchString(n) {
		t.Fatalf("not 60 digits: %q", n)
	}
	if got := safetyDigits(sid, b, a); got != n {
		t.Fatalf("asymmetric: %s vs %s", n, got)
	}
	a2 := SafetyParty{User: 1, Fingerprints: []string{"SHA256:bbb", "SHA256:aaa"}}
	if got := safetyDigits(sid, b, a2); got != n {
		t.Fatal("fingerprint order changed the number")
	}
	other := sid
	other[0] ^= 1
	for name, got := range map[string]string{
		"my key changed":    safetyDigits(sid, SafetyParty{User: 1, Fingerprints: []string{"SHA256:aaa"}}, b),
		"their key changed": safetyDigits(sid, a, SafetyParty{User: 2, Fingerprints: []string{"SHA256:ccc", "SHA256:ddd"}}),
		"keys swapped":      safetyDigits(sid, SafetyParty{User: 1, Fingerprints: b.Fingerprints}, SafetyParty{User: 2, Fingerprints: a.Fingerprints}),
		"other user":        safetyDigits(sid, a, SafetyParty{User: 3, Fingerprints: b.Fingerprints}),
		"other server":      safetyDigits(other, a, b),
	} {
		if got == n {
			t.Errorf("%s: same number", name)
		}
	}
	if g := (SafetyNumber{Digits: n}).Groups(); len(g) != 12 || strings.Join(g, "") != n {
		t.Fatalf("groups %v", g)
	}
}

// verifySetup is alice's client sharing an encrypted DM with bob (device
// b1, pinned on first use) and her TC_HOME.
func verifySetup(t *testing.T) (*Client, *e2eFake, peer, string) {
	t.Helper()
	testEnv(t)
	fs := newFakeServer(t)
	fs.caps.Store([]string{proto.CapE2E})
	home := login(t, fs)
	c := open(t, home)
	c.timing.keyNeed = 200 * time.Millisecond
	run(t, c)
	b1 := newPeer(t, bob, nil)
	f := startE2E(t, fs, proto.Snapshot{Chans: []proto.Channel{dmChan(0)}, Devs: []proto.Device{{Cert: b1.cert}}}, proto.DevicesListRes{})
	f.myKeys(c)
	waitFor(t, "bob pinned", func() bool { return slices.Equal(c.UnverifiedUsers(), []uint64{bob}) })
	return c, f, b1, home
}

// Safety number flow: stable across a restart, confirming a stale number
// is refused, confirming the current one verifies bob (pins Verified,
// audited, no longer unverified); a key change on either side changes it.
func TestSafetyNumberVerify(t *testing.T) {
	c, _, b1, home := verifySetup(t)
	sn, err := c.SafetyNumber(bob)
	if err != nil {
		t.Fatal(err)
	}
	if sn.Them.User != bob || !slices.Equal(sn.Them.Fingerprints, []string{fpOf(b1.ssh)}) || sn.Me.User != alice || len(sn.Me.Fingerprints) != 1 {
		t.Fatalf("parties %+v / %+v", sn.Me, sn.Them)
	}
	if _, err := c.SafetyNumber(alice); err == nil {
		t.Fatal("safety number with myself")
	}
	if _, err := c.SafetyNumber(77); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("unknown user: %v", err)
	}

	// Another process on the same TC_HOME (a restart) gets the same number.
	c2 := open(t, home)
	sn2, err := c2.SafetyNumber(bob)
	c2.Close()
	if err != nil || sn2.Digits != sn.Digits {
		t.Fatalf("after restart %q %v, want %q", sn2.Digits, err, sn.Digits)
	}

	wrong := []byte(sn.Digits)
	wrong[0] = '0' + (wrong[0]-'0'+1)%10
	if err := c.MarkVerified(bob, string(wrong)); !errors.Is(err, ErrSafetyChanged) {
		t.Fatalf("wrong digits: %v", err)
	}
	if len(c.UnverifiedUsers()) != 1 {
		t.Fatal("verified by a wrong number")
	}
	if err := c.MarkVerified(bob, sn.Digits); err != nil {
		t.Fatal(err)
	}
	if got := c.UnverifiedUsers(); len(got) != 0 {
		t.Fatalf("still unverified: %v", got)
	}
	sid := testSID()
	pins, _ := c.Trust().PinnedKeys(sid, bob)
	if len(pins) != 1 || pins[0].How != trust.Verified {
		t.Fatalf("pins %+v", pins)
	}
	if a, _ := c.Trust().Audit(sid); len(a) != 1 || a[0].Action != "verify" || a[0].User != bob || a[0].By != alice {
		t.Fatalf("audit %+v", a)
	}

	// Their key set changes: a new number, and the old one no longer confirms.
	b2 := newSSH(t)
	if err := c.Trust().PinKey(sid, bob, fpOf(b2), b2.PublicKey().Marshal(), trust.Endorsed); err != nil {
		t.Fatal(err)
	}
	sn3, _ := c.SafetyNumber(bob)
	if sn3.Digits == sn.Digits {
		t.Fatal("their new key didn't change the number")
	}
	// My key set changes too.
	a2 := newSSH(t)
	if err := c.Trust().PinKey(sid, alice, fpOf(a2), a2.PublicKey().Marshal(), trust.Endorsed); err != nil {
		t.Fatal(err)
	}
	if sn4, _ := c.SafetyNumber(bob); sn4.Digits == sn3.Digits || sn4.Digits == sn.Digits {
		t.Fatal("my new key didn't change the number")
	}
	if err := c.MarkVerified(bob, sn3.Digits); !errors.Is(err, ErrSafetyChanged) {
		t.Fatalf("stale number: %v", err)
	}
}

// An accepted key change makes a verified user unverified again.
func TestAcceptedKeyChangeUnverifies(t *testing.T) {
	c, f, _, _ := verifySetup(t)
	sn, _ := c.SafetyNumber(bob)
	if err := c.MarkVerified(bob, sn.Digits); err != nil {
		t.Fatal(err)
	}
	b3 := newPeer(t, bob, nil)
	f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "key change", func() bool { return len(c.KeyChanges()) == 1 })
	if len(c.UnverifiedUsers()) != 0 {
		t.Fatal("a pending change unverified bob")
	}
	if err := c.AcceptKeyChanges(bob, c.PendingKeys(bob), false); err != nil {
		t.Fatal(err)
	}
	if got := c.UnverifiedUsers(); !slices.Equal(got, []uint64{bob}) {
		t.Fatalf("after accepting a new key: %v", got)
	}
}

func githubServer(t *testing.T, keys map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".keys")
		k, ok := keys[login]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, k)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func authorized(s ssh.Signer) string { return string(ssh.MarshalAuthorizedKey(s.PublicKey())) }

// /verify --github: bob's pinned key on his GitHub account verifies him;
// an account without it is a mismatch that changes nothing; bad logins and
// HTTP errors fail.
func TestVerifyGitHub(t *testing.T) {
	c, _, b1, _ := verifySetup(t)
	other := newSSH(t)
	srv := githubServer(t, map[string]string{
		"bob-gh":  authorized(other) + "garbage line\n\n" + authorized(b1.ssh),
		"someone": authorized(other),
	})
	c.githubBase = srv.URL
	ctx := context.Background()

	res, err := c.VerifyGitHub(ctx, bob, "someone")
	if err != nil {
		t.Fatal(err)
	}
	if res.Match || !slices.Equal(res.Missing, []string{fpOf(b1.ssh)}) || !slices.Equal(res.GitHub, []string{fpOf(other)}) {
		t.Fatalf("mismatch result %+v", res)
	}
	if len(c.UnverifiedUsers()) != 1 {
		t.Fatal("a mismatch verified bob")
	}
	for _, bad := range []string{"../etc", "a b", "", "-x", strings.Repeat("a", 40)} {
		if _, err := c.VerifyGitHub(ctx, bob, bad); err == nil {
			t.Errorf("login %q accepted", bad)
		}
	}
	if _, err := c.VerifyGitHub(ctx, bob, "nobody"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("404: %v", err)
	}
	if _, err := c.VerifyGitHub(ctx, 77, "bob-gh"); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("unknown user: %v", err)
	}

	res, err = c.VerifyGitHub(ctx, bob, "bob-gh")
	if err != nil || !res.Match || len(res.Missing) != 0 || len(res.GitHub) != 2 {
		t.Fatalf("match: %+v %v", res, err)
	}
	// A match alone verifies nothing: the user must confirm the account.
	if len(c.UnverifiedUsers()) != 1 {
		t.Fatal("a GitHub match verified bob without confirmation")
	}
	mismatch, _ := c.VerifyGitHub(ctx, bob, "someone")
	if err := c.ConfirmGitHub(mismatch); !errors.Is(err, ErrNotMatched) {
		t.Fatalf("confirming a mismatch: %v", err)
	}
	if err := c.ConfirmGitHub(res); err != nil {
		t.Fatal(err)
	}
	if got := c.UnverifiedUsers(); len(got) != 0 {
		t.Fatalf("still unverified: %v", got)
	}
	if a, _ := c.Trust().Audit(testSID()); len(a) != 1 || a[0].Detail != "github:bob-gh" {
		t.Fatalf("audit %+v", a)
	}
}

// A changed key of a member blocks sending into the encrypted DM (receiving
// still works) until it's accepted.
func TestKeyChangeBlocksSend(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	if len(d.c.KeyChangedMembers(dm)) != 0 {
		t.Fatal("key change before any")
	}
	b3 := newPeer(t, bob, nil)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "key change", func() bool { return slices.Equal(d.c.KeyChangedMembers(dm), []uint64{bob}) })
	err := d.c.Send(dm, "hi", 0)
	if !errors.Is(err, ErrKeyChange) || !strings.Contains(err.Error(), "@bob") {
		t.Fatalf("send: %v", err)
	}
	if n := outboxLen(t, d.c); n != 0 {
		t.Fatalf("outbox %d", n)
	}
	id := d.f.post(proto.Message{Chan: dm, Author: bob, Enc: sealAs(t, d.b1.dev, d.k1, 1, d.e1, plainMsg{T: "msg", TS: 1, Body: "still readable"})})
	waitFor(t, "receive during key change", func() bool { m, ok := findMsg(d.c, id); return ok && m.Sec == SecE2E && m.Body == "still readable" })

	if err := d.c.AcceptKeyChanges(alice, []string{"x"}, true); err == nil {
		t.Fatal("accepted a change that doesn't exist")
	}
	if err := d.c.AcceptKeyChanges(bob, d.c.PendingKeys(bob), false); err != nil {
		t.Fatal(err)
	}
	if len(d.c.KeyChanges()) != 0 || len(d.c.KeyChangedMembers(dm)) != 0 {
		t.Fatal("change still pending")
	}
	if err := d.c.Send(dm, "hi", 0); err != nil {
		t.Fatalf("send after accept: %v", err)
	}
}

// /alarm-clear: a stored alarm is explained, cleared once (audited) and
// sending works again; a computed one can't be cleared.
func TestClearAlarm(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	if _, ok := d.c.Alarm(dm); ok {
		t.Fatal("alarm before any")
	}
	if err := d.c.ClearAlarm(dm); !errors.Is(err, ErrNoAlarm) {
		t.Fatalf("clear without alarm: %v", err)
	}
	sid := testSID()
	d.c.Trust().RaiseAlarm(sid, dm, "fork: a second bundle for epoch 1 \x1b[31m")
	info, ok := d.c.Alarm(dm)
	if !ok || info.Computed || info.Server || info.Since.IsZero() || !strings.Contains(info.Why, "two different key bundles") ||
		strings.Contains(info.Detail, "\x1b") {
		t.Fatalf("info %+v", info)
	}
	if err := d.c.Send(dm, "x", 0); !errors.Is(err, ErrE2EAlarm) {
		t.Fatalf("send in alarm: %v", err)
	}
	if err := d.c.ClearAlarm(dm); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.c.Alarm(dm); ok {
		t.Fatal("alarm still there")
	}
	a, _ := d.c.Trust().Audit(sid)
	if len(a) != 1 || a[0].Action != "clear_alarm" || a[0].Chan != dm || a[0].By != alice || !strings.HasPrefix(a[0].Detail, "fork") {
		t.Fatalf("audit %+v", a)
	}
	if err := d.c.Send(dm, "x", 0); err != nil {
		t.Fatalf("send after clear: %v", err)
	}

	d.c.Trust().RaiseServerAlarm(sid, "the server stopped offering end-to-end encryption")
	if info, ok := d.c.Alarm(dm); !ok || !info.Server || !strings.Contains(info.Why, "stopped offering") {
		t.Fatalf("server alarm info %+v", info)
	}
	if err := d.c.ClearAlarm(dm); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.c.TrustAlarm(); ok {
		t.Fatal("server alarm still there")
	}
	if a, _ := d.c.Trust().Audit(sid); len(a) != 2 || a[1].Action != "clear_server_alarm" {
		t.Fatalf("audit %+v", a)
	}
}

// Only the deliberate trust-UX path may clear alarms: no other client code
// calls the trust store's Clear* methods.
func TestAlarmClearsOnlyFromVerify(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "verify.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range []string{"ClearChannelAlarm", "ClearTrustAlarm", "ClearAlarm(", "VerifyKeys("} {
				if strings.Contains(line, m) {
					t.Errorf("%s:%d calls %s", f, i+1, m)
				}
			}
		}
	}
}

// The GitHub fetch follows redirects only within the same origin: never to
// another host (or scheme).
func TestVerifyGitHubRedirects(t *testing.T) {
	c, _, b1, _ := verifySetup(t)
	other := githubServer(t, map[string]string{"bob-gh": authorized(b1.ssh)})
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/elsewhere.keys":
			http.Redirect(w, r, other.URL+"/bob-gh.keys", http.StatusFound)
		case "/moved.keys":
			http.Redirect(w, r, base+"/bob-gh.keys", http.StatusMovedPermanently)
		case "/bob-gh.keys":
			fmt.Fprint(w, authorized(b1.ssh))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	base = srv.URL
	c.githubBase = srv.URL
	ctx := context.Background()
	if _, err := c.VerifyGitHub(ctx, bob, "elsewhere"); err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("cross-host redirect: %v", err)
	}
	if res, err := c.VerifyGitHub(ctx, bob, "moved"); err != nil || !res.Match {
		t.Fatalf("same-origin redirect: %+v %v", res, err)
	}
}

// /accept-key acknowledges exactly the keys shown: a set that changed
// meanwhile is refused; a single shown key can be accepted on its own.
func TestAcceptExactlyShown(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	b3, b4 := newPeer(t, bob, nil), newPeer(t, bob, nil)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "first change", func() bool { return len(d.c.PendingKeys(bob)) == 1 })
	shown := d.c.PendingKeys(bob)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b4.cert})
	waitFor(t, "second change", func() bool { return len(d.c.PendingKeys(bob)) == 2 })
	if err := d.c.AcceptKeyChanges(bob, shown, false); !errors.Is(err, ErrPendingChanged) {
		t.Fatalf("stale set: %v", err)
	}
	if err := d.c.AcceptKeyChanges(bob, []string{"SHA256:nope"}, true); !errors.Is(err, ErrPendingChanged) {
		t.Fatalf("unknown key: %v", err)
	}
	if len(d.c.PendingKeys(bob)) != 2 {
		t.Fatal("a refused accept acknowledged something")
	}
	if err := d.c.AcceptKeyChanges(bob, shown, true); err != nil {
		t.Fatal(err)
	}
	if got := d.c.PendingKeys(bob); len(got) != 1 || got[0] != fpOf(b4.ssh) {
		t.Fatalf("pending after accepting one: %v", got)
	}
	if !errors.Is(d.c.Send(dm, "x", 0), ErrKeyChange) {
		t.Fatal("the still-pending key didn't block")
	}
}

// An entry queued before a key change is held (failed with the key-change
// reason) when the outbox gets to it, and sent once the change is accepted.
func TestQueuedHeldForKeyChange(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	d.f.setOnSend(func(f *e2eFake, id uint32, a proto.SendArgs) {
		f.write(proto.TRes, proto.Res{ID: id, Err: proto.Errf(proto.ErrForbidden, "not now")})
	})
	if err := d.c.Send(dm, "queued", 0); err != nil {
		t.Fatal(err)
	}
	var local int64
	waitFor(t, "failed once", func() bool {
		for _, m := range d.c.Messages(dm, 10) {
			if m.Failed != "" {
				local = m.Local
				return true
			}
		}
		return false
	})
	d.f.setOnSend(nil)
	b3 := newPeer(t, bob, nil)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "key change", func() bool { return len(d.c.KeyChangedMembers(dm)) == 1 })
	if err := d.c.RetryFailed(local); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "held for the key change", func() bool {
		for _, m := range d.c.Messages(dm, 10) {
			if m.Local == local && m.Failed == keyChangeReason {
				return true
			}
		}
		return false
	})
	if err := d.c.AcceptKeyChanges(bob, d.c.PendingKeys(bob), false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "sent after accepting", func() bool { return outboxLen(t, d.c) == 0 })
}

// During a pending key change deletes still go out (they seal to nobody
// new); edits and reactions are blocked like sends.
func TestKeyChangeAllowsDeletes(t *testing.T) {
	d := newOpsDM(t)
	serveOps(d)
	mine, _ := d.post(t, alice, d.me, "mine")
	theirs, _ := d.post(t, bob, d.b1.dev, "theirs")
	b3 := newPeer(t, bob, nil)
	d.f.event(proto.KDeviceNew, 0, bob, proto.DeviceRegisterArgs{Cert: b3.cert})
	waitFor(t, "key change", func() bool { return len(d.c.KeyChangedMembers(dm)) == 1 })
	ctx := ctx5(t)
	if err := d.c.React(ctx, theirs, "👍"); !errors.Is(err, ErrKeyChange) {
		t.Fatalf("react: %v", err)
	}
	if err := d.c.Edit(ctx, mine, "changed"); !errors.Is(err, ErrKeyChange) {
		t.Fatalf("edit: %v", err)
	}
	if err := d.c.Delete(ctx, mine); err != nil {
		t.Fatalf("delete: %v", err)
	}
}

// The key-change gate fails closed: an encrypted channel it can't look up
// refuses sends.
func TestKeyChangeGateFailsClosed(t *testing.T) {
	d := newEncDM(t, proto.Snapshot{})
	if _, err := d.c.db.Exec(`DELETE FROM chans WHERE id = ?`, dm); err != nil {
		t.Fatal(err)
	}
	err := d.c.Send(dm, "x", 0)
	if !errors.Is(err, ErrKeyChange) || !strings.Contains(err.Error(), "can't check") {
		t.Fatalf("send: %v", err)
	}
}
