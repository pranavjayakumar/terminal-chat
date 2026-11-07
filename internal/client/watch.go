package client

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/screen"
)

// Pane sharing in a huddle (PROTOCOL.md §14.5): one participant streams a
// terminal pane, read-only, to the participants who watch it. Updates are
// a keyframe then cumulative diffs (internal/screen), chunked into screen
// datagrams on the huddle's own path (QUIC datagrams, or VOICE frames on
// the TCP fallback) and, in encrypted channels, sealed with the huddle's
// frame keys. The server relays them and stores nothing.

var (
	ErrNoWatchServer = errors.New("this server can't relay a shared pane yet (it needs tc update)")
	ErrNotInHuddle   = errors.New("not in a huddle (/huddle to join)")
	ErrNotLive       = errors.New("isn't sharing a pane in your huddle")
	ErrShareTooBig   = errors.New("the pane is too big to share")

	errShareCancelled = errors.New("stopped before it started")
)

// Capture reads the pane being shared: its visible text with SGR colors.
type Capture func(ctx context.Context) (string, error)

// Streamer pacing (docs/VOICE.md "Pane sharing"): at most 10 pictures a
// second while the pane changes, backing off to one a second when it
// doesn't; nothing at all while nobody watches. The current picture is
// sent again a second after it last changed and then every 5 s, so a
// viewer that lost a chunk catches up.
const (
	shareFast      = 100 * time.Millisecond
	shareSlow      = time.Second
	shareRefresh1  = time.Second
	shareRefreshN  = 5 * time.Second
	captureTimeout = 2 * time.Second
	captureFails   = 3
	// The sender's own budget, under the server's (burst 96, 48/s).
	shareBurst  = 80
	sharePerSec = 40
)

type paneState struct {
	ctx context.Context // the participation's

	mu       sync.Mutex
	share    *paneShare
	view     *paneView
	watchers int   // participants watching us, by the last roster
	watchSet map[uint32]bool
	// watchGen moves whenever someone starts watching (a src not in the
	// last roster's set), even if someone else stopped meanwhile.
	watchGen int
	stopped  error // why our stream stopped by itself, until read
	// enc, seq and upd belong to the one running share loop (StartShare
	// waits for the previous one), and outlive it: key ids, nonces and
	// update ids keep rising for the whole participation.
	enc  screen.Encoder
	seq  uint32
	upd  uint32
	kick chan struct{}
	// startMu serializes StartShare; stops counts StopShare calls (under
	// mu), so a stop during a start's round trip wins; ended: the
	// participation is over.
	startMu sync.Mutex
	stops   int
	ended   bool
}

type paneShare struct {
	label    string
	cancel   context.CancelFunc
	done     chan struct{}
	scrubbed atomic.Int64
}

type paneView struct {
	user uint64
	// Reassembled updates go to their own decode goroutine, so a hostile
	// stream's costly updates never hold up the receive loop that also
	// carries audio. stop ends it.
	in   chan decItem
	stop chan struct{}
	once sync.Once

	// mu: the receive side (the huddle's receive goroutine).
	mu    sync.Mutex
	src   uint32
	asm   screen.Reassembler
	win   replayWindow
	ended bool
	gen   uint64 // under mu and dmu: moves when the view starts over
	stats WatchStats

	// dmu: the picture (the decode goroutine). Lock order: mu, then dmu.
	dmu sync.Mutex
	dec screen.Decoder
	ver uint64
	at  time.Time
}

type decItem struct {
	upd []byte
	gen uint64
}

// decodeQueue bounds updates waiting for the decoder; more are dropped
// (the streamer's refresh repairs a gap).
const decodeQueue = 8

func newPaneView(user uint64, src uint32) *paneView {
	return &paneView{user: user, src: src, in: make(chan decItem, decodeQueue), stop: make(chan struct{})}
}

func (v *paneView) close() { v.once.Do(func() { close(v.stop) }) }

// decodeLoop applies reassembled updates until the view is dropped.
func (c *Client) decodeLoop(ctx context.Context, v *paneView) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-v.stop:
			return
		case it := <-v.in:
			v.dmu.Lock()
			changed, err := false, error(nil)
			if it.gen == v.gen {
				changed, err = v.dec.Apply(it.upd)
				if changed {
					v.ver++
					v.at = time.Now()
				}
			}
			v.dmu.Unlock()
			v.mu.Lock()
			switch {
			case err != nil:
				v.stats.BadUpdate++
			case changed:
				v.stats.Applied++
			}
			v.mu.Unlock()
			if changed {
				c.post(Update{Watch: true})
			}
		}
	}
}

// WatchStats counts what a viewer did with screen datagrams (tests,
// diagnostics).
type WatchStats struct {
	Received, Applied                    uint64
	BadFrame, Unknown, Replay, Key, Auth uint64
	Skew, BadUpdate                      uint64
}

// WatchFrame is the shared pane we watch, as last decoded.
type WatchFrame struct {
	User  uint64
	Label string
	// Grid is a copy, nil until the first keyframe arrived.
	Grid *screen.Grid
	// Ver moves with every new picture.
	Ver uint64
	At  time.Time
	// Ended: the stream stopped (Grid is its last picture).
	Ended bool
	Enc   bool
}

// ShareInfo is our own pane stream.
type ShareInfo struct {
	Label    string
	Watchers int
	Scrubbed int // secrets masked in the current picture
	Enc      bool
}

func (c *Client) curParticipation() *participation {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	return c.hud.cur
}

func (p *participation) watchCap() bool {
	return slices.Contains(p.sess.welcome.Caps, proto.CapWatch)
}

// StartShare streams a pane to our huddle: capture is called up to ten
// times a second while someone watches. A stream we already run is
// replaced.
func (c *Client) StartShare(ctx context.Context, label string, capture Capture) error {
	p := c.curParticipation()
	if p == nil {
		return ErrNotInHuddle
	}
	if !p.watchCap() {
		return ErrNoWatchServer
	}
	// One start at a time per participation: two loops would share the
	// encoder and, worse, the nonce counter.
	p.pane.startMu.Lock()
	defer p.pane.startMu.Unlock()
	if old := c.stopShare(p, false); old != nil {
		<-old.done
	}
	p.pane.mu.Lock()
	stops := p.pane.stops
	p.pane.mu.Unlock()
	if _, err := p.sess.call(ctx, proto.OpHuddleShare, proto.HuddleShareArgs{On: true}, nil); err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(p.pane.ctx)
	sh := &paneShare{label: label, cancel: cancel, done: make(chan struct{})}
	p.pane.mu.Lock()
	if p.pane.ended || p.pane.stops != stops {
		// The huddle ended, or the user stopped while we asked: never
		// start after a stop.
		p.pane.mu.Unlock()
		cancel()
		_ = p.sess.fire(proto.OpHuddleShare, proto.HuddleShareArgs{On: false})
		return errShareCancelled
	}
	if p.pane.kick == nil {
		p.pane.kick = make(chan struct{}, 1)
	}
	p.pane.share, p.pane.stopped = sh, nil
	p.pane.mu.Unlock()
	go c.shareLoop(sctx, p, sh, capture)
	c.post(Update{Watch: true})
	return nil
}

// StopShare stops our pane stream at once: nothing more is captured or
// sent, whatever the server says.
func (c *Client) StopShare() {
	if p := c.curParticipation(); p != nil {
		p.pane.mu.Lock()
		p.pane.stops++
		p.pane.mu.Unlock()
		c.stopShare(p, true)
	}
}

// stopShare cancels p's stream; tell: also tell the server. It returns
// the stopped stream (nil if none).
func (c *Client) stopShare(p *participation, tell bool) *paneShare {
	p.pane.mu.Lock()
	sh := p.pane.share
	p.pane.share = nil
	p.pane.mu.Unlock()
	if sh == nil {
		return nil
	}
	sh.cancel()
	if tell {
		_ = p.sess.fire(proto.OpHuddleShare, proto.HuddleShareArgs{On: false})
		c.post(Update{Watch: true})
	}
	return sh
}

// Sharing describes our pane stream, if one runs.
func (c *Client) Sharing() (ShareInfo, bool) {
	p := c.curParticipation()
	if p == nil {
		return ShareInfo{}, false
	}
	p.pane.mu.Lock()
	defer p.pane.mu.Unlock()
	if p.pane.share == nil {
		return ShareInfo{}, false
	}
	return ShareInfo{Label: p.pane.share.label, Watchers: p.pane.watchers, Scrubbed: int(p.pane.share.scrubbed.Load()), Enc: p.keys != nil}, true
}

// ShareStopped is why our stream stopped by itself (the pane went away),
// once; nil otherwise.
func (c *Client) ShareStopped() error {
	p := c.curParticipation()
	if p == nil {
		return nil
	}
	p.pane.mu.Lock()
	defer p.pane.mu.Unlock()
	err := p.pane.stopped
	p.pane.stopped = nil
	return err
}

func (c *Client) shareFailed(p *participation, sh *paneShare, err error) {
	p.pane.mu.Lock()
	mine := p.pane.share == sh
	if mine {
		p.pane.share, p.pane.stopped = nil, err
	}
	p.pane.mu.Unlock()
	if mine {
		_ = p.sess.fire(proto.OpHuddleShare, proto.HuddleShareArgs{On: false})
		c.post(Update{Watch: true})
	}
}

func (ps *paneState) watcherCount() (n, gen int) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.watchers, ps.watchGen
}

func (c *Client) shareLoop(ctx context.Context, p *participation, sh *paneShare, capture Capture) {
	defer close(sh.done)
	var prev *screen.Grid
	var sc screen.Scrubber
	seen, fails := -1, 0
	interval := shareFast
	var refresh time.Time
	b := bucket{tokens: shareBurst, at: time.Now()}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-p.pane.kick:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		w, gen := p.pane.watcherCount()
		if w == 0 {
			// Nobody watches: nothing is captured or sent. A roster with a
			// watcher kicks us.
			seen, prev = -1, nil
			t.Reset(shareSlow)
			continue
		}
		force := gen != seen // someone new: a keyframe now, not at the next refresh
		seen = gen
		cctx, cancel := context.WithTimeout(ctx, captureTimeout)
		text, err := capture(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if fails++; fails >= captureFails {
				c.shareFailed(p, sh, err)
				return
			}
			t.Reset(shareSlow)
			continue
		}
		fails = 0
		g := screen.Parse(text, screen.MaxCols, screen.MaxRows)
		sh.scrubbed.Store(int64(sc.Scrub(g)))
		now := time.Now()
		if !force && prev != nil && g.Equal(prev) {
			interval = min(interval*2, shareSlow)
			if now.After(refresh) {
				refresh = now.Add(shareRefreshN)
				for _, u := range p.pane.enc.Current() {
					if !c.sendUpdate(ctx, p, u, &b) {
						break
					}
				}
			}
			t.Reset(interval)
			continue
		}
		upd, _, err := p.pane.enc.Encode(g, sh.label, force)
		if err != nil {
			c.shareFailed(p, sh, ErrShareTooBig)
			return
		}
		if c.sendUpdate(ctx, p, upd, &b) {
			prev = g
		}
		interval, refresh = shareFast, now.Add(shareRefresh1)
		t.Reset(interval)
	}
}

type bucket struct {
	tokens float64
	at     time.Time
}

// take waits for a send token; false once ctx ends.
func (b *bucket) take(ctx context.Context) bool {
	for {
		now := time.Now()
		b.tokens = min(shareBurst, b.tokens+now.Sub(b.at).Seconds()*sharePerSec)
		b.at = now
		if b.tokens >= 1 {
			b.tokens--
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration((1 - b.tokens) / sharePerSec * float64(time.Second))):
		}
	}
}

// sendUpdate chunks, seals and sends one update; false if it didn't all go
// out (stopped, keys not ready, a chunk too many).
func (c *Client) sendUpdate(ctx context.Context, p *participation, upd []byte, b *bucket) bool {
	p.pane.upd++
	chunks, err := screen.Chunks(p.pane.upd, upd)
	if err != nil {
		return false
	}
	for _, ch := range chunks {
		if ctx.Err() != nil || !b.take(ctx) { // a stop stops mid-update too
			return false
		}
		d, ok := c.sealScreen(p, ch)
		if !ok {
			return false
		}
		// On the TCP fallback a chunk waits briefly for the stream rather
		// than being dropped like voice: a lost chunk loses the update.
		for try := 0; ; try++ {
			err := p.dg.SendDatagram(d)
			if !errors.Is(err, errSendBusy) || try >= 100 {
				break
			}
			select {
			case <-ctx.Done():
				return false
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return true
}

// sealScreen builds one screen datagram for chunk (§14.5): sealed under
// the huddle's frame keys in an encrypted channel.
func (c *Client) sealScreen(p *participation, chunk []byte) ([]byte, bool) {
	if p.pane.seq == ^uint32(0) {
		return nil, false // nonces spent: never reuse one
	}
	seq := p.pane.seq
	if p.keys == nil {
		p.pane.seq++
		return proto.Datagram{Kind: proto.DgScreen, Src: p.src, Seq: seq, Payload: chunk}.Marshal(), true
	}
	epoch, s, ok := p.keys.send(frameScreen)
	if !ok {
		return nil, false
	}
	p.pane.seq++
	d := proto.Datagram{Kind: proto.DgScreenEnc, Src: p.src, Seq: seq, Epoch: epoch, TS: uint32(time.Now().Unix())}
	return s.Seal(seq, d.AppendHeader(nil), chunk), true
}

// ---- watching ----

// LiveIn is who shares a pane in ch's huddle, if anyone.
func (c *Client) LiveIn(ch uint64) (uint64, bool) {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	for _, q := range c.hud.rosters[ch] {
		if q.Live {
			return q.User, true
		}
	}
	return 0, false
}

// Watch starts showing user's shared pane; they must be live in our huddle.
func (c *Client) Watch(ctx context.Context, user uint64) error {
	p := c.curParticipation()
	if p == nil {
		return ErrNotInHuddle
	}
	if !p.watchCap() {
		return ErrNoWatchServer
	}
	live, ok := c.LiveIn(p.ch)
	if !ok || live != user {
		return ErrNotLive
	}
	// The view exists before we ask: the streamer's keyframe for us may
	// overtake our RES.
	v := newPaneView(user, c.liveSrc(p.ch))
	p.pane.mu.Lock()
	if p.pane.ended {
		p.pane.mu.Unlock()
		return ErrNotInHuddle
	}
	old := p.pane.view
	p.pane.view = v
	p.pane.mu.Unlock()
	if old != nil {
		old.close()
	}
	go c.decodeLoop(p.pane.ctx, v)
	if _, err := p.sess.call(ctx, proto.OpHuddleWatch, proto.HuddleWatchArgs{On: true}, nil); err != nil {
		p.pane.mu.Lock()
		if p.pane.view == v {
			p.pane.view = nil
		}
		p.pane.mu.Unlock()
		v.close()
		return err
	}
	c.post(Update{Watch: true})
	return nil
}

func (c *Client) liveSrc(ch uint64) uint32 {
	c.hud.mu.Lock()
	defer c.hud.mu.Unlock()
	for _, q := range c.hud.rosters[ch] {
		if q.Live {
			return q.Src
		}
	}
	return 0
}

// StopWatch stops watching; the picture is dropped at once.
func (c *Client) StopWatch() {
	p := c.curParticipation()
	if p == nil {
		return
	}
	p.pane.mu.Lock()
	v := p.pane.view
	p.pane.view = nil
	p.pane.mu.Unlock()
	if v != nil {
		v.close()
		_ = p.sess.fire(proto.OpHuddleWatch, proto.HuddleWatchArgs{On: false})
		c.post(Update{Watch: true})
	}
}

// Watching returns the pane we watch; ver is the Ver the caller already
// has: when nothing changed since, Grid is nil (no copy made).
func (c *Client) Watching(ver uint64) (WatchFrame, bool) {
	p := c.curParticipation()
	if p == nil {
		return WatchFrame{}, false
	}
	p.pane.mu.Lock()
	v := p.pane.view
	p.pane.mu.Unlock()
	if v == nil {
		return WatchFrame{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.dmu.Lock()
	defer v.dmu.Unlock()
	f := WatchFrame{User: v.user, Label: v.dec.Label(), Ver: v.ver, At: v.at, Ended: v.ended, Enc: p.keys != nil}
	if g := v.dec.Grid(); g != nil && v.ver != ver {
		f.Grid = g.Clone()
	}
	return f, true
}

// WatchStats is the current view's counters.
func (c *Client) WatchStats() (WatchStats, bool) {
	p := c.curParticipation()
	if p == nil {
		return WatchStats{}, false
	}
	p.pane.mu.Lock()
	v := p.pane.view
	p.pane.mu.Unlock()
	if v == nil {
		return WatchStats{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stats, true
}

// screenIn takes one screen datagram for p (the huddle's receive
// goroutine). Without a view it is dropped before anything is parsed, so
// not watching costs nothing.
func (c *Client) screenIn(p *participation, b []byte) {
	p.pane.mu.Lock()
	v := p.pane.view
	p.pane.mu.Unlock()
	if v == nil {
		return
	}
	v.mu.Lock()
	upd, ok := v.screenIn(p, b, time.Now())
	gen := v.gen
	v.mu.Unlock()
	if !ok {
		return
	}
	select {
	case v.in <- decItem{upd, gen}:
	default:
		v.mu.Lock()
		v.stats.BadUpdate++ // the decoder is behind: dropped
		v.mu.Unlock()
	}
}

// screenIn opens b and returns a completed update, if any; caller holds
// v.mu.
func (v *paneView) screenIn(p *participation, b []byte, now time.Time) ([]byte, bool) {
	v.stats.Received++
	d, err := proto.ParseScreenDatagram(b)
	if err != nil || (d.Kind == proto.DgScreenEnc) != (p.keys != nil) {
		v.stats.BadFrame++ // never plaintext in an encrypted huddle
		return nil, false
	}
	if d.Src != v.src || v.ended {
		v.stats.Unknown++
		return nil, false
	}
	if !v.win.check(d.Seq) {
		v.stats.Replay++
		return nil, false
	}
	chunk := d.Payload
	if p.keys != nil {
		s, ok := p.keys.recv(d.Src, d.Epoch, frameScreen)
		if !ok {
			v.stats.Key++
			return nil, false
		}
		if chunk, err = s.Open(d.Seq, b[:d.HeaderLen()], d.Payload); err != nil {
			v.stats.Auth++
			return nil, false
		}
		ts := time.Unix(int64(d.TS), 0)
		if ts.Before(now.Add(-tsWindow)) || ts.After(now.Add(tsWindow)) {
			v.stats.Skew++
			return nil, false
		}
	}
	v.win.accept(d.Seq)
	return v.asm.Add(chunk, now)
}

// tsWindow is §14.3's ±60 s, which screen chunks share.
const tsWindow = 60 * time.Second

// paneRoster follows a roster: who watches us (a newcomer gets a keyframe
// at once), and whether the stream we watch still runs.
func (c *Client) paneRoster(p *participation, parts []proto.Part) {
	watchers := 0
	set := map[uint32]bool{}
	var live *proto.Part
	for i, q := range parts {
		if q.Src != p.src && q.Watch {
			watchers++
			set[q.Src] = true
		}
		if q.Live {
			live = &parts[i]
		}
	}
	p.pane.mu.Lock()
	more := false
	for src := range set {
		if !p.pane.watchSet[src] {
			more = true
		}
	}
	if more {
		p.pane.watchGen++
	}
	changed := watchers != p.pane.watchers
	p.pane.watchers, p.pane.watchSet = watchers, set
	kick, v := p.pane.kick, p.pane.view
	p.pane.mu.Unlock()
	if more && kick != nil {
		select {
		case kick <- struct{}{}:
		default:
		}
	}
	if v != nil {
		v.mu.Lock()
		switch {
		case live != nil && live.User == v.user && (live.Src != v.src || v.ended):
			// The same person streams again (a new pane, or after a
			// rejoin): start over from their next keyframe.
			v.dmu.Lock()
			v.src, v.ended, v.ver, v.at = live.Src, false, v.ver+1, time.Time{}
			v.dec, v.asm, v.win = screen.Decoder{}, screen.Reassembler{}, replayWindow{}
			v.gen++ // updates still queued for the old stream are dropped
			v.dmu.Unlock()
			changed = true
		case (live == nil || live.User != v.user) && !v.ended:
			v.ended = true
			v.dmu.Lock()
			v.ver++
			v.dmu.Unlock()
			changed = true
		}
		v.mu.Unlock()
	}
	if changed {
		c.post(Update{Watch: true})
	}
}

// paneEnded drops p's stream and view with the participation.
func (c *Client) paneEnded(p *participation) {
	p.pane.mu.Lock()
	sh, v := p.pane.share, p.pane.view
	had := sh != nil || v != nil
	p.pane.share, p.pane.view, p.pane.ended = nil, nil, true
	p.pane.mu.Unlock()
	if v != nil {
		v.close()
	}
	if sh != nil {
		sh.cancel()
	}
	if had {
		c.post(Update{Watch: true})
	}
}

// replayWindow is a 64-chunk sliding window over a stream's seq, like
// voice's (§14.3): checked before opening, advanced only once a chunk
// opened.
type replayWindow struct {
	top  uint32
	bits uint64
	init bool
}

func (w *replayWindow) check(seq uint32) bool {
	if !w.init || seq > w.top {
		return true
	}
	d := w.top - seq
	return d < 64 && w.bits&(1<<d) == 0
}

func (w *replayWindow) accept(seq uint32) {
	switch {
	case !w.init:
		w.init, w.top, w.bits = true, seq, 1
	case seq > w.top:
		if d := seq - w.top; d >= 64 {
			w.bits = 1
		} else {
			w.bits = w.bits<<d | 1
		}
		w.top = seq
	default:
		w.bits |= 1 << (w.top - seq)
	}
}
