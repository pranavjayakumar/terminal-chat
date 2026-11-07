package proto

// Pane sharing in a huddle (PROTOCOL.md §14.5).

// CapWatch gates huddle.share, huddle.watch and the screen datagram kinds.
// It is in effect only together with CapVoice.
const CapWatch = "watch1"

const (
	OpHuddleShare = "huddle.share"
	OpHuddleWatch = "huddle.watch"
)

// HuddleShareArgs starts (On) or stops this participant's pane stream.
type HuddleShareArgs struct {
	On bool `msgpack:"on"`
}

// HuddleWatchArgs asks for (On) or stops the shared pane's chunks.
type HuddleWatchArgs struct {
	On bool `msgpack:"on"`
}

// Screen chunk limits (§14.5). A chunk's plaintext is ScreenChunkHeader
// bytes (update u32, index u8, count u8) and at most ScreenChunkData bytes
// of the update, so the datagram stays within MaxDatagram, sealed or not.
const (
	ScreenChunkHeader = 6
	ScreenChunkData   = MaxDatagram - DgEncHeaderLen - dgTagLen - ScreenChunkHeader
	MaxScreenChunks   = 255
)
