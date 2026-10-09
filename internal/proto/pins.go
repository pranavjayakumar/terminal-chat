package proto

// Pinned messages (PROTOCOL.md §5, §6).

// CapPins is advertised in WELCOME when the server supports pin ops.
const CapPins = "pins"

// MaxPins is the most pins a channel may hold.
const MaxPins = 50

// Pin ops and events.
const (
	OpPin      = "pin"
	OpUnpin    = "unpin"
	OpPinsList = "pins.list"

	KPinAdd    = "pin.add"
	KPinRemove = "pin.remove"
)
