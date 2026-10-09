package proto

// User groups (PROTOCOL.md §5, §6, §8): named sets of users that @name
// mentions at once.

// CapGroups is advertised in WELCOME when the server supports group ops.
const CapGroups = "groups"

// MaxGroupMembers is the most members a group may have; MaxGroups the most
// groups a server may hold.
const (
	MaxGroupMembers = 50
	MaxGroups       = 200
)

// Group ops and event.
const (
	OpGroupSet   = "group.set"
	OpGroupsList = "groups.list"

	KGroupUpdate = "group.update"
)

// Group is a named set of users. Members is sorted and duplicate-free.
type Group struct {
	Name    string   `msgpack:"name"`
	Members []uint64 `msgpack:"members"`
}

// GroupSetArgs sets a group's members; empty Members deletes the group.
type GroupSetArgs = Group

type GroupsRes struct {
	Groups []Group `msgpack:"groups"`
}
