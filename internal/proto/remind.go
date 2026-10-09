package proto

import "time"

// Reminders (PROTOCOL.md §5): the server posts the text into the user's
// private notes at the chosen time, even if no client is connected.

// CapRemind is advertised in WELCOME when the server supports reminder ops.
const CapRemind = "remind"

// Reminder limits.
const (
	MaxReminders   = 100                  // pending per user
	MaxRemindText  = 4 << 10              // bytes
	MaxRemindAhead = 366 * 24 * time.Hour // how far ahead "at" may be
	// RemindPrefix starts the body of a delivered reminder.
	RemindPrefix = "⏰ "
)

// Reminder ops.
const (
	OpRemindCreate = "remind.create"
	OpRemindList   = "remind.list"
	OpRemindDelete = "remind.delete"
)

// Reminder is a pending reminder. At is unix milliseconds.
type Reminder struct {
	ID   uint64 `msgpack:"id"`
	At   int64  `msgpack:"at"`
	Text string `msgpack:"text"`
	Chan uint64 `msgpack:"chan,omitempty"`
}

type RemindCreateArgs struct {
	At   int64  `msgpack:"at"`
	Text string `msgpack:"text"`
	Chan uint64 `msgpack:"chan,omitempty"`
}

type RemindCreateRes struct {
	ID uint64 `msgpack:"id"`
}

type RemindListRes struct {
	Reminders []Reminder `msgpack:"reminders"`
}

type RemindDeleteArgs struct {
	ID uint64 `msgpack:"id"`
}
