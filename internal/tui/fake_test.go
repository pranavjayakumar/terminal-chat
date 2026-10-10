package tui

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

// fakeClient is an in-memory Backend.
type fakeClient struct {
	mu       sync.Mutex
	me       proto.User
	status   client.Status
	users    []proto.User
	chans    []client.ChannelView
	msgs     map[uint64][]client.Message
	threads  map[uint64][]client.Message
	typing   map[uint64][]uint64
	updates  chan client.Update
	sent     []client.Message
	reads    map[uint64]uint64
	history  int
	histMore bool
	calls    []string
	aways    []bool
	drafts   map[[2]uint64]string
	words    []string
	setErr   error // SetDraft fails with this
	seen     int64 // activity seen time
	activity []client.ActivityItem
	pinOrder []uint64 // pinned message ids, newest pin first
	e2eSt    map[uint64]client.E2EStatus
	addErr   error

	groups    []proto.Group
	reminders []proto.Reminder
	reminded  []remindCall
	remindErr error
	nextRemID uint64
}

type remindCall struct {
	at   time.Time
	text string
	ch   uint64
}

func newFake() *fakeClient {
	f := &fakeClient{
		me:      proto.User{ID: 1, Name: "alice"},
		status:  client.Status{State: client.Online, Transport: "quic"},
		msgs:    map[uint64][]client.Message{},
		threads: map[uint64][]client.Message{},
		typing:  map[uint64][]uint64{},
		updates: make(chan client.Update, 1),
		reads:   map[uint64]uint64{},
	}
	close(f.updates) // waitUpdate returns at once in tests
	f.users = []proto.User{
		{ID: 1, Name: "alice"}, {ID: 2, Name: "bob"}, {ID: 3, Name: "carol", Display: "Carol C"},
		{ID: 4, Name: "averyveryverylongusername"},
	}
	f.chans = []client.ChannelView{
		{Channel: proto.Channel{ID: 11, Name: "backend", Kind: proto.ChanPublic, Members: []uint64{1, 2}}, Display: "backend", Member: true, Unread: 3, Last: 300},
		{Channel: proto.Channel{ID: 10, Name: "general", Kind: proto.ChanPublic, Topic: "team-wide chatter about everything that matters and much more text to truncate", Members: []uint64{1, 2, 3}}, Display: "general", Member: true, Unread: 2, Last: 0},
		{Channel: proto.Channel{ID: 12, Name: "random", Kind: proto.ChanPublic, Members: []uint64{2}}, Display: "random"},
		{Channel: proto.Channel{ID: 13, Name: "secret", Kind: proto.ChanPrivate, Members: []uint64{1, 3}}, Display: "secret", Member: true, Unread: 1, Mention: true, Last: 400},
		{Channel: proto.Channel{ID: 20, Kind: proto.ChanDM, Members: []uint64{1, 2}}, Display: "bob", Member: true},
	}
	base := time.Date(2026, 10, 8, 10, 40, 0, 0, time.Local).UnixMilli()
	var ms []client.Message
	add := func(id, author uint64, dt time.Duration, body string, mod func(*client.Message)) {
		x := client.Message{Message: proto.Message{ID: id, Chan: 10, Author: author, Body: body, Time: base + dt.Milliseconds()}}
		if mod != nil {
			mod(&x)
		}
		ms = append(ms, x)
	}
	add(100, 2, 0, "morning! anyone seen the deploy logs from last night?", nil)
	add(101, 2, time.Minute, "also **bold** and _italic_ and `code` and https://example.com/path.", nil)
	add(102, 3, 2*time.Minute, "yep, pasting:\n```go\npanic: nil map write at handler.go:42 with a very long trailing line that must be truncated\n  at main.go:7\n```", nil)
	add(103, 1, 3*time.Minute, "👍 nice 你好世界你好世界你好世界你好世界 👨‍👩‍👧 family", func(x *client.Message) {
		x.Reactions = map[string][]uint64{"👍": {1, 2}, "🎉": {3}}
		x.Replies = 2
	})
	add(104, 2, 4*time.Minute, "hey @alice can you look? \x1b[31mred\x1b]0;pwned\a ‮evil", nil)
	add(105, 3, 30*time.Minute, "edited body", func(x *client.Message) { x.Edited = base })
	add(106, 4, 31*time.Minute, "", func(x *client.Message) { x.Deleted = true })
	add(107, 2, 24*time.Hour, "next day: "+string(bytes.Repeat([]byte("loooong"), 40)), nil)
	for i := 0; i < 30; i++ {
		add(uint64(110+i), uint64(2+i%2), 25*time.Hour+time.Duration(i)*time.Minute, "filler message number "+string(rune('a'+i%26)), nil)
	}
	ms = append(ms, client.Message{Message: proto.Message{Chan: 10, Author: 1, Body: "pending one", Time: base + 26*time.Hour.Milliseconds()}, Pending: true, Local: 7})
	ms = append(ms, client.Message{Message: proto.Message{Chan: 10, Author: 1, Body: "failed one", Time: base + 26*time.Hour.Milliseconds()}, Failed: "forbidden: nope", Local: 8})
	f.msgs[10] = ms
	f.chans[1].Last = 139
	f.threads[103] = []client.Message{ms[3],
		{Message: proto.Message{ID: 200, Chan: 10, Thread: 103, Author: 2, Body: "reply one", Time: base + 5*time.Minute.Milliseconds()}},
		{Message: proto.Message{ID: 201, Chan: 10, Thread: 103, Author: 3, Body: "reply two", Time: base + 6*time.Minute.Milliseconds()}},
	}
	f.msgs[20] = []client.Message{{Message: proto.Message{ID: 300, Chan: 20, Author: 2, Body: "psst", Time: base}}}
	return f
}

func (f *fakeClient) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeClient) called(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, s)
}

func (f *fakeClient) Updates() <-chan client.Update { return f.updates }
func (f *fakeClient) Status() client.Status         { return f.status }
func (f *fakeClient) Me() proto.User                { return f.me }
func (f *fakeClient) Channels() []client.ChannelView {
	return slices.Clone(f.chans)
}
func (f *fakeClient) Messages(ch uint64, limit int) []client.Message {
	ms := f.msgs[ch]
	var real, pend []client.Message
	for _, x := range ms {
		if x.ID == 0 {
			pend = append(pend, x)
		} else {
			real = append(real, x)
		}
	}
	if len(real) > limit {
		real = real[len(real)-limit:]
	}
	return append(slices.Clone(real), pend...)
}
func (f *fakeClient) Thread(root uint64) []client.Message { return slices.Clone(f.threads[root]) }
func (f *fakeClient) Users() []proto.User                 { return slices.Clone(f.users) }
func (f *fakeClient) TypingUsers(ch uint64) []uint64      { return f.typing[ch] }
func (f *fakeClient) Presence(user uint64) client.Presence {
	if user == 2 {
		return client.Presence{State: proto.PresAway, Status: "lunch"}
	}
	return client.Presence{}
}
func (f *fakeClient) Send(ch uint64, body string, thread uint64) error {
	f.sent = append(f.sent, client.Message{Message: proto.Message{Chan: ch, Body: body, Thread: thread}})
	return nil
}
func (f *fakeClient) RetryFailed(local int64) error   { f.record("retry"); return nil }
func (f *fakeClient) DiscardFailed(local int64) error { f.record("discard"); return nil }
func (f *fakeClient) MarkRead(ch, seq uint64) error {
	f.reads[ch] = seq
	return nil
}
func (f *fakeClient) Edit(ctx context.Context, msg uint64, body string) error {
	f.record("edit")
	return nil
}
func (f *fakeClient) Delete(ctx context.Context, msg uint64) error { f.record("delete"); return nil }
func (f *fakeClient) React(ctx context.Context, msg uint64, emoji string) error {
	f.record("react " + emoji)
	return nil
}
func (f *fakeClient) Unreact(ctx context.Context, msg uint64, emoji string) error {
	f.record("unreact " + emoji)
	return nil
}
func (f *fakeClient) CreateChannel(ctx context.Context, name string, private bool, topic string) (proto.Channel, error) {
	f.record("create " + name)
	return proto.Channel{ID: 99, Name: name}, nil
}
func (f *fakeClient) Join(ctx context.Context, ch uint64) error  { f.record("join"); return nil }
func (f *fakeClient) Leave(ctx context.Context, ch uint64) error { f.record("leave"); return nil }
func (f *fakeClient) AddMember(ctx context.Context, ch, user uint64) error {
	f.record("add")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addErr
}
func (f *fakeClient) RemoveMember(ctx context.Context, ch, user uint64) error {
	f.record("remove " + strconv.FormatUint(user, 10))
	return nil
}
func (f *fakeClient) EncryptChannel(ctx context.Context, ch uint64) error {
	f.record("encrypt " + strconv.FormatUint(ch, 10))
	return nil
}
func (f *fakeClient) SetTopic(ctx context.Context, ch uint64, topic string) error {
	f.record("topic " + topic)
	return nil
}
func (f *fakeClient) OpenDM(ctx context.Context, users []uint64) (proto.Channel, error) {
	f.record("dm")
	return proto.Channel{ID: 20}, nil
}
func (f *fakeClient) History(ctx context.Context, ch, before uint64) (bool, error) {
	f.mu.Lock()
	f.history++
	f.mu.Unlock()
	return f.histMore, nil
}
func (f *fakeClient) Search(ctx context.Context, q string, ch uint64) ([]proto.Message, error) {
	return []proto.Message{f.msgs[10][0].Message, f.threads[103][1].Message}, nil
}
func (f *fakeClient) CreateInvite(ctx context.Context, user uint64) (string, error) {
	return "tc1-invite", nil
}
func (f *fakeClient) SendTyping(ch uint64) error { return nil }
func (f *fakeClient) SetAway(away bool, status string) error {
	if away {
		f.record("away")
	} else {
		f.record("back")
	}
	f.mu.Lock()
	f.aways = append(f.aways, away)
	f.mu.Unlock()
	return nil
}

func (f *fakeClient) Drafts() []client.Draft {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []client.Draft
	for k, v := range f.drafts {
		out = append(out, client.Draft{Chan: k[0], Thread: k[1], Body: v})
	}
	return out
}

func (f *fakeClient) Draft(ch, thread uint64) string { return f.draft(ch, thread) }

func (f *fakeClient) SetDraft(ch, thread uint64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	if f.drafts == nil {
		f.drafts = map[[2]uint64]string{}
	}
	if body == "" {
		delete(f.drafts, [2]uint64{ch, thread})
	} else {
		f.drafts[[2]uint64{ch, thread}] = body
	}
	return nil
}

func (f *fakeClient) draft(ch, thread uint64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.drafts[[2]uint64{ch, thread}]
}

func (f *fakeClient) awayCalls() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.aways)
}

// fakeNotifier records notifications.
type fakeNotifier struct {
	mu    sync.Mutex
	notes [][2]string
}

func (n *fakeNotifier) Notify(title, body string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.notes = append(n.notes, [2]string{title, body})
	return true
}

func (n *fakeNotifier) got() [][2]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.notes)
}

// fakeHerdr is an in-memory herdr CLI.
type fakeHerdr struct {
	mu     sync.Mutex
	panes  []notify.Pane
	text   map[string]string
	reads  []string
	titles []string
}

func (h *fakeHerdr) Self() string    { return "w1:p1" }
func (h *fakeHerdr) SelfTab() string { return "w1:t1" }
func (h *fakeHerdr) Panes(ctx context.Context) ([]notify.Pane, error) {
	return slices.Clone(h.panes), nil
}
func (h *fakeHerdr) ReadPane(ctx context.Context, id string, lines int) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads = append(h.reads, id+":"+strconv.Itoa(lines))
	return h.text[id], nil
}
func (h *fakeHerdr) SetTitle(ctx context.Context, title string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.titles = append(h.titles, title)
	return nil
}
func (h *fakeHerdr) gotTitles() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.titles)
}

// run executes cmd and feeds every resulting message back into m, giving
// up on commands that block (cursor blink ticks).
func run(m *Model, cmd tea.Cmd) {
	for depth := 0; cmd != nil && depth < 8; depth++ {
		ch := make(chan tea.Msg, 1)
		go func() { ch <- cmd() }()
		var msg tea.Msg
		select {
		case msg = <-ch:
		case <-time.After(30 * time.Millisecond):
			return
		}
		cmd = nil
		switch msg := msg.(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range msg {
				run(m, c)
			}
		case tea.QuitMsg, updatesClosedMsg:
		default:
			_, cmd = m.Update(msg)
		}
	}
}

func (f *fakeClient) SetDisplay(ctx context.Context, display string) error {
	f.record("name:" + display)
	return nil
}

func (f *fakeClient) ThreadHistory(ctx context.Context, ch, root, before uint64) (bool, error) {
	f.record(fmt.Sprintf("thread-history:%d:%d", ch, root))
	return false, nil
}

func (f *fakeClient) Highlights() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.words)
}

func (f *fakeClient) AddHighlight(word string) (bool, error) {
	w, err := client.NormalizeHighlight(word)
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if slices.Contains(f.words, w) {
		return false, nil
	}
	f.words = append(f.words, w)
	slices.Sort(f.words)
	return true, nil
}

func (f *fakeClient) RemoveHighlight(word string) (bool, error) {
	w, err := client.NormalizeHighlight(word)
	if err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i := slices.Index(f.words, w)
	if i < 0 {
		return false, nil
	}
	f.words = slices.Delete(f.words, i, i+1)
	return true, nil
}

func (f *fakeClient) Activity(limit int) []client.ActivityItem {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "activity")
	return slices.Clone(f.activity)
}

func (f *fakeClient) ActivitySeen() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen
}

func (f *fakeClient) SetActivitySeen(ms int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seen = max(f.seen, ms)
	return nil
}
