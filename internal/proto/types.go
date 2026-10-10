// Package proto implements the tc wire protocol (docs/PROTOCOL.md): frame
// types, payload structs, and the frame codec. It has no I/O dependencies
// beyond io and imports nothing else from tc.
package proto

import "github.com/vmihailenco/msgpack/v5"

// ALPN is the protocol version, negotiated in TLS.
const ALPN = "tc/1"

// FrameType identifies a frame's payload (PROTOCOL.md §3).
type FrameType uint8

const (
	THello     FrameType = 0x01
	TWelcome   FrameType = 0x05
	TReq       FrameType = 0x10
	TRes       FrameType = 0x11
	TEvent     FrameType = 0x20
	TEphemeral FrameType = 0x21
	TSnapshot  FrameType = 0x22
	TPing      FrameType = 0x30
	TPong      FrameType = 0x31
	TVoice     FrameType = 0x40
	TBye       FrameType = 0x7F
)

// Size limits in bytes (PROTOCOL.md §3, §9).
const (
	MaxFrame        = 1 << 20
	MaxPreAuthFrame = 8 << 10
	MaxBody         = 64 << 10
	MaxDisplay      = 64
	MaxTopic        = 256
	MaxStatus       = 128
	MaxEmoji        = 64
	MaxHistory      = 200
	MaxSearch       = 50
	MaxResultBytes  = 768 << 10
	MaxDMOthers     = 7
	MaxQueue        = 1024
	SnapshotGap     = 10_000
	SnapshotPerChan = 50
)

// Ops (PROTOCOL.md §5).
const (
	OpSync         = "sync"
	OpSend         = "send"
	OpEdit         = "edit"
	OpDelete       = "delete"
	OpReact        = "react"
	OpUnreact      = "unreact"
	OpHistory      = "history"
	OpSearch       = "search"
	OpRead         = "read"
	OpChanCreate   = "chan.create"
	OpChanJoin     = "chan.join"
	OpChanLeave    = "chan.leave"
	OpChanAdd      = "chan.add"
	OpChanRemove   = "chan.remove"
	OpChanTopic    = "chan.topic"
	OpDMOpen       = "dm.open"
	OpUsersList    = "users.list"
	OpInviteCreate = "invite.create"
	OpUserDisable  = "user.disable"
)

// Event kinds (PROTOCOL.md §6).
const (
	KMsgNew      = "msg.new"
	KMsgEdit     = "msg.edit"
	KMsgDelete   = "msg.delete"
	KReactAdd    = "react.add"
	KReactRemove = "react.remove"
	KChanNew     = "chan.new"
	KChanUpdate  = "chan.update"
	KChanJoin    = "chan.join"
	KChanLeave   = "chan.leave"
	KUserNew     = "user.new"
	KUserUpdate  = "user.update"
	KReadUpdate  = "read.update"
)

// Ephemeral kinds (PROTOCOL.md §7).
const (
	EphTyping   = "typing"
	EphPresence = "presence"
)

// RES error codes (PROTOCOL.md §5).
const (
	ErrBadRequest   = "bad_request"
	ErrUnauthorized = "unauthorized"
	ErrForbidden    = "forbidden"
	ErrNotFound     = "not_found"
	ErrConflict     = "conflict"
	ErrRateLimited  = "rate_limited"
	ErrTooLarge     = "too_large"
	ErrUnknownOp    = "unknown_op"
	ErrInternal     = "internal"
)

// BYE codes (PROTOCOL.md §4.2).
const (
	ByeAuthFailed       = "auth_failed"
	ByeInviteInvalid    = "invite_invalid"
	ByeNameTaken        = "name_taken"
	ByeKeyExists        = "key_exists"
	ByeFrameTooLarge    = "frame_too_large"
	ByeHandshakeTimeout = "handshake_timeout"
	ByeProtocolError    = "protocol_error"
	ByeSlowConsumer     = "slow_consumer"
	ByeServerShutdown   = "server_shutdown"
	ByeRevoked          = "revoked"
)

// Channel kinds.
const (
	ChanPublic  = "public"
	ChanPrivate = "private"
	ChanDM      = "dm"
)

// Presence states.
const (
	PresOnline  = "online"
	PresAway    = "away"
	PresOffline = "offline"
)

// Raw is an undecoded msgpack value (op args, RES data, event data).
type Raw = msgpack.RawMessage

// ---- frames ----

type Auth struct {
	Pubkey []byte `msgpack:"pubkey"`
	Sig    []byte `msgpack:"sig"`
	Invite string `msgpack:"invite,omitempty"`
	Name   string `msgpack:"name,omitempty"`
}

type Hello struct {
	Client string   `msgpack:"client"`
	Caps   []string `msgpack:"caps"`
	Auth   Auth     `msgpack:"auth"`
}

type Welcome struct {
	Server   string   `msgpack:"server"`
	Caps     []string `msgpack:"caps"`
	ServerID []byte   `msgpack:"server_id"`
	User     User     `msgpack:"user"`
	Time     int64    `msgpack:"time"`
}

type Req struct {
	ID   uint32 `msgpack:"id"`
	Op   string `msgpack:"op"`
	Args Raw    `msgpack:"args"`
}

type Res struct {
	ID   uint32 `msgpack:"id"`
	OK   bool   `msgpack:"ok"`
	Data Raw    `msgpack:"data,omitempty"`
	Err  *Error `msgpack:"err,omitempty"`
}

// Error is a protocol error (RES err). It implements error.
type Error struct {
	Code string `msgpack:"code"`
	Msg  string `msgpack:"msg"`
}

func (e *Error) Error() string {
	if e.Msg == "" {
		return e.Code
	}
	return e.Code + ": " + e.Msg
}

// Errf builds a protocol error.
func Errf(code, msg string) *Error { return &Error{Code: code, Msg: msg} }

type Event struct {
	Seq   uint64 `msgpack:"seq"`
	Kind  string `msgpack:"kind"`
	Chan  uint64 `msgpack:"chan,omitempty"`
	Time  int64  `msgpack:"time"`
	Actor uint64 `msgpack:"actor"`
	Data  Raw    `msgpack:"data"`
}

type Ephemeral struct {
	Kind   string `msgpack:"kind"`
	Chan   uint64 `msgpack:"chan,omitempty"`
	User   uint64 `msgpack:"user,omitempty"`
	State  string `msgpack:"state,omitempty"`
	Status string `msgpack:"status,omitempty"`
	// Epoch and Dev are key.need's (§13.7): the device lacking K for Epoch.
	Epoch uint32 `msgpack:"epoch,omitempty"`
	Dev   []byte `msgpack:"dev,omitempty"`
}

type Snapshot struct {
	Users []User            `msgpack:"users,omitempty"`
	Chans []Channel         `msgpack:"chans,omitempty"`
	Msgs  []Message         `msgpack:"msgs,omitempty"`
	Read  map[uint64]uint64 `msgpack:"read,omitempty"`
	// Devs and Revoked are sent only with cap e2e1 (§13.2).
	Devs    []Device `msgpack:"devs,omitempty"`
	Revoked []Signed `msgpack:"revoked,omitempty"`
	// RevokedSeqs[i] is the seq of Revoked[i]'s device.revoke event.
	RevokedSeqs []uint64 `msgpack:"revoked_seqs,omitempty"`
	// Keys is the latest epoch bundle of each visible e2e channel (§13.3).
	Keys []EpochKey `msgpack:"keys,omitempty"`
	// Groups is every user group (cap groups).
	Groups []Group `msgpack:"groups,omitempty"`
}

type Ping struct {
	T int64 `msgpack:"t"`
}

type Bye struct {
	Code string `msgpack:"code"`
	Msg  string `msgpack:"msg"`
}

// ---- objects (PROTOCOL.md §8) ----

type User struct {
	ID       uint64 `msgpack:"id"`
	Name     string `msgpack:"name"`
	Display  string `msgpack:"display"`
	Admin    bool   `msgpack:"admin"`
	Disabled bool   `msgpack:"disabled"`
	Created  int64  `msgpack:"created"`
	Bot      bool   `msgpack:"bot,omitempty"` // webhook author; never logs in
}

type Channel struct {
	ID      uint64   `msgpack:"id"`
	Name    string   `msgpack:"name"`
	Kind    string   `msgpack:"kind"`
	Topic   string   `msgpack:"topic"`
	Members []uint64 `msgpack:"members"`
	Created int64    `msgpack:"created"`
	// E2E channels are end-to-end encrypted (§13); Epoch is the current
	// epoch, 0 until the first key.rotate.
	E2E   bool   `msgpack:"e2e,omitempty"`
	Epoch uint32 `msgpack:"epoch,omitempty"`
	// Creator created the channel; 0 for #general and for some old DMs.
	// Only it may sign epoch 1 of a non-DM e2e channel (§13.3).
	Creator uint64 `msgpack:"creator,omitempty"`
	// Legacy is the newest message id when chan.encrypt converted the
	// channel (0 for channels encrypted from birth): plaintext at or below
	// it predates encryption (§13.4). The server's word, unauthenticated.
	Legacy uint64 `msgpack:"legacy,omitempty"`
	// Pins are the ids of pinned messages, newest pin first (cap pins).
	Pins []uint64 `msgpack:"pins,omitempty"`
}

type Message struct {
	ID        uint64              `msgpack:"id"`
	Chan      uint64              `msgpack:"chan"`
	Author    uint64              `msgpack:"author"`
	Body      string              `msgpack:"body"`
	Thread    uint64              `msgpack:"thread"`
	Time      int64               `msgpack:"time"`
	Edited    int64               `msgpack:"edited"`
	Deleted   bool                `msgpack:"deleted"`
	Reactions map[string][]uint64 `msgpack:"reactions,omitempty"`
	// Enc is the encrypted payload of a message in an e2e channel
	// (PROTOCOL.md §13.4); Body is "" then.
	Enc []byte `msgpack:"enc,omitempty"`
	// EditEnc is the latest encrypted edit of an e2e message (§13.4, Plain
	// t = "edit"); Enc stays the original, which the edit refers to.
	EditEnc []byte `msgpack:"edit_enc,omitempty"`
	// DelEnc is the author's signed delete (Plain t = "del") of a deleted
	// e2e message; absent when someone else (an admin) deleted it.
	DelEnc []byte `msgpack:"del_enc,omitempty"`
	// Renc carries each encrypted reaction's signed enc (Plain t =
	// "react"); Reactions then maps tags, not emoji (§13.4).
	Renc []ReactEnc `msgpack:"renc,omitempty"`
	// Mid is the server's record of e2ecrypto.MID(Enc), kept after a
	// delete so a device that never saw Enc can check a delete's ref. It is
	// the server's claim, not proof (§13.4).
	Mid []byte `msgpack:"mid,omitempty"`
}

// ReactEnc is one encrypted reaction: user reacted with tag, proven by enc.
type ReactEnc struct {
	User  uint64 `msgpack:"user"`
	Emoji string `msgpack:"emoji"`
	Enc   []byte `msgpack:"enc"`
}

// ---- event data ----

type MsgEdit struct {
	Msg    uint64 `msgpack:"msg"`
	Body   string `msgpack:"body"`
	Edited int64  `msgpack:"edited"`
	// Enc is an encrypted edit (§13.4); Body is "" then.
	Enc []byte `msgpack:"enc,omitempty"`
}

type MsgRef struct {
	Msg uint64 `msgpack:"msg"`
}

// MsgDelete is the delete op's args and msg.delete's data.
type MsgDelete struct {
	Msg uint64 `msgpack:"msg"`
	// Enc is the author's signed delete in an e2e channel (§13.4).
	Enc []byte `msgpack:"enc,omitempty"`
}

type Reaction struct {
	Msg   uint64 `msgpack:"msg"`
	Emoji string `msgpack:"emoji"`
	// Enc proves an encrypted reaction (§13.4); Emoji is its tag then.
	Enc []byte `msgpack:"enc,omitempty"`
}

type ChanJoin struct {
	User uint64  `msgpack:"user"`
	Chan Channel `msgpack:"chan"`
}

type UserRef struct {
	User uint64 `msgpack:"user"`
}

type ReadUpdate struct {
	Chan uint64 `msgpack:"chan"`
	Seq  uint64 `msgpack:"seq"`
}

// ---- op args / results ----

type SyncArgs struct {
	Since    uint64 `msgpack:"since"`
	ServerID []byte `msgpack:"server_id,omitempty"`
}

type SyncRes struct {
	Head  uint64 `msgpack:"head"`
	Reset bool   `msgpack:"reset"`
}

type SendArgs struct {
	Chan   uint64 `msgpack:"chan"`
	Body   string `msgpack:"body"`
	Nonce  []byte `msgpack:"nonce"`
	Thread uint64 `msgpack:"thread,omitempty"`
	// Enc (cap e2e1) replaces Body in e2e channels: exactly one of the two
	// is set (§13.4).
	Enc []byte `msgpack:"enc,omitempty"`
}

type SendRes struct {
	Msg uint64 `msgpack:"msg"`
}

type EditArgs struct {
	Msg  uint64 `msgpack:"msg"`
	Body string `msgpack:"body"`
	// Enc (cap e2e1) replaces Body in e2e channels (§13.4).
	Enc []byte `msgpack:"enc,omitempty"`
}

type DeleteArgs = MsgDelete

type MsgArgs = MsgRef

type ReactArgs = Reaction

type SeqRes struct {
	Seq uint64 `msgpack:"seq"`
}

type HistoryArgs struct {
	Chan   uint64 `msgpack:"chan"`
	Before uint64 `msgpack:"before"`
	Limit  int    `msgpack:"limit"`
	// Thread, if set, returns that thread's replies instead of the channel.
	Thread uint64 `msgpack:"thread,omitempty"`
}

type HistoryRes struct {
	Msgs []Message `msgpack:"msgs"`
	More bool      `msgpack:"more"`
}

type SearchArgs struct {
	Q     string `msgpack:"q"`
	Chan  uint64 `msgpack:"chan,omitempty"`
	Limit int    `msgpack:"limit"`
}

type MsgsRes struct {
	Msgs []Message `msgpack:"msgs"`
}

type ReadArgs = ReadUpdate

type ChanCreateArgs struct {
	Name    string `msgpack:"name"`
	Private bool   `msgpack:"private"`
	Topic   string `msgpack:"topic,omitempty"`
	// E2E (cap e2e1, private channels only) makes the channel encrypted.
	E2E bool `msgpack:"e2e,omitempty"`
}

type ChanRes struct {
	Chan Channel `msgpack:"chan"`
}

type ChanArgs struct {
	Chan uint64 `msgpack:"chan"`
}

// ChanAddArgs is chan.add's and chan.remove's args. Bundle (§13.3) is the
// channel's next epoch with the new roster, committed with the membership
// change; required in encrypted channels (PROTOCOL.md §13.5).
type ChanAddArgs struct {
	Chan   uint64  `msgpack:"chan"`
	User   uint64  `msgpack:"user"`
	Bundle *Signed `msgpack:"bundle,omitempty"`
}

type ChanTopicArgs struct {
	Chan  uint64 `msgpack:"chan"`
	Topic string `msgpack:"topic"`
}

type DMOpenArgs struct {
	Users []uint64 `msgpack:"users"`
}

type UsersRes struct {
	Users []User `msgpack:"users"`
}

// InviteCreateArgs: Uses and TTLMs (cap `team_invites`) default to 1 and
// DefaultInviteTTL when zero.
type InviteCreateArgs struct {
	User  uint64 `msgpack:"user,omitempty"`
	Uses  int64  `msgpack:"uses,omitempty"`
	TTLMs int64  `msgpack:"ttl_ms,omitempty"`
}

type InviteRes struct {
	Code    string `msgpack:"code"`
	Uses    int64  `msgpack:"uses,omitempty"`    // redemptions allowed
	Expires int64  `msgpack:"expires,omitempty"` // Unix ms
}

type UserArgs = UserRef

type Empty struct{}
