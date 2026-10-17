package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/server"
	"github.com/skailama/tc/internal/transport"
)

// tc devices against the real server: alice lists her two devices from one
// and revokes the other (--yes), which then shows as revoked.
func TestDevicesRealServer(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	srv, err := server.New(server.Config{DataDir: t.TempDir(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := transport.ListenWith("127.0.0.1:0", srv.Cert(), transport.ListenOpts{HTTP: true, HTTPCert: srv.HTTPCert()})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetPublicAddr(ln.Addr().String())
	sctx, scancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { srv.Serve(sctx, ln); close(done) }()
	t.Cleanup(func() { scancel(); <-done; srv.Close() })

	home := loginHome(t, srv, "alice")
	laptopHome := t.TempDir()
	cfg, err := os.ReadFile(filepath.Join(home, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(laptopHome, "config.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	laptop, err := client.Open(laptopHome)
	if err != nil {
		t.Fatal(err)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	lstopped := make(chan struct{})
	go func() { laptop.Run(lctx); close(lstopped) }()
	t.Cleanup(func() { lcancel(); <-lstopped; laptop.Close() })
	var laptopDev e2ecrypto.DevID
	until(t, "laptop registered", func() bool {
		sid, ok := laptop.ServerID()
		if !ok {
			return false
		}
		k, err := laptop.Trust().DeviceKeys(sid, laptop.Me().ID)
		if err != nil {
			return false
		}
		laptopDev = k.ID
		_, ok, _ = laptop.Trust().Device(sid, k.ID)
		return ok
	})

	t.Setenv("TC_HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := connected(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	short := client.ShortDevID(laptopDev)
	var out bytes.Buffer
	until(t, "both devices listed", func() bool {
		out.Reset()
		if err := devices(ctx, c, nil, strings.NewReader(""), &out); err != nil {
			t.Fatal(err)
		}
		return strings.Count(out.String(), "active") == 1 && strings.Contains(out.String(), "this device")
	})
	if !strings.Contains(out.String(), "  "+short+"  ") {
		t.Fatalf("laptop %s not listed:\n%s", short, out.String())
	}
	out.Reset()
	if err := devices(ctx, c, []string{"revoke", short, "--yes"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("revoke: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "✓ revoked device "+short) {
		t.Fatalf("revoke output:\n%s", out.String())
	}
	out.Reset()
	if err := devices(ctx, c, nil, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), short+"  ") || !strings.Contains(out.String(), "revoked 20") {
		t.Fatalf("after revoke:\n%s", out.String())
	}
}

type fakeDevices struct {
	devs      map[uint64][]client.DeviceInfo
	refreshed []uint64
	revoked   []e2ecrypto.DevID
	allowSelf bool
}

func (f *fakeDevices) Me() proto.User { return proto.User{ID: 1, Name: "alice"} }
func (f *fakeDevices) Users() []proto.User {
	return []proto.User{{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}}
}
func (f *fakeDevices) RefreshDevices(_ context.Context, u uint64) error {
	f.refreshed = append(f.refreshed, u)
	return nil
}
func (f *fakeDevices) Devices(u uint64) []client.DeviceInfo { return f.devs[u] }
func (f *fakeDevices) FindDevice(u uint64, p string) (client.DeviceInfo, error) {
	for _, d := range f.devs[u] {
		if strings.HasPrefix(d.Short(), p) {
			return d, nil
		}
	}
	return client.DeviceInfo{}, client.ErrNoDevice
}
func (f *fakeDevices) RevokeDevice(_ context.Context, id e2ecrypto.DevID, allowSelf bool) (int, error) {
	f.revoked = append(f.revoked, id)
	f.allowSelf = allowSelf
	return 2, nil
}

func dev(b byte, this, revoked, verified bool) client.DeviceInfo {
	d := client.DeviceInfo{ID: e2ecrypto.DevID{b, b, b, b}, Created: time.Date(2026, 10, 1, 9, 0, 0, 0, time.Local),
		KeyFP: "SHA256:abcdefghijklmnopqrstuvwxyz", This: this, Revoked: revoked, Verified: verified}
	if revoked {
		d.RevokedAt = time.Date(2026, 10, 2, 10, 0, 0, 0, time.Local)
	}
	return d
}

func newFakeDevices() *fakeDevices {
	return &fakeDevices{devs: map[uint64][]client.DeviceInfo{
		1: {dev(0xaa, true, false, true), dev(0xbb, false, false, true), dev(0xcc, false, true, true), dev(0xdd, false, false, false)},
		2: {dev(0x11, false, false, true)},
	}}
}

func TestDevicesList(t *testing.T) {
	f := newFakeDevices()
	var out bytes.Buffer
	if err := devices(context.Background(), f, nil, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"Devices of @alice (you)", "* aaaaaaaa", "this device", "  bbbbbbbb", "active",
		"revoked 2026-10-02 10:00", "unverified", "SHA256:abcdefghijkl…", "tc devices revoke"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	out.Reset()
	if err := devices(context.Background(), f, []string{"list", "@bob"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "Devices of @bob:") || strings.Contains(s, "revoke") || strings.Contains(s, "*") {
		t.Errorf("bob's list:\n%s", s)
	}
	if len(f.refreshed) != 2 || f.refreshed[0] != 1 || f.refreshed[1] != 2 {
		t.Errorf("refreshed %v", f.refreshed)
	}
	if err := devices(context.Background(), f, []string{"@carol"}, strings.NewReader(""), &out); err == nil {
		t.Error("unknown user accepted")
	}
}

func TestDevicesRevoke(t *testing.T) {
	ctx := context.Background()
	run := func(f *fakeDevices, in string, args ...string) (string, error) {
		var out bytes.Buffer
		err := devices(ctx, f, args, strings.NewReader(in), &out)
		return out.String(), err
	}

	f := newFakeDevices()
	out, err := run(f, "n\n", "revoke", "bbbb")
	if err != nil || len(f.revoked) != 0 || !strings.Contains(out, "Revoke device bbbbbbbb") || !strings.Contains(out, "not revoked") {
		t.Fatalf("declined: %q %v %v", out, err, f.revoked)
	}
	out, err = run(f, "y\n", "revoke", "bbbb")
	if err != nil || len(f.revoked) != 1 || f.revoked[0] != f.devs[1][1].ID || f.allowSelf {
		t.Fatalf("confirmed: %q %v %v", out, err, f.revoked)
	}
	if !strings.Contains(out, "✓ revoked device bbbbbbbb; new keys in 2 encrypted channels") || !strings.Contains(out, "SSH key") {
		t.Fatalf("output: %q", out)
	}

	f = newFakeDevices()
	if out, err = run(f, "", "revoke", "--yes", "bb"); err != nil || len(f.revoked) != 1 || strings.Contains(out, "[y/N]") {
		t.Fatalf("--yes: %q %v", out, err)
	}

	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"revoke", "aa", "--yes"}, "--self"},
		{[]string{"revoke", "cc", "--yes"}, "already revoked"},
		{[]string{"revoke", "dd", "--yes"}, "isn't signed"},
		{[]string{"revoke", "ee", "--yes"}, "no device"},
		{[]string{"revoke", "11", "--yes"}, "no device"}, // bob's: not ours
	} {
		f := newFakeDevices()
		if _, err := run(f, "", c.args...); err == nil || !strings.Contains(err.Error(), c.want) || len(f.revoked) != 0 {
			t.Errorf("%v: %v (revoked %v)", c.args, err, f.revoked)
		}
	}

	f = newFakeDevices()
	out, err = run(f, "yes\n", "revoke", "aa", "--self")
	if err != nil || len(f.revoked) != 1 || !f.allowSelf || !strings.Contains(out, "THIS device") {
		t.Fatalf("--self: %q %v", out, err)
	}
}

func TestDevicesUsage(t *testing.T) {
	for _, args := range [][]string{{"revoke"}, {"revoke", "a", "b"}, {"revoke", "a", "--force"}, {"bob"}, {"@a", "@b"}, {"@"}} {
		if _, err := parseDevices(args); !errors.Is(err, errUsage) {
			t.Errorf("%v: %v", args, err)
		}
	}
	for _, args := range [][]string{nil, {"list"}, {"ls", "@bob"}, {"revoke", "ab12", "-y", "--self"}} {
		if _, err := parseDevices(args); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
}
