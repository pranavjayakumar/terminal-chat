//go:build tctamper

package e2e

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/skailama/tc/internal/proto"
)

// #210: the server stores a message with a file but answers its send with
// not_found, the answer that means it collected the blobs first. The
// client must find the stored copy in its history instead of uploading
// the file again and posting a second, genuinely signed message.
func TestNotFoundForStoredFileSend(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID

	for _, tc := range []struct {
		name string
		ch   uint64
	}{
		{"plaintext", alice.channel("general")},
		{"encrypted", dm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lied atomic.Bool
			env.tp.setOut(func(user uint64, f frame) []frame {
				if user != a || f.Type != proto.TRes || lied.Load() {
					return []frame{f}
				}
				var r proto.Res
				var sr proto.SendRes
				if proto.Unmarshal(f.Payload, &r) != nil || !r.OK || proto.Unmarshal(r.Data, &sr) != nil || sr.Msg == 0 {
					return []frame{f}
				}
				lied.Store(true)
				r.OK, r.Data, r.Err = false, nil, &proto.Error{Code: proto.ErrNotFound, Msg: "file not uploaded, expired or already attached"}
				return []frame{{Type: proto.TRes, Payload: proto.MustRaw(r)}}
			})
			defer env.tp.setOut(nil)

			stored := func() int {
				var n int
				if err := env.db().QueryRow(`SELECT COUNT(*) FROM messages WHERE chan = ? AND author = ?`, tc.ch, a).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			want := stored() + 1

			body := "the file, " + tc.name
			local, err := alice.SendFiles(tc.ch, 0, body, writeFile(t, t.TempDir(), "doc.txt", []byte("important")))
			if err != nil {
				t.Fatal(err)
			}
			if err := alice.WaitUpload(ctx, local); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the lie told", lied.Load)
			eventually(t, "bob has it", func() bool { return bob.has(tc.ch, body) })
			// A re-upload and second send would take well under this.
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if n := stored(); n != want {
					t.Fatalf("the server holds %d messages from alice, want %d", n, want)
				}
			}
			n := 0
			for _, m := range alice.Messages(tc.ch, 50) {
				if m.Local != 0 {
					t.Fatalf("alice still shows a local copy (failed %q)", m.Failed)
				}
				if m.Body == body {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("alice shows %d copies", n)
			}
		})
	}
}

// The same lie, with alice's own msg.new held back until after the RES
// (the order is the server's to choose): the cache has no copy yet, so the
// file goes up again, and the resend must carry the first send's nonce and
// bytes for the server to answer with the stored message.
func TestNotFoundForStoredFileSendEventAfterRes(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID

	for _, tc := range []struct {
		name string
		ch   uint64
	}{
		{"plaintext", alice.channel("general")},
		{"encrypted", dm},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var lied atomic.Bool
			var liedAt time.Time
			var held []frame // guarded by the tamperer's mu
			env.tp.setOut(func(user uint64, f frame) []frame {
				if user != a {
					return []frame{f}
				}
				if ev, ok := asEvent(f); ok && !lied.Load() && ev.Kind == proto.KMsgNew && ev.Chan == tc.ch {
					var m proto.Message
					if proto.Unmarshal(ev.Data, &m) == nil && m.Author == a && len(m.Files) > 0 {
						held = append(held, f)
						return nil
					}
				}
				if f.Type == proto.TRes && !lied.Load() {
					var r proto.Res
					var sr proto.SendRes
					if proto.Unmarshal(f.Payload, &r) == nil && r.OK && proto.Unmarshal(r.Data, &sr) == nil && sr.Msg != 0 && len(held) > 0 {
						lied.Store(true)
						liedAt = time.Now()
						r.OK, r.Data, r.Err = false, nil, &proto.Error{Code: proto.ErrNotFound, Msg: "file upload expired; upload it again"}
						return []frame{{Type: proto.TRes, Payload: proto.MustRaw(r)}}
					}
				}
				// The held event goes out with the next RES (or anything a
				// little later): once alice already acted on the lie.
				if len(held) > 0 && lied.Load() && (f.Type == proto.TRes || time.Since(liedAt) > 500*time.Millisecond) {
					out := append(held, f)
					held = nil
					return out
				}
				return []frame{f}
			})
			defer env.tp.setOut(nil)

			stored := func() int {
				var n int
				if err := env.db().QueryRow(`SELECT COUNT(*) FROM messages WHERE chan = ? AND author = ?`, tc.ch, a).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n
			}
			want := stored() + 1

			body := "late event, " + tc.name
			if _, err := alice.SendFiles(tc.ch, 0, body, writeFile(t, t.TempDir(), "doc.txt", []byte("important"))); err != nil {
				t.Fatal(err)
			}
			eventually(t, "the lie told", lied.Load)
			eventually(t, "bob has it", func() bool { return bob.has(tc.ch, body) })
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
				if n := stored(); n != want {
					t.Fatalf("the server holds %d messages from alice, want %d: alice uploaded and signed the message again", n, want)
				}
			}
			eventually(t, "alice has her copy", func() bool { return alice.has(tc.ch, body) })
			for _, m := range alice.Messages(tc.ch, 50) {
				if m.Local != 0 {
					t.Fatalf("alice still shows a local copy of a delivered message (failed %q)", m.Failed)
				}
			}
		})
	}
}

// The resend after the lie must keep the first send's bytes even if the
// channel moved on meanwhile (bob rotates while alice uploads again): sealed
// afresh under the same nonce, it would draw a conflict the cache can't
// settle, and alice would show her delivered message as failed.
func TestNotFoundForStoredFileSendRotationBeforeResend(t *testing.T) {
	env := malTeam(t, "alice", "bob")
	alice, bob := env.us[0], env.us[1]
	ctx := ctxT(t)
	dm := env.encDM(t, alice, bob)
	a := alice.Me().ID
	gen := alice.channel("general")

	const (
		watch = iota
		lied
		rotating
		done
	)
	var phase atomic.Int32
	var held, heldEv []frame // guarded by the tamperer's mu
	env.tp.setOut(func(user uint64, f frame) []frame {
		if user != a {
			return []frame{f}
		}
		ev, isEv := asEvent(f)
		if isEv && ev.Kind == proto.KMsgNew && ev.Chan == dm {
			var m proto.Message
			if proto.Unmarshal(ev.Data, &m) == nil && m.Author == a && len(m.Files) > 0 {
				heldEv = append(heldEv, f) // shown only once she acted on the lie
				return nil
			}
		}
		switch phase.Load() {
		case watch:
			if f.Type == proto.TRes {
				var r proto.Res
				var sr proto.SendRes
				if proto.Unmarshal(f.Payload, &r) == nil && r.OK && proto.Unmarshal(r.Data, &sr) == nil && sr.Msg != 0 {
					r.OK, r.Data, r.Err = false, nil, &proto.Error{Code: proto.ErrNotFound, Msg: "file upload expired; upload it again"}
					phase.Store(lied)
					return []frame{{Type: proto.TRes, Payload: proto.MustRaw(r)}}
				}
			}
		case lied:
			if f.Type == proto.TRes { // the re-upload's first blob.put
				held = append(held, f)
				phase.Store(rotating)
				return heldEv
			}
		case rotating:
			if f.Type == proto.TRes {
				held = append(held, f)
				return nil
			}
			if isEv && ev.Kind == proto.KMsgNew && ev.Chan == gen {
				phase.Store(done)
				return append([]frame{f}, held...)
			}
		}
		return []frame{f}
	})
	defer env.tp.setOut(nil)

	body := "rotated meanwhile"
	if _, err := alice.SendFiles(dm, 0, body, writeFile(t, t.TempDir(), "doc.txt", []byte("important"))); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the re-upload held", func() bool { return phase.Load() == rotating })
	rotateCap()
	rotate(t, ctx, bob, dm)
	eventually(t, "alice at bob's epoch", alice.e2eAt(dm, 2))
	bob.send(t, ctx, gen, "release")
	eventually(t, "the re-upload released", func() bool { return phase.Load() == done })
	eventually(t, "bob has it", func() bool { return bob.has(dm, body) })
	var n int
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if err := env.db().QueryRow(`SELECT COUNT(*) FROM messages WHERE chan = ? AND author = ?`, dm, a).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("the server holds %d messages from alice", n)
		}
		if !alice.has(dm, body) {
			continue
		}
		for _, m := range alice.Messages(dm, 50) {
			if m.Local != 0 && m.Failed != "" {
				t.Fatalf("alice shows the delivered message as failed: %q", m.Failed)
			}
		}
	}
}
