package client

import (
	"context"
	"crypto/rand"
	"errors"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
	"github.com/skailama/tc/internal/trust"
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
	skew     atomic.Int64                   // ns added to the voice engine's clock (tests)
	// gen moves on every join attempt and leave, so a join that finishes
	// after a newer join or a leave tears itself down instead of
	// replacing (and leaking) what's current.
	gen uint64
	// Rosters are a baseline (never a "start") until liveAt, which is
	// zero until a sync completes and then a moment after it (the server
	// sends every live roster right after sync), and for a channel we
	// just joined (joinedAt).
	liveAt   time.Time
	joinedAt map[uint64]time.Time
}

type participation struct {
	ch     uint64
	src    uint32
	sess   *session
	vs     *voice.Session
	keys   *huddleKeys // nil in plaintext channels
	cancel context.CancelFunc
	// skewed: frames kept failing the ±60 s ts check (under hud.mu); the
	// UI warns once per participation.
	skewed bool

	cueMu    sync.Mutex
	started  time.Time
	others   map[uint64]bool // users in the roster besides us, last time
	lastJoin time.Time
	lastMic  time.Time
	micOn    bool
}

// In-huddle cue rules (NOTIFICATIONS-2026-10.md §2.5 rule 6).
const (
	cueSettle      = 2 * time.Second // no join/leave cues right after our own join
	cueCoalesce    = time.Second     // one join (or leave) cue per second
	cueMicDebounce = 150 * time.Millisecond
	cueMaxRoster   = 8 // bigger huddles get no join/leave cues
)

// rosterCues plays one join or leave cue for a roster change, if wanted.
func (c *Client) rosterCues(p *participation, parts []proto.Part) {
	now := time.Now()
	users := map[uint64]bool{}
	for _, q := range parts {
		if q.Src != p.src && q.User != c.Me().ID {
			users[q.User] = true
		}
	}
	p.cueMu.Lock()
	joined, left := 0, 0
	for u := range users {
		if !p.others[u] {
			joined++
		}
	}
	for u := range p.others {
		if !users[u] {
			left++
		}
	}
	first := p.others == nil
	p.others = users
	var cue voice.Cue
	switch {
	case first || now.Sub(p.started) < cueSettle || len(parts) >= cueMaxRoster || now.Sub(p.lastJoin) < cueCoalesce:
	case joined > 0:
		cue = voice.CueJoin
	case left > 0:
		cue = voice.CueLeave
	}
	if cue != 0 {
		p.lastJoin = now
	}
	p.cueMu.Unlock()
	if cue != 0 {
		if pr := c.NotifyPrefs(); pr.Sound("join") {
			p.vs.PlayCue(cue, pr.Volume)
		}
	}
}

// micCue clicks when our mic opens or closes (debounced, so the ctrl+t
// repeat heuristic can't chatter).
func (c *Client) micCue(p *participation, on bool) {
	now := time.Now()
	p.cueMu.Lock()
	play := on != p.micOn && now.Sub(p.lastMic) >= cueMicDebounce
	p.micOn = on
	if play {
		p.lastMic = now
	}
	p.cueMu.Unlock()
	if !play {
		return
	}
	if pr := c.NotifyPrefs(); pr.Sound("mic") {
		cue := voice.CueMicOff
		if on {
			cue = voice.CueMicOn
		}
		p.vs.PlayCue(cue, pr.Volume)
	}
}

// skewWarnAfter is how many frames dropped for clock skew within one
// check (about half a second of one speaker) mean a peer's clock is more
// than a minute off, not a stray replay.
const skewWarnAfter = 25

// ErrClockSkew is the HuddleWarning for repeated clock-skew drops.
var ErrClockSkew = errors.New("clock skew: some huddle audio dropped — check NTP on both machines")

// HuddleWarning is a problem with the current huddle the user should know
// about (nil if none); the client posts Update{Huddle} when it appears.
func (c *Client) HuddleWarning() error {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	if p := c.hud.cur; p != nil && p.skewed {
		return ErrClockSkew
	}
	return nil
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

func (c *Client) rosterHas(ch uint64, src uint32) bool {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	return slices.ContainsFunc(c.hud.rosters[ch], func(q proto.Part) bool { return q.Src == src })
}

// SetVoiceClockSkew shifts the voice engine's clock by d (tests: a peer
// whose clock is off).
func (c *Client) SetVoiceClockSkew(d time.Duration) { c.hud.skew.Store(int64(d)) }

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
	// Whether voice is encrypted is our trust state's call, never the
	// server's channel flag alone: plaintext only where the channel is
	// provably not encrypted (no sticky flag, no alarm).
	st := c.E2EStatus(ch)
	enc := st.State != E2EOff
	if enc && st.State != E2EOK {
		return ErrVoiceKeys
	}
	c.LeaveHuddle()
	c.hud.mu.Lock()
	c.hud.gen++
	gen := c.hud.gen
	c.hud.mu.Unlock()

	args := proto.HuddleJoinArgs{Chan: ch}
	var keys *huddleKeys
	if enc {
		sid, ok := c.cacheServerID()
		dev := s.myDevice()
		if !ok || dev == nil {
			return ErrVoiceKeys
		}
		keys = &huddleKeys{c: c, sid: sid, ch: ch, me: c.Me().ID, dev: dev.ID, parts: map[uint32]proto.Part{},
			cache: map[[2]uint32]cachedKey{}}
		if _, err := rand.Read(keys.stream[:]); err != nil {
			return err
		}
		// Catch up and rotate first if the current epoch is stale (a member
		// left, a device was revoked), exactly as a text send would.
		if err := keys.refresh(ctx, s); err != nil {
			return err
		}
		args.Dev, args.Stream = dev.ID[:], keys.stream[:]
	}
	raw, err := s.call(ctx, proto.OpHuddleJoin, args, nil)
	if err != nil {
		return err
	}
	var res proto.HuddleJoinRes
	if err := proto.Unmarshal(raw, &res); err != nil {
		return err
	}
	rctx, cancel := context.WithCancel(c.ctx)
	cfg := voice.Config{Src: res.Src, Enc: enc, Send: nonBlocking(rctx, dg),
		Now: func() time.Time { return time.Now().Add(time.Duration(c.hud.skew.Load())) }}
	if keys != nil {
		cfg.Keys = keys
	}
	vs, err := voice.Start(cfg, plat)
	if err != nil {
		cancel()
		_ = s.fire(proto.OpHuddleLeave, struct{}{})
		return err
	}
	p := &participation{ch: ch, src: res.Src, sess: s, vs: vs, keys: keys, cancel: cancel, started: time.Now()}
	// The roster with our src comes before the RES (§14.1); allow a little
	// slack before calling ourselves dropped.
	for i := 0; i < 20 && !c.rosterHas(ch, res.Src); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	c.hud.mu.Lock()
	roster := c.hud.rosters[ch]
	// The server sends our roster before the RES; anything since (we were
	// dropped while the device opened) or a newer join/leave wins.
	stale := c.hud.gen != gen || c.hud.cur != nil
	dropped := !slices.ContainsFunc(roster, func(q proto.Part) bool { return q.Src == res.Src })
	if !stale && !dropped {
		c.hud.cur = p
	}
	c.hud.mu.Unlock()
	if stale || dropped {
		cancel()
		_ = vs.Close()
		if !stale {
			_ = s.fire(proto.OpHuddleLeave, struct{}{})
		}
		if dropped {
			return errors.New("dropped from the huddle while joining")
		}
		return errors.New("huddle join superseded")
	}
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
	go c.watchHuddle(rctx, p)
	c.post(Update{Huddle: true})
	return nil
}

// sendQueue bounds frames waiting for the network: SendDatagram can block
// while QUIC is congested, and the audio thread must never wait on it.
const sendQueue = 16

func nonBlocking(ctx context.Context, dg transport.DatagramConn) func([]byte) error {
	q := make(chan []byte, sendQueue)
	go func() {
		for {
			select {
			case b := <-q:
				_ = dg.SendDatagram(b)
			case <-ctx.Done():
				return
			}
		}
	}()
	return func(b []byte) error {
		select {
		case q <- b:
			return nil
		default:
			return errors.New("voice: send queue full")
		}
	}
}

const (
	watchEvery = 100 * time.Millisecond
	checkEvery = time.Second
)

// watchHuddle ends p with its connection, re-checks encryption state (a
// channel that changed encryption, an alarm, a stale epoch) and tells the
// UI when the set of people talking changes, so the UI needn't poll.
func (c *Client) watchHuddle(ctx context.Context, p *participation) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	var last map[uint64]bool
	var checked time.Time
	var skew uint64
	for {
		select {
		case <-p.sess.done: // the server ends our participation with the connection
			c.endHuddle(p)
			return
		case <-ctx.Done():
			return
		case now := <-t.C:
			if sp := p.vs.Speaking(); !maps.Equal(sp, last) {
				last = sp
				c.post(Update{Huddle: true})
			}
			if now.Sub(checked) >= checkEvery {
				checked = now
				n := p.vs.Stats().Skew
				if n-skew >= skewWarnAfter {
					c.hud.mu.Lock()
					warn := !p.skewed
					p.skewed = true
					c.hud.mu.Unlock()
					if warn {
						c.post(Update{Huddle: true})
					}
				}
				skew = n
				if !c.huddleStillValid(ctx, p) && ctx.Err() == nil {
					// End p only: the user may have moved to another
					// huddle while this check ran.
					if c.endHuddle(p) {
						_ = p.sess.fire(proto.OpHuddleLeave, struct{}{})
					}
					return
				}
			}
		}
	}
}

// huddleStillValid reports whether p may go on; for encrypted huddles it
// blocks sending while the epoch is stale and rotates in the background.
func (c *Client) huddleStillValid(ctx context.Context, p *participation) bool {
	st := c.E2EStatus(p.ch)
	if (st.State != E2EOff) != (p.keys != nil) {
		return false // encryption changed under us: never keep sending plaintext
	}
	if p.keys == nil {
		return true
	}
	if st.State == E2EOK && p.keys.fresh() {
		p.keys.prime()
		// Fresh again some other way (our owed rotation paid by the e2e
		// worker, someone else rotated) after a failed refresh: unmute.
		p.keys.mu.Lock()
		if !p.keys.refreshing {
			p.keys.blocked = false
		}
		p.keys.mu.Unlock()
		return true
	}
	k := p.keys
	k.mu.Lock()
	k.blocked = true
	busy := k.refreshing
	k.refreshing = true
	k.mu.Unlock()
	if !busy {
		go func() {
			ok := k.refresh(ctx, p.sess) == nil && c.E2EStatus(p.ch).State == E2EOK
			k.mu.Lock()
			k.refreshing = false
			k.blocked = !ok
			k.mu.Unlock()
		}()
	}
	return true
}

// LeaveHuddle leaves the current huddle, if any, and closes the audio device.
func (c *Client) LeaveHuddle() {
	c.hud.mu.Lock()
	c.hud.gen++ // a join still in flight gives up
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
		c.micCue(p, on)
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

// HuddleStart is a huddle someone started while we were online.
type HuddleStart struct {
	Chan, By uint64
}

// baselineFor is how long after a sync or chan.join rosters are taken as
// what was already there.
const baselineFor = 2 * time.Second

// huddlesLive starts treating new rosters as starts, baselineFor after a
// completed sync; huddlesOffline stops it until the next one.
func (c *Client) huddlesLive(now time.Time) {
	c.hud.mu.Lock()
	c.hud.liveAt = now.Add(baselineFor)
	c.hud.mu.Unlock()
}

func (c *Client) huddleBaseline(ch uint64) {
	c.hud.mu.Lock()
	if c.hud.joinedAt == nil {
		c.hud.joinedAt = map[uint64]time.Time{}
	}
	c.hud.joinedAt[ch] = time.Now()
	c.hud.mu.Unlock()
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
	had := len(c.hud.rosters[e.Chan]) > 0
	var start []HuddleStart
	if len(parts) == 0 || !ok || !cv.Member {
		delete(c.hud.rosters, e.Chan)
	} else {
		c.hud.rosters[e.Chan] = parts
		now := time.Now()
		live := !c.hud.liveAt.IsZero() && now.After(c.hud.liveAt) && now.Sub(c.hud.joinedAt[e.Chan]) > baselineFor
		if !had && live && parts[0].User != c.Me().ID {
			start = []HuddleStart{{Chan: e.Chan, By: parts[0].User}}
		}
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
	c.post(Update{Huddle: true, HuddleStarted: start})
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
	c.rosterCues(p, parts)
}

// clearHuddles forgets rosters when the connection drops (the server
// resends them after the next sync).
func (c *Client) clearHuddles() {
	c.hud.mu.Lock()
	had := len(c.hud.rosters) > 0
	c.hud.rosters = nil
	c.hud.liveAt = time.Time{} // the next sync's rosters are a baseline
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
	// grace is until when frames under the epoch before the head are
	// still accepted: briefly after the head moves, never indefinitely,
	// so an ex-member holding the old K can't keep injecting audio.
	grace      time.Time
	blocked    bool // sending paused: the epoch is stale or the channel in alarm
	refreshing bool // a background refresh is running
}

// prevEpochGrace is how long after we see a new head the previous epoch's
// frames are still accepted (senders switch as soon as they verify it).
const prevEpochGrace = 10 * time.Second

func (k *huddleKeys) block(on bool) {
	k.mu.Lock()
	k.blocked = on
	k.mu.Unlock()
}

// refresh catches up on the channel's chain and rotates when the current
// epoch is stale, exactly as a text send does (sealWith), then forgets
// cached keys so the next frame uses the fresh head.
func (k *huddleKeys) refresh(ctx context.Context, s *session) error {
	_, err := k.c.sealWith(ctx, s, k.ch, nil, func(e2ecrypto.ServerID, *e2ecrypto.DeviceKeys, trust.Epoch) ([]byte, error) {
		return nil, nil
	})
	if err != nil {
		return err
	}
	k.mu.Lock()
	k.headAt = time.Time{}
	clear(k.cache)
	k.mu.Unlock()
	if !k.fresh() {
		return ErrVoiceKeys
	}
	return nil
}

// fresh reports whether the verified head may still be sealed to: we hold
// its K, its roster is who we'd seal to now, no device revoked since may
// hold its K and we owe no rotation for a revoke (revokedStale, the text
// path's rule), so a removed member or a stolen device never hears what is
// said next.
func (k *huddleKeys) fresh() bool {
	h, ok, err := k.c.trust.ChainHead(k.sid, k.ch)
	if err != nil || !ok {
		return false
	}
	ep, ok, err := k.c.trust.Epoch(k.sid, k.ch, h.Epoch)
	if err != nil || !ok || ep.Key == nil {
		return false
	}
	ch, ok, err := getChan(k.c.db, k.ch)
	if err != nil || !ok {
		return false
	}
	return slices.Equal(ep.Roster, k.c.rosterNow(ch)) && !k.c.revokedStale(k.sid, k.ch, ep)
}

// prime re-reads the head and re-derives our sender key off the audio
// thread, so Send (called from the audio callback) always finds them
// cached and never touches the trust store.
func (k *huddleKeys) prime() {
	h, ok, err := k.c.trust.ChainHead(k.sid, k.ch)
	if err != nil || !ok || h.Epoch == 0 {
		return
	}
	vk, ok := k.epochKey(h.Epoch, k.me)
	if !ok {
		return
	}
	s, err := e2ecrypto.NewVoiceSender(vk, k.dev, k.stream)
	if err != nil {
		return
	}
	now := time.Now()
	k.mu.Lock()
	defer k.mu.Unlock()
	k.moveHead(h.Epoch, now)
	k.cache[[2]uint32{0, h.Epoch}] = cachedKey{s, now}
}

// member reports whether user is a current member of the channel.
func (k *huddleKeys) member(user uint64) bool {
	ch, ok, err := getChan(k.c.db, k.ch)
	return err == nil && ok && slices.Contains(k.c.liveMembers(ch), user)
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

// epochTrusted reports whether nobody outside the channel may hold the
// epoch's K: everyone in its roster is still a live member and no device
// revoked since may have it (revokedStale, the text path's rule).
func (k *huddleKeys) epochTrusted(epoch uint32) bool {
	ep, ok, err := k.c.trust.Epoch(k.sid, k.ch, epoch)
	if err != nil || !ok {
		return false
	}
	ch, ok, err := getChan(k.c.db, k.ch)
	if err != nil || !ok {
		return false
	}
	live := k.c.liveMembers(ch)
	for _, u := range ep.Roster {
		if !slices.Contains(live, u) {
			return false
		}
	}
	return !k.c.revokedStale(k.sid, k.ch, ep)
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
	k.moveHead(h.Epoch, time.Now())
	k.mu.Unlock()
	return h.Epoch, true
}

// moveHead records the head; the previous epoch's grace starts when we
// see the head move, never on first sight (at join the rotation may be
// long past). Caller holds k.mu.
func (k *huddleKeys) moveHead(epoch uint32, now time.Time) {
	if k.headEp != 0 && epoch != k.headEp {
		k.grace = now.Add(prevEpochGrace)
	}
	k.headEp, k.headAt = epoch, now
}

func (k *huddleKeys) Send() (uint32, *e2ecrypto.VoiceSender, bool) {
	k.mu.Lock()
	blocked := k.blocked
	k.mu.Unlock()
	if blocked {
		return 0, nil, false
	}
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
	head, ok := k.head()
	// The newest verified epoch and the one before, so a rotation
	// mid-sentence doesn't cut anyone off (§14.3) — checked before the
	// cache, so a cached key never outlives the window.
	if !ok || epoch == 0 || epoch > head || head-epoch > 1 {
		return nil, false
	}
	k.mu.Lock()
	inGrace := time.Now().Before(k.grace)
	k.mu.Unlock()
	if epoch != head && !inGrace {
		return nil, false
	}
	if s, ok := k.cached([2]uint32{src, epoch}); ok {
		return s, true
	}
	// Voice keys are group-symmetric: whoever holds K can speak as anyone
	// in the roster. So an epoch someone who left or a revoked device may
	// hold is refused whoever the frame claims to be from (a malicious
	// server could refuse the rotation and relay their frames).
	if !k.epochTrusted(epoch) {
		return nil, false
	}
	k.mu.Lock()
	p, ok := k.parts[src]
	k.mu.Unlock()
	if !ok || !k.member(p.User) {
		return nil, false
	}
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
