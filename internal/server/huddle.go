package server

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Voice huddles (PROTOCOL.md §14, docs/VOICE.md): ephemeral rosters plus a
// datagram relay that forwards bytes it never decodes.

const (
	dgQueueLen  = 64 // per receiving session; full → drop
	dgBurst     = 100
	dgPerSecond = 60
)

// dgItem is a datagram queued for one participation.
type dgItem struct {
	b  []byte
	to *participant
}

// voiceOps exist only in sessions that negotiated proto.CapVoice.
var voiceOps = map[string]bool{proto.OpHuddleJoin: true, proto.OpHuddleLeave: true}

type participant struct {
	sess        *session
	src         uint32
	dev, stream []byte
	h           *huddle
}

type huddle struct {
	ch    uint64
	enc   bool
	parts []*participant // join order
}

// huddles has its own lock so the datagram hot path never waits on a write
// transaction. Lock order: Server.mu, then huddles.mu.
type huddles struct {
	mu      sync.RWMutex
	byChan  map[uint64]*huddle
	bySess  map[*session]*participant
	nextSrc uint32

	relayed, dropped atomic.Uint64
}

func newHuddles() *huddles {
	return &huddles{byChan: map[uint64]*huddle{}, bySess: map[*session]*participant{}}
}

// removeLocked ends p's participation and returns the huddle's channel.
func (hs *huddles) removeLocked(p *participant) uint64 {
	h := p.h
	h.parts = slices.DeleteFunc(h.parts, func(q *participant) bool { return q == p })
	if len(h.parts) == 0 {
		delete(hs.byChan, h.ch)
	}
	delete(hs.bySess, p.sess)
	return h.ch
}

func (hs *huddles) roster(ch uint64) []proto.Part {
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	h := hs.byChan[ch]
	if h == nil {
		return nil
	}
	out := make([]proto.Part, len(h.parts))
	for i, p := range h.parts {
		out[i] = proto.Part{User: p.sess.user.ID, Src: p.src, Dev: p.dev, Stream: p.stream}
	}
	return out
}

// changed records a roster change: chan → users whose participation ended
// there (they get the roster even if no longer members).
type changed map[uint64]map[uint64]bool

func (c changed) add(ch, user uint64) {
	if c[ch] == nil {
		c[ch] = map[uint64]bool{}
	}
	if user != 0 {
		c[ch][user] = true
	}
}

// dropWhere ends every participation matching keep == false.
func (hs *huddles) dropWhere(match func(*participant) bool) changed {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	c := changed{}
	for _, p := range hs.bySess {
		if match(p) {
			c.add(hs.removeLocked(p), p.sess.user.ID)
		}
	}
	return c
}

// broadcastLocked sends each changed channel's roster to its members' and
// the listed users' voice sessions. Caller holds Server.mu.
func (s *Server) broadcastHuddlesLocked(c changed) {
	for ch, extra := range c {
		payload, err := proto.Marshal(proto.Ephemeral{Kind: proto.EphHuddle, Chan: ch, Parts: s.hud.roster(ch)})
		if err != nil {
			continue
		}
		// Someone who just lost access learns only that they're out, not
		// who is still talking (§14.2).
		gone, err := proto.Marshal(proto.Ephemeral{Kind: proto.EphHuddle, Chan: ch})
		if err != nil {
			continue
		}
		for sess := range s.sessions {
			switch {
			case !sess.voice:
			case s.members[ch][sess.user.ID]:
				sess.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
			case extra[sess.user.ID]:
				sess.pushLive(outFrame{t: proto.TEphemeral, payload: gone})
			}
		}
	}
}

func (s *Server) opHuddleJoin(ctx context.Context, sess *session, a proto.HuddleJoinArgs) (any, error) {
	if sess.dg == nil {
		return nil, proto.Errf(proto.ErrBadRequest, "voice needs UDP")
	}
	u := sess.user.ID
	// Honest-server device check outside the hub lock; a revoke that races
	// it ends the participation through huddleEventLocked.
	devOK := false
	if len(a.Dev) == 16 {
		var one int
		err := s.st.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE dev = ? AND user = ? AND revoked IS NULL`, a.Dev, u).Scan(&one)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		devOK = err == nil
	}
	// Under Server.mu, membership and the e2e flag can't change underneath.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.disabled[u] {
		// Disabled after dispatch checked: its sweep already ran.
		return nil, proto.Errf(proto.ErrUnauthorized, "account disabled")
	}
	mem, ok := s.members[a.Chan]
	if !ok {
		return nil, errNotFound
	}
	if !mem[u] {
		return nil, errForbidden
	}
	var enc bool
	if err := s.st.db.QueryRowContext(ctx, `SELECT e2e FROM channels WHERE id = ?`, a.Chan).Scan(&enc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	dev, stream := a.Dev, a.Stream
	if enc {
		if len(dev) != 16 || len(stream) != proto.VoiceStreamSize {
			return nil, proto.Errf(proto.ErrBadRequest, "encrypted huddles need dev and stream")
		}
		if devOK {
			// Re-check under the lock: a revoke that committed (and swept)
			// since the fast check above must not be let in.
			var one int
			err := s.st.db.QueryRowContext(ctx, `SELECT 1 FROM devices WHERE dev = ? AND user = ? AND revoked IS NULL`, dev, u).Scan(&one)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			devOK = err == nil
		}
		if !devOK {
			return nil, errForbidden
		}
		dev, stream = slices.Clone(dev), slices.Clone(stream)
	} else {
		dev, stream = nil, nil
	}

	hs := s.hud
	hs.mu.Lock()
	h := hs.byChan[a.Chan]
	mine := func(p *participant) bool { return p.sess.user.ID == u }
	switch {
	case h == nil && len(hs.byChan) >= proto.MaxHuddles:
		// Leaving our own single-person huddle elsewhere would free a slot.
		freed := false
		for _, p := range hs.bySess {
			if mine(p) && len(p.h.parts) == 1 {
				freed = true
			}
		}
		if !freed {
			hs.mu.Unlock()
			return nil, proto.Errf(proto.ErrConflict, "too many huddles")
		}
	case h != nil && len(h.parts)-countFunc(h.parts, mine) >= proto.MaxHuddleParts:
		hs.mu.Unlock()
		return nil, proto.Errf(proto.ErrConflict, "huddle is full")
	}
	c := changed{}
	for _, p := range hs.bySess {
		if mine(p) {
			c.add(hs.removeLocked(p), u)
		}
	}
	if h = hs.byChan[a.Chan]; h == nil {
		h = &huddle{ch: a.Chan, enc: enc}
		hs.byChan[a.Chan] = h
	}
	if hs.nextSrc == ^uint32(0) {
		hs.mu.Unlock()
		return nil, proto.Errf(proto.ErrConflict, "out of huddle ids until the server restarts")
	}
	hs.nextSrc++
	p := &participant{sess: sess, src: hs.nextSrc, dev: dev, stream: stream, h: h}
	h.parts = append(h.parts, p)
	hs.bySess[sess] = p
	hs.mu.Unlock()
	c.add(a.Chan, 0)
	s.broadcastHuddlesLocked(c)
	return proto.HuddleJoinRes{Src: p.src}, nil
}

func countFunc[T any](s []T, f func(T) bool) int {
	n := 0
	for _, v := range s {
		if f(v) {
			n++
		}
	}
	return n
}

func (s *Server) opHuddleLeave(sess *session) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.huddleGoneLocked(sess)
	return proto.Empty{}, nil
}

// huddleGoneLocked ends sess's participation (leave, disconnect).
func (s *Server) huddleGoneLocked(sess *session) {
	if !sess.voice {
		return
	}
	s.broadcastHuddlesLocked(s.hud.dropWhere(func(p *participant) bool { return p.sess == sess }))
}

// huddleEventLocked keeps huddles in step with membership (PROTOCOL.md
// §14.1): runs under Server.mu right after the cache applied ev, so no
// datagram flows to or from someone who just lost access.
func (s *Server) huddleEventLocked(ev proto.Event) {
	switch ev.Kind {
	case proto.KChanLeave:
		var r proto.UserRef
		if proto.Unmarshal(ev.Data, &r) != nil {
			return
		}
		s.broadcastHuddlesLocked(s.hud.dropWhere(func(p *participant) bool {
			return p.h.ch == ev.Chan && p.sess.user.ID == r.User
		}))
	case proto.KChanJoin:
		var j proto.ChanJoin
		if proto.Unmarshal(ev.Data, &j) != nil {
			return
		}
		if parts := s.hud.roster(ev.Chan); parts != nil {
			s.sendRosterLocked(j.User, ev.Chan, parts)
		}
	case proto.KChanUpdate:
		// A channel that became encrypted ends its plaintext huddle: everyone
		// rejoins with device keys.
		var c proto.Channel
		if proto.Unmarshal(ev.Data, &c) != nil || !c.E2E {
			return
		}
		s.broadcastHuddlesLocked(s.hud.dropWhere(func(p *participant) bool {
			return p.h.ch == ev.Chan && !p.h.enc
		}))
	case proto.KDeviceRevoke:
		// The revoked device's participation ends at once (receivers would
		// drop its frames anyway, once their key cache expires).
		// Query outside hud.mu so the relay never waits on the database.
		s.hud.mu.RLock()
		var devs [][]byte
		for _, p := range s.hud.bySess {
			if p.dev != nil {
				devs = append(devs, p.dev)
			}
		}
		s.hud.mu.RUnlock()
		revoked := map[string]bool{}
		for _, d := range devs {
			var r bool
			if s.st.db.QueryRow(`SELECT revoked IS NOT NULL FROM devices WHERE dev = ?`, d).Scan(&r) == nil && r {
				revoked[string(d)] = true
			}
		}
		if len(revoked) > 0 {
			s.broadcastHuddlesLocked(s.hud.dropWhere(func(p *participant) bool { return revoked[string(p.dev)] }))
		}
	case proto.KUserUpdate:
		var u proto.User
		if proto.Unmarshal(ev.Data, &u) != nil || !u.Disabled {
			return
		}
		s.broadcastHuddlesLocked(s.hud.dropWhere(func(p *participant) bool { return p.sess.user.ID == u.ID }))
	}
}

func (s *Server) sendRosterLocked(user, ch uint64, parts []proto.Part) {
	payload, err := proto.Marshal(proto.Ephemeral{Kind: proto.EphHuddle, Chan: ch, Parts: parts})
	if err != nil {
		return
	}
	for sess := range s.sessions {
		if sess.voice && sess.user.ID == user {
			sess.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

// sendHuddles gives a freshly synced session every live roster it may see.
func (s *Server) sendHuddles(sess *session) {
	if !sess.voice {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hud.mu.RLock()
	chans := make([]uint64, 0, len(s.hud.byChan))
	for ch := range s.hud.byChan {
		if s.members[ch][sess.user.ID] {
			chans = append(chans, ch)
		}
	}
	s.hud.mu.RUnlock()
	slices.Sort(chans)
	for _, ch := range chans {
		if payload, err := proto.Marshal(proto.Ephemeral{Kind: proto.EphHuddle, Chan: ch, Parts: s.hud.roster(ch)}); err == nil {
			sess.push(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

// ---- datagram relay ----

// startDatagrams runs the session's datagram reader and writer until ctx
// ends or the session closes.
func (sess *session) startDatagrams(ctx context.Context, dg transport.DatagramConn) {
	sess.dg = dg
	sess.dgOut = make(chan dgItem, dgQueueLen)
	hs := sess.srv.hud
	go func() {
		for {
			select {
			case it := <-sess.dgOut:
				// Queued for a participation that has since ended (left,
				// removed, revoked): never deliver it (§14.1).
				hs.mu.RLock()
				live := hs.bySess[sess] == it.to
				hs.mu.RUnlock()
				if live {
					// Errors are a closing connection or a datagram that no
					// longer fits the path MTU; voice just loses that frame.
					_ = dg.SendDatagram(it.b)
				}
			case <-sess.done:
				return
			}
		}
	}()
	go func() {
		for {
			b, err := dg.ReceiveDatagram(ctx)
			if err != nil {
				return
			}
			sess.srv.relay(sess, b)
		}
	}()
}

// relay forwards one datagram to the sender's huddle (PROTOCOL.md §14.3).
// Only the reader goroutine of sess calls it, so sess.dgBucket needs no lock.
func (s *Server) relay(sess *session, b []byte) {
	hs := s.hud
	kind, src, err := proto.RelayHeader(b)
	if err != nil {
		hs.dropped.Add(1)
		return
	}
	now := s.cfg.Now()
	hs.mu.RLock()
	defer hs.mu.RUnlock()
	p := hs.bySess[sess]
	if p == nil || p.src != src || (kind == proto.DgAudioEnc) != p.h.enc || !takeBucket(&sess.dgBucket, now, dgBurst, dgPerSecond) {
		hs.dropped.Add(1)
		return
	}
	for _, q := range p.h.parts {
		if q == p {
			continue
		}
		select {
		case q.sess.dgOut <- dgItem{b, q}:
			hs.relayed.Add(1)
		default:
			hs.dropped.Add(1)
		}
	}
}

func takeBucket(b *bucket, now time.Time, burst, perSec float64) bool {
	if b.last.IsZero() {
		b.tokens, b.last = burst, now
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
