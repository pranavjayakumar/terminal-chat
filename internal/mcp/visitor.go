package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// VisitorChat is what the visitor needs from the owner's client.
type VisitorChat interface {
	Chat
	Send(ch uint64, body string, thread uint64) error
	Flush(ctx context.Context) error
}

// Visitor serves an outside Claude session from the owner's tc home. It
// reads only the channels the owner allowed for agents, and posts as the
// owner with VisitorMark.
type Visitor struct {
	Chat VisitorChat
	// Allowed lists the channels the owner allowed in the agent panel. It
	// is asked on every call, so a change in the panel applies at once.
	Allowed func(ctx context.Context) ([]uint64, error)
}

// Call implements CallFunc.
func (v *Visitor) Call(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	switch name {
	case ToolWhoami:
		me := v.Chat.Me()
		return fmt.Sprintf("You are a visitor in tc, acting for %s. Posts go out as %s, marked ⇢. You can read only the channels %s allowed for agents.",
			sanitize.Line(me.Name), sanitize.Line(me.Name), sanitize.Line(me.Name)), nil
	case ToolListChannels:
		allowed, err := v.allowed(ctx)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, cv := range v.Chat.Channels() {
			if allowed[cv.ID] {
				fmt.Fprintf(&b, "%s (id %d)", ChanLabel(cv), cv.ID)
				if cv.Topic != "" {
					b.WriteString(": " + sanitize.Line(cv.Topic))
				}
				b.WriteByte('\n')
			}
		}
		if b.Len() == 0 {
			return "No channels are allowed yet: the owner picks them in tc's agent panel (Access).", nil
		}
		return b.String(), nil
	case ToolReadChannel:
		var a struct {
			Chan  string `json:"chan"`
			Limit int    `json:"limit"`
		}
		if err := Args(raw, &a); err != nil {
			return "", err
		}
		cv, err := v.channel(ctx, a.Chan)
		if err != nil {
			return "", err
		}
		return FormatMessages(v.Chat, v.Chat.Messages(cv.ID, Limit(a.Limit, 30, 200))), nil
	case ToolReadThread:
		var a struct {
			Chan   string `json:"chan"`
			Thread uint64 `json:"thread"`
			Limit  int    `json:"limit"`
		}
		if err := Args(raw, &a); err != nil {
			return "", err
		}
		cv, err := v.channel(ctx, a.Chan)
		if err != nil {
			return "", err
		}
		return ReadThread(v.Chat, cv.ID, a.Thread, Limit(a.Limit, 50, 200))
	case ToolSearch:
		var a struct {
			Q    string `json:"q"`
			Chan string `json:"chan"`
		}
		if err := Args(raw, &a); err != nil {
			return "", err
		}
		allowed, err := v.allowed(ctx)
		if err != nil {
			return "", err
		}
		var ch uint64
		if a.Chan != "" {
			cv, err := v.channel(ctx, a.Chan)
			if err != nil {
				return "", err
			}
			ch = cv.ID
		}
		hits, err := v.Chat.Search(ctx, a.Q, ch)
		if err != nil {
			return "", err
		}
		hits = slices.DeleteFunc(hits, func(m proto.Message) bool { return !allowed[m.Chan] })
		return FormatSearch(v.Chat, hits), nil
	case ToolPostReply:
		var a struct {
			Chan   string `json:"chan"`
			Thread uint64 `json:"thread"`
			Text   string `json:"text"`
		}
		if err := Args(raw, &a); err != nil {
			return "", err
		}
		cv, err := v.channel(ctx, a.Chan)
		if err != nil {
			return "", err
		}
		text := strings.TrimSpace(sanitize.StripANSI(a.Text))
		if text == "" {
			return "", errors.New("nothing to post")
		}
		body := VisitorMark + text
		if len(body) > proto.MaxBody {
			return "", fmt.Errorf("too long: %d bytes, the limit is %d", len(body), proto.MaxBody)
		}
		if a.Thread != 0 && !inChan(v.Chat, a.Thread, cv.ID) {
			return "", fmt.Errorf("no thread %d in %s", a.Thread, ChanLabel(cv))
		}
		if err := v.Chat.Send(cv.ID, body, a.Thread); err != nil {
			return "", err
		}
		if err := v.Chat.Flush(ctx); err != nil {
			return "queued; it goes out when tc is back online", nil
		}
		return "posted", nil
	}
	return "", fmt.Errorf("unknown tool %s", name)
}

func (v *Visitor) allowed(ctx context.Context) (map[uint64]bool, error) {
	ids, err := v.Allowed(ctx)
	if err != nil {
		return nil, fmt.Errorf("can't read the allowed channels: %w", err)
	}
	out := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		// Never a DM or the agent DM, even if listed.
		if cv, ok := v.Chat.Channel(id); ok && cv.Kind != proto.ChanDM && cv.Kind != proto.ChanAgentDM {
			out[id] = true
		}
	}
	return out, nil
}

func (v *Visitor) channel(ctx context.Context, s string) (client.ChannelView, error) {
	cv, err := ResolveChan(v.Chat, s)
	if err != nil {
		return cv, err
	}
	allowed, err := v.allowed(ctx)
	if err != nil {
		return cv, err
	}
	if !allowed[cv.ID] {
		return cv, fmt.Errorf("%s isn't in the channels the owner allowed for agents", ChanLabel(cv))
	}
	return cv, nil
}

// inChan reports whether root is a cached top-level message of ch.
func inChan(c Chat, root, ch uint64) bool {
	t := c.Thread(root)
	return len(t) > 0 && t[0].ID == root && t[0].Chan == ch && t[0].Thread == 0
}

// ReadThread renders a thread of ch: its root and the last limit-1 replies.
func ReadThread(c Chat, ch, root uint64, limit int) (string, error) {
	if !inChan(c, root, ch) {
		return "", fmt.Errorf("no thread %d in that channel (use the id of its first message)", root)
	}
	t := c.Thread(root)
	if len(t) > limit {
		t = append(t[:1:1], t[len(t)-(limit-1):]...)
	}
	return FormatMessages(c, t), nil
}
