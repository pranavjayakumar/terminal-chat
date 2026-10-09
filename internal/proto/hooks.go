package proto

import "strings"

// Incoming webhooks (PROTOCOL.md §5, §12).

// CapHooks is advertised in WELCOME when the server supports hook.* ops
// and the HTTP webhook endpoint.
const CapHooks = "hooks"

// Hook ops.
const (
	OpHookCreate = "hook.create"
	OpHookList   = "hook.list"
	OpHookDelete = "hook.delete"
)

// HookPath is the URL path prefix of the HTTP endpoint: POST /hooks/<token>.
const HookPath = "/hooks/"

// BotSuffix ends every bot user's name; people can't register such names,
// so a bot can never squat a person's name (PROTOCOL.md §5 hook.create).
const BotSuffix = "-bot"

// BotName is the bot user name for a hook named name: name with BotSuffix
// appended unless it already ends in it. ok is false if the result isn't a
// valid user name (§9).
func BotName(name string) (bot string, ok bool) {
	bot = name
	if !strings.HasSuffix(bot, BotSuffix) {
		bot += BotSuffix
	}
	return bot, ValidName(bot)
}

type HookCreateArgs struct {
	Chan uint64 `msgpack:"chan"`
	Name string `msgpack:"name"`
}

// HookCreateRes is the result of hook.create. Clients build any command
// from URL and Pin themselves, after validating both (PROTOCOL.md §12).
type HookCreateRes struct {
	ID   uint64 `msgpack:"id"`
	Name string `msgpack:"name"` // the bot user's name (with BotSuffix)
	URL  string `msgpack:"url"`
	Pin  string `msgpack:"pin"` // curl --pinnedpubkey form: "sha256//" + base64
}

// Hook describes a webhook; its secret URL is only returned by hook.create.
type Hook struct {
	ID        uint64 `msgpack:"id"`
	Chan      uint64 `msgpack:"chan"`
	Name      string `msgpack:"name"`
	CreatedBy uint64 `msgpack:"created_by"`
	Created   int64  `msgpack:"created"`
}

type HooksRes struct {
	Hooks []Hook `msgpack:"hooks"`
}

type HookArgs struct {
	ID uint64 `msgpack:"id"`
}
