package client

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/proto"
)

func rsaSSH(t *testing.T, bits int) ssh.Signer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Only keys tc can log in with are kept; hostile or odd responses fail
// cleanly.
func TestFetchGitHubInviteKeys(t *testing.T) {
	ed, ed2, r2048, r1024 := newSSH(t), newSSH(t), rsaSSH(t, 2048), rsaSSH(t, 1024)
	var many strings.Builder
	for range proto.MaxInviteKeys + 1 {
		many.WriteString(authorized(newSSH(t)))
	}
	huge := strings.Repeat(authorized(ed), maxGitHubKeys/len(authorized(ed))+2)
	srv := githubServer(t, map[string]string{
		"alice": authorized(ed) + "\n# comment\nssh-dss AAAAB3NzaC1kc3MAAAA bad\ngarbage\n" + authorized(r1024) +
			authorized(r2048) + authorized(ed) + "  " + authorized(ed2) + "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA== truncated\n",
		"weak":  authorized(r1024),
		"empty": "",
		"many":  many.String(),
		"huge":  huge,
		"ansi":  "\x1b[2J\x1b]0;pwned\a\n",
	})
	ctx := context.Background()
	keys, err := FetchGitHubInviteKeys(ctx, srv.URL, "alice")
	if err != nil {
		t.Fatal(err)
	}
	want := []ssh.PublicKey{ed.PublicKey(), r2048.PublicKey(), ed2.PublicKey()}
	if len(keys) != len(want) {
		t.Fatalf("got %d keys, want %d", len(keys), len(want))
	}
	for i, k := range keys {
		if string(k) != string(want[i].Marshal()) {
			t.Fatalf("key %d differs", i)
		}
	}
	for login, wantErr := range map[string]string{
		"weak":    "can log in",
		"empty":   "no SSH keys",
		"ansi":    "no SSH keys",
		"many":    "at most",
		"huge":    "KiB",
		"nobody":  "404",
		"../etc":  "not a GitHub user name",
		"a b":     "not a GitHub user name",
		"":        "not a GitHub user name",
		"-x":      "not a GitHub user name",
		"x?y=1":   "not a GitHub user name",
		"alice/x": "not a GitHub user name",
	} {
		if _, err := FetchGitHubInviteKeys(ctx, srv.URL, login); err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("%q: %v (want %q)", login, err, wantErr)
		}
	}

	// Redirects to another origin are refused.
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, authorized(ed)) }))
	defer evil.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/x.keys", http.StatusFound)
	}))
	defer redir.Close()
	if _, err := FetchGitHubInviteKeys(ctx, redir.URL, "alice"); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("cross-origin redirect: %v", err)
	}
}

// Keys are only ever sent to a server that advertises key_invites (an
// older one would mint an unbound invite), and nothing is fetched before
// that check. A server that answers without binding is refused.
func TestCreateGitHubInviteGating(t *testing.T) {
	testEnv(t)
	carol := newSSH(t)
	var hits atomic.Int32
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, authorized(carol))
	}))
	defer gh.Close()

	fs := newFakeServer(t)
	c := open(t, login(t, fs))
	c.SetGitHubBase(gh.URL)
	run(t, c)
	fc := fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.CreateGitHubInvite(ctx, "carol", 0); err == nil || !strings.Contains(err.Error(), "doesn't support") {
		t.Fatalf("no key_invites: %v", err)
	}
	if _, err := c.CreateGitHubInvite(ctx, "bad login", 0); err == nil {
		t.Fatal("bad login accepted")
	}
	if hits.Load() != 0 {
		t.Fatal("fetched keys for a server that can't bind them")
	}
	fc.noReq(proto.OpInviteCreate, 100*time.Millisecond)

	// A server with key_invites.
	fs = newFakeServer(t)
	fs.caps.Store([]string{proto.CapKeyInvites, proto.CapTeamInvites})
	c = open(t, login(t, fs))
	c.SetGitHubBase(gh.URL)
	run(t, c)
	fc = fs.accept()
	fc.bootstrap()
	waitFor(t, "online", online(c))
	code := proto.FormatInvite("h:1", "tok", "pin")

	type result struct {
		res proto.InviteRes
		err error
	}
	call := func() chan result {
		done := make(chan result, 1)
		go func() {
			r, err := c.CreateGitHubInvite(ctx, "carol", time.Hour)
			done <- result{r, err}
		}()
		return done
	}
	done := call()
	var a proto.InviteCreateArgs
	r := fc.expectReq(proto.OpInviteCreate, &a)
	if len(a.Keys) != 1 || string(a.Keys[0]) != string(carol.PublicKey().Marshal()) || a.KeySrc != "github:carol" ||
		a.Uses > 1 || a.TTLMs != time.Hour.Milliseconds() {
		t.Fatalf("invite.create %+v", a)
	}
	fc.ok(r.ID, proto.InviteRes{Code: code, Uses: 1, Keys: 1})
	if res := <-done; res.err != nil || res.res.Keys != 1 || res.res.Code != code {
		t.Fatalf("bound invite: %+v", res)
	}

	done = call()
	r = fc.expectReq(proto.OpInviteCreate, &a)
	fc.ok(r.ID, proto.InviteRes{Code: code, Uses: 1}) // binding dropped
	if res := <-done; res.err == nil || res.res.Code != "" {
		t.Fatalf("unbound answer accepted: %+v", res)
	}
}

// Whatever the .keys parser returns re-encodes to a valid authorized key.
func FuzzGitHubKeys(f *testing.F) {
	f.Add([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl x\n"))
	f.Add([]byte("garbage\n\n\x00\xff"))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, pk := range parseGitHubKeys(body) {
			if _, err := ssh.ParsePublicKey(pk.Marshal()); err != nil {
				t.Fatalf("parsed key doesn't re-parse: %v", err)
			}
		}
		_ = inviteKeys(parseGitHubKeys(body))
	})
}
