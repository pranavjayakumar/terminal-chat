package client

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/skailama/tc/internal/proto"
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

// LoginAgent logs agentHome in as the agent handle of the user logged in at
// ownerHome, with the same SSH key: a new device of that user whose
// sessions carry HELLO agent = handle. The agent must already be registered
// (RegisterAgent).
func LoginAgent(ctx context.Context, ownerHome, agentHome, handle string) (proto.Agent, error) {
	owner, err := Open(ownerHome)
	if err != nil {
		return proto.Agent{}, err
	}
	cfg := owner.Config()
	owner.Close()
	if !proto.ValidAgentHandle(handle) {
		return proto.Agent{}, fmt.Errorf("invalid agent handle %q", handle)
	}
	cfg.Agent = handle
	if err := ensurePrivateDir(agentHome); err != nil {
		return proto.Agent{}, fmt.Errorf("creating %s: %w", agentHome, err)
	}
	if err := writeConfig(agentHome, cfg); err != nil {
		return proto.Agent{}, err
	}
	return proto.Agent{Handle: handle, Owner: cfg.UserID}, nil
}

// IsAgentSession reports whether this client logs in as an agent.
func (c *Client) IsAgentSession() bool { return c.cfg.Agent != "" }

// AgentHandle is the handle this client logs in as ("" for a person).
func (c *Client) AgentHandle() string { return c.cfg.Agent }

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
func (c *Client) UpdateAgent(ctx context.Context, handle, icon string) (proto.Agent, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return proto.Agent{}, err
	}
	var r proto.AgentRes
	err := c.call(ctx, proto.OpAgentUpdate, proto.AgentUpdateArgs{Handle: handle, Icon: icon}, &r, nil)
	if err == nil {
		c.setAgent(r.Agent)
	}
	return r.Agent, err
}

// RetireAgent retires the caller's agent; its sessions are closed.
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
	var r proto.AgentsRes
	if err := c.call(ctx, proto.OpAgentsList, proto.Empty{}, &r, nil); err != nil {
		return nil, err
	}
	c.agentsMu.Lock()
	clear(c.agents)
	for _, a := range r.Agents {
		c.agents[a.Owner] = a
	}
	c.agentsMu.Unlock()
	c.post(Update{Agents: true})
	return r.Agents, nil
}

func (c *Client) setAgent(a proto.Agent) {
	c.agentsMu.Lock()
	c.agents[a.Owner] = a
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
	slices.SortFunc(out, func(a, b proto.Agent) int {
		switch {
		case a.Handle < b.Handle:
			return -1
		case a.Handle > b.Handle:
			return 1
		}
		return 0
	})
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

// AgentAccess is the list of channels my agent may see besides its DM.
func (c *Client) AgentAccess(ctx context.Context) ([]uint64, error) {
	return c.SetAgentAccess(ctx, nil, nil)
}

// SetAgentAccess adds and removes channels from my agent's access list and
// returns the list after the change.
func (c *Client) SetAgentAccess(ctx context.Context, add, remove []uint64) ([]uint64, error) {
	if err := c.requireCap(proto.CapAgent, "agents"); err != nil {
		return nil, err
	}
	var r proto.AgentAccessRes
	err := c.call(ctx, proto.OpAgentAccess, proto.AgentAccessArgs{Add: add, Remove: remove}, &r, nil)
	return r.Chans, err
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
		if ch.Kind == proto.ChanAgentDM {
			return ch, true
		}
	}
	return ChannelView{}, false
}

// SendAgentStatus publishes this agent's live status in thread of ch (0 =
// top level). Agent sessions only; preview is the reply so far.
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
	if !slices.Contains(s.welcome.Caps, proto.CapAgent) {
		return errors.New("this server doesn't support agents")
	}
	e := proto.Ephemeral{Kind: proto.EphAgent, Chan: ch, Thread: thread, State: state, Status: status, Text: preview}
	if err := s.send(proto.TEphemeral, e); err != nil {
		return ErrOffline
	}
	return nil
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
	slices.SortFunc(out, func(a, b AgentLive) int {
		switch {
		case a.Agent.Handle < b.Agent.Handle:
			return -1
		case a.Agent.Handle > b.Agent.Handle:
			return 1
		}
		return 0
	})
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
