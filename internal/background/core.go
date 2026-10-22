package background

import (
	"time"

	"github.com/skailama/tc/internal/alert"
	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

// Source is the part of *client.Client the notifier reads. Everything is
// local (the cache); nothing here writes, and in particular nothing marks
// a message read.
type Source interface {
	Me() proto.User
	Channels() []client.ChannelView
	Users() []proto.User
	Groups() []proto.Group
	Highlights() []string
	NotifyPrefs() client.NotifyPrefs
	InMyThread(m proto.Message) bool
	Messages(ch uint64, limit int) []client.Message
	Thread(root uint64) []client.Message
}

// huddleSource is the optional roster lookup (*client.Client has it).
type huddleSource interface {
	Huddle(ch uint64) []uint64
}

var (
	_ Source       = (*client.Client)(nil)
	_ huddleSource = (*client.Client)(nil)
)

// pageLimit matches the chat UI's page, for re-reading a pending message.
const pageLimit = 100

// world answers the engine from a snapshot of the cache, refreshed before
// every engine call. Nobody is looking at a channel and this process is in
// no huddle: there is no window.
type world struct {
	src   Source
	me    proto.User
	chans map[uint64]client.ChannelView
	users map[uint64]proto.User
	names []string
	words []string
}

func (w *world) refresh() {
	w.me = w.src.Me()
	w.chans = map[uint64]client.ChannelView{}
	for _, v := range w.src.Channels() {
		w.chans[v.ID] = v
	}
	w.users = map[uint64]proto.User{}
	for _, u := range w.src.Users() {
		w.users[u.ID] = u
	}
	w.names = client.MentionNamesOf(w.me, w.src.Groups())
	w.words = w.src.Highlights()
}

func (w *world) Me() uint64 { return w.me.ID }
func (w *world) Chan(id uint64) (client.ChannelView, bool) {
	v, ok := w.chans[id]
	return v, ok
}
func (w *world) User(id uint64) proto.User       { return w.users[id] }
func (w *world) MentionNames() []string          { return w.names }
func (w *world) Highlights() []string            { return w.words }
func (w *world) InMyThread(x proto.Message) bool { return w.src.InMyThread(x) }
func (w *world) Looking(uint64) bool             { return false }
func (w *world) InHuddle(uint64) bool            { return false }
func (w *world) Roster(ch uint64) ([]uint64, bool) {
	if h, ok := w.src.(huddleSource); ok {
		return h.Huddle(ch), true
	}
	return nil, false
}

func (w *world) Current(x proto.Message) (proto.Message, bool) {
	var list []client.Message
	if x.Thread != 0 {
		list = w.src.Thread(x.Thread)
	} else {
		list = w.src.Messages(x.Chan, pageLimit)
	}
	for _, c := range list {
		if c.ID == x.ID && x.ID != 0 {
			return c.Message, true
		}
	}
	return proto.Message{}, false
}

// Core is the notifier without the connection: updates in, notifications
// and timers out. One goroutine owns it.
type Core struct {
	w      *world
	Eng    *alert.Engine
	env    alert.Env
	uiOpen func() bool
	out    alert.Sender
	// Sent is called after each notification the backend took (status).
	Sent func()
}

// NewCore notifies through out unless uiOpen says a chat window is open.
func NewCore(src Source, out alert.Sender, env alert.Env, uiOpen func() bool, now func() time.Time) *Core {
	c := &Core{w: &world{src: src}, env: env, uiOpen: uiOpen, out: out}
	c.Eng = alert.New(c.w, gate{c}, now)
	return c
}

// gate is the engine's Sender: it re-checks the chat window right before
// every notification, since one may have opened while a window waited.
type gate struct{ c *Core }

func (g gate) Send(n notify.Note) bool {
	if g.c.uiOpen() || g.c.out == nil {
		return false
	}
	ok := g.c.out.Send(n)
	if ok && g.c.Sent != nil {
		g.c.Sent()
	}
	return ok
}

func (c *Core) refresh() {
	c.w.refresh()
	c.Eng.SetPrefs(c.env.Apply(c.w.src.NotifyPrefs()))
}

// Handle runs one client update through the policy. While a chat window is
// open it does nothing but forget what was pending: the window notifies.
func (c *Core) Handle(u client.Update) []alert.Timer {
	if len(u.New) == 0 && len(u.HuddleStarted) == 0 && !u.Huddle && !u.Prefs && !u.Reset {
		return nil
	}
	if c.uiOpen() {
		c.quiet()
		return nil
	}
	c.refresh()
	ts := c.Eng.Messages(u.New)
	ts = append(ts, c.Eng.HuddleStarts(u.HuddleStarted)...)
	c.Eng.CheckRing()
	return ts
}

// quiet forgets pending windows and the ring: the chat window has them.
func (c *Core) quiet() {
	clear(c.Eng.Notes)
	c.Eng.StopRing(false)
}

// Fire runs a timer the engine asked for.
func (c *Core) Fire(t alert.Timer) []alert.Timer {
	if c.uiOpen() {
		// Drop the window or ring this timer belonged to: left open with
		// its timer gone, it would swallow the channel's next messages
		// (or keep "ringing" forever).
		c.quiet()
		return nil
	}
	c.refresh()
	switch t.Kind {
	case alert.FlushTimer:
		return c.Eng.Flush(t.Ch, t.Gen)
	case alert.RingResendT:
		c.Eng.RingTimer(t.Gen, true)
	case alert.RingEndT:
		c.Eng.RingTimer(t.Gen, false)
	}
	return nil
}
