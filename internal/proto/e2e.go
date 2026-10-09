package proto

// End-to-end encryption, cap e2e1 (PROTOCOL.md §13). Only devices so far;
// epochs and encrypted messages follow (docs/E2E.md).

// CapE2E is the capability for §13. Device ops, device events and the
// snapshot's devs/revoked fields exist only in sessions where both sides
// advertise it.
const CapE2E = "e2e1"

// Device ops (PROTOCOL.md §13.2).
const (
	OpDeviceRegister = "device.register"
	OpDeviceRevoke   = "device.revoke"
	OpDevicesList    = "devices.list"
)

// Device event kinds, visible to every e2e1 session (PROTOCOL.md §13.2).
const (
	KDeviceNew    = "device.new"
	KDeviceRevoke = "device.revoke"
)

// Device limits (PROTOCOL.md §13.2). They mirror e2ecrypto's, which proto
// can't import; a server test keeps them equal.
const (
	// MaxSigned bounds len(b)+len(sig) of a DeviceCert, KeyEndorse or DeviceRevoke.
	MaxSigned = 4 << 10
	// MaxDevices is the most non-revoked devices a user may have.
	MaxDevices = 8
	// MaxDeviceRegistrations is the most devices a user may ever register,
	// revoked ones included, so devices.list and snapshots stay bounded.
	MaxDeviceRegistrations = 32
)

// Signed is a signed object: sig over context || b (§13.1). B is opaque on
// the wire and must never be re-encoded; internal/identity verifies and
// decodes it (convert with identity.Signed(s)).
type Signed struct {
	B   []byte `msgpack:"b"`
	Sig []byte `msgpack:"sig"`
}

// Device is a registered device: the device.register args, the device.new
// event data and one entry of Snapshot.Devs. Endorse is the KeyEndorse for
// the SSH key that signed Cert, when the registering client supplied one.
type Device struct {
	Cert    Signed  `msgpack:"cert"`
	Endorse *Signed `msgpack:"endorse,omitempty"`
}

// DeviceRegisterArgs is device.register's args.
type DeviceRegisterArgs = Device

// DeviceRevokeArgs is device.revoke's args and the device.revoke event data.
type DeviceRevokeArgs struct {
	Revoke Signed `msgpack:"revoke"`
}

// DevicesListArgs selects users (empty = all). After pages: only users with
// id > After are returned.
type DevicesListArgs struct {
	Users []uint64 `msgpack:"users,omitempty"`
	After uint64   `msgpack:"after,omitempty"`
}

// DevicesListRes lists every cert (revoked ones too), the distinct
// endorsements attached to them, and every revoke, for whole users in id
// order. Next != 0 means the result was cut at a user boundary to fit
// MaxResultBytes: ask again with the same Users and After = Next.
type DevicesListRes struct {
	Certs   []Signed `msgpack:"certs"`
	Endorse []Signed `msgpack:"endorse"`
	Revoked []Signed `msgpack:"revoked"`
	Next    uint64   `msgpack:"next,omitempty"`
}
