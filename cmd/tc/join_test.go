package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

const testInvite = "tc://127.0.0.1:7443/abcdefghijklmnopqrstuvwxyz#pin"

type loginCall struct{ home, invite, name, key string }

type fakeLogin struct {
	calls []loginCall
	errs  []error // returned in order; then success
}

func (f *fakeLogin) login(_ context.Context, home, invite, name, key string) (proto.User, error) {
	f.calls = append(f.calls, loginCall{home, invite, name, key})
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return proto.User{}, err
	}
	return proto.User{ID: 1, Name: name}, nil
}

// noRead fails the test if the wizard reads stdin (non-TTY must not).
type noRead struct{ t *testing.T }

func (r noRead) Read([]byte) (int, error) {
	r.t.Error("wizard read stdin without a terminal")
	return 0, io.EOF
}

func testJoinEnv(t *testing.T, in io.Reader, tty bool) (joinEnv, *fakeLogin, *bytes.Buffer) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	f := &fakeLogin{}
	out := &bytes.Buffer{}
	return joinEnv{
		in: in, out: out, tty: tty, home: filepath.Join(t.TempDir(), "tc"), sshDir: filepath.Join(t.TempDir(), ".ssh"),
		gitName: func() string { return "Ada Lovelace" }, user: "aduser", login: f.login,
	}, f, out
}

func writeKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	blk, _ := ssh.MarshalPrivateKey(priv, "")
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	pk, _ := ssh.NewPublicKey(priv.Public())
	return pk
}

func serveAgent(t *testing.T, keys ...any) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tcag")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	kr := agent.NewKeyring()
	for _, k := range keys {
		kr.Add(agent.AddedKey{PrivateKey: k})
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { agent.ServeAgent(kr, c); c.Close() }()
		}
	}()
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(dir, "s"))
}

func TestJoinUsesAgentKey(t *testing.T) {
	env, f, out := testJoinEnv(t, noRead{t}, false)
	writeKey(t, filepath.Join(env.sshDir, "id_ed25519")) // agent wins over the file
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	serveAgent(t, priv)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	agentPub, _ := ssh.NewPublicKey(priv.Public())
	// The agent key is named by fingerprint, so the config keeps using it.
	if len(f.calls) != 1 || f.calls[0].key != identity.Fingerprint(agentPub) || f.calls[0].name != "ada" || f.calls[0].home != env.home {
		t.Fatalf("calls %+v", f.calls)
	}
	if !strings.Contains(out.String(), "ssh-agent key (SHA256:") {
		t.Fatalf("output:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(env.home, "id_ed25519")); !os.IsNotExist(err) {
		t.Fatal("generated a key despite the agent")
	}
}

func TestJoinUsesExistingKey(t *testing.T) {
	env, f, out := testJoinEnv(t, noRead{t}, false)
	key := filepath.Join(env.sshDir, "id_ed25519")
	writeKey(t, key)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].key != key || !strings.Contains(out.String(), "Using SSH key "+key) {
		t.Fatalf("calls %+v\n%s", f.calls, out)
	}
}

func TestJoinGeneratesKey(t *testing.T) {
	env, f, out := testJoinEnv(t, noRead{t}, false)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(env.home, "id_ed25519")
	if f.calls[0].key != key {
		t.Fatalf("key %q, want %q", f.calls[0].key, key)
	}
	st, err := os.Stat(key)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v", st, err)
	}
	pub, _ := os.ReadFile(key + ".pub")
	if !strings.HasSuffix(string(pub), " tc\n") {
		t.Fatalf("comment: %q", pub)
	}
	if !strings.Contains(out.String(), "made one just for itself: "+key) || !strings.Contains(out.String(), "no passphrase") {
		t.Fatalf("output:\n%s", out)
	}
	// A second join reuses it.
	before, _ := os.ReadFile(key)
	out.Reset()
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(key)
	if !bytes.Equal(before, after) || f.calls[1].key != key || !strings.Contains(out.String(), "Using tc's SSH key") {
		t.Fatalf("not reused:\n%s", out)
	}
}

func TestJoinExplicitKeyAndName(t *testing.T) {
	env, f, _ := testJoinEnv(t, noRead{t}, true)
	key := filepath.Join(t.TempDir(), "mykey")
	writeKey(t, key)
	if err := runJoin(context.Background(), env, testInvite, "zed", key); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].key != key || f.calls[0].name != "zed" {
		t.Fatalf("%+v", f.calls)
	}
	if err := runJoin(context.Background(), env, testInvite, "Not Valid", key); err == nil {
		t.Fatal("invalid --name accepted")
	}
	if err := runJoin(context.Background(), env, testInvite, "zed", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing --key accepted")
	}
	if err := runJoin(context.Background(), env, "not an invite", "zed", key); err == nil {
		t.Fatal("bad invite accepted")
	}
}

func TestDefaultJoinName(t *testing.T) {
	for _, c := range []struct{ git, user, want string }{
		{"Ada Lovelace", "x", "ada"},
		{"  Grace   Hopper ", "x", "grace"},
		{"", "Bob.Smith", "bobsmith"},
		{"José García", "x", "jos"},
		{"李雷", "lilei", "lilei"},
		{"-_Kim", "x", "kim"},
		{"ci-bot", "dana", "dana"},
		{"", "", ""},
		{"!!!", "???", ""},
		{strings.Repeat("a", 40), "x", strings.Repeat("a", 32)},
	} {
		got := defaultJoinName(c.git, c.user)
		if got != c.want {
			t.Errorf("defaultJoinName(%q, %q) = %q, want %q", c.git, c.user, got, c.want)
		}
		if got != "" && !proto.ValidName(got) {
			t.Errorf("%q is not a valid name", got)
		}
	}
}

func TestJoinNonTTYNeedsName(t *testing.T) {
	env, f, _ := testJoinEnv(t, noRead{t}, false)
	env.gitName = func() string { return "" }
	env.user = ""
	err := runJoin(context.Background(), env, testInvite, "", "")
	if err == nil || !strings.Contains(err.Error(), "--name") || len(f.calls) != 0 {
		t.Fatalf("err %v calls %v", err, f.calls)
	}
}

func TestJoinPrompts(t *testing.T) {
	// Enter takes the default.
	env, f, out := testJoinEnv(t, strings.NewReader("\n"), true)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].name != "ada" || !strings.Contains(out.String(), "Your name in chat [ada]: ") {
		t.Fatalf("%+v\n%s", f.calls, out)
	}
	// An override, after one invalid answer.
	env, f, _ = testJoinEnv(t, strings.NewReader("Bad Name\nCarol\n"), true)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	if f.calls[0].name != "carol" {
		t.Fatalf("%+v", f.calls)
	}
	// Name taken: one more prompt, then success.
	env, f, out = testJoinEnv(t, strings.NewReader("\nada2\n"), true)
	f.errs = []error{&client.ByeError{Code: proto.ByeNameTaken}}
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 || f.calls[1].name != "ada2" || strings.Count(out.String(), "Your name in chat") != 2 {
		t.Fatalf("%+v\n%s", f.calls, out)
	}
	// Without a terminal a taken name is an error naming --name.
	env, f, _ = testJoinEnv(t, noRead{t}, false)
	f.errs = []error{&client.ByeError{Code: proto.ByeNameTaken}}
	if err := runJoin(context.Background(), env, testInvite, "", ""); err == nil || !strings.Contains(err.Error(), "--name") {
		t.Fatalf("err %v", err)
	}
	// Other errors pass through.
	env, f, _ = testJoinEnv(t, noRead{t}, false)
	boom := errors.New("boom")
	f.errs = []error{boom}
	if err := runJoin(context.Background(), env, testInvite, "", ""); !errors.Is(err, boom) {
		t.Fatalf("err %v", err)
	}
}

func TestJoinTips(t *testing.T) {
	env, _, out := testJoinEnv(t, noRead{t}, false)
	if err := runJoin(context.Background(), env, testInvite, "", ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"You're in as ada", "tc ", "Ctrl-K", "F1", "/dm @name"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out.String(), "herdr") {
		t.Fatal("herdr hint outside herdr")
	}
	env, _, out = testJoinEnv(t, noRead{t}, false)
	env.herdr = true
	runJoin(context.Background(), env, testInvite, "", "")
	if !strings.Contains(out.String(), "In herdr") {
		t.Fatalf("no herdr hint:\n%s", out)
	}
}

// The wizard against a real server: generated key, team invite, then the
// saved config opens a working client.
func TestJoinRealServer(t *testing.T) {
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.Listen("127.0.0.1:0", srv.Cert())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPublicAddr(ln.Addr().String())
	sctx, scancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(sctx, ln); close(done) }()
	t.Cleanup(func() { scancel(); <-done; srv.Close() })

	inv, err := srv.CreateInviteOpts(context.Background(), server.InviteOpts{Uses: 2, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"Ada Lovelace", "Grace Hopper"} {
		env, _, out := testJoinEnv(t, noRead{t}, false)
		env.gitName = func() string { return who }
		env.login = client.Login
		if err := runJoin(context.Background(), env, inv.Code, "", ""); err != nil {
			t.Fatalf("%s: %v\n%s", who, err, out)
		}
		c, err := client.Open(env.home)
		if err != nil {
			t.Fatal(err)
		}
		if want := strings.ToLower(strings.Fields(who)[0]); c.Config().Name != want || c.Config().KeyPath != filepath.Join(env.home, "id_ed25519") {
			t.Fatalf("config %+v", c.Config())
		}
		c.Close()
	}
	env, _, _ := testJoinEnv(t, noRead{t}, false)
	env.login = client.Login
	env.gitName = func() string { return "Third" }
	var bye *client.ByeError
	if err := runJoin(context.Background(), env, inv.Code, "", ""); !errors.As(err, &bye) || bye.Code != proto.ByeInviteInvalid {
		t.Fatalf("third join of a 2-use invite: %v", err)
	}
}

func TestParseJoinArgs(t *testing.T) {
	for _, args := range [][]string{
		{testInvite, "--name", "ann", "--key", "/k"},
		{"--name", "ann", "--key", "/k", testInvite},
		{"--name=ann", testInvite, "--key=/k"},
	} {
		inv, name, key, err := parseJoinArgs(args, "")
		if err != nil || inv != testInvite || name != "ann" || key != "/k" {
			t.Errorf("%v: %q %q %q %v", args, inv, name, key, err)
		}
	}
	for _, args := range [][]string{{}, {"--name", "ann"}, {testInvite, "extra"}, {"--bogus", testInvite}} {
		if _, _, _, err := parseJoinArgs(args, ""); !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

// With no invite argument, TC_INVITE supplies it (install.sh passes it that
// way, so it isn't in tc's argv for ps to show); an argument wins.
func TestParseJoinArgsEnvInvite(t *testing.T) {
	const other = "tc://h:1/tok#pin"
	for _, c := range []struct {
		args []string
		env  string
		want string
	}{
		{nil, testInvite, testInvite},
		{[]string{"--name", "ann"}, " " + testInvite + "\n", testInvite},
		{[]string{other}, testInvite, other},
	} {
		inv, _, _, err := parseJoinArgs(c.args, c.env)
		if err != nil || inv != c.want {
			t.Errorf("%v env %q: %q %v", c.args, c.env, inv, err)
		}
	}
	if _, _, _, err := parseJoinArgs([]string{"--name", "ann", "extra", "more"}, testInvite); !errors.Is(err, errUsage) {
		t.Errorf("extra args with env invite: %v", err)
	}
	if _, _, _, err := parseJoinArgs(nil, "  "); !errors.Is(err, errUsage) {
		t.Errorf("blank env: %v", err)
	}
}
