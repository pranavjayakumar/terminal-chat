package server

import (
	"slices"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Pane sharing in a huddle (PROTOCOL.md §14.5): one participant per huddle
// streams a terminal pane as screen datagrams, relayed unchanged to the
// participants who asked to watch. Like voice, the server stores nothing
// and, in encrypted channels, can't read or forge what it relays.

// watchOps exist only in sessions that negotiated proto.CapWatch.
var watchOps = map[string]bool{proto.OpHuddleShare: true, proto.OpHuddleWatch: true}

// Screen relay limits, per session: their own token bucket (a keyframe's
// burst must not eat voice's), a queue apart from voice's, and on the
// stream a short queue of chunks written after chat and voice.
const (
	scBurst     = 96
	scPerSecond = 48
	scQueueLen  = 128
	// Stream side: chunks may wait longer than voice (a late picture is
	// still a picture), but not forever.
	scStreamLen = 64
	scMaxAge    = 2 * time.Second
)

// watchOn: turning sharing or watching on counts as a write op (§9);
// turning it off never does, so stopping a stream is never refused.
func watchOn(op string, args proto.Raw) bool {
	if !watchOps[op] {
		return false
	}
	var a struct {
		On bool `msgpack:"on"`
	}
	return proto.Unmarshal(args, &a) != nil || a.On
}

func (s *Server) opHuddleShare(sess *session, on bool) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := s.hud
	hs.mu.Lock()
	p := hs.bySess[sess]
	switch {
	case p == nil && !on:
		hs.mu.Unlock()
		return proto.Empty{}, nil
	case p == nil:
		hs.mu.Unlock()
		return nil, proto.Errf(proto.ErrBadRequest, "not in a huddle")
	case on && p.h.live != nil && p.h.live != p:
		hs.mu.Unlock()
		return nil, proto.Errf(proto.ErrConflict, "someone in this huddle is already sharing a pane")
	case on == (p.h.live == p):
		hs.mu.Unlock()
		return proto.Empty{}, nil
	}
	if on {
		p.h.live = p
	} else {
		p.h.live = nil
	}
	p.shareGen++
	ch := p.h.ch
	hs.mu.Unlock()
	s.broadcastHuddlesLocked(changed{ch: nil})
	return proto.Empty{}, nil
}

func (s *Server) opHuddleWatch(sess *session, on bool) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	hs := s.hud
	hs.mu.Lock()
	p := hs.bySess[sess]
	switch {
	case p == nil && !on:
		hs.mu.Unlock()
		return proto.Empty{}, nil
	case p == nil:
		hs.mu.Unlock()
		return nil, proto.Errf(proto.ErrBadRequest, "not in a huddle")
	case p.watch == on:
		hs.mu.Unlock()
		return proto.Empty{}, nil
	}
	p.watch = on
	p.watchGen++
	ch := p.h.ch
	hs.mu.Unlock()
	// The roster tells the streamer someone started watching, so it sends
	// a keyframe at once instead of at its next refresh.
	s.broadcastHuddlesLocked(changed{ch: nil})
	return proto.Empty{}, nil
}

// relayScreen forwards one screen chunk from p (§14.5): only from the
// huddle's live participant, within its own bucket, to watchers. Caller
// holds hs.mu (read) and checked src and kind.
func relayScreen(hs *huddles, sess *session, p *participant, b []byte, now time.Time) {
	if p.h.live != p || !takeBucket(&sess.scBucket, now, scBurst, scPerSecond) {
		hs.dropped.Add(1)
		return
	}
	for _, q := range p.h.parts {
		if q == p || !q.watch || q.sess.scOut == nil {
			continue
		}
		select {
		case q.sess.scOut <- dgItem{b: b, from: p, to: q, shareGen: p.shareGen, watchGen: q.watchGen}:
			hs.relayed.Add(1)
		default:
			hs.dropped.Add(1)
		}
	}
}

// outgoing is what goes out for one relayed datagram: b itself, or what
// the tamper hook (tctamper test builds only) makes of it.
func (sess *session) outgoing(b []byte, one *[1][]byte) [][]byte {
	if tamperEnabled {
		return sess.srv.tamper.datagram(sess.user.ID, b)
	}
	one[0] = b
	return one[:]
}

// startScreen runs sess's screen writer: chunks go out as datagrams, or
// on the stream behind chat and voice (writeScreen).
func (sess *session) startScreen(dg transport.DatagramConn) {
	sess.scOut = make(chan dgItem, scQueueLen)
	hs := sess.srv.hud
	go func() {
		for {
			select {
			case it := <-sess.scOut:
				// Never deliver a chunk once its stream stopped or the
				// viewer stopped watching (or either left).
				hs.mu.RLock()
				live := screenLive(hs, sess, it)
				hs.mu.RUnlock()
				if !live {
					hs.dropped.Add(1)
					continue
				}
				var one [1][]byte
				for _, b := range sess.outgoing(it.b, &one) {
					it.b = b
					if dg == nil {
						if !sess.pushScreen(it) {
							hs.dropped.Add(1)
						}
					} else {
						_ = dg.SendDatagram(it.b)
					}
				}
			case <-sess.done:
				return
			}
		}
	}()
}

// screenLive: it may still go to sess: both participations, the stream
// and the watch it was queued for still run (a stop then restart in
// between discards it). Caller holds hs.mu (read).
func screenLive(hs *huddles, sess *session, it dgItem) bool {
	return hs.bySess[sess] == it.to && hs.bySess[it.from.sess] == it.from &&
		it.from.h.live == it.from && it.to.watch &&
		it.from.shareGen == it.shareGen && it.to.watchGen == it.watchGen
}

// pushScreen queues a chunk for sess's stream writer; false = dropped.
func (sess *session) pushScreen(it dgItem) bool {
	now := time.Now()
	if w := sess.writing.Load(); w != 0 && now.UnixNano()-w > int64(sess.srv.hud.stall) {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.closed || !sess.live || sess.queued >= voiceBackedUp {
		return false
	}
	if len(sess.sq) >= scStreamLen {
		sess.sq = slices.Delete(sess.sq, 0, 1)
		sess.srv.hud.dropped.Add(1)
	}
	sess.sq = append(sess.sq, voiceOut{it, now})
	sess.signal()
	return true
}

// writeScreen writes the chunks the writer took, after chat and voice:
// when either arrives meanwhile, the rest waits for the next round. False
// on a write error.
func (sess *session) writeScreen(sq []voiceOut) bool {
	hs := sess.srv.hud
	for i, v := range sq {
		sess.mu.Lock()
		if len(sess.queue) > 0 || len(sess.vq) > 0 {
			sess.sq = append(sq[i:len(sq):len(sq)], sess.sq...)
			if n := len(sess.sq) - scStreamLen; n > 0 {
				sess.sq = sess.sq[n:]
				hs.dropped.Add(uint64(n))
			}
			sess.signal()
			sess.mu.Unlock()
			return true
		}
		sess.mu.Unlock()
		hs.mu.RLock()
		live := screenLive(hs, sess, v.it)
		hs.mu.RUnlock()
		if !live || time.Since(v.at) > scMaxAge || transport.Backlogged(sess.conn) {
			hs.dropped.Add(1)
			continue
		}
		sess.writing.Store(time.Now().UnixNano())
		err := sess.codec.WriteRaw(proto.TVoice, v.it.b)
		sess.writing.Store(0)
		if err != nil {
			return false
		}
	}
	return true
}
