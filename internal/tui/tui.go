// Package tui is tc's Bubble Tea interface (docs/TUI.md).
//
// The model reads everything from the client's local cache when an Update
// arrives and View only formats what is already loaded. Anything that may
// touch the network runs in a tea.Cmd. Purely local writes (Send into the
// outbox, MarkRead, RetryFailed, DiscardFailed) run inline in Update so that
// two quick sends keep their order.
package tui

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
	"github.com/skailama/tc/internal/notify"
	"github.com/skailama/tc/internal/proto"
)

// Backend is the subset of *client.Client the UI uses. Tests fake it.
type Backend interface {
	Updates() <-chan client.Update
	Status() client.Status
	Me() proto.User

	Channels() []client.ChannelView
	Messages(ch uint64, limit int) []client.Message
	Thread(root uint64) []client.Message
	Users() []proto.User
	TypingUsers(ch uint64) []uint64
	Presence(user uint64) client.Presence

	Send(ch uint64, body string, thread uint64) error
	RetryFailed(local int64) error
	DiscardFailed(local int64) error
	MarkRead(ch, seq uint64) error
	Drafts() []client.Draft
	Draft(ch, thread uint64) string
	SetDraft(ch, thread uint64, body string) error
	Highlights() []string
	AddHighlight(word string) (bool, error)
	RemoveHighlight(word string) (bool, error)
	Activity(limit int) []client.ActivityItem
	ActivitySeen() int64
	SetActivitySeen(ms int64) error

	Edit(ctx context.Context, msg uint64, body string) error
	Delete(ctx context.Context, msg uint64) error
	React(ctx context.Context, msg uint64, emoji string) error
	Unreact(ctx context.Context, msg uint64, emoji string) error
	CreateChannel(ctx context.Context, name string, private bool, topic string) (proto.Channel, error)
	Join(ctx context.Context, ch uint64) error
	Leave(ctx context.Context, ch uint64) error
	AddMember(ctx context.Context, ch, user uint64) error
	SetTopic(ctx context.Context, ch uint64, topic string) error
	OpenDM(ctx context.Context, users []uint64) (proto.Channel, error)
	History(ctx context.Context, ch, before uint64) (bool, error)
	Search(ctx context.Context, q string, ch uint64) ([]proto.Message, error)
	CreateInvite(ctx context.Context, user uint64) (string, error)
	SendTyping(ch uint64) error
	SetAway(away bool, status string) error
	SetDisplay(ctx context.Context, display string) error
	ThreadHistory(ctx context.Context, ch, root, before uint64) (bool, error)
	Pin(ctx context.Context, msg uint64) error
	Unpin(ctx context.Context, msg uint64) error
	Pins(ch uint64) []client.Message
	FetchPins(ctx context.Context, ch uint64) error
	Groups() []proto.Group
	SetGroup(ctx context.Context, name string, members []uint64) error
	Remind(ctx context.Context, at time.Time, text string, ch uint64) (uint64, error)
	Reminders(ctx context.Context) ([]proto.Reminder, error)
	DeleteReminder(ctx context.Context, id uint64) error
	// E2EStatus is an encrypted channel's key state (Off for others).
	E2EStatus(ch uint64) client.E2EStatus
}

var _ Backend = (*client.Client)(nil)

// Run shows the UI until the user quits or ctx is cancelled. The caller
// runs c.Run(ctx) separately; the UI works from the cache while offline.
func Run(ctx context.Context, c *client.Client) error {
	env := notify.OSEnv()
	o := Options{Out: os.Stdout, Notifier: notify.New(env, os.Stdout), AwayAfter: awayAfter(os.Getenv("TC_AWAY_MIN"))}
	if h := notify.DetectHerdr(env); h != nil {
		o.Herdr = h
	}
	m := New(ctx, c, o)
	defer m.Close()
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(),
		tea.WithContext(ctx), tea.WithOutput(os.Stdout))
	_, err := p.Run()
	if errors.Is(err, tea.ErrProgramKilled) && ctx.Err() != nil {
		return nil
	}
	return err
}

// Options configures a Model.
type Options struct {
	// Out receives the bell and OSC 52 clipboard sequences. It should be the
	// terminal the program draws on. Both are single short writes made
	// between renderer frames; neither moves the cursor, so they don't
	// disturb the alt screen.
	Out io.Writer
	// Notifier gets mentions and DMs the user isn't looking at; nil: none.
	Notifier Notifier
	// Herdr is set when running inside herdr: unread badge, /share-pane.
	Herdr Herdr
	// AwayAfter sets presence to away after this long without a key; 0: never.
	AwayAfter time.Duration

	// SnippetDir is the base for relative /snippet paths; "": the working directory.
	SnippetDir string

	styles *styles // nil: from Out and NO_COLOR
}

// awayAfter parses TC_AWAY_MIN (minutes, default 15, 0 disables).
func awayAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 15 * time.Minute
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n * float64(time.Minute))
}
