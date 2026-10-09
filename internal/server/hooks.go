package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/transport"
)

// Incoming webhooks (PROTOCOL.md §5 hook.*, §12). The HTTP side is in http.go.

const hooksSchema = `
CREATE TABLE IF NOT EXISTS hooks (
	id INTEGER PRIMARY KEY,
	token TEXT NOT NULL UNIQUE, -- tokenHash, like invites
	chan INTEGER NOT NULL,
	bot INTEGER NOT NULL,
	name TEXT NOT NULL,
	created_by INTEGER NOT NULL,
	created INTEGER NOT NULL
);
`

// Per-hook HTTP rate limit (PROTOCOL.md §12).
const (
	hookBurst  = 20
	hookPerSec = 1
)

// migrateHooks adds the webhook tables and the users.bot column to
// databases created before webhooks existed.
func migrateHooks(db *sql.DB) error {
	if _, err := db.Exec(hooksSchema); err != nil {
		return fmt.Errorf("creating hooks schema: %w", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'bot'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN bot INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("adding users.bot: %w", err)
		}
	}
	return nil
}

// hookState is the webhook part of the server: the HTTP certificate and
// per-hook rate limits.
type hookState struct {
	cert tls.Certificate
	pin  string
	curl string // pin in curl --pinnedpubkey form

	mu     sync.Mutex
	limits map[uint64]*bucket
}

func newHookState(dataDir string) (*hookState, error) {
	cert, err := transport.LoadOrCreateHTTPCert(dataDir)
	if err != nil {
		return nil, err
	}
	pin := transport.Pin(cert)
	curl, err := transport.CurlPin(pin)
	if err != nil {
		return nil, err
	}
	return &hookState{cert: cert, pin: pin, curl: curl, limits: map[uint64]*bucket{}}, nil
}

// HTTPCert is the certificate for webhook (HTTP) clients, for
// transport.ListenOpts.HTTPCert.
func (s *Server) HTTPCert() tls.Certificate { return s.hk.cert }

// HTTPPin is the SPKI pin of HTTPCert.
func (s *Server) HTTPPin() string { return s.hk.pin }

// allowHook takes a token from hook id's bucket, or reports how long until
// one is available.
func (s *Server) allowHook(id uint64) (bool, time.Duration) {
	s.hk.mu.Lock()
	defer s.hk.mu.Unlock()
	if s.take(s.hk.limits, id, hookBurst, hookPerSec) {
		return true, 0
	}
	b := s.hk.limits[id]
	return false, time.Duration((1 - b.tokens) / hookPerSec * float64(time.Second))
}

func newHookToken() string {
	raw := make([]byte, 16)
	rand.Read(raw)
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
}

// validHookToken checks the shape of a token before it touches the database.
func validHookToken(t string) bool {
	if len(t) != 26 {
		return false
	}
	for i := 0; i < len(t); i++ {
		if c := t[i]; !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func (s *Server) hookURL(token string) string {
	return "https://" + s.cfg.PublicAddr + proto.HookPath + token
}

func (s *Server) hookCurl(url string) string {
	return fmt.Sprintf(`curl -fsS -k --pinnedpubkey '%s' -H 'Content-Type: application/json' -d '{"text":"hello from a webhook"}' '%s'`,
		s.hk.curl, url)
}

// hookOp dispatches hook.* ops; ok is false for any other op.
func (s *Server) hookOp(ctx context.Context, u proto.User, op string, args proto.Raw) (res any, ok bool, err error) {
	switch op {
	case proto.OpHookCreate:
		res, err = withArgs(args, func(a proto.HookCreateArgs) (any, error) { return s.opHookCreate(ctx, u, a) })
	case proto.OpHookList:
		res, err = s.opHookList(ctx, u)
	case proto.OpHookDelete:
		res, err = withArgs(args, func(a proto.HookArgs) (any, error) { return s.opHookDelete(ctx, u, a) })
	default:
		return nil, false, nil
	}
	return res, true, err
}

func (s *Server) opHookCreate(ctx context.Context, u proto.User, a proto.HookCreateArgs) (any, error) {
	if !proto.ValidName(a.Name) {
		return nil, badRequest("hook name must match ^[a-z0-9][a-z0-9_-]{0,31}$")
	}
	token := newHookToken()
	var res proto.HookCreateRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		kind, ok := s.kinds[a.Chan]
		switch {
		case !ok:
			return nil, errNotFound
		case !s.members[a.Chan][u.ID]:
			return nil, errForbidden
		case kind == proto.ChanDM:
			return nil, badRequest("webhooks can't post into DMs")
		}
		now := s.now()
		var evs []proto.Event
		var botID uint64
		var isBot, disabled bool
		err := tx.QueryRowContext(ctx, `SELECT id, bot, disabled FROM users WHERE name = ?`, a.Name).Scan(&botID, &isBot, &disabled)
		switch {
		case err == nil && !isBot:
			return nil, proto.Errf(proto.ErrConflict, "that name belongs to a person")
		case err == nil && disabled:
			return nil, proto.Errf(proto.ErrConflict, "that bot is disabled")
		case errors.Is(err, sql.ErrNoRows):
			r, err := tx.ExecContext(ctx, `INSERT INTO users (name, created, bot) VALUES (?, ?, 1)`, a.Name, now)
			if err != nil {
				return nil, err
			}
			id, _ := r.LastInsertId()
			botID = uint64(id)
			ev, err := appendEvent(ctx, tx, proto.KUserNew, 0, u.ID, botID, now,
				proto.User{ID: botID, Name: a.Name, Created: now, Bot: true})
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		case err != nil:
			return nil, err
		}
		if !s.members[a.Chan][botID] {
			ev, err := addMember(ctx, tx, now, u.ID, a.Chan, botID)
			if err != nil {
				return nil, err
			}
			evs = append(evs, ev)
		}
		r, err := tx.ExecContext(ctx, `INSERT INTO hooks (token, chan, bot, name, created_by, created) VALUES (?, ?, ?, ?, ?, ?)`,
			tokenHash(token), a.Chan, botID, a.Name, u.ID, now)
		if err != nil {
			return nil, err
		}
		id, _ := r.LastInsertId()
		res.ID = uint64(id)
		return evs, nil
	})
	if err != nil {
		return nil, err
	}
	res.URL = s.hookURL(token)
	res.Curl = s.hookCurl(res.URL)
	return res, nil
}

func (s *Server) opHookList(ctx context.Context, u proto.User) (any, error) {
	rows, err := s.st.db.QueryContext(ctx, `SELECT id, chan, name, created_by, created FROM hooks ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := proto.HooksRes{Hooks: []proto.Hook{}}
	var all []proto.Hook
	for rows.Next() {
		var h proto.Hook
		if err := rows.Scan(&h.ID, &h.Chan, &h.Name, &h.CreatedBy, &h.Created); err != nil {
			return nil, err
		}
		all = append(all, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, h := range all {
		if u.Admin || s.members[h.Chan][u.ID] {
			res.Hooks = append(res.Hooks, h)
		}
	}
	s.mu.Unlock()
	return res, nil
}

func (s *Server) opHookDelete(ctx context.Context, u proto.User, a proto.HookArgs) (any, error) {
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var ch, by uint64
		err := tx.QueryRowContext(ctx, `SELECT chan, created_by FROM hooks WHERE id = ?`, a.ID).Scan(&ch, &by)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		if err != nil {
			return nil, err
		}
		// Any member can revoke a hook posting into their channel, so it
		// doesn't outlive its creator's access.
		if by != u.ID && !u.Admin && !s.members[ch][u.ID] {
			return nil, errNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM hooks WHERE id = ?`, a.ID)
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	s.hk.mu.Lock()
	delete(s.hk.limits, a.ID)
	s.hk.mu.Unlock()
	return proto.Empty{}, nil
}

// liveHook is a hook as looked up by its token.
type liveHook struct {
	id, ch, creator uint64
	bot             proto.User
}

// hookByToken returns the hook for token and its bot user.
func (s *Server) hookByToken(ctx context.Context, token string) (liveHook, error) {
	var h liveHook
	var botID uint64
	err := s.st.db.QueryRowContext(ctx, `SELECT id, chan, bot, created_by FROM hooks WHERE token = ?`,
		tokenHash(token)).Scan(&h.id, &h.ch, &botID, &h.creator)
	if err != nil {
		return h, err
	}
	h.bot, err = getUser(ctx, s.st.db, botID)
	return h, err
}

// hookLiveLocked checks, inside a write transaction, that h still exists and
// may post: its creator is enabled and still in the channel, and so is its
// bot. Anything else is not_found (HTTP 404).
func (s *Server) hookLiveLocked(ctx context.Context, tx *sql.Tx, h *liveHook) error {
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hooks WHERE id = ?`, h.id).Scan(&n); err != nil {
		return err
	}
	if n == 0 || s.disabled[h.creator] || s.disabled[h.bot.ID] ||
		!s.members[h.ch][h.creator] || !s.members[h.ch][h.bot.ID] {
		return errNotFound
	}
	return nil
}
