package proto

// Personal agents, cap agent1 (PROTOCOL.md §15). An agent is a device on its
// owner's account that logs in with HELLO agent = its handle; the server
// stamps Message.Agent from that session, so it can't be forged.

// CapAgent is the capability for §15. A server that supports it advertises
// it in WELCOME; agent fields, ops, events and ephemerals exist only in
// sessions where both sides advertise it.
const CapAgent = "agent1"

// Agent ops (PROTOCOL.md §15.2).
const (
	OpAgentRegister = "agent.register"
	OpAgentUpdate   = "agent.update"
	OpAgentRetire   = "agent.retire"
	OpAgentsList    = "agents.list"
	OpAgentAccess   = "agent.access"
	OpAgentDM       = "agent.dm"
)

// Agent events. KAgentUpdate carries an Agent that was registered, changed
// or retired (Retired != 0), to every agent1 session; KAgentAccess carries
// the owner's new access list (AgentAccessRes) to the owner's agent1
// sessions.
const (
	KAgentUpdate = "agent.update"
	KAgentAccess = "agent.access"
)

// IsAgentKind reports whether an event kind exists only for agent1 sessions.
func IsAgentKind(kind string) bool { return kind == KAgentUpdate || kind == KAgentAccess }

// IsMsgKind reports whether an event kind carries message content: what an
// agent session sees only in the channels it may read (§15.4).
func IsMsgKind(kind string) bool {
	switch kind {
	case KMsgNew, KMsgEdit, KMsgDelete, KReactAdd, KReactRemove, KPinAdd, KPinRemove:
		return true
	}
	return false
}

// IsDMKind reports whether a channel kind has a fixed roster (DMs and agent
// DMs): no joins, adds, removes, leaves or topics.
func IsDMKind(kind string) bool { return kind == ChanDM || kind == ChanAgentDM }

// Ephemeral kinds (PROTOCOL.md §15.5).
const (
	// EphAgent is an agent's live status in a thread: State, Status and a
	// streamed preview (Text, or Enc in e2e channels). Never stored.
	EphAgent = "agent"
	// EphAgentPresence says an agent's owner (User) has an agent session
	// connected (State PresOnline) or none (PresOffline).
	EphAgentPresence = "agent.presence"
)

// Agent live states (Ephemeral.State of kind EphAgent).
const (
	AgentThinking  = "thinking"
	AgentWorking   = "working"
	AgentStreaming = "streaming"
	AgentDone      = "done"
	AgentAsleep    = "asleep"
	AgentError     = "error"
)

// ValidAgentState reports whether s is one of the agent live states.
func ValidAgentState(s string) bool {
	switch s {
	case AgentThinking, AgentWorking, AgentStreaming, AgentDone, AgentAsleep, AgentError:
		return true
	}
	return false
}

// ChanAgentDM is the kind of an owner's private chat with their own agent:
// one per user, sole member the owner, readable only by the owner's sessions
// (person and agent devices). Older clients list it as a channel of unknown
// kind.
const ChanAgentDM = "agent_dm"

// Agent limits (PROTOCOL.md §15.6).
const (
	MaxAgentIcon    = 32       // bytes
	MaxAgentPreview = 16 << 10 // EphAgent Text, bytes
	MaxAgentEnc     = MaxAgentPreview + 1<<10
	MaxAgentAccess  = 256 // channels in an agent's access list
	// AgentEphRate is the most EphAgent frames per second per session.
	AgentEphRate = 4
)

// ViaMCP marks a message an outside Claude session posted for its author
// through tc mcp (Message.Via).
const ViaMCP = "mcp"

// MaxVia bounds Message.Via.
const MaxVia = 16

// ValidVia reports whether v may be a Message.Via: "" or a short name.
func ValidVia(v string) bool { return v == "" || len(v) <= MaxVia && ValidName(v) }

// ValidAgentHandle reports whether h may name a new agent: a ValidName that
// isn't reserved. Uniqueness across user names and handles is the server's.
func ValidAgentHandle(h string) bool { return ValidName(h) && !ReservedName(h) }

// Agent is a user's personal agent: the agent.register/update results, the
// agent.update event data and one entry of Snapshot.Agents/AgentsRes.
type Agent struct {
	Handle string `msgpack:"handle"`
	Icon   string `msgpack:"icon,omitempty"`
	Owner  uint64 `msgpack:"owner"`
	// Online: an agent session of Owner is connected.
	Online  bool  `msgpack:"online,omitempty"`
	Created int64 `msgpack:"created"`
	// Retired is when the owner retired it (Unix ms); 0 = active.
	Retired int64 `msgpack:"retired,omitempty"`
}

type AgentRegisterArgs struct {
	Handle string `msgpack:"handle"`
	Icon   string `msgpack:"icon,omitempty"`
}

// AgentUpdateArgs changes the caller's active agent; "" leaves a field as
// is, and ClearIcon removes the icon.
type AgentUpdateArgs struct {
	Handle    string `msgpack:"handle,omitempty"`
	Icon      string `msgpack:"icon,omitempty"`
	ClearIcon bool   `msgpack:"clear_icon,omitempty"`
}

type AgentRes struct {
	Agent Agent `msgpack:"agent"`
}

// AgentsRes is agents.list's result: every agent, retired ones included.
type AgentsRes struct {
	Agents []Agent `msgpack:"agents"`
}

// AgentAccessArgs edits the channels the caller's agent sessions may see
// besides the agent DM. Empty args just read the list.
type AgentAccessArgs struct {
	Add    []uint64 `msgpack:"add,omitempty"`
	Remove []uint64 `msgpack:"remove,omitempty"`
}

// AgentAccessRes is the access list after the change.
type AgentAccessRes struct {
	Chans []uint64 `msgpack:"chans"`
}
