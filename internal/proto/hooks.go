package proto

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

type HookCreateArgs struct {
	Chan uint64 `msgpack:"chan"`
	Name string `msgpack:"name"`
}

type HookCreateRes struct {
	ID   uint64 `msgpack:"id"`
	URL  string `msgpack:"url"`
	Curl string `msgpack:"curl"`
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
