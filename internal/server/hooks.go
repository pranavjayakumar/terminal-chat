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
	-- AUTOINCREMENT: a deleted hook's id is never reused, so its per-hook
	-- rate-limit state can't carry over to a new hook.
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token TEXT NOT NULL UNIQUE, -- tokenHash, like invites
	chan INTEGER NOT NULL,
	bot INTEGER NOT NULL,
	name TEXT NOT NULL,
	created_by INTEGER NOT NULL,
	created INTEGER NOT NULL
);
`

// Webhook limits (PROTOCOL.md §5 hook.create, §12).
const (
	hookBurst  = 20 // per hook
	hookPerSec = 1
	// Every hook send across the server, so many hooks can't add up to a
	// flood that starves people on the single writer.
	hookGlobalBurst  = 50
	hookGlobalPerSec = 10

	maxHooksPerUser = 10
	maxHooksPerChan = 20
	maxBots         = 100
)

// migrateHooks adds the webhook tables and the users.bot and users.bot_owner
// columns to databases created before they existed.
func migrateHooks(db *sql.DB) error {
	if _, err := db.Exec(hooksSchema); err != nil {
		return fmt.Errorf("creating hooks schema: %w", err)
	}
	if err := hooksAutoincrement(db); err != nil {
		return err
	}
	if err := addColumn(db, "users", "bot", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	var had int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = 'bot_owner'`).Scan(&had); err != nil {
		return err
	}
	if err := addColumn(db, "users", "bot_owner", `INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if had == 0 {
		// Bots made before owners existed belong to whoever made their first hook.
		if _, err := db.Exec(`UPDATE users SET bot_owner = COALESCE(
			(SELECT created_by FROM hooks WHERE hooks.bot = users.id ORDER BY id LIMIT 1), 0) WHERE bot = 1`); err != nil {
			return fmt.Errorf("backfilling users.bot_owner: %w", err)
		}
	}
	return nil
}

// hooksAutoincrement rebuilds a hooks table created without AUTOINCREMENT.
func hooksAutoincrement(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'hooks'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT") {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`ALTER TABLE hooks RENAME TO hooks_old`,
		hooksSchema,
		`INSERT INTO hooks (id, token, chan, bot, name, created_by, created)
			SELECT id, token, chan, bot, name, created_by, created FROM hooks_old`,
		`DROP TABLE hooks_old`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migrating hooks to AUTOINCREMENT: %w", err)
		}
	}
	return tx.Commit()
}

func addColumn(db *sql.DB, table, col, def string) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + def); err != nil {
			return fmt.Errorf("adding %s.%s: %w", table, col, err)
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
	limits map[uint64]*bucket // per hook id
	global map[uint64]*bucket // one bucket, key 0
	ips    *ipLimiter
	conns  connCap
}

func newHookState(dataDir string, now func() time.Time) (*hookState, error) {
	cert, err := transport.LoadOrCreateHTTPCert(dataDir)
	if err != nil {
		return nil, err
	}
	pin := transport.Pin(cert)
	curl, err := transport.CurlPin(pin)
	if err != nil {
		return nil, err
	}
	return &hookState{
		cert: cert, pin: pin, curl: curl,
		limits: map[uint64]*bucket{}, global: map[uint64]*bucket{},
		ips:   newIPLimiter(now, ipBurst, ipPerSec, ipMaxEntries),
		conns: connCap{max: maxHTTPConns},
	}, nil
}

// HTTPCert is the certificate for webhook (HTTP) clients, for
// transport.ListenOpts.HTTPCert.
func (s *Server) HTTPCert() tls.Certificate { return s.hk.cert }

// HTTPPin is the SPKI pin of HTTPCert.
func (s *Server) HTTPPin() string { return s.hk.pin }

// allowHook takes a token from hook id's bucket and the global hook bucket,
// or reports how long until one is available.
func (s *Server) allowHook(id uint64) (bool, time.Duration) {
	s.hk.mu.Lock()
	defer s.hk.mu.Unlock()
	if !s.take(s.hk.limits, id, hookBurst, hookPerSec) {
		return false, wait(s.hk.limits[id], hookPerSec)
	}
	if !s.take(s.hk.global, 0, hookGlobalBurst, hookGlobalPerSec) {
		s.hk.limits[id].tokens++ // not sent: give the hook its token back
		return false, wait(s.hk.global[0], hookGlobalPerSec)
	}
	return true, 0
}

func wait(b *bucket, perSec float64) time.Duration {
	return time.Duration((1 - b.tokens) / perSec * float64(time.Second))
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
	name, ok := proto.BotName(a.Name)
	if !ok {
		return nil, badRequest(`hook name plus "-bot" must match ^[a-z0-9][a-z0-9_-]{0,31}$`)
	}
	a.Name = name
	token := newHookToken()
	res := proto.HookCreateRes{Name: name}
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
		var mine, inChan int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FILTER (WHERE created_by = ?), COUNT(*) FILTER (WHERE chan = ?) FROM hooks`,
			u.ID, a.Chan).Scan(&mine, &inChan); err != nil {
			return nil, err
		}
		switch {
		case mine >= maxHooksPerUser:
			return nil, proto.Errf(proto.ErrConflict, fmt.Sprintf("you already have %d webhooks; delete one first (/webhooks)", maxHooksPerUser))
		case inChan >= maxHooksPerChan:
			return nil, proto.Errf(proto.ErrConflict, fmt.Sprintf("this channel already has %d webhooks; delete one first (/webhooks)", maxHooksPerChan))
		}
		now := s.now()
		var evs []proto.Event
		var botID, owner uint64
		var isBot, disabled bool
		err := tx.QueryRowContext(ctx, `SELECT id, bot, disabled, bot_owner FROM users WHERE name = ?`, a.Name).Scan(&botID, &isBot, &disabled, &owner)
		switch {
		case err == nil && !isBot:
			return nil, proto.Errf(proto.ErrConflict, "that name belongs to a person")
		case err == nil && disabled:
			return nil, proto.Errf(proto.ErrConflict, "that bot is disabled")
		case err == nil && owner != u.ID && !u.Admin:
			// Otherwise anyone could post as someone else's trusted bot.
			return nil, proto.Errf(proto.ErrConflict, "that bot belongs to someone else; pick another name")
		case errors.Is(err, sql.ErrNoRows):
			var bots int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE bot = 1`).Scan(&bots); err != nil {
				return nil, err
			}
			if bots >= maxBots {
				return nil, proto.Errf(proto.ErrConflict, fmt.Sprintf("this server already has %d bots; reuse one of yours", maxBots))
			}
			r, err := tx.ExecContext(ctx, `INSERT INTO users (name, created, bot, bot_owner) VALUES (?, ?, 1, ?)`, a.Name, now, u.ID)
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
	rows, err := s.st.db.QueryContext(ctx, `SELECT id, chan, name, created_by, created FROM hooks
		WHERE ? OR chan IN (SELECT chan FROM membership WHERE user = ? AND leave_seq IS NULL) ORDER BY id`, u.Admin, u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	res := proto.HooksRes{Hooks: []proto.Hook{}}
	for rows.Next() {
		var h proto.Hook
		if err := rows.Scan(&h.ID, &h.Chan, &h.Name, &h.CreatedBy, &h.Created); err != nil {
			return nil, err
		}
		res.Hooks = append(res.Hooks, h)
	}
	return res, rows.Err()
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
	tokenHash       string
}

// hookByToken returns the hook for token and its bot user.
func (s *Server) hookByToken(ctx context.Context, token string) (liveHook, error) {
	h := liveHook{tokenHash: tokenHash(token)}
	var botID uint64
	err := s.st.db.QueryRowContext(ctx, `SELECT id, chan, bot, created_by FROM hooks WHERE token = ?`,
		h.tokenHash).Scan(&h.id, &h.ch, &botID, &h.creator)
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
