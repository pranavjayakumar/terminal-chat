//go:build tctamper

package server

import (
	"context"
	"database/sql"
	"sync/atomic"

	"github.com/skailama/tc/internal/proto"
)

// Malicious-server hooks for the E2E suite (internal/e2e/malicious_test.go,
// docs/E2E.md step 11). This file is compiled only with `-tags tctamper`;
// `tc server` refuses to run from such a build (cmd/tc), and release builds
// (make build) never pass the tag, so none of this exists in production.
//
// The hooks turn the real server into an attacker that holds its database
// and controls every byte it sends: it can rewrite WELCOME, rewrite, drop,
// inject and reorder any frame (EVENT, RES, SNAPSHOT, EPHEMERAL) on its way
// to one user's sessions, and change its database directly.

// TamperBuild reports whether this binary was built with the tctamper tag.
const TamperBuild = true

const tamperEnabled = true

// TamperFrame is one outgoing frame as written on the wire.
type TamperFrame struct {
	Type    proto.FrameType
	Payload []byte
}

// Tamper is a set of hooks; nil fields leave that path honest. Hooks run on
// the receiving session's writer goroutine (one per connection), so a hook
// shared by several sessions must synchronize itself.
type Tamper struct {
	// Welcome may rewrite the WELCOME sent to user.
	Welcome func(user uint64, w *proto.Welcome)
	// Out sees every frame about to be written to a session of user and
	// returns the frames to write instead: f unchanged, nothing (drop),
	// a rewrite, or several (inject; or hold frames back and return them
	// later to reorder).
	Out func(user uint64, f TamperFrame) []TamperFrame
}

type tamperState struct{ p atomic.Pointer[Tamper] }

func (t *tamperState) welcome(user uint64, w *proto.Welcome) {
	if h := t.p.Load(); h != nil && h.Welcome != nil {
		h.Welcome(user, w)
	}
}

func (t *tamperState) out(user uint64, f outFrame) []outFrame {
	h := t.p.Load()
	if h == nil || h.Out == nil {
		return []outFrame{f}
	}
	var out []outFrame
	for _, g := range h.Out(user, TamperFrame{Type: f.t, Payload: f.payload}) {
		out = append(out, outFrame{t: g.Type, payload: g.Payload})
	}
	return out
}

// SetTamper installs hooks (nil: honest again).
func (s *Server) SetTamper(t *Tamper) { s.tamper.p.Store(t) }

// TamperDB is the server's database, for direct reads and mutations. It
// bypasses the in-memory caches (membership, kinds); use TamperWrite for
// changes those must see.
func (s *Server) TamperDB() *sql.DB { return s.st.db }

// TamperWrite runs fn like a handler's write: in a transaction under the
// write lock, then the produced events update the caches and fan out (and
// pass through the Out hook like any other event).
func (s *Server) TamperWrite(ctx context.Context, fn func(tx *sql.Tx) ([]proto.Event, error)) ([]proto.Event, error) {
	return s.write(ctx, fn)
}

// TamperAppendEvent appends an event to the log inside a TamperWrite.
func (s *Server) TamperAppendEvent(ctx context.Context, tx *sql.Tx, kind string, ch, actor, about uint64, data any) (proto.Event, error) {
	return appendEvent(ctx, tx, kind, ch, actor, about, s.now(), data)
}

// TamperAddMember adds user to channel ch (membership row and chan.join
// event, no epoch bundle) inside a TamperWrite.
func (s *Server) TamperAddMember(ctx context.Context, tx *sql.Tx, ch, user uint64) (proto.Event, error) {
	return addMember(ctx, tx, s.now(), user, ch, user)
}

// TamperPush queues a frame to every live session of user, bypassing the
// log (it is not stored and other users don't see it). It does not pass
// through the Out hook.
func (s *Server) TamperPush(user uint64, f TamperFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sess := range s.sessions {
		if sess.user.ID == user {
			sess.push(outFrame{t: f.Type, payload: f.Payload})
		}
	}
}

// TamperKick closes every session of user, so its client reconnects and
// syncs again.
func (s *Server) TamperKick(user uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sess := range s.sessions {
		if sess.user.ID == user {
			sess.conn.Close()
		}
	}
}
