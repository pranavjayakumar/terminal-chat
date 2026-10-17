// Package client is tc's connection manager and local cache. The TUI and the
// CLI subcommands talk to the server only through it.
//
// Lifecycle:
//
//	func Login(ctx context.Context, home, invite, name, keyPath string) (proto.User, error)
//	func Open(home string) (*Client, error)   // config + cache only, no network
//	func (c *Client) Run(ctx context.Context) // connect/sync/reconnect until ctx or Close
//	func (c *Client) Close() error
//	func (c *Client) Trust() *trust.Store    // e2e.db, never touched by cache resets
//	func DefaultHome() string                 // $TC_HOME or ~/.config/tc
//
// State and notifications:
//
//	func (c *Client) Updates() <-chan Update  // coalesced; closed by Close
//	func (c *Client) Status() Status
//	func (c *Client) Me() proto.User
//
// Reads (local cache, work offline):
//
//	func (c *Client) Channels() []ChannelView
//	func (c *Client) Channel(id uint64) (ChannelView, bool)
//	func (c *Client) Messages(ch uint64, limit int) []Message // latest top-level + pending
//	func (c *Client) Thread(root uint64) []Message            // root + replies + pending
//	func (c *Client) User(id uint64) proto.User
//	func (c *Client) Users() []proto.User
//	func (c *Client) TypingUsers(ch uint64) []uint64
//	func (c *Client) Presence(user uint64) Presence
//
// Writes:
//
//	func (c *Client) Send(ch uint64, body string, thread uint64) error // outbox, works offline; encrypts in e2e channels (encmsg.go), ErrE2EAlarm in alarm
//	func (c *Client) Flush(ctx context.Context) error                   // wait until the outbox has nothing left to send
//	func (c *Client) RetryFailed(local int64) error
//	func (c *Client) DiscardFailed(local int64) error
//	func (c *Client) MarkRead(ch, seq uint64) error                     // local now, server best-effort
//
// Local-only state (cache, never sent to the server, kept across snapshot resets):
//
//	func (c *Client) Drafts() []Draft                                   // unsent input per (channel, thread)
//	func (c *Client) Draft(ch, thread uint64) string
//	func (c *Client) SetDraft(ch, thread uint64, body string) error     // "" removes
//	func (c *Client) Highlights() []string                              // keywords that count like an @mention
//	func (c *Client) AddHighlight(word string) (added bool, err error)
//	func (c *Client) RemoveHighlight(word string) (removed bool, err error)
//	func Highlighted(body string, words []string) bool                  // whole word, case-insensitive
//	func (c *Client) Activity(limit int) []ActivityItem                 // what concerns me, newest first (see activity.go)
//	func (c *Client) Groups() []proto.Group                             // user groups from the cache (see groups.go)
//	func (c *Client) MentionNames() []string                            // my name plus my groups: what @-mentions me
//	func Mentioned(body string, names []string) bool                    // whole-word @name for any of names
//
// Online-only (return ErrOffline unless Status().State == Online):
//
//	Edit(ctx, msg, body) error; Delete(ctx, msg) error
//	React(ctx, msg, emoji) error; Unreact(ctx, msg, emoji) error
//	CreateChannel(ctx, name, private, topic) (proto.Channel, error)
//	Join(ctx, ch) error; Leave(ctx, ch) error; AddMember(ctx, ch, user) error
//	SetTopic(ctx, ch, topic) error; OpenDM(ctx, users) (proto.Channel, error)
//	History(ctx, ch, before) (more bool, err error)  // older messages into the cache
//	Search(ctx, q, ch) ([]proto.Message, error)        // server + decrypted e2e messages in the cache
//	In e2e channels Edit/Delete/React/Unreact send signed encs (encops.go); ErrNotEditable for
//	messages that aren't our own verified ones (edit) or verified (react); E2ERejects() []string
//	says why received ones were ignored
//	CreateInvite(ctx, user) (string, error); CreateTeamInvite(ctx, uses, ttl) (proto.InviteRes, error)
//	CreateGitHubInvite(ctx, login, ttl) (proto.InviteRes, error) // key-bound, cap key_invites; FetchGitHubInviteKeys
//	ListInvites(ctx) ([]proto.InviteInfo, error); RevokeInvite(ctx, id) error (cap invite_manage; members: own, cap member_invites)
//	DisableUser(ctx, user) error
//	SetGroup(ctx, name, members) error (admin); FetchGroups(ctx) ([]proto.Group, error)
//	Remind(ctx, at, text, ch) (id, error); Reminders(ctx) ([]proto.Reminder, error); DeleteReminder(ctx, id) error
//	func ParseRemind(args string, now time.Time) (at time.Time, text string, err error) // "10m text", "tomorrow 9am text"
//	SendTyping(ch) error; SetAway(away bool, status string) error
//	CreateEncryptedChannel(ctx, name, topic) (proto.Channel, error)
//	RotateKey(ctx, ch) error                          // new epoch key (e2e.go)
//
// Messages in e2e channels carry Message.Sec (encrypted and verified, waiting
// for a key, invalid, legacy or server-inserted plaintext); the cache holds
// decrypted text and is as sensitive as the conversation (encmsg.go).
//
// End-to-end keys (e2e.go; verification runs in the background per session):
//
//	func (c *Client) E2EStatus(ch uint64) E2EStatus  // off / ok / waiting_key / unverified / alarm
//	func (c *Client) ServerID() (e2ecrypto.ServerID, bool)
//	func (c *Client) UnverifiedUsers() []uint64      // e2e channel members trusted only on first use
//	func (c *Client) TrustAlarm() (string, bool)      // server dropped e2e1 after speaking it (sticky)
//
// Trust UX (verify.go; local, offline except the GitHub fetch):
//
//	func (c *Client) SafetyNumber(user uint64) (SafetyNumber, error)   // 60 digits over both users' pinned keys
//	func (c *Client) MarkVerified(user uint64, digits string) error    // numbers matched: their pins become Verified
//	func (c *Client) VerifyGitHub(ctx, user, login) (GitHubCheck, error); ConfirmGitHub(res) error // compare, then confirm
//	func (c *Client) KeyChanges() []KeyChange; KeyChangedMembers(ch) []uint64 // sends blocked there (ErrKeyChange)
//	func (c *Client) PendingKeys(user) []string; AcceptKeyChanges(user, fps, only) error // exactly the shown keys
//	func (c *Client) Alarm(ch uint64) (AlarmInfo, bool); ClearAlarm(ch) error // clearing is audited
//
// Server-side refusals are returned as *proto.Error (check Code).
package client

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/skailama/tc/internal/e2ecrypto"
	"github.com/skailama/tc/internal/proto"
	"github.com/skailama/tc/internal/sanitize"
	"github.com/skailama/tc/internal/trust"
)

var (
	// ErrOffline means the request needs a live, synced connection.
	ErrOffline = errors.New("offline")
	// ErrNotLoggedIn means Home has no config.json.
	ErrNotLoggedIn = errors.New("not logged in (run: tc join <invite>)")
)

// ByeError is a BYE received from the server.
type ByeError struct{ Code, Msg string }

func (e *ByeError) Error() string {
	if e.Msg == "" {
		return "server closed connection: " + sanitize.Line(e.Code)
	}
	return "server closed connection: " + sanitize.Line(e.Code) + ": " + sanitize.Line(e.Msg)
}

// State is the connection state.
type State int

const (
	Offline State = iota
	Connecting
	Syncing
	Online
)

func (s State) String() string {
	return [...]string{"offline", "connecting", "syncing", "online"}[s]
}

// Status describes the connection. Stopped means Run gave up for good
// (revoked, auth failure, pin mismatch, server identity changed) and Err says why.
type Status struct {
	State     State
	Err       string
	Transport string // "quic" or "tcp" while connected
	Stopped   bool
}

// Presence is a user's last known presence; State is "" when unknown.
type Presence struct{ State, Status string }

// Config is Home/config.json.
type Config struct {
	Addr    string `json:"addr"`
	Pin     string `json:"pin"`
	KeyPath string `json:"key_path,omitempty"`
	UserID  uint64 `json:"user_id"`
	Name    string `json:"name"`
}

// Client owns one user's connection and cache. All methods are safe for
// concurrent use.
type Client struct {
	home  string
	cfg   Config
	db    *sql.DB
	trust *trust.Store // e2e.db: separate from the cache so no reset reaches it
	// procSeq is the highest event seq this process has handled. Only the
	// read goroutine touches it.
	procSeq uint64
	cancel  context.CancelFunc
	ctx     context.Context // cancelled by Close

	mu           sync.Mutex
	status       Status
	sess         *session // set only while Online
	me           proto.User
	users        map[uint64]proto.User
	typing       map[uint64]map[uint64]time.Time
	typingTimers map[uint64]*time.Timer
	presence     map[uint64]Presence
	away         *proto.Ephemeral // our own presence to restore after reconnect
	typed        map[uint64]time.Time
	runDone      chan struct{}

	updMu     sync.Mutex
	updClosed bool
	updates   chan Update

	obKick    chan struct{}
	obMu      sync.Mutex
	obChanged chan struct{} // closed and replaced on every outbox change

	running atomic.Bool
	timing  timing

	githubBase string // VerifyGitHub's base URL; "" = https://github.com (tests)

	// E2E (e2e.go). e2eMu serializes chain processing and rotation.
	e2eMu    sync.Mutex
	e2eKick  chan struct{}
	keyNeeds chan proto.Ephemeral
	myDev    atomic.Pointer[e2ecrypto.DevID] // this device's id, once known (ourDev)
	// under mu:
	e2eSoft      map[uint64]string // channel → why its next bundle can't be judged yet
	needSent     map[uint64]time.Time
	shared       map[shareKey]time.Time
	autoTried    map[uint64]time.Time
	reopenAfter  uint64 // reopenMsgs' cursor (under e2eMu)
	reopenBatch  int
	poisonSeen   map[[2]uint64]time.Time // (chan, epoch) → when our box there first proved unusable
	probed       map[uint64]uint32       // channel → highest waiting-message epoch fetched ahead for, this session
	badCerts     map[[32]byte]struct{}   // hashes of certs that failed verification
	needVerified time.Time               // last verifyDevices run from answerKeyNeed
	certVerifies atomic.Int64            // cert signature checks (tests)
	rejects      []string                // why recent encrypted edits/deletes/reactions were ignored (encops.go)
}

// timing holds the protocol's timers; tests shrink them.
type timing struct {
	pingIdle, recvTimeout, tick   time.Duration
	backoffMin, backoffMax        time.Duration
	dial, handshake, retry, type_ time.Duration
	keyNeed, autoEpoch            time.Duration // §13.7: ≤ 1 key.need per channel per keyNeed
	devWait                       time.Duration // how long an encrypted send waits for device registration
	poisonWait                    time.Duration // how long a provably bad box waits for a key.share before rotating past it
}

var defaultTiming = timing{
	pingIdle: 20 * time.Second, recvTimeout: 45 * time.Second, tick: time.Second,
	backoffMin: 250 * time.Millisecond, backoffMax: 30 * time.Second,
	dial: 15 * time.Second, handshake: 30 * time.Second, retry: 2 * time.Second,
	type_: 6 * time.Second, keyNeed: 30 * time.Second, autoEpoch: time.Minute,
	devWait: 15 * time.Second, poisonWait: 2 * time.Minute,
}

// DefaultHome is $TC_HOME, else ~/.config/tc.
func DefaultHome() string {
	if h := os.Getenv("TC_HOME"); h != "" {
		return h
	}
	if d, err := os.UserHomeDir(); err == nil {
		return filepath.Join(d, ".config", "tc")
	}
	return ".tc"
}

func homeOr(home string) string {
	if home == "" {
		return DefaultHome()
	}
	return home
}

// Open loads config and cache from home ("" = DefaultHome). It never touches
// the network; call Run for that.
func Open(home string) (*Client, error) {
	home = homeOr(home)
	b, err := os.ReadFile(filepath.Join(home, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLoggedIn
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Addr == "" || cfg.Pin == "" || cfg.UserID == 0 {
		return nil, ErrNotLoggedIn
	}
	db, err := openCache(filepath.Join(home, "cache.db"))
	if err != nil {
		return nil, err
	}
	users, err := loadUsers(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("loading cache: %w", err)
	}
	ts, err := trust.Open(home)
	if err != nil {
		db.Close()
		return nil, err
	}
	if err := migrateDeviceKeyFile(home, ts); err != nil {
		db.Close()
		ts.Close()
		return nil, err
	}
	c := &Client{
		home: home, cfg: cfg, db: db, trust: ts, users: users,
		typing: map[uint64]map[uint64]time.Time{}, typingTimers: map[uint64]*time.Timer{}, presence: map[uint64]Presence{},
		typed:   map[uint64]time.Time{},
		updates: make(chan Update, 1), obKick: make(chan struct{}, 1),
		obChanged: make(chan struct{}), timing: defaultTiming,
		e2eKick: make(chan struct{}, 1), keyNeeds: make(chan proto.Ephemeral, 32),
		e2eSoft: map[uint64]string{}, needSent: map[uint64]time.Time{}, shared: map[shareKey]time.Time{},
		autoTried: map[uint64]time.Time{}, poisonSeen: map[[2]uint64]time.Time{}, reopenBatch: reopenBatch, badCerts: map[[32]byte]struct{}{}, probed: map[uint64]uint32{},
	}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	c.me = users[cfg.UserID]
	if c.me.ID == 0 {
		c.me = proto.User{ID: cfg.UserID, Name: cfg.Name}
	}
	return c, nil
}

// Close stops Run, waits for it, and closes the cache, trust store and Updates channel.
func (c *Client) Close() error {
	c.cancel()
	c.mu.Lock()
	done := c.runDone
	c.mu.Unlock()
	if done != nil {
		<-done
	}
	c.updMu.Lock()
	if !c.updClosed {
		c.updClosed = true
		close(c.updates)
	}
	c.updMu.Unlock()
	return errors.Join(c.db.Close(), c.trust.Close())
}

// Trust is the E2E trust store, open for the Client's lifetime.
func (c *Client) Trust() *trust.Store { return c.trust }

// Home is the client's TC_HOME.
func (c *Client) Home() string { return c.home }

// Config returns the loaded configuration.
func (c *Client) Config() Config { return c.cfg }

// Status is the current connection status.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.status
}

func (c *Client) setStatus(s Status) {
	c.mu.Lock()
	changed := c.status != s
	c.status = s
	c.mu.Unlock()
	if changed {
		c.post(Update{Status: true})
	}
}

// Me is the logged-in user.
func (c *Client) Me() proto.User {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.me
}

// ---- updates ----

// Update says what changed since the previous Update was received. Updates
// coalesce: if the consumer is slow, several changes merge into one, so
// re-read whatever the flags point at rather than expecting one Update per
// change.
type Update struct {
	Status   bool
	Channels bool // channel list, membership, topics, unread or mention state
	Users    bool
	Presence bool
	Reset    bool            // the cache was replaced by a snapshot: reload everything
	Msgs     map[uint64]bool // channels whose messages (incl. threads and pending) changed
	Typing   map[uint64]bool // channels whose typing set changed
	// New lists messages that arrived via msg.new (replay or live, own
	// included), oldest first, at most maxNew. For notifications and `tc tail`.
	// Encrypted messages carry their decrypted body; one that can't be
	// opened yet is listed when it can (if still unread). Check Sec before
	// trusting the text.
	New []Message
	// Failed lists outbox messages that the server permanently rejected.
	Failed []Message
	// Revoked lists verified devices that a verified revoke just retired
	// (devmgmt.go), so the UI can say so. Never set on a first sync.
	Revoked []DeviceRef
	// NewOwn lists newly verified devices of ours other than this one
	// (never on our first verification): "was that you?".
	NewOwn []DeviceRef
}

const maxNew = 256

func (u *Update) msgs(ch uint64) {
	if u.Msgs == nil {
		u.Msgs = map[uint64]bool{}
	}
	u.Msgs[ch] = true
}

func (u Update) empty() bool {
	return !u.Status && !u.Channels && !u.Users && !u.Presence && !u.Reset &&
		len(u.Msgs) == 0 && len(u.Typing) == 0 && len(u.New) == 0 && len(u.Failed) == 0 && len(u.Revoked) == 0 && len(u.NewOwn) == 0
}

func (u Update) merge(v Update) Update {
	u.Status = u.Status || v.Status
	u.Channels = u.Channels || v.Channels
	u.Users = u.Users || v.Users
	u.Presence = u.Presence || v.Presence
	u.Reset = u.Reset || v.Reset
	for k := range v.Msgs {
		u.msgs(k)
	}
	for k := range v.Typing {
		if u.Typing == nil {
			u.Typing = map[uint64]bool{}
		}
		u.Typing[k] = true
	}
	u.New = append(u.New, v.New...)
	if len(u.New) > maxNew {
		u.New = append([]Message(nil), u.New[len(u.New)-maxNew:]...)
	}
	u.Failed = append(u.Failed, v.Failed...)
	if len(u.Failed) > maxNew {
		u.Failed = append([]Message(nil), u.Failed[len(u.Failed)-maxNew:]...)
	}
	u.NewOwn = append(u.NewOwn, v.NewOwn...)
	if len(u.NewOwn) > maxNew {
		u.NewOwn = append([]DeviceRef(nil), u.NewOwn[len(u.NewOwn)-maxNew:]...)
	}
	u.Revoked = append(u.Revoked, v.Revoked...)
	if len(u.Revoked) > maxNew {
		u.Revoked = append([]DeviceRef(nil), u.Revoked[len(u.Revoked)-maxNew:]...)
	}
	return u
}

// Updates delivers change notifications. The channel has capacity one and
// pending changes are merged into the buffered value, so the client never
// blocks on a slow UI. Closed by Close.
func (c *Client) Updates() <-chan Update { return c.updates }

func (c *Client) post(u Update) {
	if u.empty() {
		return
	}
	c.updMu.Lock()
	defer c.updMu.Unlock()
	if c.updClosed {
		return
	}
	select {
	case old := <-c.updates:
		u = old.merge(u)
	default:
	}
	// Only post sends, under updMu, and we just emptied the buffer.
	c.updates <- u
}
