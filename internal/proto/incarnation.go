package proto

// CapIncarnation (server-advertised, PROTOCOL.md §6): sync takes and returns
// `inc`, the id of the server's current event history. It changes when the
// server finds its database restored from a backup, and a sync whose `inc`
// differs gets a reset, so a cursor from the lost history never replays into
// reused message ids.
const CapIncarnation = "incarnation"
