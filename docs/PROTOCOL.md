# tc wire protocol — v1

Status: **draft r2** (after protocol-guardian review). Changes to this file must
be reviewed by the `protocol-guardian` agent. `internal/proto` is the executable
form of this document; if they disagree, that is a bug.

## 1. Goals

- One connection per client carries everything (control, chat, later voice).
- Survives flaky networks: cheap reconnect, gap-free catch-up, no duplicate sends.
- Small and boring: length-prefixed frames, MessagePack payloads, one request/response pattern.
- Forward compatible: old clients keep working when the server adds features.

## 2. Transport

| Transport | Port | Use |
|---|---|---|
| QUIC (UDP) | 7443 | Primary. ALPN `tc/1`. Client opens **one bidirectional stream** for frames. Voice (v2) uses QUIC datagrams on the same connection. |
| TLS over TCP | 7443 | Fallback when UDP is blocked. ALPN `tc/1`. Same frames. |

Client dials both, giving QUIC a 300 ms head start, uses whichever finishes the
TLS handshake first and cancels the other. No persisted preference.

TLS 1.3 only (`MinVersion = VersionTLS13`). QUIC 0-RTT and TLS early data are disabled.

### TLS identity (no domain / CA needed)

The server generates a long-lived self-signed ed25519 certificate on first start.
Clients **pin** the SHA-256 of its SubjectPublicKeyInfo, delivered inside the
invite code. The client sets `InsecureSkipVerify` and in `VerifyConnection`
requires exactly one certificate whose SPKI SHA-256 equals the pin; expiry,
hostname and chain are ignored. Mismatch is fatal (never trust-on-first-use).
The server key never rotates in v1; rotating means re-issuing invites.

## 3. Framing

```
+----------------+---------+--------------------------+
| length: u32 BE | type: u8| payload: msgpack (len-1) |
+----------------+---------+--------------------------+
```

- `length` counts `type` + `payload` bytes. 1 ≤ length ≤ 1 MiB (1,048,576).
  Before authentication the limit is 8 KiB. Over the limit → BYE `frame_too_large`.
- Payload is a MessagePack map with **string keys**. Unknown keys MUST be ignored.
  Decoders cap nesting depth (16) and container lengths by remaining bytes.
- Unknown frame `type` values MUST be skipped, not fatal.

### Frame types

| Type | Name | Dir | Payload |
|---|---|---|---|
| 0x01 | HELLO | C→S | `{client: str, caps: [str], auth: Auth}` |
| 0x05 | WELCOME | S→C | `{server: str, caps: [str], server_id: bin16, user: User, time: i64}` |
| 0x10 | REQ | C→S | `{id: u32, op: str, args: map}` |
| 0x11 | RES | S→C | `{id: u32, ok: bool, data?: any, err?: {code: str, msg: str}}` |
| 0x20 | EVENT | S→C | `Event` (persistent, carries `seq`) |
| 0x21 | EPHEMERAL | both | `{kind: str, ...}` (typing, presence; never stored) |
| 0x22 | SNAPSHOT | S→C | `{users?: [User], chans?: [Channel], msgs?: [Message], read?: {chan: seq}, devs?, revoked?}` (one part; `devs`/`revoked` §12.2) |
| 0x30 | PING | both | `{t: i64}` |
| 0x31 | PONG | both | `{t: i64}` (echo) |
| 0x40 | VOICE | both | reserved for v2 (TCP fallback only; QUIC uses datagrams) |
| 0x7F | BYE | both | `{code: str, msg: str}` then close |

Effective caps = intersection of HELLO and WELCOME caps. Protocol version is the ALPN.

## 4. Handshake & authentication

Identity = the user's **SSH public key**. ed25519 preferred; ECDSA and RSA
accepted; RSA must sign with `rsa-sha2-256`/`rsa-sha2-512` (`ssh-rsa`/SHA-1 is
rejected). Signing goes through `ssh-agent` when available, so
passphrase-protected and hardware-backed keys work.

One round trip:

```
C → HELLO {auth}
S → WELCOME | BYE{code}
```

```
Auth = {pubkey: bin, sig: bin, invite?: str, name?: str}
```

- `pubkey`: SSH wire format (`ssh.PublicKey.Marshal`).
- `sig`: SSH wire format `ssh.Signature` over
  `"tc-auth-v1\x00" || tls_exporter(32)`.
- `tls_exporter`: TLS 1.3 keying-material exporter, label `"EXPORTER-tc-auth"`,
  empty context, 32 bytes, computed after the handshake. Identical on quic-go and
  crypto/tls. It is unique per TLS session (contains both sides' randomness), so a
  signature can't be replayed on another connection or relayed by a MITM.
- Without `invite`: log in an existing key.
- With `invite` (+ `name` for a new user): redeem an invite (§4.1).

The handshake must complete within 30 s (hardware keys may wait for a touch).

### 4.1 Invites

Created by an admin (`/invite`, op `invite.create`) or on the server host
(`tc server invite [--admin]`). Single-use, expire in 7 days. Redemption marks
the invite used and creates the user/key in one transaction.

```
tc://<host>:<port>/<token>#<pin>
```

`token` = 16 random bytes, lowercase base32 without padding, compared in
constant time. `pin` = base64url (no padding) SHA-256 SPKI pin.

Kinds:
- **new user**: requires `name`; `--admin` invites (CLI only) create admins.
  Admin rights come from the invite, never from registration order.
- **add key** (`invite.create {user}`): attaches `pubkey` to an existing user
  (new laptop). `name` is ignored.

### 4.2 BYE codes

`auth_failed, invite_invalid, name_taken, key_exists, frame_too_large,
handshake_timeout, protocol_error, slow_consumer, server_shutdown, revoked`.

## 5. Requests

Every REQ gets exactly one RES with the same `id`. Client chooses ids. Error codes:
`bad_request, unauthorized, forbidden, not_found, conflict, rate_limited,
too_large, unknown_op, internal`.

`sync` MUST be the first REQ, once per connection. Other REQs before its RES → `bad_request`.

For every write op the server delivers the resulting EVENT to the caller's
session **before** the RES. The `seq` in a RES is informational: clients advance
`last_seq` only by applying EVENT/SNAPSHOT frames.

| op | args | data |
|---|---|---|
| `sync` | `{since: u64, server_id?: bin16}` | `{head: u64, reset: bool}` (see §6) |
| `send` | `{chan, body: str, nonce: bin16, thread?: u64}` | `{msg: u64}` |
| `edit` | `{msg, body: str}` | `{seq}` |
| `delete` | `{msg}` | `{seq}` |
| `react` / `unreact` | `{msg, emoji: str}` | `{seq}` |
| `history` | `{chan, before: u64 (0 = latest), limit ≤ 200, thread?: u64}` | `{msgs: [Message], more: bool}` — with `thread`, that thread's replies only |
| `search` | `{q: str, chan?: u64, limit ≤ 50}` | `{msgs: [Message]}` |
| `read` | `{chan, seq}` | `{}` |
| `chan.create` | `{name, private: bool, topic?: str}` | `{chan: Channel}` |
| `chan.join` / `chan.leave` | `{chan}` | `{}` |
| `chan.add` | `{chan, user}` | `{}` |
| `chan.topic` | `{chan, topic: str}` | `{seq}` |
| `dm.open` | `{users: [u64]}` (0–7 others; 0 = your private notes) | `{chan: Channel}` |
| `users.list` | `{}` | `{users: [User]}` |
| `invite.create` | `{user?: u64}` (admin) | `{code: str}` |
| `user.disable` | `{user}` (admin) | `{}` — closes their sessions with BYE `revoked` |
| `user.update` | `{display: str}` (self) | `{seq}` — emits `user.update`; unchanged → `{seq: 0}` |

Rules:
- **send idempotency**: `(author, nonce)` is permanently unique. A repeat returns
  the original `{msg}` (even if since deleted). Same nonce with different
  `chan`/`body`/`thread` → `conflict`. Clients retry from their outbox freely.
- **thread** must be a top-level message in the same `chan`; no nested threads.
- **edit**: author, while a member of the channel (else `not_found`). **delete**: the author (even after
  leaving) or any admin; deleted messages keep their
  id with `body = ""`, `deleted = true`. Edit or react on a deleted message → `not_found`.
- **react/unreact/delete** are set-semantics: a no-op returns ok `{seq: 0}` and emits nothing.
- **read** only moves forward (server keeps the max) and is clamped to the channel's newest message id.
- **react**: at most 32 distinct emoji per message and 10 reactions per user per message (`bad_request`).
- **history/search** cover public channels and channels where the caller is
  currently a member; `history` on others → `forbidden`, `search` filters silently.
  Responses stop at `limit` or ~768 KiB of encoded messages, whichever comes first.
- **chan.join** works on public channels only (private → `forbidden`).
  **chan.add**: any member of a public channel; members of a private channel.
  DMs can't be joined, added to, or left.
- **dm.open** returns the existing DM for that exact member set (caller included)
  or creates it. DMs have `name = ""`.

## 6. Events & sync

The server keeps one **global, monotonically increasing `seq`** (u64) over an
append-only event log (see *Edited and deleted text* for the one exception).

```
Event = {seq: u64, kind: str, chan?: u64, time: i64, actor: u64, data: map}
```

| kind | data |
|---|---|
| `msg.new` | `Message` |
| `msg.edit` | `{msg, body, edited: i64}` |
| `msg.delete` | `{msg}` |
| `react.add` / `react.remove` | `{msg, emoji}` |
| `chan.new` / `chan.update` | `Channel` |
| `chan.join` | `{user, chan: Channel}` |
| `chan.leave` | `{user}` |
| `user.new` / `user.update` | `User` |
| `read.update` | `{chan, seq}` (visible only to the actor) |

### Visibility

An event on channel C is visible to user U iff U is a member of C **at that
event's seq**. Exceptions: `chan.join`/`chan.leave` about U are always visible
to U; `chan.new`/`chan.update` of public channels and all `user.*` are visible
to everyone. Live fan-out checks membership at send time; after U's `chan.leave`
commits, no more events for C reach U's sessions. A new member gets the full
Channel in their `chan.join` event and fetches older messages with `history`.

Seq values a client sees are therefore **not contiguous**; gaps are normal.

### Edited and deleted text

Edits and deletes rewrite the log so replay never resurrects replaced text
(events keep their seq and kind; only `data` changes):
- the message's `msg.new` (seq = message id) always carries its **current**
  state — latest `body` and `edited`, or `body = ""`, `deleted = true`;
- every superseded `msg.edit` of it (all of them once it is deleted) carries
  `body = ""`. A real edit never has an empty body, so clients ignore a
  `msg.edit` whose `body` is empty.

### Sync algorithm

After WELCOME the client sends `sync {since: last_seq, server_id}`.

1. The server registers the session as a live subscriber, then records
   H = current head. Live events with seq > H are buffered.
2. **Snapshot path** — if `since == 0`, `server_id` differs from the server's,
   `since > H`, or `H − since > 10,000`: send SNAPSHOT parts (each ≤ 1 MiB) with
   users, visible channels, read markers and the last 50 messages per channel,
   then RES `{head: H, reset: true}`. With `reset` the client discards its local
   cache (but **not** its outbox) before applying the snapshot.
3. **Replay path** — otherwise replay visible events with since < seq ≤ H in
   order, then RES `{head: H, reset: false}`.
4. Then flush the buffer and stream live events in seq order.

Each session's outbound queue is bounded (1024 frames). On overflow the server
sends BYE `slow_consumer`; the client reconnects and syncs.

Clients apply events idempotently (`seq ≤ last_seq` → ignore) and persist
`last_seq` only after applying.

## 7. Ephemeral

| kind | fields | notes |
|---|---|---|
| `typing` | `{chan, user}` | C→S at most every 3 s while typing; dropped unless sender is a member; fan-out to members |
| `presence` | `{user, state: "online"\|"away"\|"offline", status?: str}` | S→C on change; C→S sets own away/status |

The server overwrites `user` in every C→S EPHEMERAL with the authenticated user.

## 8. Objects

```
User    = {id, name, display, admin: bool, disabled: bool, created: i64}
Channel = {id, name, kind: "public"|"private"|"dm", topic, members: [u64], created}
Message = {id, chan, author, body, thread: u64 (0 = top level), time,
           edited: i64 (0 = never), deleted: bool, reactions: {emoji: [u64]}}
```

`Message.id` is the seq of its `msg.new` event, so ids order chronologically
and map directly onto read markers.

Times are Unix milliseconds.

## 9. Limits & liveness

All sizes are UTF-8 bytes.

| Field | Limit |
|---|---|
| message body | ≤ 64 KiB |
| user/channel `name` | `^[a-z0-9][a-z0-9_-]{0,31}$` |
| `display` | ≤ 64 B |
| `topic` | ≤ 256 B |
| `status` | ≤ 128 B |
| `emoji` | ≤ 64 B, no whitespace |

- Rate limits (token buckets per user, `rate_limited` beyond): `send` burst 30, refill 3/s; all other
  write ops share a bucket of burst 60, refill 5/s.
- `search.q` ≤ 256 B; only the first 8 terms are used.
- Presence updates: at most 1/s per session, and only broadcast when state or status changes.
- Each session's unsent output is capped at 8 MiB (sync output excluded); beyond that → BYE `slow_consumer`.
- Keepalive: the client sends PING after 20 s without sending anything. Either
  side closes after 45 s without receiving a frame. QUIC `MaxIdleTimeout` = 60 s.
- On QUIC, BYE is followed by `CloseWithError(0, code)`.
- Reconnect: exponential backoff 0.25 s → 30 s with jitter, reset on success.

## 10. Content safety

All user-controlled text is **untrusted terminal input**. The server rejects
(`bad_request`) any body, display name, topic, status or emoji that is invalid
UTF-8 or contains control characters other than `\n` and `\t` in bodies (C0,
DEL, C1, ESC). Display names, topics, status and emoji additionally reject bidi
controls (U+202A–U+202E, U+2066–U+2069). Clients MUST still sanitize everything
on render (`internal/sanitize`): defense in depth, so a buggy or old server can
never drive a terminal.

## 11. Versioning rules

- Breaking change → new ALPN (`tc/2`). Server may speak both.
- Additive change → new `caps` string, optional fields, new ops/kinds/frame types.
- Clients ignore unknown event kinds, fields and frame types.
- Servers return `unknown_op` for unknown ops and never close the connection for it.

## 12. End-to-end encryption (cap `e2e1`)

Design and threat model: `docs/E2E.md` ("Review r1 → r2" is authoritative).
Everything in this section exists only in sessions whose effective caps
(§3) include `e2e1`: without it the server answers the ops below with
`unknown_op`, never sends the events below, and omits the snapshot fields.
The server advertises `e2e1` in WELCOME.

**The server is not trusted for any of this.** It checks what it can (so an
honest server rejects garbage early), but clients verify every signed object
themselves against SSH keys they pinned; server-side checks are no evidence.

### 12.1 Signed objects

```
Signed = {b: bin, sig: bin}
```

`sig` is over `context || b`, where `context` is a NUL-terminated tag that
names the object type. Verify `sig` before decoding `b`; `b` is never
re-encoded (forward it byte for byte). `b` is a msgpack map in canonical form:
the exact encoding `internal/proto` produces (fields in the listed order,
smallest int encodings, no extra keys); anything else is rejected. H =
SHA-256; integers in binary layouts are big-endian. Ids are `bin16`, keys
`bin32`, times Unix ms.

SSH signatures use the §4 wire format and algorithm rules (no `ssh-rsa`/SHA-1;
`sk-*` signatures must have the user-presence flag).

### 12.2 Devices

Each device has its own X25519 and Ed25519 keys (the SSH agent can sign but
not do ECDH). The user's SSH key vouches for them:

```
DeviceCert.b   = {v: 1, server_id: bin16, user: u64, dev: bin16, x25519: bin32,
                  ed25519: bin32, ssh: bin, created: i64}
DeviceCert.sig = SSH signature by `ssh` (wire-encoded key), context "tc-device-v1\x00"

KeyEndorse.b   = {v: 1, server_id, user, new_ssh: bin, time: i64}
KeyEndorse.sig = SSH signature by an already-endorsed key of `user`, context "tc-endorse-v1\x00"

DeviceRevoke.b   = {v: 1, server_id, user, dev, time: i64}
DeviceRevoke.sig = SSH signature by an endorsed key of `user` ("tc-revoke-v1\x00"), or
                   Ed25519 by another non-revoked device of `user` ("tc-revoke-dev-v1\x00")
```

`Device = {cert: Signed, endorse?: Signed}` (`endorse` is a KeyEndorse for
`cert`'s `ssh`, for clients that pinned an older key of the user).

| op | args | data |
|---|---|---|
| `device.register` | `{cert: Signed, endorse?: Signed}` | `{seq}` |
| `device.revoke` | `{revoke: Signed}` | `{seq}` |
| `devices.list` | `{users?: [u64], after?: u64}` | `{certs: [Signed], endorse: [Signed], revoked: [Signed], next?: u64}` |

| event kind | data | visible to |
|---|---|---|
| `device.new` | `Device` | every `e2e1` session |
| `device.revoke` | `{revoke: Signed}` | every `e2e1` session |

Server rules (honest case):
- **device.register** (write op, rate limited): `cert` and `endorse` are
  each ≤ 4 KiB (`len(b) + len(sig)`, else `too_large`). The cert must verify
  under one of the caller's registered SSH keys (§4.1) — not signed by one →
  `forbidden`; cert for another `user` → `forbidden`; other `server_id`,
  bad signature or malformed → `bad_request`. An `endorse` must verify
  under one of the caller's registered keys other than `new_ssh`, with
  `user` = caller and `new_ssh` = the cert's `ssh` (else `bad_request`); the
  server stores and forwards it but does not register `new_ssh` as a login key.
  `dev` already registered → `conflict`, except an exact repeat of the
  same cert and endorse by the same user, which is ok `{seq: 0}` and
  emits nothing. An `x25519` or `ed25519` key already used by any device →
  `conflict` (clients must check this too: a shared X25519 key would reuse
  key and nonce in epoch boxes). A user may have at most 8 non-revoked devices and register
  at most 32 ever (`bad_request`). Emits `device.new`.
- **device.revoke** (write op, rate limited): `revoke` ≤ 4 KiB (`too_large`).
  It must verify under a registered SSH key of the caller, or under the
  Ed25519 key of a different, non-revoked device of the caller (else
  `forbidden`); `user` must be the caller (`forbidden`); other `server_id`
  → `bad_request`; unknown `dev` or a device of someone else → `not_found`.
  Revoking a revoked device is ok `{seq: 0}` and emits nothing. Revocation
  is permanent. Emits `device.revoke`.
- **devices.list**: devices of `users` (absent/empty = everyone; at most
  4096 entries, else `bad_request`), including
  revoked ones, with every distinct `endorse` attached to them and every
  revoke, ordered by user. Whole users only; when the next user would push
  the result past ~768 KiB the server stops and sets `next`: call again with
  the same `users` and `after = next`.
- **Snapshot** (§6) adds `devs: [Device]` (every device, revoked ones too) and
  `revoked: [Signed]` (every DeviceRevoke), possibly over several parts.

Clients store what they receive and decide trust locally: a cert counts only
if it verifies under an SSH key pinned for its `user` (directly or via a
KeyEndorse chain from a pinned key), and a revoke only if it verifies under a
pinned key or a verified device of that user. Unverifiable objects are kept
but ignored.

Limits: `Signed` objects here ≤ 4 KiB; ≤ 8 non-revoked devices per user;
≤ 32 device registrations per user, ever.

### 12.3+ Epochs and encrypted messages

See `docs/E2E.md` (in progress).
