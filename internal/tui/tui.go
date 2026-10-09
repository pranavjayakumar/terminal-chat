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

	tea "github.com/charmbracelet/bubbletea"

	"github.com/skailama/tc/internal/client"
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
}

var _ Backend = (*client.Client)(nil)

// Run shows the UI until the user quits or ctx is cancelled. The caller
// runs c.Run(ctx) separately; the UI works from the cache while offline.
func Run(ctx context.Context, c *client.Client) error {
	m := New(ctx, c, Options{Out: os.Stdout, Bell: os.Getenv("TC_BELL") != "0"})
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
	Out  io.Writer
	Bell bool // ring on mentions/DMs outside the focused channel

	styles *styles // nil: from Out and NO_COLOR
}
