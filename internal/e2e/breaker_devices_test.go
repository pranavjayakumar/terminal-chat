package e2e

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// revokeSetup: alice (desk) + her laptop + bob, an encrypted DM alice/bob
// that both of alice's devices read.
func revokeSetup(t *testing.T) (s *srv, addr string, alice, laptop, bob *user, dm uint64, ctx context.Context) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr = startServer(t, t.TempDir(), "127.0.0.1:0")
	alice = login(t, s, addr, "alice", true)
	bob = login(t, s, addr, "bob", false)
	laptop = secondDevice(t, alice)
	eventually(t, "online", func() bool { return alice.online() && bob.online() && laptop.online() })
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified", func() bool {
		return bob.verifiedDevices(sid, a) == 2 && alice.verifiedDevices(sid, b) == 1 &&
			laptop.verifiedDevices(sid, b) == 1 && alice.verifiedDevices(sid, a) == 2 && laptop.verifiedDevices(sid, a) == 2
	})
	c, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	ch, err := alice.OpenDM(c, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(ch.ID); return ok })
	bob.send(t, c, ch.ID, "before")
	for _, u := range []*user{alice, laptop} {
		eventually(t, "alice's devices read bob", u.reads(ch.ID, "before"))
	}
	return s, addr, alice, laptop, bob, ch.ID, c
}

// `tc devices revoke <id> --self` from a shell while the TUI runs on the
// same TC_HOME (two processes, one home, as cmd/tc does). The TUI's
// session must notice the retired keys and reconnect as the new device;
// it used to keep the revoked keys and fail every encrypted send.
func TestRevokeSelfFromSiblingProcessReconnects(t *testing.T) {
	_, _, alice, _, bob, dm, ctx := revokeSetup(t)
	a := alice.Me().ID

	cli := reopen(t, alice.home)
	eventually(t, "cli online", cli.online)
	var self client.DeviceInfo
	if err := cli.RefreshDevices(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, d := range cli.Devices(a) {
		if d.This {
			self = d
		}
	}
	if _, err := cli.RevokeDevice(ctx, self.ID, true); err != nil {
		t.Fatal(err)
	}
	cli.cancel()
	cli.Close()

	time.Sleep(time.Second)
	if err := alice.Send(dm, "from the tui after --self", 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if bob.reads(dm, "from the tui after --self")() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("bob never got the TUI's message after a --self revoke from the CLI; tui failed=%q", alice.brFailed(dm))
}

func (u *user) brFailed(ch uint64) string {
	for _, m := range u.Messages(ch, 200) {
		if m.Failed != "" {
			return m.Failed
		}
	}
	return ""
}

// gateProxy forwards TCP to upstream; while held, nothing flows from the
// server to the client (client to server still flows). Models a client
// that hasn't read the newest events yet.
type gateProxy struct {
	ln   net.Listener
	mu   sync.Mutex
	held bool
	cond *sync.Cond
}

func newGateProxy(t *testing.T, upstream string) *gateProxy {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &gateProxy{ln: ln}
	p.cond = sync.NewCond(&p.mu)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				c.Close()
				continue
			}
			go p.pipe(up, c, false)
			go p.pipe(c, up, true)
		}
	}()
	t.Cleanup(func() { ln.Close(); p.hold(false) })
	return p
}

func (p *gateProxy) hold(h bool) {
	p.mu.Lock()
	p.held = h
	p.mu.Unlock()
	p.cond.Broadcast()
}

func (p *gateProxy) pipe(dst, src net.Conn, gated bool) {
	buf := make([]byte, 16<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if gated {
				p.mu.Lock()
				for p.held {
					p.cond.Wait()
				}
				p.mu.Unlock()
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	src.Close()
	dst.Close()
}

func isRateLimited(err error) bool {
	var pe *proto.Error
	return errors.As(err, &pe) && pe.Code == proto.ErrRateLimited
}

// raceSetup: alice (desk) + laptop direct, bob through a gate proxy (and
// carol direct). lost is alice's laptop device.
func raceSetup(t *testing.T, withCarol bool) (alice, laptop, bob, carol *user, gate *gateProxy, lost e2ecrypto.DevID, ctx context.Context) {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
	gate = newGateProxy(t, addr)
	alice = login(t, s, addr, "alice", true)
	bob = login(t, s, gate.ln.Addr().String(), "bob", false)
	us := []*user{alice, bob}
	if withCarol {
		carol = login(t, s, addr, "carol", false)
		us = append(us, carol)
	}
	laptop = secondDevice(t, alice)
	us = append(us, laptop)
	eventually(t, "online", func() bool {
		for _, u := range us {
			if !u.online() {
				return false
			}
		}
		return true
	})
	sid := alice.sid(t)
	eventually(t, "devices verified", func() bool {
		for _, u := range us {
			if u.verifiedDevices(sid, alice.Me().ID) != 2 || u.verifiedDevices(sid, bob.Me().ID) != 1 {
				return false
			}
			if carol != nil && u.verifiedDevices(sid, carol.Me().ID) != 1 {
				return false
			}
		}
		return true
	})
	c, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	if err := alice.RefreshDevices(c, alice.Me().ID); err != nil {
		t.Fatal(err)
	}
	for _, d := range alice.Devices(alice.Me().ID) {
		if !d.This {
			lost = d.ID
		}
	}
	return alice, laptop, bob, carol, gate, lost, c
}

// Bob runs /rotate while alice revokes her laptop, before bob's client has
// read the device.revoke event. The server answers stale_epoch; the retry
// must re-verify devices (commitBundle does) or, holding e2eMu so the
// worker can't apply the revoke, it seals to the revoked laptop again.
func TestRotateRacingRevoke(t *testing.T) {
	alice, _, bob, _, gate, lost, ctx := raceSetup(t, false)
	dm, err := alice.OpenDM(ctx, []uint64{bob.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "DM epoch 1 everywhere", func() bool { return alice.e2eAt(dm.ID, 1)() && bob.e2eAt(dm.ID, 1)() })

	gate.hold(true)
	if _, err := alice.RevokeDevice(ctx, lost, false); err != nil {
		gate.hold(false)
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond) // the per-channel rotation cap after alice's rotation
	errc := make(chan error, 1)
	go func() { errc <- bob.RotateKey(ctx, dm.ID) }()
	time.Sleep(300 * time.Millisecond)
	gate.hold(false)
	err = <-errc
	if isRateLimited(err) {
		t.Skipf("inconclusive: %v", err)
	}
	if err != nil {
		t.Fatalf("bob's rotation racing alice's revoke: %v", err)
	}
}

// Same race through changeMembers: bob removes carol from his encrypted
// channel while alice revokes her laptop.
func TestRemoveMemberRacingRevoke(t *testing.T) {
	alice, _, bob, carol, gate, lost, ctx := raceSetup(t, true)
	ch, err := bob.CreateEncryptedChannel(ctx, "ops", "")
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "epoch 1", bob.e2eAt(ch.ID, 1))
	for _, m := range []*user{alice, carol} {
		if err := bob.AddMember(ctx, ch.ID, m.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range []*user{alice, bob, carol} {
		eventually(t, "epoch 3", u.e2eAt(ch.ID, 3))
	}

	gate.hold(true)
	if _, err := alice.RevokeDevice(ctx, lost, false); err != nil {
		gate.hold(false)
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	errc := make(chan error, 1)
	go func() { errc <- bob.RemoveMember(ctx, ch.ID, carol.Me().ID) }()
	time.Sleep(300 * time.Millisecond)
	gate.hold(false)
	err = <-errc
	if isRateLimited(err) {
		t.Skipf("inconclusive: %v", err)
	}
	if err != nil {
		t.Fatalf("bob removing carol racing alice's revoke: %v", err)
	}
}
