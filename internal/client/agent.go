package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/identity"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/trust"
)

// Personal agents (PROTOCOL.md §15, cap agent1).

// AgentLive is one agent's live status in a thread (an EphAgent).
type AgentLive struct {
	Agent  proto.Agent
	State  string // proto.AgentThinking … proto.AgentError
	Status string // short status line
	// Preview is the reply streamed so far, decrypted in e2e channels; ""
	// when none or when it couldn't be opened.
	Preview string
	At      time.Time // when this status arrived
}

type agentKey struct{ ch, thread uint64 }

// ErrNotAgent refuses an agent-only call on a person's session.
var ErrNotAgent = errors.New("this is not an agent session")

const (
	// agentEvery spaces our EphAgent frames (the server allows 4/s).
	agentEvery = time.Second / proto.AgentEphRate
	// A live status nobody refreshes is gone after agentLiveTTL; done and
	// error stay only long enough to be seen.
	agentLiveTTL = 90 * time.Second
	agentDoneTTL = 5 * time.Second
	// agentPreviewSkew bounds how old a sealed preview's ts may be, so a
	// server can't replay yesterday's preview.
	agentPreviewSkew = 5 * time.Minute
	// agentCallWindow: a mention older than this doesn't grant access or
	// count as a call when a channel is granted (the harness's stale-ask cutoff).
	agentCallWindow = 24 * time.Hour
)

// plainAgent is Plain t = "agent": a sealed preview (§15.5).
type plainAgent struct {
	T      string `msgpack:"t"`
	TS     int64  `msgpack:"ts"`
	Body   string `msgpack:"body"`
	Agent  string `msgpack:"agent"`
	Thread uint64 `msgpack:"thread,omitempty"`
}

// LoginAgent sets agentHome up as the agent of the user logged in at
// ownerHome (the agent must be registered, RegisterAgent): it makes the
// agent its own SSH key (agentHome/id_ed25519), has the owner's session
// register it as the agent's key (agent.key), signs an AgentEndorse for it
// with the owner's key, logs the new key in (the server confirms the agent)
// and writes agentHome's config. The agent is then its own device on its
// first Run; its key can never log in as the owner (§15.1). handle is the
// agent's current handle.
func LoginAgent(ctx context.Context, ownerHome, agentHome, handle string) (proto.Agent, error) {
	if !proto.ValidAgentHandle(handle) {
		return proto.Agent{}, fmt.Errorf("invalid agent handle %q", handle)
	}
	owner, err := Open(ownerHome)
	if err != nil {
		return proto.Agent{}, err
	}
	defer owner.Close()
	if owner.IsAgentSession() {
		return proto.Agent{}, errors.New("that home is an agent's, not its owner's")
	}
	cfg := owner.Config()
	ownerKey, err := identity.Load(cfg.KeySpec())
	if err != nil {
		return proto.Agent{}, err
	}
	rctx, stop := context.WithCancel(ctx)
	defer stop()
	go owner.Run(rctx)
	for owner.Status().State != Online {
		if st := owner.Status(); st.Stopped {
			return proto.Agent{}, errors.New(st.Err)
		}
		select {
		case <-ctx.Done():
			return proto.Agent{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if _, err := owner.FetchAgents(ctx); err != nil {
		return proto.Agent{}, err
	}
	if a, ok := owner.MyAgent(); !ok || a.Handle != handle {
		return proto.Agent{}, fmt.Errorf("@%s is not your agent (register it first)", handle)
	}
	sid, ok := owner.ServerID()
	if !ok {
		return proto.Agent{}, errors.New("the server has no identity yet")
	}

	if err := ensurePrivateDir(agentHome); err != nil {
		return proto.Agent{}, fmt.Errorf("creating %s: %w", agentHome, err)
	}
	keyPath := filepath.Join(agentHome, "id_ed25519")
	signer, err := newAgentKey(keyPath)
	if err != nil {
		return proto.Agent{}, err
	}
	if err := owner.call(ctx, proto.OpAgentKey, proto.AgentKeyArgs{Pubkey: signer.PublicKey().Marshal()}, nil, nil); err != nil {
		return proto.Agent{}, fmt.Errorf("registering the agent's key: %w", err)
	}
	endorse, err := identity.NewAgentEndorse(ownerKey, sid, cfg.UserID, signer.PublicKey(), time.Now())
	if err != nil {
		return proto.Agent{}, err
	}
	w, err := loginOnce(ctx, proto.Invite{Addr: cfg.Addr, Pin: cfg.Pin}, signer, "", "", handle)
	if err != nil {
		return proto.Agent{}, err
	}
	if w.User.ID != cfg.UserID || w.Agent == nil {
		return proto.Agent{}, errors.New("the server didn't log the agent's key in as the agent")
	}
	acfg := Config{Addr: cfg.Addr, Pin: cfg.Pin, KeyPath: keyPath, UserID: cfg.UserID, Name: cfg.Name,
		Agent: w.Agent.Handle, AgentEndorse: proto.MustRaw(proto.Signed(endorse))}
	if err := saveLogin(agentHome, acfg, w); err != nil {
		return proto.Agent{}, err
	}
	// The agent trusts its owner's key from the start: it is the owner's
	// own device, set up by the owner.
	ts, err := trust.Open(agentHome)
	if err != nil {
		return proto.Agent{}, err
	}
	defer ts.Close()
	ow := ownerKey.PublicKey()
	if err := ts.PinKey(sid, cfg.UserID, ssh.FingerprintSHA256(ow), ow.Marshal(), trust.Verified); err != nil {
		return proto.Agent{}, err
	}
	return *w.Agent, nil
}

// newAgentKey makes the agent's ed25519 SSH key at path (0600), or loads
// the one a previous LoginAgent made.
func newAgentKey(path string) (ssh.Signer, error) {
	if b, err := os.ReadFile(path); err == nil {
		return ssh.ParsePrivateKey(b)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	blk, err := ssh.MarshalPrivateKey(priv, "tc agent")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(priv)
}

// IsAgentSession reports whether this client logs in as an agent.
func (c *Client) IsAgentSession() bool { return c.cfg.Agent != "" }

// AgentHandle is the handle this client logs in as ("" for a person). It
// follows renames.
func (c *Client) AgentHandle() string {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	return c.agentName
}

func (s *session) agent1() bool { return slices.Contains(s.welcome.Caps, proto.CapAgent) }

// RegisterAgent creates the caller's agent. Owner sessions only; one active
// agent per user.
func (c *Client) RegisterAgent(ctx context.Context, handle, icon string) (proto.Agent, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.Agent{}, err
	}
	var r proto.AgentRes
	err := c.call(ctx, proto.OpAgentRegister, proto.AgentRegisterArgs{Handle: handle, Icon: icon}, &r, nil)
	if err == nil {
		c.setAgent(r.Agent)
	}
	return r.Agent, err
}

// UpdateAgent renames or re-icons the caller's agent; "" keeps a field.
// The owner's sessions and the agent's own may call it (5 renames a day).
// A running agent follows a rename on its own.
func (c *Client) UpdateAgent(ctx context.Context, handle, icon string) (proto.Agent, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.Agent{}, err
	}
	var r proto.AgentRes
	err := c.call(ctx, proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: handle, Icon: icon}, &r, nil)
	if err == nil {
		c.setAgent(r.Agent)
		c.followRename(r.Agent)
	}
	return r.Agent, err
}

// ClearAgentIcon removes my agent's icon.
func (c *Client) ClearAgentIcon(ctx context.Context) (proto.Agent, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.Agent{}, err
	}
	var r proto.AgentRes
	err := c.call(ctx, proto.OpAgentUpdate, proto.AgentUpdateArgs{ClearIcon: true}, &r, nil)
	if err == nil {
		c.setAgent(r.Agent)
	}
	return r.Agent, err
}

// followRename makes an agent session use its new handle at once (signed
// Plain agent, mentions) and at its next login.
func (c *Client) followRename(a proto.Agent) {
	if a.Owner != c.cfg.UserID || !c.IsAgentSession() || a.Retired != 0 {
		return
	}
	c.agentsMu.Lock()
	renamed := a.Handle != c.agentName
	c.agentName = a.Handle
	c.agentsMu.Unlock()
	if renamed {
		cfg := c.cfg
		cfg.Agent = a.Handle
		_ = writeConfig(c.home, cfg)
	}
}

// RetireAgent retires the caller's agent; the server closes its sessions.
func (c *Client) RetireAgent(ctx context.Context) error {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return err
	}
	var r proto.AgentRes
	err := c.call(ctx, proto.OpAgentRetire, proto.Empty{}, &r, nil)
	if err == nil && r.Agent.Owner != 0 {
		c.setAgent(r.Agent)
	}
	return err
}

// FetchAgents loads every agent from the server into the cache.
func (c *Client) FetchAgents(ctx context.Context) ([]proto.Agent, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return nil, err
	}
	c.agentsMu.Lock()
	gen := c.agentGen
	c.agentsMu.Unlock()
	var r proto.AgentsRes
	if err := c.call(ctx, proto.OpAgentsList, proto.Empty{}, &r, nil); err != nil {
		return nil, err
	}
	c.replaceAgentsAt(r.Agents, gen)
	return r.Agents, nil
}

func (c *Client) replaceAgents(agents []proto.Agent) {
	c.agentsMu.Lock()
	gen := c.agentGen
	c.agentsMu.Unlock()
	c.replaceAgentsAt(agents, gen)
}

// replaceAgentsAt swaps in a list read when the cache was at gen. An
// agent.update applied since then is newer than the list, so the list is
// only merged in and never drops what that event added.
func (c *Client) replaceAgentsAt(agents []proto.Agent, gen uint64) {
	c.agentsMu.Lock()
	stale := c.agentGen != gen
	if !stale {
		clear(c.agents)
	}
	for _, a := range agents {
		if old, ok := c.agents[a.Owner]; stale && ok && (old.Handle != a.Handle || old.Retired != a.Retired) {
			continue
		}
		c.putAgentLocked(a)
	}
	c.agentsMu.Unlock()
	c.post(Update{Agents: true})
}

// putAgentLocked keeps the newest record per owner: an active agent over a
// retired one, a later retirement over an earlier one.
func (c *Client) putAgentLocked(a proto.Agent) {
	if a.Owner == 0 || !proto.ValidName(a.Handle) {
		return
	}
	if old, ok := c.agents[a.Owner]; ok && old.Retired == 0 && a.Retired != 0 && old.Handle != a.Handle {
		return
	}
	if len(a.Icon) > proto.MaxAgentIcon {
		a.Icon = ""
	}
	c.agents[a.Owner] = a
}

func (c *Client) setAgent(a proto.Agent) {
	c.agentsMu.Lock()
	if old, ok := c.agents[a.Owner]; ok && old.Retired == 0 && a.Retired == 0 {
		a.Online = a.Online || old.Online // events never carry it
	}
	c.putAgentLocked(a)
	c.agentsMu.Unlock()
	c.post(Update{Agents: true})
}

// Agents lists the active (non-retired) agents, by handle.
func (c *Client) Agents() []proto.Agent {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	out := make([]proto.Agent, 0, len(c.agents))
	for _, a := range c.agents {
		if a.Retired == 0 {
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b proto.Agent) int { return strings.Compare(a.Handle, b.Handle) })
	return out
}

// AgentOf is user's active agent.
func (c *Client) AgentOf(user uint64) (proto.Agent, bool) {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	a, ok := c.agents[user]
	return a, ok && a.Retired == 0
}

// AgentByHandle is the active agent with handle h.
func (c *Client) AgentByHandle(h string) (proto.Agent, bool) {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	for _, a := range c.agents {
		if a.Handle == h && a.Retired == 0 {
			return a, true
		}
	}
	return proto.Agent{}, false
}

// MyAgent is my active agent (also on the agent's own session).
func (c *Client) MyAgent() (proto.Agent, bool) { return c.AgentOf(c.cfg.UserID) }

// AgentAccess is the list of channels my agent may read besides its DM
// (also on the agent's own session).
func (c *Client) AgentAccess(ctx context.Context) ([]uint64, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return nil, err
	}
	var r proto.AgentAccessRes
	if err := c.call(ctx, proto.OpAgentAccess, proto.AgentAccessArgs{}, &r, nil); err != nil {
		return nil, err
	}
	c.noteAccess(r.Chans)
	return r.Chans, nil
}

// AgentAccessInfo is AgentAccess with the end of each temporary grant (a
// call by mention, proto.AgentGrantTTL); SetAgentAccess's add makes one lasting.
func (c *Client) AgentAccessInfo(ctx context.Context) (proto.AgentAccessRes, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.AgentAccessRes{}, err
	}
	var r proto.AgentAccessRes
	if err := c.call(ctx, proto.OpAgentAccess, proto.AgentAccessArgs{}, &r, nil); err != nil {
		return r, err
	}
	c.noteAccess(r.Chans)
	return r, nil
}

// SetAgentAccess adds (until removed) and removes channels from my agent's
// access list and returns the list after the change. Owner sessions only.
func (c *Client) SetAgentAccess(ctx context.Context, add, remove []uint64) ([]uint64, error) {
	if c.IsAgentSession() {
		return nil, proto.Errf(proto.ErrForbidden, "an agent can't change its own access")
	}
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return nil, err
	}
	var r proto.AgentAccessRes
	if err := c.call(ctx, proto.OpAgentAccess, proto.AgentAccessArgs{Add: add, Remove: remove}, &r, nil); err != nil {
		return nil, err
	}
	c.noteAccess(r.Chans)
	return r.Chans, nil
}

// noteAccess stores the access list we know of and, on the agent's
// session, loads the channels it just gained.
func (c *Client) noteAccess(chans []uint64) {
	var added []uint64
	err := c.tx(func(tx *sql.Tx) error {
		var err error
		added, err = storeAccess(tx, chans)
		return err
	})
	if err == nil && len(added) > 0 && c.IsAgentSession() {
		go c.loadGranted(added)
	}
}

// storeAccess records chans as the access list and returns those that are new.
func storeAccess(q dbtx, chans []uint64) ([]uint64, error) {
	old := knownAccess(q)
	b, err := json.Marshal(chans)
	if err != nil {
		return nil, err
	}
	if err := setMeta(q, "agent_access", string(b)); err != nil {
		return nil, err
	}
	var added []uint64
	for _, ch := range chans {
		if !slices.Contains(old, ch) {
			added = append(added, ch)
		}
	}
	return added, nil
}

func knownAccess(q dbtx) []uint64 {
	var s string
	if q.QueryRow(`SELECT v FROM meta WHERE k = 'agent_access'`).Scan(&s) != nil {
		return nil
	}
	var out []uint64
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

// loadGranted fetches the latest messages of channels the agent just
// gained and reports the recent ones that call it in Update.New: the ask
// that got the channel granted came before the agent could see it.
func (c *Client) loadGranted(chans []uint64) {
	ctx, cancel := context.WithTimeout(c.ctx, time.Minute)
	defer cancel()
	names := c.MentionNames()
	since := time.Now().Add(-agentCallWindow).UnixMilli()
	for _, ch := range chans {
		if _, err := c.History(ctx, ch, 0); err != nil {
			continue
		}
		var u Update
		rows, err := c.db.Query(`SELECT `+msgCols+`, sec FROM msgs WHERE chan = ? AND time >= ? AND deleted = 0 ORDER BY id`, ch, since)
		if err != nil {
			continue
		}
		for rows.Next() {
			var sec Sec
			m, err := scanMsg(rows, &sec)
			if err == nil && sec.Trusted() && m.Agent == "" && Mentioned(m.Body, names) {
				u.New = append(u.New, Message{Message: m, Sec: sec})
			}
		}
		rows.Close()
		u.msgs(ch)
		c.post(u)
	}
}

// grantOnMention gives my agent the channel of a message that calls it,
// for proto.AgentGrantTTL (§15.4): the owner's sessions do this because
// only they can read an encrypted channel's text. People (never bots or
// agents) call it in public channels; in private channels and DMs only I
// can. Called inside the event transaction.
func (c *Client) grantOnMention(q dbtx, m proto.Message, sec Sec) {
	if c.IsAgentSession() || !sec.Trusted() || m.Deleted || m.Agent != "" || time.Since(time.UnixMilli(m.Time)) > agentCallWindow {
		return
	}
	a, ok := c.MyAgent()
	if !ok || !Mentioned(m.Body, []string{a.Handle}) || slices.Contains(knownAccess(q), m.Chan) || c.User(m.Author).Bot {
		return
	}
	ch, ok, err := getChan(q, m.Chan)
	if err != nil || !ok || ch.Kind == proto.ChanAgentDM || ch.Kind != proto.ChanPublic && m.Author != c.cfg.UserID {
		return
	}
	c.agentsMu.Lock()
	busy := c.granting[m.Chan]
	c.granting[m.Chan] = true
	c.agentsMu.Unlock()
	if busy {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
		defer cancel()
		var r proto.AgentAccessRes
		if c.call(ctx, proto.OpAgentAccess, proto.AgentAccessArgs{Temp: []uint64{m.Chan}}, &r, nil) == nil {
			c.noteAccess(r.Chans)
		}
		c.agentsMu.Lock()
		delete(c.granting, m.Chan)
		c.agentsMu.Unlock()
	}()
}

// OpenAgentDM returns (creating if needed) my private chat with my agent.
func (c *Client) OpenAgentDM(ctx context.Context) (proto.Channel, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.Channel{}, err
	}
	var r proto.ChanRes
	err := c.call(ctx, proto.OpAgentDM, proto.Empty{}, &r, c.chanHook)
	c.kickE2E()
	return r.Chan, err
}

// AgentDM is my agent DM from the cache.
func (c *Client) AgentDM() (ChannelView, bool) {
	for _, ch := range c.Channels() {
		if ch.Kind == proto.ChanAgentDM && ch.Member {
			return ch, true
		}
	}
	return ChannelView{}, false
}

// SendAgentStatus publishes this agent's live status in thread of ch (0 =
// top level); preview is the reply so far. Agent sessions only. Updates
// faster than the server's 4/s coalesce: only the latest per thread goes
// out. In an encrypted channel the preview is sealed; without the key only
// the status goes out.
func (c *Client) SendAgentStatus(ch, thread uint64, state, status, preview string) error {
	if !c.IsAgentSession() {
		return ErrNotAgent
	}
	if !proto.ValidAgentState(state) {
		return proto.Errf(proto.ErrBadRequest, "unknown agent state "+state)
	}
	if len(status) > proto.MaxStatus {
		return proto.Errf(proto.ErrTooLarge, "status is over 128 bytes")
	}
	if len(preview) > proto.MaxAgentPreview {
		return proto.Errf(proto.ErrTooLarge, "preview is over 16 KiB")
	}
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s == nil {
		return ErrOffline
	}
	if !s.agent1() {
		return errors.New("this server doesn't support agents")
	}
	c.agentsMu.Lock()
	k := agentKey{ch, thread}
	if _, queued := c.agentPend[k]; !queued {
		c.agentOrder = append(c.agentOrder, k)
	}
	c.agentPend[k] = proto.Ephemeral{Kind: proto.EphAgent, Chan: ch, Thread: thread, State: state, Status: status, Text: preview}
	if c.agentTimer == nil {
		c.agentTimer = time.AfterFunc(max(0, time.Until(c.agentLast.Add(agentEvery))), c.flushAgentStatus)
	}
	c.agentsMu.Unlock()
	return nil
}

// flushAgentStatus sends the oldest queued status and schedules the next.
func (c *Client) flushAgentStatus() {
	c.agentsMu.Lock()
	c.agentTimer = nil
	if len(c.agentOrder) == 0 {
		c.agentsMu.Unlock()
		return
	}
	k := c.agentOrder[0]
	c.agentOrder = c.agentOrder[1:]
	e := c.agentPend[k]
	delete(c.agentPend, k)
	c.agentLast = time.Now()
	if len(c.agentOrder) > 0 {
		c.agentTimer = time.AfterFunc(agentEvery, c.flushAgentStatus)
	}
	handle := c.agentName
	c.agentsMu.Unlock()

	if c.needsEnc(e.Chan) {
		enc := c.sealPreview(e.Chan, e.Thread, handle, e.Text)
		e.Text, e.Enc = "", enc
	}
	c.mu.Lock()
	s := c.sess
	c.mu.Unlock()
	if s != nil {
		_ = s.send(proto.TEphemeral, e)
	}
}

// sealPreview encrypts text under ch's verified head epoch; nil when there
// is nothing to seal or we don't hold the key.
func (c *Client) sealPreview(ch, thread uint64, handle, text string) []byte {
	if text == "" {
		return nil
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return nil
	}
	head, ok, err := c.trust.ChainHead(sid, ch)
	if err != nil || !ok {
		return nil
	}
	ep, ok, err := c.trust.Epoch(sid, ch, head.Epoch)
	if err != nil || !ok || ep.Key == nil {
		return nil
	}
	dev, err := c.trust.DeviceKeys(sid, c.Me().ID)
	if err != nil {
		return nil
	}
	raw, err := proto.Marshal(plainAgent{T: "agent", TS: time.Now().UnixMilli(), Body: text, Agent: handle, Thread: thread})
	if err != nil {
		return nil
	}
	enc, err := e2ecrypto.Encrypt(rand.Reader, *ep.Key, dev.Ed25519, sid, ch, ep.Epoch,
		e2ecrypto.EpochHash(ep.Hash[:e2ecrypto.IDSize]), dev.ID, raw)
	if err != nil || len(enc) > proto.MaxAgentEnc {
		return nil
	}
	return enc
}

// openPreview decrypts a sealed preview: signed by a verified device of the
// agent's owner, in the roster of its epoch, naming the same agent and
// thread, and recent.
func (c *Client) openPreview(e proto.Ephemeral) string {
	if e.Enc == nil {
		return ""
	}
	sid, ok := c.cacheServerID()
	if !ok {
		return ""
	}
	memo := newKeyMemo()
	raw, _, sec, _ := c.openEnc(sid, e.Chan, e.User, 0, e.Enc, memo)
	if sec != SecE2E {
		return ""
	}
	if sec, _ := c.checkSealer(sid, e.Chan, e.User, 0, e.Enc, memo); sec != SecE2E {
		return ""
	}
	var p plainAgent
	if proto.Unmarshal(raw, &p) != nil || p.T != "agent" || p.Agent != e.Agent || p.Thread != e.Thread {
		return ""
	}
	if d := time.Since(time.UnixMilli(p.TS)); d > agentPreviewSkew || d < -agentPreviewSkew {
		return ""
	}
	return p.Body
}

// agentEphemeral applies an EphAgent or EphAgentPresence.
func (c *Client) agentEphemeral(e proto.Ephemeral) {
	if e.User == 0 {
		return
	}
	if e.Kind == proto.EphAgentPresence {
		c.agentsMu.Lock()
		c.agentPres[e.User] = e.State == proto.PresOnline // the agent's record may come after this
		a, ok := c.agents[e.User]
		if ok {
			a.Online = e.State == proto.PresOnline
			c.agents[e.User] = a
		}
		c.agentsMu.Unlock()
		if ok {
			c.post(Update{Agents: true})
		}
		if e.State != proto.PresOnline {
			c.clearLiveOf(e.User) // a gone agent isn't working on anything
		}
		return
	}
	if e.Chan == 0 || !proto.ValidAgentState(e.State) {
		return
	}
	if cv, ok := c.Channel(e.Chan); !ok || !cv.Member {
		return
	}
	a, ok := c.AgentOf(e.User)
	if !ok || a.Handle != e.Agent {
		return
	}
	var text string
	if c.needsEnc(e.Chan) {
		text = c.openPreview(e) // never plaintext in an encrypted channel
	} else if e.Enc == nil {
		text = e.Text
	}
	if len(text) > proto.MaxAgentPreview || !validPreview(text) {
		text = ""
	}
	status := e.Status
	if len(status) > proto.MaxStatus {
		status = strings.ToValidUTF8(status[:proto.MaxStatus], "")
	}
	k := agentKey{e.Chan, e.Thread}
	now := time.Now()
	l := AgentLive{Agent: a, State: e.State, Status: status, Preview: text, At: now}
	c.agentsMu.Lock()
	m := c.agentLive[k]
	if m == nil {
		m = map[uint64]AgentLive{}
		c.agentLive[k] = m
	}
	m[e.User] = l
	c.agentsMu.Unlock()
	c.post(Update{AgentStatus: map[uint64]bool{e.Chan: true}})
	ttl := agentLiveTTL
	if e.State == proto.AgentDone || e.State == proto.AgentError {
		ttl = agentDoneTTL
	}
	owner := e.User
	time.AfterFunc(ttl, func() {
		c.agentsMu.Lock()
		cur, ok := c.agentLive[k][owner]
		gone := ok && cur.At.Equal(now)
		if gone {
			delete(c.agentLive[k], owner)
			if len(c.agentLive[k]) == 0 {
				delete(c.agentLive, k)
			}
		}
		c.agentsMu.Unlock()
		if gone {
			c.post(Update{AgentStatus: map[uint64]bool{k.ch: true}})
		}
	})
}

func validPreview(s string) bool { return s == "" || validText(s) }

// clearLiveOf drops owner's agent's live statuses.
func (c *Client) clearLiveOf(owner uint64) {
	chans := map[uint64]bool{}
	c.agentsMu.Lock()
	for k, m := range c.agentLive {
		if _, ok := m[owner]; ok {
			delete(m, owner)
			chans[k.ch] = true
			if len(m) == 0 {
				delete(c.agentLive, k)
			}
		}
	}
	c.agentsMu.Unlock()
	if len(chans) > 0 {
		c.post(Update{AgentStatus: chans})
	}
}

// clearAgentLive drops every live status (the connection ended).
func (c *Client) clearAgentLive() {
	c.agentsMu.Lock()
	chans := map[uint64]bool{}
	for k := range c.agentLive {
		chans[k.ch] = true
	}
	clear(c.agentLive)
	for o, a := range c.agents {
		a.Online = false
		c.agents[o] = a
	}
	c.agentsMu.Unlock()
	if len(chans) > 0 {
		c.post(Update{AgentStatus: chans, Agents: true})
	}
}

// applyAgentEvent applies agent.update and agent.access (inside the event
// transaction).
func (c *Client) applyAgentEvent(q dbtx, ev proto.Event, u *Update) {
	switch ev.Kind {
	case proto.KAgentUpdate:
		var a proto.Agent
		if proto.Unmarshal(ev.Data, &a) != nil {
			return
		}
		c.agentsMu.Lock()
		c.agentGen++
		old, had := c.agents[a.Owner]
		if had && old.Retired == 0 && a.Retired == 0 {
			a.Online = old.Online // events never carry it
		} else if !had && a.Retired == 0 {
			a.Online = c.agentPres[a.Owner]
		}
		c.putAgentLocked(a)
		c.agentsMu.Unlock()
		if had && (a.Retired != 0 || old.Handle != a.Handle) {
			c.clearLiveOf(a.Owner)
		}
		c.followRename(a)
		u.Agents = true
	case proto.KAgentAccess:
		var r proto.AgentAccessRes
		if proto.Unmarshal(ev.Data, &r) != nil || ev.Actor != c.cfg.UserID {
			return
		}
		added, err := storeAccess(q, r.Chans)
		if err == nil && len(added) > 0 && c.IsAgentSession() {
			go c.loadGranted(added)
		}
		u.Channels = true
	}
}

// agentAfterSync refreshes the agents (with who's online) and the access
// list after each sync: incremental syncs carry neither.
func (c *Client) agentAfterSync(ctx context.Context) {
	if _, err := c.FetchAgents(ctx); err != nil {
		return
	}
	if c.IsAgentSession() {
		_, _ = c.AgentAccess(ctx)
		return
	}
	if _, ok := c.MyAgent(); ok {
		_, _ = c.AgentAccess(ctx)
	}
}

// AgentStatus lists the agents live in thread of ch (0 = top level), by
// handle.
func (c *Client) AgentStatus(ch, thread uint64) []AgentLive {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	m := c.agentLive[agentKey{ch, thread}]
	out := make([]AgentLive, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b AgentLive) int { return strings.Compare(a.Agent.Handle, b.Agent.Handle) })
	return out
}

// AgentThreads lists the threads of ch (0 = top level) with a live agent,
// ascending.
func (c *Client) AgentThreads(ch uint64) []uint64 {
	c.agentsMu.Lock()
	defer c.agentsMu.Unlock()
	var out []uint64
	for k, m := range c.agentLive {
		if k.ch == ch && len(m) > 0 {
			out = append(out, k.thread)
		}
	}
	slices.Sort(out)
	return out
}

// agentDevice reports whether d is an agent's device: its cert is signed by
// a key pinned as an agent key (§15.1).
func (c *Client) agentDevice(sid e2ecrypto.ServerID, d trust.Device) bool {
	var s proto.Signed
	if proto.Unmarshal(d.Cert, &s) != nil {
		return false
	}
	dc, err := identity.PeekDeviceCert(identity.Signed(s))
	if err != nil {
		return false
	}
	keys, _ := c.trust.PinnedKeys(sid, d.User)
	for _, k := range keys {
		if bytes.Equal(k.Wire, dc.SSH) {
			return k.How == trust.Agent
		}
	}
	return false
}

// agentMsg reports whether the cached message id was sent by an agent.
func agentMsg(q dbtx, id uint64) bool {
	var a string
	return q.QueryRow(`SELECT agent FROM msgs WHERE id = ?`, id).Scan(&a) == nil && a != ""
}
