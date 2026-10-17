package client

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
	"github.com/skailama/tc/internal/voice"
)

// Voice huddles, client side (PROTOCOL.md §14, docs/VOICE.md).

var (
	ErrNoVoiceServer = errors.New("this server has no voice huddles")
	ErrNoUDP         = errors.New("huddle needs UDP, and this connection fell back to TCP (UDP blocked?)")
	ErrVoiceKeys     = errors.New("voice waits for the channel's keys")
)

// keyTTL bounds how long a resolved frame key is trusted before the trust
// store is asked again (device revocations, rotations).
const keyTTL = 2 * time.Second

type huddleState struct {
	mu       sync.Mutex
	rosters  map[uint64][]proto.Part // chan → roster, as the server last said
	cur      *participation
	platform func() (voice.Platform, error) // nil = voice.Native (tests inject)
}

type participation struct {
	ch     uint64
	src    uint32
	sess   *session
	vs     *voice.Session
	keys   *huddleKeys // nil in plaintext channels
	cancel context.CancelFunc
}

// Huddle lists the users in ch's huddle, in join order.
func (c *Client) Huddle(ch uint64) []uint64 {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	var out []uint64
	for _, p := range c.hud.rosters[ch] {
		out = append(out, p.User)
	}
	return out
}

// Huddles maps each channel with a live huddle to its size.
func (c *Client) Huddles() map[uint64]int {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	out := map[uint64]int{}
	for ch, r := range c.hud.rosters {
		out[ch] = len(r)
	}
	return out
}

// InHuddle returns the channel whose huddle we're in.
func (c *Client) InHuddle() (uint64, bool) {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	if c.hud.cur == nil {
		return 0, false
	}
	return c.hud.cur.ch, true
}

// VoiceAvailable reports whether this build has audio.
func VoiceAvailable() bool { return voice.Available() }

// SetVoicePlatform replaces the audio platform (tests: fake codec and
// device). Call before JoinHuddle.
func (c *Client) SetVoicePlatform(f func() (voice.Platform, error)) {
	c.hud.mu.Lock()
	c.hud.platform = f
	c.hud.mu.Unlock()
}

func (c *Client) voicePlatform() (voice.Platform, error) {
	c.hud.mu.Lock()
	f := c.hud.platform
	c.hud.mu.Unlock()
	if f != nil {
		return f()
	}
	return voice.Native()
}

// JoinHuddle joins ch's huddle (leaving any other) and opens the audio
// device. Not talking until SetTalking(true).
func (c *Client) JoinHuddle(ctx context.Context, ch uint64) error {
	plat, err := c.voicePlatform()
	if err != nil {
		return err
	}
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	if !slices.Contains(s.welcome.Caps, proto.CapVoice) {
		return ErrNoVoiceServer
	}
	dg, ok := transport.Datagrams(s.conn)
	if !ok {
		return ErrNoUDP
	}
	cv, ok := c.Channel(ch)
	if !ok || !cv.Member {
		return errors.New("not a member of that channel")
	}
	c.LeaveHuddle()

	args := proto.HuddleJoinArgs{Chan: ch}
	var keys *huddleKeys
	if cv.E2E {
		sid, ok := c.cacheServerID()
		dev := s.myDevice()
		if !ok || dev == nil || c.E2EStatus(ch).State != E2EOK {
			return ErrVoiceKeys
		}
		keys = &huddleKeys{c: c, sid: sid, ch: ch, me: c.Me().ID, dev: dev.ID, parts: map[uint32]proto.Part{},
			cache: map[[2]uint32]cachedKey{}}
		if _, err := rand.Read(keys.stream[:]); err != nil {
			return err
		}
		if _, _, ok := keys.Send(); !ok {
			return ErrVoiceKeys
		}
		args.Dev, args.Stream = dev.ID[:], keys.stream[:]
	}
	var res proto.HuddleJoinRes
	if err := c.call(ctx, proto.OpHuddleJoin, args, &res, nil); err != nil {
		return err
	}
	cfg := voice.Config{Src: res.Src, Enc: cv.E2E, Send: dg.SendDatagram}
	if keys != nil {
		cfg.Keys = keys
	}
	vs, err := voice.Start(cfg, plat)
	if err != nil {
		_ = c.call(ctx, proto.OpHuddleLeave, struct{}{}, nil, nil)
		return err
	}
	rctx, cancel := context.WithCancel(c.ctx)
	p := &participation{ch: ch, src: res.Src, sess: s, vs: vs, keys: keys, cancel: cancel}
	c.hud.mu.Lock()
	c.hud.cur = p
	roster := c.hud.rosters[ch]
	c.hud.mu.Unlock()
	c.applyRoster(p, roster)
	go func() {
		for {
			b, err := dg.ReceiveDatagram(rctx)
			if err != nil {
				return
			}
			vs.Receive(b)
		}
	}()
	go func() {
		select {
		case <-s.done: // the server ends our participation with the connection
			c.endHuddle(p)
		case <-rctx.Done():
		}
	}()
	c.post(Update{Huddle: true})
	return nil
}

// LeaveHuddle leaves the current huddle, if any, and closes the audio device.
func (c *Client) LeaveHuddle() {
	c.hud.mu.Lock()
	p := c.hud.cur
	c.hud.mu.Unlock()
	if p == nil {
		return
	}
	if c.endHuddle(p) {
		_ = p.sess.fire(proto.OpHuddleLeave, struct{}{})
	}
}

// endHuddle tears p down once; it reports whether it did.
func (c *Client) endHuddle(p *participation) bool {
	c.hud.mu.Lock()
	if c.hud.cur != p {
		c.hud.mu.Unlock()
		return false
	}
	c.hud.cur = nil
	c.hud.mu.Unlock()
	p.cancel()
	_ = p.vs.Close()
	c.post(Update{Huddle: true})
	return true
}

// SetTalking turns our microphone stream on or off.
func (c *Client) SetTalking(on bool) {
	c.hud.mu.Lock()
	p := c.hud.cur
	c.hud.mu.Unlock()
	if p != nil {
		p.vs.SetTalking(on)
	}
}

// Talking reports whether our mic stream is on.
func (c *Client) Talking() bool {
	c.hud.mu.Lock()
	p := c.hud.cur
	c.hud.mu.Unlock()
	return p != nil && p.vs.Talking()
}

// Speaking returns who was heard in the last ~300 ms (not us).
func (c *Client) Speaking() map[uint64]bool {
	c.hud.mu.Lock()
	p := c.hud.cur
	c.hud.mu.Unlock()
	if p == nil {
		return nil
	}
	return p.vs.Speaking()
}

// VoiceStats exposes the current engine's counters (tests, diagnostics).
func (c *Client) VoiceStats() (voice.Stats, bool) {
	c.hud.mu.Lock()
	p := c.hud.cur
	c.hud.mu.Unlock()
	if p == nil {
		return voice.Stats{}, false
	}
	return p.vs.Stats(), true
}

// huddleEphemeral applies a roster (§14.2).
func (c *Client) huddleEphemeral(e proto.Ephemeral) {
	if e.Chan == 0 {
		return
	}
	parts := e.Parts
	if len(parts) > proto.MaxHuddleParts {
		parts = parts[:proto.MaxHuddleParts]
	}
	cv, ok := c.Channel(e.Chan)
	c.hud.mu.Lock()
	if c.hud.rosters == nil {
		c.hud.rosters = map[uint64][]proto.Part{}
	}
	if len(parts) == 0 || !ok || !cv.Member {
		delete(c.hud.rosters, e.Chan)
	} else {
		c.hud.rosters[e.Chan] = parts
	}
	p := c.hud.cur
	c.hud.mu.Unlock()
	if p != nil && p.ch == e.Chan {
		if !slices.ContainsFunc(parts, func(q proto.Part) bool { return q.Src == p.src }) {
			c.endHuddle(p) // the server dropped us (membership, another device took over)
		} else {
			c.applyRoster(p, parts)
		}
	}
	c.post(Update{Huddle: true})
}

func (c *Client) applyRoster(p *participation, parts []proto.Part) {
	m := map[uint32]uint64{}
	for _, q := range parts {
		if q.Src != p.src {
			m[q.Src] = q.User
		}
	}
	if p.keys != nil {
		p.keys.setParts(parts)
	}
	_ = p.vs.SetRoster(m)
}

// clearHuddles forgets rosters when the connection drops (the server
// resends them after the next sync).
func (c *Client) clearHuddles() {
	c.hud.mu.Lock()
	had := len(c.hud.rosters) > 0
	c.hud.rosters = nil
	c.hud.mu.Unlock()
	if had {
		c.post(Update{Huddle: true})
	}
}

// ---- frame keys (voice.Keys) ----

type cachedKey struct {
	s  *e2ecrypto.VoiceSender
	at time.Time
}

// huddleKeys answers the engine from the trust store: only verified epochs
// we hold K for, only rostered users, only their verified, unrevoked devices.
type huddleKeys struct {
	c      *Client
	sid    e2ecrypto.ServerID
	ch, me uint64
	dev    e2ecrypto.DevID
	stream [e2ecrypto.VoiceStreamSize]byte

	mu     sync.Mutex
	parts  map[uint32]proto.Part
	cache  map[[2]uint32]cachedKey // (src, epoch); src 0 = our sender
	headEp uint32
	headAt time.Time
}

func (k *huddleKeys) setParts(parts []proto.Part) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.parts = map[uint32]proto.Part{}
	for _, p := range parts {
		k.parts[p.Src] = p
	}
	clear(k.cache)
}

func (k *huddleKeys) cached(key [2]uint32) (*e2ecrypto.VoiceSender, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if ck, ok := k.cache[key]; ok && time.Since(ck.at) < keyTTL {
		return ck.s, true
	}
	return nil, false
}

func (k *huddleKeys) put(key [2]uint32, s *e2ecrypto.VoiceSender) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.cache[key] = cachedKey{s, time.Now()}
}

// epochKey returns vk for a verified epoch we hold K for whose roster has user.
func (k *huddleKeys) epochKey(epoch uint32, user uint64) (e2ecrypto.Key, bool) {
	ep, ok, err := k.c.trust.Epoch(k.sid, k.ch, epoch)
	if err != nil || !ok || ep.Key == nil || !slices.Contains(ep.Roster, user) {
		return e2ecrypto.Key{}, false
	}
	vk, err := e2ecrypto.VoiceKey(*ep.Key, k.sid, k.ch, epoch)
	return vk, err == nil
}

// head is the newest verified epoch, re-read at most every keyTTL: Send
// runs on the audio thread 50 times a second.
func (k *huddleKeys) head() (uint32, bool) {
	k.mu.Lock()
	if k.headEp > 0 && time.Since(k.headAt) < keyTTL {
		defer k.mu.Unlock()
		return k.headEp, true
	}
	k.mu.Unlock()
	h, ok, err := k.c.trust.ChainHead(k.sid, k.ch)
	if err != nil || !ok || h.Epoch == 0 {
		return 0, false
	}
	k.mu.Lock()
	k.headEp, k.headAt = h.Epoch, time.Now()
	k.mu.Unlock()
	return h.Epoch, true
}

func (k *huddleKeys) Send() (uint32, *e2ecrypto.VoiceSender, bool) {
	head, ok := k.head()
	if !ok {
		return 0, nil, false
	}
	if s, ok := k.cached([2]uint32{0, head}); ok {
		return head, s, true
	}
	vk, ok := k.epochKey(head, k.me)
	if !ok {
		return 0, nil, false
	}
	s, err := e2ecrypto.NewVoiceSender(vk, k.dev, k.stream)
	if err != nil {
		return 0, nil, false
	}
	k.put([2]uint32{0, head}, s)
	return head, s, true
}

func (k *huddleKeys) Recv(src, epoch uint32) (*e2ecrypto.VoiceSender, bool) {
	if s, ok := k.cached([2]uint32{src, epoch}); ok {
		return s, true
	}
	head, ok := k.head()
	// The newest verified epoch and the one before, so a rotation
	// mid-sentence doesn't cut anyone off (§14.3).
	if !ok || epoch == 0 || epoch > head || head-epoch > 1 {
		return nil, false
	}
	k.mu.Lock()
	p, ok := k.parts[src]
	k.mu.Unlock()
	if !ok || len(p.Dev) != e2ecrypto.IDSize || len(p.Stream) != e2ecrypto.VoiceStreamSize {
		return nil, false
	}
	dev := e2ecrypto.DevID(p.Dev)
	d, ok, err := k.c.trust.Device(k.sid, dev)
	if err != nil || !ok || d.User != p.User || d.Revoked {
		return nil, false
	}
	vk, ok := k.epochKey(epoch, p.User)
	if !ok {
		return nil, false
	}
	s, err := e2ecrypto.NewVoiceSender(vk, dev, [e2ecrypto.VoiceStreamSize]byte(p.Stream))
	if err != nil {
		return nil, false
	}
	k.put([2]uint32{src, epoch}, s)
	return s, true
}
