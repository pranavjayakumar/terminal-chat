package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
)

// Two people talking at once: each hears only the other, never their own
// captured audio (no self-echo path through the relay, the roster or the
// mixer), and a third listener hears both mixed.
func TestHuddleTwoTalkersNoSelfEcho(t *testing.T) {
	us := huddlers(t, "quic", "ana", "bo", "cy")
	gen := us[0].channel("general")
	audio := make([]*fakeAudio, len(us))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i, u := range us {
		audio[i] = &fakeAudio{}
		u.SetVoicePlatform(audio[i].platform)
		if err := u.JoinHuddle(ctx, gen); err != nil {
			t.Fatalf("%s join: %v", u.Me().Name, err)
		}
	}
	for _, u := range us {
		eventually(t, "full roster", func() bool { return len(u.Huddle(gen)) == 3 })
	}
	us[0].SetTalking(true)
	us[1].SetTalking(true)
	const anaV, boV = 7, 9
	heard := make([]map[int16]int, 3)
	for i := range heard {
		heard[i] = map[int16]int{}
	}
	for i := range 80 {
		vs := []int16{anaV, boV, 0}
		if i >= 60 { // stop talking, drain the jitter buffers
			vs = []int16{0, 0, 0}
		}
		for j, a := range audio {
			heard[j][a.tick(vs[j])]++
		}
		time.Sleep(20 * time.Millisecond)
	}
	if heard[0][anaV] != 0 || heard[0][anaV+boV] != 0 {
		t.Fatalf("ana heard her own voice: %v", heard[0])
	}
	if heard[1][boV] != 0 || heard[1][anaV+boV] != 0 {
		t.Fatalf("bo heard his own voice: %v", heard[1])
	}
	if heard[0][boV] < 30 || heard[1][anaV] < 30 {
		t.Fatalf("talkers didn't hear each other: ana %v, bo %v", heard[0], heard[1])
	}
	if heard[2][anaV+boV] < 30 {
		t.Fatalf("cy didn't hear both mixed: %v", heard[2])
	}
	for i, u := range us {
		st, _ := u.VoiceStats()
		if st.Unknown+st.BadFrame+st.Replay != 0 {
			t.Fatalf("%d: dropped frames %+v", i, st)
		}
	}
}

// encDMHuddle puts alice and bob in an encrypted DM huddle over QUIC; alice
// has a second device (laptop) that isn't in the call.
func encDMHuddle(t *testing.T, ctx context.Context) (alice, laptop, bob *user, dm uint64, fa, fb *fakeAudio) {
	t.Helper()
	_, _, us := huddlersSrv(t, "quic", "alice", "bob")
	alice, bob = us[0], us[1]
	laptop = secondDevice(t, alice)
	sid := alice.sid(t)
	a, b := alice.Me().ID, bob.Me().ID
	eventually(t, "devices verified", func() bool {
		return laptop.online() && bob.verifiedDevices(sid, a) == 2 && alice.verifiedDevices(sid, b) == 1 &&
			alice.verifiedDevices(sid, a) == 2 && laptop.verifiedDevices(sid, b) == 1
	})
	ch, err := alice.OpenDM(ctx, []uint64{b})
	if err != nil {
		t.Fatal(err)
	}
	dm = ch.ID
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(dm); return ok })
	alice.send(t, ctx, dm, "hi")
	eventually(t, "keyed", func() bool {
		return alice.E2EStatus(dm).State == client.E2EOK && bob.E2EStatus(dm).State == client.E2EOK
	})
	fa, fb = &fakeAudio{}, &fakeAudio{}
	alice.SetVoicePlatform(fa.platform)
	bob.SetVoicePlatform(fb.platform)
	if err := alice.JoinHuddle(ctx, dm); err != nil {
		t.Fatal(err)
	}
	if err := bob.JoinHuddle(ctx, dm); err != nil {
		t.Fatal(err)
	}
	eventually(t, "roster", func() bool { return len(alice.Huddle(dm)) == 2 && len(bob.Huddle(dm)) == 2 })
	return
}

// Revoking a device mid-call rotates the key; audio keeps flowing both
// ways after at most a short gap.
func TestHuddleRevokeMidCallContinuity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	alice, laptop, bob, dm, fa, fb := encDMHuddle(t, ctx)
	sid := alice.sid(t)
	lost, err := laptop.Trust().DeviceKeys(sid, alice.Me().ID)
	if err != nil {
		t.Fatal(err)
	}
	before := alice.status(dm).Epoch
	alice.SetTalking(true)
	bob.SetTalking(true)
	type sample struct {
		at   time.Time
		a, b bool
	}
	var got []sample
	start := time.Now()
	revoked := false
	for time.Since(start) < 9*time.Second {
		if !revoked && time.Since(start) > 1500*time.Millisecond {
			revoked = true
			go func() {
				if _, err := alice.RevokeDevice(ctx, lost.ID, false); err != nil {
					t.Error(err)
				}
			}()
		}
		ha := fa.tick(3) // alice says 3, hears bob's 5
		hb := fb.tick(5)
		got = append(got, sample{time.Now(), ha == 5, hb == 3})
		time.Sleep(20 * time.Millisecond)
	}
	if after := alice.status(dm).Epoch; after <= before {
		t.Fatalf("no rotation: epoch %d → %d", before, after)
	}
	// Longest silence for each listener once audio first flowed.
	gap := func(heard func(sample) bool) (longest time.Duration, tail bool) {
		var last time.Time
		for _, s := range got {
			if heard(s) {
				if !last.IsZero() {
					longest = max(longest, s.at.Sub(last))
				}
				last = s.at
			}
		}
		return longest, !last.IsZero() && time.Since(last) < 1500*time.Millisecond
	}
	ga, ta := gap(func(s sample) bool { return s.a })
	gb, tb := gap(func(s sample) bool { return s.b })
	t.Logf("longest gap: alice hearing bob %v, bob hearing alice %v", ga, gb)
	if !ta || !tb {
		t.Fatalf("audio didn't resume after the rotation (alice hears: %v, bob hears: %v)", ta, tb)
	}
	if ga > 3500*time.Millisecond || gb > 3500*time.Millisecond {
		t.Fatalf("audio gap too long across rotation: %v / %v", ga, gb)
	}
}
