// Package server implements the tc chat server: authentication, the event
// log, live fan-out, and sync (docs/PROTOCOL.md).
package server

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Version is reported in WELCOME.
var Version = "tc/dev"

// Config configures a Server.
type Config struct {
	DataDir string
	// PublicAddr is the host:port put into invite codes.
	PublicAddr string
	Logger     *slog.Logger
	// Now is the clock; nil means time.Now. Tests override it.
	Now func() time.Time
	// MemberInvites lets non-admins create, list and revoke their own
	// invites, within proto.MaxMember* limits (cap member_invites).
	MemberInvites bool
}

// Server is a running tc server.
type Server struct {
	cfg  Config
	st   *store
	cert tls.Certificate
	pin  string
	log  *slog.Logger
	hk   *hookState
	hud  *huddles
	// tamper holds the malicious-server test hooks; an empty struct unless
	// built with -tags tctamper (tamper_on.go).
	tamper tamperState

	// mu serializes every write transaction together with its fan-out, so
	// sessions observe events in seq order and sync can take an exact head.
	mu       sync.Mutex
	sessions map[*session]struct{}
	members  map[uint64]map[uint64]bool // chan -> current members
	kinds    map[uint64]string          // chan -> kind
	online   map[uint64]int             // user -> live session count
	presence map[uint64]proto.Ephemeral // user -> last presence
	limits   map[uint64]*bucket         // sends
	wlimits  map[uint64]*bucket         // other write ops
	disabled map[uint64]bool
	// Agents (agents.go): owner → active agent, access list, agent DM,
	// connected agent sessions; agent sessions' own rate buckets.
	agents      map[uint64]proto.Agent
	agentAccess map[uint64]map[uint64]bool
	agentDMs    map[uint64]uint64
	agentOnline map[uint64]int
	alimits     map[uint64]*bucket
	awlimits    map[uint64]*bucket

	remindWake chan struct{} // nudges the reminder scheduler
}

// New opens (or creates) the data directory: database and TLS identity.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PublicAddr != "" {
		if err := ValidPublicAddr(cfg.PublicAddr); err != nil {
			return nil, err
		}
		if host, _, _ := net.SplitHostPort(cfg.PublicAddr); host == "" {
			cfg.Logger.Warn("public address has no host; webhook URLs will use localhost. Pass --public host:port so teammates can reach them",
				"public", cfg.PublicAddr)
		}
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data dir: %w", err)
	}
	// The database holds every DM; MkdirAll doesn't tighten an existing dir.
	if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(cfg.DataDir, "tc.db")
	st, err := openStore(dbPath)
	if err != nil {
		return nil, err
	}
	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !os.IsNotExist(err) {
			st.close()
			return nil, err
		}
	}
	cert, err := transport.LoadOrCreateCert(cfg.DataDir)
	if err != nil {
		st.close()
		return nil, err
	}
	s := &Server{
		cfg: cfg, st: st, cert: cert, pin: transport.Pin(cert), log: cfg.Logger,
		sessions:   map[*session]struct{}{},
		members:    map[uint64]map[uint64]bool{},
		kinds:      map[uint64]string{},
		online:     map[uint64]int{},
		presence:   map[uint64]proto.Ephemeral{},
		limits:     map[uint64]*bucket{},
		wlimits:    map[uint64]*bucket{},
		disabled:   map[uint64]bool{},
		agents:     map[uint64]proto.Agent{}, agentAccess: map[uint64]map[uint64]bool{},
		agentDMs: map[uint64]uint64{}, agentOnline: map[uint64]int{},
		alimits: map[uint64]*bucket{}, awlimits: map[uint64]*bucket{},
		remindWake: make(chan struct{}, 1),
		hud:        newHuddles(),
	}
	if s.hk, err = newHookState(cfg.DataDir, func() time.Time { return s.cfg.Now() }); err != nil {
		st.close()
		return nil, err
	}
	if err := s.loadCache(); err != nil {
		st.close()
		return nil, err
	}
	if err := s.ensureGeneral(); err != nil {
		st.close()
		return nil, err
	}
	return s, nil
}

// Cert is the server's TLS certificate, for transport.Listen.
func (s *Server) Cert() tls.Certificate { return s.cert }

// Pin is the SPKI pin clients verify.
func (s *Server) Pin() string { return s.pin }

// Close closes the database. Call after Serve returns.
func (s *Server) Close() error { return s.st.close() }

func (s *Server) now() int64 { return s.cfg.Now().UnixMilli() }

func (s *Server) loadCache() error {
	ctx := context.Background()
	rows, err := s.st.db.QueryContext(ctx, `SELECT id, kind FROM channels`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uint64
		var kind string
		if err := rows.Scan(&id, &kind); err != nil {
			rows.Close()
			return err
		}
		s.kinds[id] = kind
		s.members[id] = map[uint64]bool{}
	}
	rows.Close()
	rows, err = s.st.db.QueryContext(ctx, `SELECT id FROM users WHERE disabled = 1`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		s.disabled[id] = true
	}
	rows.Close()
	if err := s.loadAgents(ctx); err != nil {
		return err
	}
	rows, err = s.st.db.QueryContext(ctx, `SELECT chan, user FROM membership WHERE leave_seq IS NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c, u uint64
		if err := rows.Scan(&c, &u); err != nil {
			return err
		}
		if s.members[c] != nil {
			s.members[c][u] = true
		}
	}
	return rows.Err()
}

// ensureGeneral creates the default #general channel on a fresh server.
func (s *Server) ensureGeneral() error {
	var n int
	if err := s.st.db.QueryRow(`SELECT COUNT(*) FROM channels WHERE kind != 'dm'`).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := s.write(context.Background(), func(tx *sql.Tx) ([]proto.Event, error) {
		_, ev, err := createChannel(context.Background(), tx, s.now(), 0, "general", proto.ChanPublic, "", nil, false)
		return []proto.Event{ev}, err
	})
	return err
}

// write runs fn in a transaction while holding the write lock, then updates
// the membership cache and fans out the produced events in seq order.
func (s *Server) write(ctx context.Context, fn func(tx *sql.Tx) ([]proto.Event, error)) ([]proto.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.st.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	evs, err := fn(tx)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		if ctx.Err() != nil {
			// The context ended after fn: database/sql rolled the
			// transaction back itself, so Commit only says sql.ErrTxDone.
			return nil, ctx.Err()
		}
		return nil, err
	}
	for _, ev := range evs {
		s.applyCache(ev)
		s.publishLocked(ev)
		s.huddleEventLocked(ev)
	}
	return evs, nil
}

func (s *Server) applyCache(ev proto.Event) {
	s.applyAgentCache(ev)
	switch ev.Kind {
	case proto.KChanNew:
		var c proto.Channel
		if proto.Unmarshal(ev.Data, &c) == nil {
			s.kinds[c.ID] = c.Kind
			s.members[c.ID] = map[uint64]bool{}
			for _, u := range c.Members {
				s.members[c.ID][u] = true
			}
		}
	case proto.KChanJoin:
		var j proto.ChanJoin
		if proto.Unmarshal(ev.Data, &j) == nil && s.members[ev.Chan] != nil {
			s.members[ev.Chan][j.User] = true
		}
	case proto.KChanLeave:
		var r proto.UserRef
		if proto.Unmarshal(ev.Data, &r) == nil && s.members[ev.Chan] != nil {
			delete(s.members[ev.Chan], r.User)
		}
	}
}

// visibleLocked implements PROTOCOL.md §6 Visibility for live fan-out, using
// membership after the event was applied. Keep in step with visibleEvents;
// e2e-only kinds are filtered by publishLocked.
func (s *Server) visibleLocked(ev proto.Event, u uint64) bool {
	switch ev.Kind {
	case proto.KUserNew, proto.KUserUpdate, proto.KGroupUpdate, proto.KDeviceNew, proto.KDeviceRevoke, proto.KAgentUpdate:
		return true
	case proto.KReadUpdate, proto.KAgentAccess:
		return ev.Actor == u
	case proto.KChanJoin, proto.KChanLeave:
		var r proto.UserRef
		if proto.Unmarshal(ev.Data, &r) == nil && r.User == u {
			return true
		}
	case proto.KChanNew, proto.KChanUpdate:
		if s.kinds[ev.Chan] == proto.ChanPublic {
			return true
		}
	}
	return ev.Chan != 0 && s.members[ev.Chan][u]
}

func (s *Server) publishLocked(ev proto.Event) {
	payload, err := proto.Marshal(ev)
	if err != nil {
		s.log.Error("encoding event", "seq", ev.Seq, "err", err)
		return
	}
	e2eOnly, agentOnly, msg := isE2EKind(ev.Kind), proto.IsAgentKind(ev.Kind), proto.IsMsgKind(ev.Kind)
	for sess := range s.sessions {
		if e2eOnly && !sess.e2e || agentOnly && !sess.agentCap || msg && !s.agentSeesLocked(sess, ev.Chan) {
			continue
		}
		if s.visibleLocked(ev, sess.user.ID) {
			sess.pushLive(outFrame{t: proto.TEvent, payload: payload, seq: ev.Seq})
		}
	}
}

// ephemeralLocked fans an ephemeral frame out to the sessions of users in to
// (nil = everyone), skipping the session it came from.
func (s *Server) ephemeralLocked(e proto.Ephemeral, to map[uint64]bool, from *session) {
	payload, err := proto.Marshal(e)
	if err != nil {
		return
	}
	for sess := range s.sessions {
		if sess == from || (to != nil && !to[sess.user.ID]) || !s.agentSeesLocked(sess, e.Chan) {
			continue
		}
		sess.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
	}
}

// Serve accepts connections until ctx is cancelled or ln fails.
func (s *Server) Serve(ctx context.Context, ln transport.Listener) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	// Deferred after wg.Wait, so it runs first: the scheduler stops even when
	// Serve returns because the listener failed, not because ctx ended.
	rctx, stopReminders := context.WithCancel(ctx)
	defer stopReminders()
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.remindLoop(rctx)
	}()
	if hl := transport.HTTPListener(ln); hl != nil {
		defer s.serveHTTP(ctx, hl)()
	}
	var connMu sync.Mutex
	conns := map[transport.Conn]struct{}{}
	stopped := false
	go func() {
		<-ctx.Done()
		ln.Close()
		s.mu.Lock()
		for sess := range s.sessions {
			sess.bye(proto.ByeServerShutdown, "server shutting down")
		}
		s.mu.Unlock()
		// Connections still in the handshake aren't sessions yet.
		time.Sleep(byeGrace)
		connMu.Lock()
		stopped = true
		for c := range conns {
			c.CloseWithCode(proto.ByeServerShutdown)
		}
		connMu.Unlock()
	}()
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		connMu.Lock()
		if stopped {
			connMu.Unlock()
			conn.Close()
			continue
		}
		conns[conn] = struct{}{}
		connMu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(ctx, conn)
			connMu.Lock()
			delete(conns, conn)
			connMu.Unlock()
		}()
	}
}

// tokenHash is what the database stores, so lookups don't compare secrets
// byte-by-byte and a leaked db doesn't leak usable invites.
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bucket is a token-bucket rate limiter for sends (PROTOCOL.md §9).
type bucket struct {
	tokens float64
	last   time.Time
}

// Sends: 30 burst, refilling 3/s. Other write ops (edit, react, channel
// changes, read markers) share a separate, looser bucket.
func (s *Server) allowSend(u uint64) bool  { return s.take(s.limits, u, 30, 3) }
func (s *Server) allowWrite(u uint64) bool { return s.take(s.wlimits, u, 60, 5) }

// An agent session spends its own buckets, so a busy agent never slows its
// owner down.
func (s *Server) allowSendSess(sess *session, u uint64) bool {
	if sess != nil && sess.agent != nil {
		return s.take(s.alimits, u, 30, 3)
	}
	return s.allowSend(u)
}

func (s *Server) allowWriteSess(sess *session) bool {
	if sess.agent != nil {
		return s.take(s.awlimits, sess.user.ID, 60, 5)
	}
	return s.allowWrite(sess.user.ID)
}

func (s *Server) take(m map[uint64]*bucket, u uint64, burst, perSec float64) bool {
	now := s.cfg.Now()
	b := m[u]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		m[u] = b
	}
	b.tokens = min(burst, b.tokens+now.Sub(b.last).Seconds()*perSec)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// UserCount returns the number of registered users.
func (s *Server) UserCount() int {
	var n int
	s.st.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// SetPublicAddr changes the host:port written into new invite codes.
func (s *Server) SetPublicAddr(addr string) { s.cfg.PublicAddr = addr }
