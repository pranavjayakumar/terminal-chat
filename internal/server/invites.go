package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
)

// inviteKeysSchema holds the SSH keys a key-bound invite (cap key_invites)
// may be redeemed with, by fingerprint. Rows go with their invite.
const inviteKeysSchema = `
CREATE TABLE IF NOT EXISTS invite_keys (
	token TEXT NOT NULL REFERENCES invites(token) ON DELETE CASCADE,
	fp TEXT NOT NULL,
	PRIMARY KEY (token, fp)
);
CREATE INDEX IF NOT EXISTS invites_created_by ON invites(created_by);
`

// boundTokenHash is what key-bound and member invites are stored under
// instead of tokenHash: a server from before key_invites/member_invites
// (after a downgrade) looks invites up by tokenHash only, so it can't
// redeem them without the binding or creator checks it doesn't know.
func boundTokenHash(token string) string { return tokenHash("tc-invite-v2\x00" + token) }

// inviteGrace is how long spent-by-time invites are kept before
// CreateInviteOpts deletes them, so expired rows don't pile up (a member
// can churn short-lived invites without hitting the live cap).
const inviteGrace = 24 * time.Hour

// liveInvite is the SQL condition for an invite (i, its creator c LEFT
// JOINed from users) that can still be redeemed at ?1 while member invites
// are ?2: not used up or expired, and made by the server host, an enabled
// admin, or (while member invites are on) an enabled member, if it is a
// new-user invite. redeem checks the same rules in Go.
const liveInvite = `i.used = 0 AND i.uses > 0 AND i.expires > ?1 AND
	(i.created_by = 0 OR (c.disabled = 0 AND (c.admin = 1 OR (?2 AND i.user IS NULL))))`

// CreateInvite mints an invite code. admin makes a new-user invite that
// grants admin; user != 0 makes an add-key invite for that user.
// It is single-use and lasts proto.DefaultInviteTTL.
func (s *Server) CreateInvite(ctx context.Context, admin bool, user uint64) (string, error) {
	inv, err := s.CreateInviteOpts(ctx, InviteOpts{Admin: admin, User: user})
	return inv.Code, err
}

// InviteOpts configures CreateInviteOpts. Zero Uses means 1, zero TTL means
// proto.DefaultInviteTTL (proto.MaxAdminInviteTTL for admin invites).
type InviteOpts struct {
	Admin bool   // single-use, at most proto.MaxAdminInviteTTL
	User  uint64 // add-key invite for this user (always single-use)
	Uses  int
	TTL   time.Duration
	// CreatedBy is who mints it over the protocol; 0: the server host.
	CreatedBy uint64
	// Member applies the member limits (proto.MaxMember*) to CreatedBy,
	// with the live-invite cap checked in the same transaction as the insert.
	Member bool
	// Keys binds the invite to these wire-format SSH public keys (always
	// single-use); KeySrc says where they came from ("github:<login>").
	Keys   [][]byte
	KeySrc string
}

// errAddKeyAdminOnly: members may not mint add-key invites, not even for
// themselves. A stolen SSH key could otherwise add a second login key of
// the thief's (and endorse it), which outlives the device revoke and has
// no per-key removal; only an admin can add a key (PROTOCOL.md §4.1).
var errAddKeyAdminOnly = proto.Errf(proto.ErrForbidden, "only an admin can add an SSH key to an account")

var errMemberInviteCap = proto.Errf(proto.ErrConflict,
	fmt.Sprintf("you already have %d open invites; revoke one (/invites) first", proto.MaxMemberInvites))

// inviteKeyFPs validates the keys an invite is to be bound to and returns
// their fingerprints, without duplicates.
func inviteKeyFPs(keys [][]byte) ([]string, error) {
	if len(keys) > proto.MaxInviteKeys {
		return nil, fmt.Errorf("at most %d keys per invite", proto.MaxInviteKeys)
	}
	fps := make([]string, 0, len(keys))
	for _, k := range keys {
		if len(k) > proto.MaxInviteKeyLen {
			return nil, fmt.Errorf("a key is over %d bytes", proto.MaxInviteKeyLen)
		}
		pk, err := identity.InviteKey(k)
		if err != nil {
			return nil, err
		}
		if fp := identity.Fingerprint(pk); !slices.Contains(fps, fp) {
			fps = append(fps, fp)
		}
	}
	return fps, nil
}

// CreateInviteOpts mints an invite that Uses people can redeem before TTL
// passes (PROTOCOL.md §4.1).
func (s *Server) CreateInviteOpts(ctx context.Context, o InviteOpts) (proto.InviteRes, error) {
	if o.Uses == 0 {
		o.Uses = 1
	}
	if o.TTL == 0 {
		o.TTL = proto.DefaultInviteTTL
		if o.Admin {
			o.TTL = proto.MaxAdminInviteTTL
		}
	}
	if o.Uses < 1 || o.Uses > proto.MaxInviteUses {
		return proto.InviteRes{}, fmt.Errorf("uses must be 1..%d", proto.MaxInviteUses)
	}
	if o.TTL < time.Minute || o.TTL > proto.MaxInviteTTL {
		return proto.InviteRes{}, errors.New("ttl must be between 1 minute and 30 days")
	}
	if o.User != 0 && o.Uses != 1 {
		return proto.InviteRes{}, errors.New("add-key invites are single-use")
	}
	if o.Admin && o.Uses != 1 {
		return proto.InviteRes{}, errors.New("admin invites are single-use")
	}
	if o.Admin && o.TTL > proto.MaxAdminInviteTTL {
		return proto.InviteRes{}, errors.New("admin invites last at most 24 hours")
	}
	if o.Admin && o.User != 0 {
		return proto.InviteRes{}, errors.New("an invite is either admin or add-key")
	}
	if o.Member {
		switch {
		case o.CreatedBy == 0 || o.Admin:
			return proto.InviteRes{}, errors.New("member invites are made by a member and never grant admin")
		case o.User != 0:
			return proto.InviteRes{}, errAddKeyAdminOnly
		case o.Uses > proto.MaxMemberInviteUses:
			return proto.InviteRes{}, fmt.Errorf("member invites allow at most %d uses", proto.MaxMemberInviteUses)
		case o.TTL > proto.MaxMemberInviteTTL:
			return proto.InviteRes{}, errors.New("member invites last at most 7 days")
		}
	}
	fps, err := inviteKeyFPs(o.Keys)
	if err != nil {
		return proto.InviteRes{}, err
	}
	if len(o.Keys) > 0 && o.Uses != 1 {
		return proto.InviteRes{}, errors.New("key-bound invites are single-use")
	}
	if !proto.ValidKeySrc(o.KeySrc) || (o.KeySrc != "" && len(o.Keys) == 0) {
		return proto.InviteRes{}, errors.New(`key source must be "github:<login>" and come with keys`)
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	token := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	now := s.now()
	expires := now + o.TTL.Milliseconds()
	stored := tokenHash(token)
	if len(fps) > 0 || o.Member {
		stored = boundTokenHash(token)
	}
	// Serialized with redemption, revocation and user.disable, so the member
	// cap can't be raced past and a disabled member can't slip one in.
	_, err = s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if _, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE expires <= ?`, now-inviteGrace.Milliseconds()); err != nil {
			return nil, err
		}
		if o.Member {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM invites
				WHERE created_by = ? AND user IS NULL AND used = 0 AND uses > 0 AND expires > ?`, o.CreatedBy, now).Scan(&n); err != nil {
				return nil, err
			}
			if n >= proto.MaxMemberInvites {
				return nil, errMemberInviteCap
			}
		}
		if o.CreatedBy != 0 {
			c, err := getUser(ctx, tx, o.CreatedBy)
			if err != nil || c.Disabled {
				return nil, errForbidden
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO invites (token, admin, user, created, expires, uses, created_by, key_src) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			stored, o.Admin, nullable(o.User), now, expires, o.Uses, o.CreatedBy, o.KeySrc); err != nil {
			return nil, fmt.Errorf("creating invite: %w", err)
		}
		for _, fp := range fps {
			if _, err := tx.ExecContext(ctx, `INSERT INTO invite_keys (token, fp) VALUES (?, ?)`, stored, fp); err != nil {
				return nil, fmt.Errorf("binding invite keys: %w", err)
			}
		}
		return nil, nil
	})
	if err != nil {
		return proto.InviteRes{}, err
	}
	return proto.InviteRes{Code: proto.FormatInvite(s.cfg.PublicAddr, token, s.pin), Uses: int64(o.Uses),
		Expires: expires, Keys: int64(len(fps))}, nil
}

// ListInvites returns every invite that can still be redeemed, soonest to
// expire first. Tokens aren't stored, so they can't be returned.
func (s *Server) ListInvites(ctx context.Context) ([]proto.InviteInfo, error) {
	return s.listInvites(ctx, 0)
}

// listInvites is ListInvites, only creator's when creator != 0.
func (s *Server) listInvites(ctx context.Context, creator uint64) ([]proto.InviteInfo, error) {
	rows, err := s.st.db.QueryContext(ctx, `SELECT i.token, i.admin, COALESCE(i.user, 0), i.uses, i.expires, i.created,
		i.created_by, i.key_src, (SELECT COUNT(*) FROM invite_keys k WHERE k.token = i.token)
		FROM invites i LEFT JOIN users c ON c.id = i.created_by
		WHERE `+liveInvite+` AND (?3 = 0 OR i.created_by = ?3)
		ORDER BY i.expires, i.token`, s.now(), s.cfg.MemberInvites, creator)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []proto.InviteInfo{}
	for rows.Next() {
		var hash string
		var admin bool
		var inv proto.InviteInfo
		if err := rows.Scan(&hash, &admin, &inv.User, &inv.UsesLeft, &inv.Expires, &inv.Created, &inv.CreatedBy,
			&inv.KeySrc, &inv.Keys); err != nil {
			return nil, err
		}
		inv.ID = hash[:proto.InviteIDLen]
		switch {
		case inv.User != 0:
			inv.Kind = proto.InviteKindKey
		case admin:
			inv.Kind = proto.InviteKindAdmin
		default:
			inv.Kind = proto.InviteKindUser
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// UserName is user id's name, or "" if there is none.
func (s *Server) UserName(ctx context.Context, id uint64) string {
	u, err := getUser(ctx, s.st.db, id)
	if err != nil {
		return ""
	}
	return u.Name
}

// ErrInviteNotFound: no live invite has that id.
var ErrInviteNotFound = errors.New("no such invite (see the invite list)")

var errInviteAmbiguous = errors.New("invite id matches more than one invite")

// RevokeInvite deletes the live invite with id (InviteInfo.ID). It runs in
// the same serialized write path as redemption, so a redemption either
// finished before it or fails after.
func (s *Server) RevokeInvite(ctx context.Context, id string) error {
	return s.revokeInvite(ctx, id, 0)
}

// revokeInvite is RevokeInvite limited to creator's invites when creator
// != 0; anyone else's is ErrInviteNotFound, so ids of others' invites
// can't be probed.
func (s *Server) revokeInvite(ctx context.Context, id string, creator uint64) error {
	if !proto.ValidInviteID(id) {
		return ErrInviteNotFound
	}
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		res, err := tx.ExecContext(ctx, `DELETE FROM invites WHERE substr(token, 1, ?) = ? AND used = 0 AND uses > 0
			AND expires > ? AND (? = 0 OR created_by = ?)`,
			proto.InviteIDLen, id, s.now(), creator, creator)
		if err != nil {
			return nil, err
		}
		switch n, _ := res.RowsAffected(); n {
		case 0:
			return nil, ErrInviteNotFound
		case 1:
			return nil, nil
		default: // a 48-bit prefix collision: revoke neither, say so
			return nil, errInviteAmbiguous
		}
	})
	return err
}

// redeemCheck applies the invite rules that go beyond uses and expiry, in
// redeem's transaction: the creator must still be allowed to invite (the
// liveInvite rule; an add-key invite needs an admin creator, which also
// voids member add-key invites minted before they were admin-only), and a
// key-bound invite needs one of its keys (fp). It returns a BYE message, or
// "" if the invite may be redeemed; for a key mismatch also the fingerprints
// the invite accepts (the key_hint, §4.1).
func (s *Server) redeemCheck(ctx context.Context, tx *sql.Tx, token string, createdBy uint64, addKey bool, fp string) (string, []string, error) {
	if createdBy != 0 {
		c, err := getUser(ctx, tx, createdBy)
		if err != nil || c.Disabled || (!c.Admin && (addKey || !s.cfg.MemberInvites)) {
			return "invite is invalid, used or expired", nil, nil
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT fp FROM invite_keys WHERE token = ? ORDER BY fp`, token)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	var fps []string
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return "", nil, err
		}
		if f == fp {
			return "", nil, nil
		}
		fps = append(fps, f)
	}
	if err := rows.Err(); err != nil {
		return "", nil, err
	}
	if len(fps) > 0 {
		return "this invite is for other SSH keys; join with one of them (tc join --key path)", fps, nil
	}
	return "", nil, nil
}

// ---- ops ----

// memberInvites reports whether u may use the invite ops: admins always,
// members while Config.MemberInvites is on.
func (s *Server) memberInvites(u proto.User) bool { return u.Admin || (s.cfg.MemberInvites && !u.Bot) }

// inviteScope is the creator filter for u's invite.list/revoke: 0 (all) for
// admins, else u's own.
func inviteScope(u proto.User) uint64 {
	if u.Admin {
		return 0
	}
	return u.ID
}

func (s *Server) opInviteCreate(ctx context.Context, u proto.User, a proto.InviteCreateArgs) (any, error) {
	if !s.memberInvites(u) {
		return nil, errForbidden
	}
	member := !u.Admin
	if member && a.User != 0 {
		return nil, errAddKeyAdminOnly
	}
	if a.User != 0 {
		target, err := getUser(ctx, s.st.db, a.User)
		if err != nil {
			return nil, errNotFound
		}
		if target.Bot {
			return nil, badRequest("bots can't log in")
		}
		if target.Disabled {
			return nil, badRequest("that account is disabled")
		}
	}
	maxUses, maxTTL := int64(proto.MaxInviteUses), proto.MaxInviteTTL
	if member {
		maxUses, maxTTL = proto.MaxMemberInviteUses, proto.MaxMemberInviteTTL
	}
	if a.Uses < 0 || a.Uses > maxUses {
		return nil, badRequest(fmt.Sprintf("uses must be 1..%d", maxUses))
	}
	if a.User != 0 && a.Uses > 1 {
		return nil, badRequest("add-key invites are single-use")
	}
	if a.TTLMs < 0 || a.TTLMs > maxTTL.Milliseconds() ||
		(a.TTLMs != 0 && a.TTLMs < time.Minute.Milliseconds()) {
		return nil, badRequest(fmt.Sprintf("ttl must be between 1 minute and %d days", int(maxTTL.Hours()/24)))
	}
	if _, err := inviteKeyFPs(a.Keys); err != nil {
		return nil, badRequest(err.Error())
	}
	if len(a.Keys) > 0 && a.Uses > 1 {
		return nil, badRequest("key-bound invites are single-use")
	}
	if !proto.ValidKeySrc(a.KeySrc) || (a.KeySrc != "" && len(a.Keys) == 0) {
		return nil, badRequest(`key_src must be "github:<login>" and come with keys`)
	}
	return s.CreateInviteOpts(ctx, InviteOpts{User: a.User, Uses: int(a.Uses), TTL: time.Duration(a.TTLMs) * time.Millisecond,
		CreatedBy: u.ID, Member: member, Keys: a.Keys, KeySrc: a.KeySrc})
}

func (s *Server) opInviteList(ctx context.Context, u proto.User) (any, error) {
	if !s.memberInvites(u) {
		return nil, errForbidden
	}
	invs, err := s.listInvites(ctx, inviteScope(u))
	return proto.InvitesRes{Invites: invs}, err
}

func (s *Server) opInviteRevoke(ctx context.Context, u proto.User, a proto.InviteRevokeArgs) (any, error) {
	if !s.memberInvites(u) {
		return nil, errForbidden
	}
	if !proto.ValidInviteID(a.ID) {
		return nil, badRequest(fmt.Sprintf("invite id is %d hex digits (see invite.list)", proto.InviteIDLen))
	}
	switch err := s.revokeInvite(ctx, a.ID, inviteScope(u)); {
	case errors.Is(err, ErrInviteNotFound):
		return nil, errNotFound
	case errors.Is(err, errInviteAmbiguous):
		return nil, proto.Errf(proto.ErrConflict, err.Error())
	case err != nil:
		return nil, err
	}
	return proto.Empty{}, nil
}

// inviteCaps are the invite caps this server advertises.
func (s *Server) inviteCaps() []string {
	caps := []string{proto.CapTeamInvites, proto.CapInviteManage, proto.CapKeyInvites}
	if s.cfg.MemberInvites {
		caps = append(caps, proto.CapMemberInvites)
	}
	return caps
}
