package e2e

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
)

// Restoring a server from a backup (issue #84): the epochs committed after
// the backup are gone, so devices that verified them are in alarm. Only an
// explicit, audited acceptance gets them out, and only where the server's
// chain is what they verified cut short (or that, extended by a member's
// recovery).

// copyDir copies the files of a stopped server's data dir (flat).
func copyDir(t *testing.T, from, to string) {
	t.Helper()
	ents, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// restoreEnv is alice, bob and carol in encrypted #secret at epoch 3, with
// a backup of the server's data dir taken there.
type restoreEnv struct {
	s                 *srv
	dir, backup       string
	addr              string
	sid               e2ecrypto.ServerID
	alice, bob, carol *user
	ch                uint64
}

func restoreTeam(t *testing.T) *restoreEnv {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", "tcp")
	env := &restoreEnv{dir: t.TempDir(), backup: t.TempDir()}
	env.s, env.addr = startServer(t, env.dir, "127.0.0.1:0")
	var us []*user
	for i, n := range []string{"alice", "bob", "carol"} {
		us = append(us, login(t, env.s, env.addr, n, i == 0))
	}
	env.alice, env.bob, env.carol = us[0], us[1], us[2]
	env.waitOnline(t)
	env.sid = env.alice.sid(t)
	eventually(t, "devices verified", func() bool {
		for _, a := range us {
			for _, b := range us {
				if a.verifiedDevices(env.sid, b.Me().ID) != 1 {
					return false
				}
			}
		}
		return true
	})
	ctx := ctxRestore(t)
	ch, err := env.alice.CreateEncryptedChannel(ctx, "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	env.ch = ch.ID
	eventually(t, "epoch 1", env.alice.e2eAt(env.ch, 1))
	for _, u := range []*user{env.bob, env.carol} {
		if err := env.alice.AddMember(ctx, env.ch, u.Me().ID); err != nil {
			t.Fatal(err)
		}
	}
	for _, u := range us {
		eventually(t, "everyone at epoch 3", u.e2eAt(env.ch, 3))
	}
	env.alice.send(t, ctx, env.ch, "before the backup")
	eventually(t, "carol reads", env.carol.reads(env.ch, "before the backup"))

	// The backup, as DEPLOY.md recommends: of a stopped server's data dir.
	env.s.stop()
	copyDir(t, env.dir, env.backup)
	env.s, _ = startServer(t, env.dir, env.addr)
	env.waitOnline(t)
	return env
}

func ctxRestore(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (env *restoreEnv) waitOnline(t *testing.T) {
	t.Helper()
	eventually(t, "online", func() bool {
		return env.alice.online() && env.bob.online() && env.carol.online()
	})
}

// restore stops the server, puts the backup back and starts it again.
func (env *restoreEnv) restore(t *testing.T) {
	t.Helper()
	env.s.stop()
	ents, _ := os.ReadDir(env.dir)
	for _, e := range ents {
		if !e.IsDir() {
			os.Remove(filepath.Join(env.dir, e.Name()))
		}
	}
	copyDir(t, env.backup, env.dir)
	env.s, _ = startServer(t, env.dir, env.addr)
	env.waitOnline(t)
}

// afterBackup is the issue's repro: alice removes bob (epoch 4) and sends,
// then the backup is restored. alice and carol verified epoch 4.
func (env *restoreEnv) afterBackup(t *testing.T) {
	t.Helper()
	ctx := ctxRestore(t)
	if err := env.alice.RemoveMember(ctx, env.ch, env.bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol at epoch 4", env.carol.e2eAt(env.ch, 4))
	env.alice.send(t, ctx, env.ch, "after the backup")
	eventually(t, "carol reads", env.carol.reads(env.ch, "after the backup"))
	env.restore(t)
}

// rotateRetry has u rotate ch, waiting out the server's rotation caps.
func rotateRetry(t *testing.T, ctx context.Context, u *user, ch uint64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := u.RotateKey(ctx, ch)
		var pe *proto.Error
		if err == nil {
			return
		}
		if !errors.As(err, &pe) || pe.Code != proto.ErrRateLimited || time.Now().After(deadline) {
			t.Fatalf("rotate: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func inAlarm(u *user, ch uint64) func() bool {
	return func() bool { return u.status(ch).State == client.E2EAlarm }
}

func (u *user) audited(sid e2ecrypto.ServerID, action string) int {
	a, _ := u.Trust().Audit(sid)
	n := 0
	for _, e := range a {
		if e.Action == action {
			n++
		}
	}
	return n
}

func TestRestoreRecoverRollback(t *testing.T) {
	env := restoreTeam(t)
	alice, bob, carol, ch, sid := env.alice, env.bob, env.carol, env.ch, env.sid
	ctx := ctxRestore(t)
	env.afterBackup(t)

	// The issue: alice is in alarm, can't send, and clearing doesn't help.
	eventually(t, "alice's rollback alarm", func() bool {
		st := alice.status(ch)
		return st.State == client.E2EAlarm && strings.Contains(st.Detail, "rollback")
	})
	if err := alice.Send(ch, "blocked", 0); !errors.Is(err, client.ErrE2EAlarm) {
		t.Fatalf("send in alarm: %v", err)
	}
	if info, ok := alice.Alarm(ch); !ok || !info.Chain {
		t.Fatalf("alarm %+v not marked as a chain alarm", info)
	}
	if err := alice.ClearAlarm(ch); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the alarm comes straight back", inAlarm(alice, ch))
	eventually(t, "carol's alarm", inAlarm(carol, ch))
	if h, _, _ := bob.Trust().ChainHead(sid, ch); h.Epoch != 3 {
		t.Fatalf("bob (removed in epoch 4) verified epoch %d", h.Epoch)
	}

	// Recovery: the preview says what is accepted, and nothing changes yet.
	plan, err := alice.ChainRecoveryPlan(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Fork || plan.Head != 4 || plan.Ancestor != 3 || plan.Top != 3 || plan.Discarded() != 1 ||
		!slices.Equal(plan.Readded, []uint64{bob.Me().ID}) || len(plan.Dropped) != 0 ||
		!slices.Equal(plan.Roster, []uint64{alice.Me().ID, bob.Me().ID, carol.Me().ID}) {
		t.Fatalf("plan %+v", plan)
	}
	if h, _, _ := alice.Trust().ChainHead(sid, ch); h.Epoch != 4 || !inAlarm(alice, ch)() {
		t.Fatal("the preview changed something")
	}
	if err := alice.RecoverChain(ctx, plan); err != nil {
		t.Fatal(err)
	}
	if !alice.e2eAt(ch, 4)() {
		t.Fatalf("alice after recovering: %+v", alice.status(ch))
	}
	if n := alice.audited(sid, "rewind_chain"); n != 1 {
		t.Fatalf("%d rewind_chain audit entries", n)
	}
	// A fresh epoch 4, not the lost one: its key is new.
	if *alice.epochKey(sid, ch, 4) == *carol.epochKey(sid, ch, 4) {
		t.Fatal("the recovered epoch reuses the lost key")
	}
	if err := alice.RecoverChain(ctx, plan); err == nil {
		t.Fatal("a plan was accepted twice")
	}
	alice.send(t, ctx, ch, "after recovering")
	// bob verified no lost epoch: he follows without asking (and is a
	// member again, as the preview said).
	eventually(t, "bob at the new epoch 4", bob.e2eAt(ch, 4))
	eventually(t, "bob reads", bob.reads(ch, "after recovering"))

	// carol verified the lost epoch 4 and now sees a different one: a fork
	// she can accept the same way, since alice (a member of her epoch 4)
	// made it on top of the shared epoch 3.
	eventually(t, "carol still in alarm", inAlarm(carol, ch))
	if err := carol.Send(ch, "blocked", 0); !errors.Is(err, client.ErrE2EAlarm) {
		t.Fatalf("carol's send in alarm: %v", err)
	}
	cplan, err := carol.ChainRecoveryPlan(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	if !cplan.Fork || cplan.Head != 4 || cplan.Ancestor != 3 || cplan.Top != 4 || cplan.Rotator != alice.Me().ID ||
		!slices.Equal(cplan.Readded, []uint64{bob.Me().ID}) {
		t.Fatalf("carol's plan %+v", cplan)
	}
	// The chain moves on before carol confirms (alice removes bob again):
	// the confirmed preview isn't what accepting would do any more.
	if err := alice.RemoveMember(ctx, ch, bob.Me().ID); err != nil {
		t.Fatal(err)
	}
	if err := carol.RecoverChain(ctx, cplan); !errors.Is(err, client.ErrRecoveryChanged) {
		t.Fatalf("stale plan: %v", err)
	}
	if h, _, _ := carol.Trust().ChainHead(sid, ch); h.Epoch != 4 || !inAlarm(carol, ch)() {
		t.Fatal("a stale plan changed carol's chain")
	}
	cplan, err = carol.ChainRecoveryPlan(ctx, ch)
	if err != nil {
		t.Fatal(err)
	}
	// bob held epoch 4's key on this branch, though he's out again at 5:
	// the preview still names him.
	if !cplan.Fork || cplan.Ancestor != 3 || cplan.Top != 5 || !slices.Equal(cplan.Readded, []uint64{bob.Me().ID}) ||
		!slices.Equal(cplan.Roster, []uint64{alice.Me().ID, carol.Me().ID}) {
		t.Fatalf("carol's second plan %+v", cplan)
	}
	if err := carol.RecoverChain(ctx, cplan); err != nil {
		t.Fatal(err)
	}
	eventually(t, "carol at epoch 5", carol.e2eAt(ch, 5))
	if carol.audited(sid, "rewind_chain") != 1 {
		t.Fatal("carol's recovery not audited")
	}
	eventually(t, "carol reads what alice sent while she was in alarm", carol.reads(ch, "after recovering"))
	alice.send(t, ctx, ch, "everyone back")
	eventually(t, "carol reads", carol.reads(ch, "everyone back"))
	carol.send(t, ctx, ch, "carol sends again")
	eventually(t, "alice reads carol", alice.reads(ch, "carol sends again"))
	if bob.epochKey(sid, ch, 5) != nil {
		t.Fatal("bob, removed again, holds epoch 5")
	}
}

// bob, removed in the lost epoch, rotates on the restored server before
// anyone recovers (he and the server could be hiding his removal). Devices
// that saw him removed refuse to follow him: his epoch is a fork they can't
// accept, and nothing they hold changes.
func TestRestoreRefusesForkByRemovedMember(t *testing.T) {
	env := restoreTeam(t)
	alice, bob, carol, ch, sid := env.alice, env.bob, env.carol, env.ch, env.sid
	ctx := ctxRestore(t)
	env.afterBackup(t)
	eventually(t, "carol's alarm", inAlarm(carol, ch))
	eventually(t, "alice's alarm", inAlarm(alice, ch))
	// alice previews the strict prefix; bob rotates before she confirms.
	// What she confirmed is gone: nothing is applied (her device never
	// rests on epoch 3, where bob counts as a member).
	plan, err := alice.ChainRecoveryPlan(ctx, ch)
	if err != nil || plan.Fork {
		t.Fatalf("alice's plan %+v %v", plan, err)
	}
	rotateRetry(t, ctx, bob, ch)
	if err := alice.RecoverChain(ctx, plan); !errors.Is(err, client.ErrNotRecoverable) {
		t.Fatalf("recovering after bob's rotation: %v", err)
	}
	eventually(t, "bob at his epoch 4", bob.e2eAt(ch, 4))
	for _, u := range []*user{alice, carol} {
		before, _, _ := u.Trust().ChainHead(sid, ch)
		_, err := u.ChainRecoveryPlan(ctx, ch)
		if !errors.Is(err, client.ErrNotRecoverable) || !strings.Contains(err.Error(), "made by someone") {
			t.Fatalf("%s's plan: %v", u.Me().Name, err)
		}
		if h, _, _ := u.Trust().ChainHead(sid, ch); h != before || !inAlarm(u, ch)() {
			t.Fatalf("%s's chain changed", u.Me().Name)
		}
		if err := u.Send(ch, "blocked", 0); !errors.Is(err, client.ErrE2EAlarm) {
			t.Fatalf("%s's send: %v", u.Me().Name, err)
		}
		if u.audited(sid, "rewind_chain") != 0 {
			t.Fatal("a refused recovery was audited")
		}
	}
}
