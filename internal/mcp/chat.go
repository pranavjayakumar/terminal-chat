package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
)

// Chat is the part of *client.Client the tools read.
type Chat interface {
	Me() proto.User
	User(id uint64) proto.User
	Channels() []client.ChannelView
	Channel(id uint64) (client.ChannelView, bool)
	Messages(ch uint64, limit int) []client.Message
	Thread(root uint64) []client.Message
	Search(ctx context.Context, q string, ch uint64) ([]proto.Message, error)
	AgentOf(user uint64) (proto.Agent, bool)
}

// VisitorMark starts every post made through the visitor `tc mcp`, so
// people can tell the owner didn't type it.
const VisitorMark = "⇢ "

// ResolveChan finds a channel by "#name", "name", its id, or "dm" (the
// agent DM). It only finds channels in the cache.
func ResolveChan(c Chat, s string) (client.ChannelView, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return client.ChannelView{}, errors.New("which channel?")
	}
	if id, err := strconv.ParseUint(s, 10, 64); err == nil {
		if cv, ok := c.Channel(id); ok {
			return cv, nil
		}
		return client.ChannelView{}, fmt.Errorf("no channel %d", id)
	}
	if s == "dm" {
		for _, cv := range c.Channels() {
			if cv.Kind == proto.ChanAgentDM {
				return cv, nil
			}
		}
		return client.ChannelView{}, errors.New("no agent DM")
	}
	name := strings.TrimPrefix(s, "#")
	for _, cv := range c.Channels() {
		if cv.Name == name && cv.Kind != proto.ChanDM && cv.Kind != proto.ChanAgentDM {
			return cv, nil
		}
	}
	return client.ChannelView{}, fmt.Errorf("no channel #%s", sanitize.Line(name))
}

// ChanLabel is how a tool names a channel to the model.
func ChanLabel(cv client.ChannelView) string {
	switch cv.Kind {
	case proto.ChanAgentDM:
		return "your DM with your owner"
	case proto.ChanDM:
		return "DM " + sanitize.Line(cv.Display)
	}
	return "#" + sanitize.Line(cv.Name)
}

// Author names a message's sender, agents with their owner.
func Author(c Chat, m proto.Message) string {
	name := sanitize.Line(c.User(m.Author).Name)
	if name == "" {
		name = fmt.Sprintf("user%d", m.Author)
	}
	if m.Agent != "" {
		return sanitize.Line(m.Agent) + " (" + name + "'s agent)"
	}
	return name
}

// Readable reports whether a message's text can be trusted to be what its
// author sent: plaintext channels and verified encrypted messages.
func Readable(m client.Message) bool {
	return !m.Deleted && m.Sec.Trusted()
}

// FormatMessages renders messages for the model, one per line:
// "[id] 2026-10-24 15:04 alice: text".
func FormatMessages(c Chat, msgs []client.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.ID == 0 {
			continue // still in our outbox
		}
		body := sanitize.Text(m.Body)
		switch {
		case m.Deleted:
			body = "(deleted)"
		case m.Sec == client.SecWaiting:
			body = "(encrypted, can't read it yet)"
		case !Readable(m):
			body = "(unverified, not shown)"
		}
		t := time.UnixMilli(m.Time).Local().Format("2006-01-02 15:04")
		fmt.Fprintf(&b, "[%d] %s %s: %s", m.ID, t, Author(c, m.Message), strings.ReplaceAll(body, "\n", "\n    "))
		if m.Replies > 0 {
			fmt.Fprintf(&b, "  (%d replies)", m.Replies)
		}
		b.WriteByte('\n')
	}
	if b.Len() == 0 {
		return "(no messages)"
	}
	return b.String()
}

// FormatSearch renders search hits.
func FormatSearch(c Chat, hits []proto.Message) string {
	var b strings.Builder
	for _, m := range hits {
		if m.Deleted {
			continue
		}
		label := fmt.Sprintf("%d", m.Chan)
		if cv, ok := c.Channel(m.Chan); ok {
			label = ChanLabel(cv)
		}
		t := time.UnixMilli(m.Time).Local().Format("2006-01-02 15:04")
		where := label
		if m.Thread != 0 {
			where += fmt.Sprintf(" thread %d", m.Thread)
		}
		fmt.Fprintf(&b, "[%d] %s %s in %s: %s\n", m.ID, t, Author(c, m), where, strings.ReplaceAll(sanitize.Text(m.Body), "\n", "\n    "))
	}
	if b.Len() == 0 {
		return "(nothing found)"
	}
	return b.String()
}

// Limit clamps a requested count.
func Limit(n, def, most int) int {
	switch {
	case n <= 0:
		return def
	case n > most:
		return most
	}
	return n
}

// Args decodes tool arguments, refusing unknown fields' wrong types.
func Args(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("bad arguments: %v", err)
	}
	return nil
}

// LastN keeps the last n messages.
func LastN(ms []client.Message, n int) []client.Message {
	if len(ms) > n {
		return ms[len(ms)-n:]
	}
	return ms
}
