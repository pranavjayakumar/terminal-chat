package server

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"time"

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
CREATE TABLE IF NOT EXISTS agent_access (
	owner INTEGER NOT NULL,
	chan INTEGER NOT NULL,
	PRIMARY KEY (owner, chan)
);
`

// migrateAgents adds the agent tables and the message agent column, and
// moves the channel-name index off agent DMs (they have no name, like DMs).
func migrateAgents(db *sql.DB) error {
	if _, err := db.Exec(agentsSchema); err != nil {
		return err
	}
	if err := addColumn(db, "messages", "agent", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	_, err := db.Exec(`DROP INDEX IF EXISTS channels_name`)
	return err
}

// agentOps are refused to agent sessions: an agent acts in its owner's name
// only by sending, reacting and pinning in the channels it may read.
var agentRefused = map[string]bool{
	proto.OpAgentRegister: true, proto.OpAgentUpdate: true, proto.OpAgentRetire: true,
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
	proto.OpAgentsList: true, proto.OpAgentAccess: true, proto.OpAgentDM: true,
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

// agentHandleTaken reports whether name is an agent's handle (retired ones
// keep theirs), so no user, group or other agent can take it.
func agentHandleTaken(ctx context.Context, q querier, name string) bool {
	var n int
	q.QueryRowContext(ctx, `SELECT COUNT(*) FROM agents WHERE handle = ?`, name).Scan(&n)
	return n > 0
}

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
	rows, err := s.st.db.QueryContext(ctx, `SELECT owner, chan FROM agent_access`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o, c uint64
		if err := rows.Scan(&o, &c); err != nil {
			rows.Close()
			return err
		}
		if s.agentAccess[o] == nil {
			s.agentAccess[o] = map[uint64]bool{}
		}
		s.agentAccess[o][c] = true
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
		m := map[uint64]bool{}
		for _, c := range r.Chans {
			m[c] = true
		}
		s.agentAccess[ev.Actor] = m
	case proto.KChanNew:
		var c proto.Channel
		if proto.Unmarshal(ev.Data, &c) == nil && c.Kind == proto.ChanAgentDM {
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
	return s.agentDMs[sess.user.ID] == ch || s.agentAccess[sess.user.ID][ch]
}

func (s *Server) agentSees(sess *session, ch uint64) bool {
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

// agentLogin checks HELLO agent: handle must be u's active agent.
func (s *Server) agentLogin(ctx context.Context, sess *session, u proto.User, handle string) (string, string) {
	if !sess.agentCap {
		return proto.ByeProtocolError, "agent sessions need cap agent1"
	}
	a, err := activeAgent(ctx, s.st.db, u.ID)
	if err != nil || a.Handle != handle {
		return proto.ByeAuthFailed, "no such agent"
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
	case proto.OpEdit, proto.OpDelete, proto.OpReact, proto.OpUnreact, proto.OpPin, proto.OpUnpin:
		a, err := decodeArgs[proto.MsgRef](args)
		if err != nil {
			return err
		}
		if s.st.db.QueryRowContext(ctx, `SELECT chan FROM messages WHERE id = ?`, a.Msg).Scan(&ch) != nil {
			return errNotFound
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

// handleTakenErr says why handle can't name an agent ("" if it can); own is
// the caller's current handle, which it may keep.
func handleTakenErr(ctx context.Context, tx *sql.Tx, handle, own string) error {
	if !proto.ValidAgentHandle(handle) {
		return badRequest("handle must match ^[a-z0-9][a-z0-9_-]{0,31}$ and not be me or end in -bot")
	}
	if handle == own {
		return nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE name = ?`, handle).Scan(&n); err != nil {
		return err
	}
	if n > 0 || groupNameTaken(ctx, tx, handle) || agentHandleTaken(ctx, tx, handle) {
		return proto.Errf(proto.ErrConflict, "that name is taken")
	}
	return nil
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
		if err := handleTakenErr(ctx, tx, a.Handle, ""); err != nil {
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
		if a.Handle != "" {
			if err := handleTakenErr(ctx, tx, a.Handle, ag.Handle); err != nil {
				return nil, err
			}
			next.Handle = a.Handle
		}
		if a.Icon != "" {
			next.Icon = a.Icon
		}
		next.Online = s.agentOnline[u.ID] > 0
		res.Agent = next
		if next.Handle == ag.Handle && next.Icon == ag.Icon {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET handle = ?, icon = ? WHERE owner = ? AND retired = 0`,
			next.Handle, next.Icon, u.ID); err != nil {
			return nil, err
		}
		ev, err := appendEvent(ctx, tx, proto.KAgentUpdate, 0, u.ID, u.ID, s.now(), next)
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
		return []proto.Event{ev}, err
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
	if len(a.Add) > proto.MaxAgentAccess || len(a.Remove) > proto.MaxAgentAccess {
		return nil, proto.Errf(proto.ErrTooLarge, "too many channels")
	}
	var res proto.AgentAccessRes
	_, err := s.write(ctx, func(tx *sql.Tx) ([]proto.Event, error) {
		cur := s.agentAccess[u.ID]
		next := map[uint64]bool{}
		for c := range cur {
			next[c] = true
		}
		for _, c := range a.Remove {
			delete(next, c)
		}
		for _, c := range a.Add {
			kind, ok := s.kinds[c]
			if !ok || kind == proto.ChanAgentDM || !(kind == proto.ChanPublic || s.members[c][u.ID]) {
				return nil, proto.Errf(proto.ErrNotFound, "no such channel")
			}
			next[c] = true
		}
		if len(next) > proto.MaxAgentAccess {
			return nil, proto.Errf(proto.ErrTooLarge, "an agent may read at most 256 channels")
		}
		res.Chans = make([]uint64, 0, len(next))
		for c := range next {
			res.Chans = append(res.Chans, c)
		}
		slices.Sort(res.Chans)
		if len(next) == len(cur) && !slices.ContainsFunc(res.Chans, func(c uint64) bool { return !cur[c] }) {
			return nil, nil
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM agent_access WHERE owner = ?`, u.ID); err != nil {
			return nil, err
		}
		for _, c := range res.Chans {
			if _, err := tx.ExecContext(ctx, `INSERT INTO agent_access (owner, chan) VALUES (?, ?)`, u.ID, c); err != nil {
				return nil, err
			}
		}
		ev, err := appendEvent(ctx, tx, proto.KAgentAccess, 0, u.ID, u.ID, s.now(), res)
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
	var e2e bool
	if s.st.db.QueryRow(`SELECT e2e FROM channels WHERE id = ?`, e.Chan).Scan(&e2e) != nil {
		return
	}
	if e2e && e.Text != "" || !e2e && e.Enc != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u := sess.user.ID
	if !s.members[e.Chan][u] || !s.agentSeesLocked(sess, e.Chan) || !takeBucket(&sess.agentBucket, time.Now(), agentEphBurst, proto.AgentEphRate) {
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
