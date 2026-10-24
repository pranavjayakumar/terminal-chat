package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"maps"
	"slices"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Personal agents (PROTOCOL.md §15).

const agentsSchema = `
CREATE TABLE IF NOT EXISTS agents (
	id INTEGER PRIMARY KEY,
	owner INTEGER NOT NULL REFERENCES users(id),
	handle TEXT NOT NULL UNIQUE,
	icon TEXT NOT NULL DEFAULT '',
	created INTEGER NOT NULL,
	retired INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS agents_active ON agents(owner) WHERE retired = 0;
-- Every handle an agent ever had: a renamed agent's old handle stays its
-- owner's, so old messages and mentions never point at someone else.
CREATE TABLE IF NOT EXISTS agent_handles (
	handle TEXT PRIMARY KEY,
	owner INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_renames (
	owner INTEGER NOT NULL,
	time INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_access (
	owner INTEGER NOT NULL,
	chan INTEGER NOT NULL,
	PRIMARY KEY (owner, chan)
);
`

// migrateAgents adds the agent tables and the message agent column, and
// keys and access grants.
func migrateAgents(db *sql.DB) error {
	if _, err := db.Exec(agentsSchema); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO agent_handles (handle, owner) SELECT handle, owner FROM agents`); err != nil {
		return err
	}
	for _, c := range []string{"agent", "via"} {
		if err := addColumn(db, "messages", c, "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	// keys.agent: the key logs in only as its user's agent (§15.1).
	if err := addColumn(db, "keys", "agent", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// agent_access.expires: the end of a temporary grant (0 = until removed).
	return addColumn(db, "agent_access", "expires", "INTEGER NOT NULL DEFAULT 0")
}

// agentRefused are refused to agent sessions: an agent acts in its owner's
// name only by sending (and editing or deleting what it sent) in the
// channels it may read. Reactions
// are refused because the owner's reactions are its approvals: an agent's
// would look like the owner's tap. It may rename itself (agent.update).
var agentRefused = map[string]bool{
	proto.OpReact: true, proto.OpUnreact: true, proto.OpPin: true, proto.OpUnpin: true,
	proto.OpRemindCreate: true, proto.OpRemindList: true, proto.OpRemindDelete: true, proto.OpAgentKey: true,
	proto.OpAgentRegister: true, proto.OpAgentRetire: true,
	proto.OpAgentAccess: true, proto.OpAgentDM: true,
	proto.OpInviteCreate: true, proto.OpInviteList: true, proto.OpInviteRevoke: true,
	proto.OpUserDisable: true, proto.OpUserUpdate: true, proto.OpGroupSet: true,
	proto.OpHookCreate: true, proto.OpHookDelete: true, proto.OpHookList: true,
	proto.OpDeviceRevoke: true, proto.OpChanEncrypt: true, proto.OpChanCreate: true,
	proto.OpChanJoin: true, proto.OpChanLeave: true, proto.OpChanAdd: true, proto.OpChanRemove: true,
	proto.OpChanTopic: true, proto.OpDMOpen: true, proto.OpHuddleJoin: true,
}

var agentOps = map[string]bool{
	proto.OpAgentRegister: true, proto.OpAgentUpdate: true, proto.OpAgentRetire: true,
	proto.OpAgentsList: true, proto.OpAgentAccess: true, proto.OpAgentDM: true, proto.OpAgentKey: true,
}

// agentDMKey is an agent DM's channels.dmkey: one per owner.
func agentDMKey(owner uint64) string { return "agent:" + strconv.FormatUint(owner, 10) }

const agentCols = `owner, handle, icon, created, retired`

func scanAgent(sc interface{ Scan(...any) error }) (proto.Agent, error) {
	var a proto.Agent
	err := sc.Scan(&a.Owner, &a.Handle, &a.Icon, &a.Created, &a.Retired)
	return a, err
}

func listAgents(ctx context.Context, q querier) ([]proto.Agent, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+agentCols+` FROM agents ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []proto.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func activeAgent(ctx context.Context, q querier, owner uint64) (proto.Agent, error) {
	return scanAgent(q.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE owner = ? AND retired = 0`, owner))
}

// agentHandleTaken reports whether name is or was an agent's handle (retired
// and renamed agents keep theirs), so no user, group or other agent can take it.
func agentHandleTaken(ctx context.Context, q querier, name string) bool {
	return agentHandleOwner(ctx, q, name) != 0
}

func agentHandleOwner(ctx context.Context, q querier, name string) uint64 {
	var o uint64
	q.QueryRowContext(ctx, `SELECT owner FROM agent_handles WHERE handle = ?`, name).Scan(&o)
	return o
}

// maxAgentRenames is how many times an agent's handle may change per day.
const maxAgentRenames = 5

func (s *Server) loadAgents(ctx context.Context) error {
	agents, err := listAgents(ctx, s.st.db)
	if err != nil {
		return err
	}
	for _, a := range agents {
		if a.Retired == 0 {
			s.agents[a.Owner] = a
		}
	}
	rows, err := s.st.db.QueryContext(ctx, `SELECT owner, chan, expires FROM agent_access`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o, c uint64
		var exp int64
		if err := rows.Scan(&o, &c, &exp); err != nil {
			rows.Close()
			return err
		}
		if s.agentAccess[o] == nil {
			s.agentAccess[o] = map[uint64]int64{}
		}
		s.agentAccess[o][c] = exp
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = s.st.db.QueryContext(ctx, `SELECT id, creator FROM channels WHERE kind = ?`, proto.ChanAgentDM)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, owner uint64
		if err := rows.Scan(&id, &owner); err != nil {
			return err
		}
		s.agentDMs[owner] = id
	}
	return rows.Err()
}

// applyAgentCache keeps the agent caches in step with ev. Called with s.mu held.
func (s *Server) applyAgentCache(ev proto.Event) {
	switch ev.Kind {
	case proto.KAgentUpdate:
		var a proto.Agent
		if proto.Unmarshal(ev.Data, &a) != nil {
			return
		}
		if a.Retired != 0 {
			if s.agents[a.Owner].Handle == a.Handle {
				delete(s.agents, a.Owner)
			}
			return
		}
		s.agents[a.Owner] = a
	case proto.KAgentAccess:
		var r proto.AgentAccessRes
		if proto.Unmarshal(ev.Data, &r) != nil {
			return
		}
		m := map[uint64]int64{}
		for _, c := range r.Chans {
			m[c] = r.Expires[c]
		}
		s.agentAccess[ev.Actor] = m
	case proto.KChanLeave:
		// Leaving (or being removed from) a channel ends the agent's grant.
		var r proto.UserRef
		if proto.Unmarshal(ev.Data, &r) == nil {
			if _, ok := s.agentAccess[r.User][ev.Chan]; ok {
				delete(s.agentAccess[r.User], ev.Chan)
				s.st.db.Exec(`DELETE FROM agent_access WHERE owner = ? AND chan = ?`, r.User, ev.Chan)
			}
		}
	case proto.KChanNew, proto.KChanUpdate:
		var c proto.Channel
		if proto.Unmarshal(ev.Data, &c) != nil {
			return
		}
		s.chanE2E[c.ID] = c.E2E
		if c.Kind == proto.ChanAgentDM {
			s.agentDMs[c.Creator] = c.ID
		}
	}
}

// agentSeesLocked reports whether sess may see ch's messages: always for a
// person's session; for an agent session, its DM and its access list.
// Called with s.mu held.
func (s *Server) agentSeesLocked(sess *session, ch uint64) bool {
	if sess.agent == nil || ch == 0 {
		return true
	}
	if s.agentDMs[sess.user.ID] == ch {
		return true
	}
	exp, ok := s.agentAccess[sess.user.ID][ch]
	return ok && (exp == 0 || exp > s.now())
}

func (s *Server) agentSees(sess *session, ch uint64) bool {
	if sess.agent == nil { // without taking s.mu: sync replays call this per event
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agentSeesLocked(sess, ch)
}

// agentHandle is the handle an agent session's messages carry now (a
// rename applies at once). Called with s.mu held.
func (s *Server) agentHandleLocked(sess *session) string {
	if sess == nil || sess.agent == nil {
		return ""
	}
	if a, ok := s.agents[sess.user.ID]; ok {
		return a.Handle
	}
	return sess.agent.Handle
}

// agentLogin makes sess an agent session of u's active agent. With an
// agent key (byKey) any HELLO agent is fine; with one of u's own keys handle
// must name the agent (now or before a rename) and the agent must have no
// key of its own yet.
func (s *Server) agentLogin(ctx context.Context, sess *session, u proto.User, handle string, byKey bool) (string, string) {
	if !sess.agentCap {
		return proto.ByeProtocolError, "agent sessions need cap agent1"
	}
	a, err := activeAgent(ctx, s.st.db, u.ID)
	if err != nil {
		return proto.ByeAuthFailed, "no such agent"
	}
	if !byKey {
		if a.Handle != handle && agentHandleOwner(ctx, s.st.db, handle) != u.ID {
			return proto.ByeAuthFailed, "no such agent"
		}
		var n int
		if err := s.st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM keys WHERE user = ? AND agent = 1`, u.ID).Scan(&n); err != nil || n > 0 {
			return proto.ByeAuthFailed, "this agent logs in with its own key"
		}
	}
	a.Online = true
	sess.agent = &a
	sess.voice = false
	sess.passive = false
	return "", ""
}

// gateAgentOp refuses what an agent session may not do (§15.2, §15.4).
// Message ops are checked against the channels it may read here; send
// checks inside its transaction.
func (s *Server) gateAgentOp(ctx context.Context, sess *session, op string, args proto.Raw) error {
	if op == proto.OpAgentAccess {
		// It may read its own access list, never change it.
		a, err := decodeArgs[proto.AgentAccessArgs](args)
		if err != nil || len(a.Add) > 0 || len(a.Remove) > 0 {
			return proto.Errf(proto.ErrForbidden, "agents can't do that")
		}
		return nil
	}
	if agentRefused[op] {
		return proto.Errf(proto.ErrForbidden, "agents can't do that")
	}
	var ch uint64
	switch op {
	case proto.OpHistory:
		a, err := decodeArgs[proto.HistoryArgs](args)
		if err != nil {
			return err
		}
		ch = a.Chan
	case proto.OpSearch:
		a, err := decodeArgs[proto.SearchArgs](args)
		if err != nil {
			return err
		}
		if a.Chan == 0 {
			return proto.Errf(proto.ErrForbidden, "agents search one channel at a time")
		}
		ch = a.Chan
	case proto.OpPinsList:
		a, err := decodeArgs[proto.ChanArgs](args)
		if err != nil {
			return err
		}
		ch = a.Chan
	case proto.OpKeyRotate:
		a, err := decodeArgs[proto.KeyRotateArgs](args)
		if err != nil {
			return err
		}
		ch = a.Chan
	case proto.OpEdit, proto.OpDelete:
		a, err := decodeArgs[proto.MsgRef](args)
		if err != nil {
			return err
		}
		var agent string
		if s.st.db.QueryRowContext(ctx, `SELECT chan, agent FROM messages WHERE id = ?`, a.Msg).Scan(&ch, &agent) != nil {
			return errNotFound
		}
		if agent == "" { // its owner's own words are not the agent's to change
			return proto.Errf(proto.ErrForbidden, "an agent may change only what an agent sent")
		}
	default:
		return nil
	}
	if !s.agentSees(sess, ch) {
		return proto.Errf(proto.ErrForbidden, "this agent may not read that channel")
	}
	return nil
}

func validAgentIcon(icon string) bool { return validLine(icon, proto.MaxAgentIcon) }

// handleTakenErr says why owner's agent can't take handle (nil if it can):
// a handle it had before is its own to take back.
func handleTakenErr(ctx context.Context, tx *sql.Tx, handle string, owner uint64) error {
	if !proto.ValidAgentHandle(handle) {
		return badRequest("handle must match ^[a-z0-9][a-z0-9_-]{0,31}$ and not be me or end in -bot")
	}
	if o := agentHandleOwner(ctx, tx, handle); o != 0 {
		if o == owner {
			return nil
		}
		return proto.Errf(proto.ErrConflict, "that name is taken")
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE handle = ? AND retired != 0`, handle).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return proto.Errf(proto.ErrConflict, "a retired agent keeps that name")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, handle).Scan(&n); err != nil {
		return err
	}
	if n > 0 || groupNameTaken(ctx, tx, handle) {
		return proto.Errf(proto.ErrConflict, "that name is taken")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO agent_handles (handle, owner) VALUES (?, ?)`, handle, owner)
	return err
}

func (s *Server) opAgentRegister(ctx context.Context, u proto.User, a proto.AgentRegisterArgs) (any, error) {
	if !validAgentIcon(a.Icon) {
		return nil, badRequest("invalid icon")
	}
	var res proto.AgentRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if _, err := activeAgent(ctx, tx, u.ID); err == nil {
			return nil, proto.Errf(proto.ErrConflict, "you already have an agent; retire it first")
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err := handleTakenErr(ctx, tx, a.Handle, u.ID); err != nil {
			return nil, err
		}
		now := s.now()
		ag := proto.Agent{Handle: a.Handle, Icon: a.Icon, Owner: u.ID, Created: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agents (owner, handle, icon, created) VALUES (?, ?, ?, ?)`,
			ag.Owner, ag.Handle, ag.Icon, ag.Created); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KAgentUpdate, 0, u.ID, u.ID, now, ag)
		res.Agent = ag
		return []proto.Event{ev}, err
	})
	return res, err
}

func (s *Server) opAgentUpdate(ctx context.Context, u proto.User, a proto.AgentUpdateArgs) (any, error) {
	if a.Icon != "" && !validAgentIcon(a.Icon) {
		return nil, badRequest("invalid icon")
	}
	var res proto.AgentRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ag, err := activeAgent(ctx, tx, u.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, proto.Errf(proto.ErrNotFound, "you have no agent")
		} else if err != nil {
			return nil, err
		}
		next := ag
		now := s.now()
		if a.Handle != "" && a.Handle != ag.Handle {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_renames WHERE owner = ? AND time > ?`,
				u.ID, now-24*time.Hour.Milliseconds()).Scan(&n); err != nil {
				return nil, err
			}
			if n >= maxAgentRenames {
				return nil, proto.Errf(proto.ErrRateLimited, "an agent can change its name 5 times a day")
			}
			if err := handleTakenErr(ctx, tx, a.Handle, u.ID); err != nil {
				return nil, err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_renames (owner, time) VALUES (?, ?)`, u.ID, now); err != nil {
				return nil, err
			}
			next.Handle = a.Handle
		}
		switch {
		case a.ClearIcon:
			next.Icon = ""
		case a.Icon != "":
			next.Icon = a.Icon
		}
		res.Agent = next
		res.Agent.Online = s.agentOnline[u.ID] > 0
		if next.Handle == ag.Handle && next.Icon == ag.Icon {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET handle = ?, icon = ? WHERE owner = ? AND retired = 0`,
			next.Handle, next.Icon, u.ID); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KAgentUpdate, 0, u.ID, u.ID, now, next)
		return []proto.Event{ev}, err
	})
	return res, err
}

func (s *Server) opAgentRetire(ctx context.Context, u proto.User) (any, error) {
	var res proto.AgentRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		ag, err := activeAgent(ctx, tx, u.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, proto.Errf(proto.ErrNotFound, "you have no agent")
		} else if err != nil {
			return nil, err
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET retired = ? WHERE owner = ? AND retired = 0`, now, u.ID); err != nil {
			return nil, err
		}
		ag.Retired = now
		res.Agent = ag
		ev, err := appendEvent(ctx, tx, proto.KAgentUpdate, 0, u.ID, u.ID, now, ag)
		if err != nil {
			return nil, err
		}
		// A new agent starts from nothing: no grants, no key.
		if _, err := tx.ExecContext(ctx, `DELETE FROM keys WHERE user = ? AND agent = 1`, u.ID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_access WHERE owner = ?`, u.ID); err != nil {
			return nil, err
		}
		acc, err := appendEvent(ctx, tx, proto.KAgentAccess, 0, u.ID, u.ID, now, proto.AgentAccessRes{Chans: []uint64{}})
		return []proto.Event{ev, acc}, err
	})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for sess := range s.sessions {
		if sess.agent != nil && sess.user.ID == u.ID {
			sess.revoke()
		}
	}
	s.mu.Unlock()
	return res, nil
}

func (s *Server) opAgentsList(ctx context.Context) (any, error) {
	agents, err := listAgents(ctx, s.st.db)
	if err != nil {
		return nil, err
	}
	s.markAgentsOnline(agents)
	return proto.AgentsRes{Agents: agents}, nil
}

func (s *Server) markAgentsOnline(agents []proto.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range agents {
		agents[i].Online = agents[i].Retired == 0 && s.agentOnline[agents[i].Owner] > 0
	}
}

func (s *Server) opAgentAccess(ctx context.Context, u proto.User, a proto.AgentAccessArgs) (any, error) {
	if len(a.Add) > proto.MaxAgentAccess || len(a.Remove) > proto.MaxAgentAccess || len(a.Temp) > proto.MaxAgentAccess {
		return nil, proto.Errf(proto.ErrTooLarge, "too many channels")
	}
	var res proto.AgentAccessRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		now := s.now()
		cur := map[uint64]int64{}
		for c, exp := range s.agentAccess[u.ID] {
			if exp == 0 || exp > now {
				cur[c] = exp
			}
		}
		next := maps.Clone(cur)
		for _, c := range a.Remove {
			delete(next, c)
		}
		check := func(c uint64) error {
			kind, ok := s.kinds[c]
			if !ok || kind == proto.ChanAgentDM || !(kind == proto.ChanPublic || s.members[c][u.ID]) {
				return proto.Errf(proto.ErrNotFound, "no such channel")
			}
			return nil
		}
		for _, c := range a.Add {
			if err := check(c); err != nil {
				return nil, err
			}
			next[c] = 0
		}
		for _, c := range a.Temp {
			if err := check(c); err != nil {
				return nil, err
			}
			if exp, ok := next[c]; !ok || exp != 0 {
				next[c] = now + proto.AgentGrantTTL.Milliseconds()
			}
		}
		if len(next) > proto.MaxAgentAccess {
			return nil, proto.Errf(proto.ErrTooLarge, "an agent may read at most 256 channels")
		}
		res.Chans = make([]uint64, 0, len(next))
		for c, exp := range next {
			res.Chans = append(res.Chans, c)
			if exp != 0 {
				if res.Expires == nil {
					res.Expires = map[uint64]int64{}
				}
				res.Expires[c] = exp
			}
		}
		slices.Sort(res.Chans)
		if maps.Equal(next, cur) && len(cur) == len(s.agentAccess[u.ID]) {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_access WHERE owner = ?`, u.ID); err != nil {
			return nil, err
		}
		for _, c := range res.Chans {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_access (owner, chan, expires) VALUES (?, ?, ?)`, u.ID, c, next[c]); err != nil {
				return nil, err
			}
		}
		ev, err := appendEvent(ctx, tx, proto.KAgentAccess, 0, u.ID, u.ID, now, res)
		return []proto.Event{ev}, err
	})
	return res, err
}

func (s *Server) opAgentDM(ctx context.Context, sess *session) (any, error) {
	u := sess.user
	var res proto.ChanRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		var id uint64
		err := tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE dmkey = ?`, agentDMKey(u.ID)).Scan(&id)
		if err == nil {
			res.Chan, err = getChannel(ctx, tx, id)
			return nil, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if _, err := activeAgent(ctx, tx, u.ID); errors.Is(err, sql.ErrNoRows) {
			return nil, proto.Errf(proto.ErrNotFound, "register an agent first")
		} else if err != nil {
			return nil, err
		}
		c, ev, err := createChannel(ctx, tx, s.now(), u.ID, "", proto.ChanAgentDM, "", []uint64{u.ID}, sess.e2e)
		res.Chan = c
		return []proto.Event{ev}, err
	})
	return res, err
}

// agentOnlineLocked counts an agent session in or out and tells agent1
// sessions when the owner's agent comes online or goes. Called with s.mu held.
func (s *Server) agentOnlineLocked(sess *session, delta int) {
	u := sess.user.ID
	s.agentOnline[u] += delta
	state := ""
	switch {
	case delta > 0 && s.agentOnline[u] == 1:
		state = proto.PresOnline
	case delta < 0 && s.agentOnline[u] <= 0:
		delete(s.agentOnline, u)
		state = proto.PresOffline
	}
	if state != "" {
		s.agentEphLocked(proto.Ephemeral{Kind: proto.EphAgentPresence, User: u, State: state}, sess)
	}
}

// agentEphLocked sends e to every agent1 session but from. Called with s.mu held.
func (s *Server) agentEphLocked(e proto.Ephemeral, from *session) {
	payload, err := proto.Marshal(e)
	if err != nil {
		return
	}
	for sess := range s.sessions {
		if sess != from && sess.agentCap {
			sess.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

// sendAgentPresence tells a newly synced agent1 session which agents are online.
func (s *Server) sendAgentPresence(sess *session) {
	if !sess.agentCap {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for u := range s.agentOnline {
		e := proto.Ephemeral{Kind: proto.EphAgentPresence, User: u, State: proto.PresOnline}
		if payload, err := proto.Marshal(e); err == nil {
			sess.push(outFrame{t: proto.TEphemeral, payload: payload})
		}
	}
}

const agentEphBurst = proto.AgentEphRate

// agentEph relays an agent's live status (§15.5).
func (s *Server) agentEph(sess *session, e proto.Ephemeral) {
	if sess.agent == nil || !sess.agentCap || !proto.ValidAgentState(e.State) || !validLine(e.Status, proto.MaxStatus) {
		return
	}
	if len(e.Text) > proto.MaxAgentPreview || !sanitize.Valid(e.Text) || len(e.Enc) > proto.MaxAgentEnc || (e.Text != "" && e.Enc != nil) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !takeBucket(&sess.agentBucket, time.Now(), agentEphBurst, proto.AgentEphRate) {
		return
	}
	u := sess.user.ID
	e2e := s.chanE2E[e.Chan]
	if !s.members[e.Chan][u] || !s.agentSeesLocked(sess, e.Chan) || e2e && e.Text != "" || !e2e && e.Enc != nil {
		return
	}
	out := proto.Ephemeral{Kind: proto.EphAgent, Chan: e.Chan, Thread: e.Thread, User: u, Agent: s.agentHandleLocked(sess),
		State: e.State, Status: e.Status, Text: e.Text, Enc: e.Enc}
	payload, err := proto.Marshal(out)
	if err != nil {
		return
	}
	for t := range s.sessions {
		if t == sess || !t.agentCap || !s.members[e.Chan][t.user.ID] || !s.agentSeesLocked(t, e.Chan) {
			continue
		}
		t.pushLive(outFrame{t: proto.TEphemeral, payload: payload})
	}
}

// keyIsAgent reports whether the key with fingerprint fp is an agent key.
func keyIsAgent(ctx context.Context, q querier, fp string) bool {
	var a bool
	q.QueryRowContext(ctx, `SELECT agent FROM keys WHERE fp = ?`, fp).Scan(&a)
	return a
}

// agentKeys are the keys owner's agent logs in with (at most one).
func agentKeys(ctx context.Context, q querier, owner uint64) ([][]byte, error) {
	rows, err := q.QueryContext(ctx, `SELECT pubkey FROM keys WHERE user = ? AND agent = 1`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]byte
	for rows.Next() {
		var k []byte
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// opAgentKey gives the caller's active agent its own SSH key, replacing any
// earlier one. The key is never the owner: it logs in only as the agent.
func (s *Server) opAgentKey(ctx context.Context, u proto.User, a proto.AgentKeyArgs) (any, error) {
	pk, err := ssh.ParsePublicKey(a.Pubkey)
	if err != nil || !identity.AllowedKeyType(pk.Type()) {
		return nil, badRequest("not an accepted SSH public key")
	}
	fp := identity.Fingerprint(pk)
	_, err = s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		if _, err := activeAgent(ctx, tx, u.ID); errors.Is(err, sql.ErrNoRows) {
			return nil, proto.Errf(proto.ErrNotFound, "register an agent first")
		} else if err != nil {
			return nil, err
		}
		var owner uint64
		var agent bool
		err := tx.QueryRowContext(ctx, `SELECT user, agent FROM keys WHERE fp = ?`, fp).Scan(&owner, &agent)
		switch {
		case err == nil && owner == u.ID && agent:
			return nil, nil
		case err == nil:
			return nil, proto.Errf(proto.ErrConflict, "this key is already registered")
		case !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM keys WHERE user = ? AND agent = 1`, u.ID); err != nil {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO keys (fp, user, pubkey, created, agent) VALUES (?, ?, ?, ?, 1)`,
			fp, u.ID, pk.Marshal(), s.now())
		return nil, err
	})
	if err != nil {
		return nil, err
	}
	// Sessions that logged in with the owner's key as the agent go: from now
	// on the agent is its key.
	s.mu.Lock()
	for sess := range s.sessions {
		if sess.agent != nil && sess.user.ID == u.ID && !bytes.Equal(sess.loginKey, pk.Marshal()) {
			sess.revoke()
		}
	}
	s.mu.Unlock()
	return proto.Empty{}, nil
}
