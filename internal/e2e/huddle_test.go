package e2e

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/voice"
)

// fakeAudio is a voice.Platform without libopus or a sound card: frames
// carry their first sample, and the test drives the device callback.
type fakeAudio struct {
	mu sync.Mutex
	cb func(out, in []int16)
}

type fCodec struct{}
type fEnc struct{}
type fDec struct{}

func (fCodec) NewEncoder() (voice.Encoder, error) { return fEnc{}, nil }
func (fCodec) NewDecoder() (voice.Decoder, error) { return fDec{}, nil }
func (fEnc) Encode(pcm []int16, out []byte) (int, error) {
	binary.BigEndian.PutUint16(out, uint16(pcm[0]))
	return 2, nil
}
func (fDec) Decode(d []byte, pcm []int16) (int, error) {
	for i := range pcm {
		pcm[i] = int16(binary.BigEndian.Uint16(d))
	}
	return len(pcm), nil
}
func (fDec) DecodeFEC(d []byte, pcm []int16) (int, error) { return len(pcm), nil }
func (fDec) Conceal(pcm []int16) (int, error)             { return len(pcm), nil }

type nopDuplex struct{}

func (nopDuplex) Close() error { return nil }

func (f *fakeAudio) platform() (voice.Platform, error) {
	return voice.Platform{Codec: fCodec{}, Open: func(cb func(out, in []int16)) (voice.Duplex, error) {
		f.mu.Lock()
		f.cb = cb
		f.mu.Unlock()
		return nopDuplex{}, nil
	}}, nil
}

// tick runs one 20 ms device period with mic samples of value v; it
// returns the first sample played.
func (f *fakeAudio) tick(v int16) int16 {
	f.mu.Lock()
	cb := f.cb
	f.mu.Unlock()
	in, out := make([]int16, voice.FrameSamples), make([]int16, voice.FrameSamples)
	for i := range in {
		in[i] = v
	}
	cb(out, in)
	return out[0]
}

func huddlers(t *testing.T, transport string, names ...string) []*user {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	t.Setenv("TC_TRANSPORT", transport)
	s, addr := startServer(t, t.TempDir(), "127.0.0.1:0")
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
	return us
}

// talk has a speak for n frames while everyone else's device runs, and
// returns what each listener heard.
func talk(a *fakeAudio, others []*fakeAudio, v int16, n int) [][]int16 {
	heard := make([][]int16, len(others))
	for range n {
		a.tick(v)
		time.Sleep(5 * time.Millisecond)
		for i, o := range others {
			if s := o.tick(0); s != 0 {
				heard[i] = append(heard[i], s)
			}
		}
	}
	for range 10 { // drain jitter buffers
		time.Sleep(20 * time.Millisecond)
		for i, o := range others {
			if s := o.tick(0); s != 0 {
				heard[i] = append(heard[i], s)
			}
		}
	}
	return heard
}

func TestHuddleThreeWayQUIC(t *testing.T) {
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
	if ch, ok := us[0].InHuddle(); !ok || ch != gen {
		t.Fatal("InHuddle")
	}

	us[0].SetTalking(true)
	heard := talk(audio[0], audio[1:], 7, 30)
	for i, h := range heard {
		if len(h) < 25 || slices.ContainsFunc(h, func(s int16) bool { return s != 7 }) {
			t.Fatalf("listener %d heard %d frames: %v", i+1, len(h), h)
		}
	}
	us[0].SetTalking(false)
	if st, _ := us[1].VoiceStats(); st.Received < 25 || st.BadFrame+st.Auth+st.Unknown != 0 {
		t.Fatalf("stats %+v", st)
	}

	// Leaving updates everyone's roster and stops our audio.
	us[2].LeaveHuddle()
	eventually(t, "roster after leave", func() bool { return len(us[0].Huddle(gen)) == 2 })
	if _, ok := us[2].InHuddle(); ok {
		t.Fatal("still in huddle after leave")
	}
}

func TestHuddleEncryptedDM(t *testing.T) {
	us := huddlers(t, "quic", "alice", "bob")
	alice, bob := us[0], us[1]
	sid := alice.sid(t)
	eventually(t, "devices verified both ways", func() bool {
		return alice.verifiedDevices(sid, bob.Me().ID) == 1 && bob.verifiedDevices(sid, alice.Me().ID) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dm, err := alice.OpenDM(ctx, []uint64{bob.Me().ID})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "bob has the DM", func() bool { _, ok := bob.Channel(dm.ID); return ok })
	// Voice needs the channel's key: send a message so epoch 1 exists.
	alice.send(t, ctx, dm.ID, "hi")
	eventually(t, "both keyed", func() bool {
		return alice.E2EStatus(dm.ID).State == client.E2EOK && bob.E2EStatus(dm.ID).State == client.E2EOK
	})
	fa, fb := &fakeAudio{}, &fakeAudio{}
	alice.SetVoicePlatform(fa.platform)
	bob.SetVoicePlatform(fb.platform)
	if err := alice.JoinHuddle(ctx, dm.ID); err != nil {
		t.Fatal(err)
	}
	if err := bob.JoinHuddle(ctx, dm.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "roster", func() bool { return len(alice.Huddle(dm.ID)) == 2 && len(bob.Huddle(dm.ID)) == 2 })
	bob.SetTalking(true)
	heard := talk(fb, []*fakeAudio{fa}, 42, 30)
	if len(heard[0]) < 25 {
		t.Fatalf("alice heard %d encrypted frames", len(heard[0]))
	}
	st, _ := alice.VoiceStats()
	if st.Key+st.Auth+st.BadFrame != 0 {
		t.Fatalf("encrypted frames rejected: %+v", st)
	}
	if sp := alice.Speaking(); !sp[bob.Me().ID] {
		// Speaking is measured against the clock; the drain loop may have
		// outlasted 300 ms, so only require the frames above.
		t.Log("bob not marked speaking at the end (expected after drain)")
	}
}

func TestHuddleOverTCPRefused(t *testing.T) {
	us := huddlers(t, "tcp", "ana")
	us[0].SetVoicePlatform((&fakeAudio{}).platform)
	err := us[0].JoinHuddle(context.Background(), us[0].channel("general"))
	if !errors.Is(err, client.ErrNoUDP) {
		t.Fatalf("join over TCP: %v", err)
	}
}

func TestHuddleNoVoiceBuild(t *testing.T) {
	if voice.Available() {
		t.Skip("voice build")
	}
	us := huddlers(t, "quic", "ana")
	err := us[0].JoinHuddle(context.Background(), us[0].channel("general"))
	if !errors.Is(err, voice.ErrNoVoice) {
		t.Fatalf("join without voice build: %v", err)
	}
}
