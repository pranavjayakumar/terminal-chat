package tui

import (
	"context"
	"slices"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
)

// fakeAgents is the agent part of fakeClient.
type fakeAgents struct {
	agents  []proto.Agent
	live    map[[2]uint64][]client.AgentLive // (ch, thread) → live
	access  []uint64
	dmID    uint64 // the agent DM OpenAgentDM creates
	regErr  error
	reacted []string // "msg emoji", in order
}

func (f *fakeClient) Agents() []proto.Agent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ag.agents)
}

func (f *fakeClient) AgentStatus(ch, thread uint64) []client.AgentLive {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ag.live[[2]uint64{ch, thread}])
}

func (f *fakeClient) AgentThreads(ch uint64) []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []uint64
	for k, v := range f.ag.live {
		if k[0] == ch && len(v) > 0 {
			out = append(out, k[1])
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeClient) AgentDM() (client.ChannelView, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.chans {
		if v.Kind == proto.ChanAgentDM {
			return v, true
		}
	}
	return client.ChannelView{}, false
}

func (f *fakeClient) OpenAgentDM(ctx context.Context) (proto.Channel, error) {
	f.record("agentdm")
	if v, ok := f.AgentDM(); ok {
		return v.Channel, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := f.ag.dmID
	if id == 0 {
		id = 90
	}
	ch := proto.Channel{ID: id, Kind: proto.ChanAgentDM, Members: []uint64{f.me.ID}}
	f.chans = append(f.chans, client.ChannelView{Channel: ch, Member: true})
	return ch, nil
}

func (f *fakeClient) RegisterAgent(ctx context.Context, handle, icon string) (proto.Agent, error) {
	f.record("register " + handle + " " + icon)
	if f.ag.regErr != nil {
		return proto.Agent{}, f.ag.regErr
	}
	a := proto.Agent{Handle: handle, Icon: icon, Owner: f.me.ID, Created: 1}
	f.mu.Lock()
	f.ag.agents = append(f.ag.agents, a)
	f.mu.Unlock()
	return a, nil
}

func (f *fakeClient) UpdateAgent(ctx context.Context, handle, icon string) (proto.Agent, error) {
	f.record("updateagent " + handle + " " + icon)
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, a := range f.ag.agents {
		if a.Owner == f.me.ID {
			if handle != "" {
				a.Handle = handle
			}
			if icon != "" {
				a.Icon = icon
			}
			f.ag.agents[i] = a
			return a, nil
		}
	}
	return proto.Agent{}, &proto.Error{Code: proto.ErrNotFound}
}

func (f *fakeClient) AgentAccess(ctx context.Context) ([]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.ag.access), nil
}

func (f *fakeClient) SetAgentAccess(ctx context.Context, add, remove []uint64) ([]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range add {
		if !slices.Contains(f.ag.access, c) {
			f.ag.access = append(f.ag.access, c)
		}
	}
	f.ag.access = slices.DeleteFunc(f.ag.access, func(c uint64) bool { return slices.Contains(remove, c) })
	return slices.Clone(f.ag.access), nil
}
