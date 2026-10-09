package proto

// OpUserUpdate changes the caller's own profile (PROTOCOL.md §5).
const OpUserUpdate = "user.update"

type UserUpdateArgs struct {
	Display string `msgpack:"display"`
}
