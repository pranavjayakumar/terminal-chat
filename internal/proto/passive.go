package proto

// CapPassive in HELLO marks a session that only listens (`tc background`,
// PROTOCOL.md §4.3): it never counts toward the user's presence, its
// typing and presence ephemerals are dropped, and it can't join huddles.
// A server that honors it echoes it in WELCOME.
const CapPassive = "passive"
