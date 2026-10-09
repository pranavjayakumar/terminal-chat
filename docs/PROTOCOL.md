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
| HTTPS (TLS over TCP) | 7443 | Webhooks only (§12). ALPN `http/1.1` on the same TCP listener. |

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
| 0x22 | SNAPSHOT | S→C | `{users?: [User], chans?: [Channel], msgs?: [Message], read?: {chan: seq}, groups?: [Group], devs?, revoked?, keys?}` (one part; `groups` §8, `devs`/`revoked` §13.2, `keys` §13.6) |
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
  Admin rights come from the invite, never from registration order. Names ending in
  `-bot` are reserved for webhook bots (§5 `hook.create`) → BYE `protocol_error`; a name
  that exists → BYE `name_taken`.
- **add key** (`invite.create {user}`): attaches `pubkey` to an existing user
  (new laptop). `name` is ignored.

### 4.2 BYE codes

`auth_failed, invite_invalid, name_taken, key_exists, frame_too_large,
handshake_timeout, protocol_error, slow_consumer, server_shutdown, revoked`.

## 5. Requests

Every REQ gets exactly one RES with the same `id`. Client chooses ids. Error codes:
`bad_request, unauthorized, forbidden, not_found, conflict, rate_limited,
too_large, unknown_op, internal`, and `stale_epoch` (§13.8).

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
| `pin` / `unpin` | `{msg}` | `{seq}` (cap `pins`) |
| `pins.list` | `{chan}` | `{msgs: [Message]}` — current pins, newest pin first (cap `pins`) |
| `history` | `{chan, before: u64 (0 = latest), limit ≤ 200, thread?: u64}` | `{msgs: [Message], more: bool}` — with `thread`, that thread's replies only |
| `search` | `{q: str, chan?: u64, limit ≤ 50}` | `{msgs: [Message]}` |
| `read` | `{chan, seq}` | `{}` |
| `chan.create` | `{name, private: bool, topic?: str, e2e?: bool}` | `{chan: Channel}` (`e2e`: §13.3) |
| `chan.join` / `chan.leave` | `{chan}` | `{}` |
| `chan.add` | `{chan, user}` | `{}` |
| `chan.topic` | `{chan, topic: str}` | `{seq}` |
| `dm.open` | `{users: [u64]}` (0–7 others; 0 = your private notes) | `{chan: Channel}` |
| `users.list` | `{}` | `{users: [User]}` |
| `invite.create` | `{user?: u64}` (admin) | `{code: str}` |
| `user.disable` | `{user}` (admin) | `{}` — closes their sessions with BYE `revoked` |
| `user.update` | `{display: str}` (self) | `{seq}` — emits `user.update`; unchanged → `{seq: 0}` |
| `hook.create` | `{chan, name}` | `{id: u64, name: str, url: str, pin: str}` (cap `hooks`, §12) |
| `hook.list` | `{}` | `{hooks: [{id, chan, name, created_by: u64, created: i64}]}` |
| `hook.delete` | `{id}` | `{}` |
| `group.set` | `{name, members: [u64]}` (admin) | `{seq}` (cap `groups`) |
| `groups.list` | `{}` | `{groups: [Group]}` (cap `groups`) |
| `remind.create` | `{at: i64, text: str, chan?: u64}` | `{id: u64}` (cap `remind`) |
| `remind.list` | `{}` | `{reminders: [{id, at, text, chan?}]}` — the caller's, soonest first (cap `remind`) |
| `remind.delete` | `{id}` | `{}` (cap `remind`) |

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
- **pin/unpin** are set-semantics like react: a no-op returns ok `{seq: 0}` and emits nothing. The caller
  must currently be a member of the message's channel and the message must exist and not be deleted
  (else `not_found`). At most 50 pins per channel; pinning a 51st → `conflict` (re-pinning an existing
  pin is still a no-op). Deleting a pinned message unpins it: `pin.remove` is emitted in the same
  transaction, right after `msg.delete`. `pin` and `unpin` count as write ops for rate limits.
  **pins.list** works for public channels and channels where the caller is a member (like `history`;
  others → `forbidden`) and omits deleted messages.
- **react**: at most 32 distinct emoji per message and 10 reactions per user per message (`bad_request`).
- **history/search** cover public channels and channels where the caller is
  currently a member; `history` on others → `forbidden`, `search` filters silently.
  Responses stop at `limit` or ~768 KiB of encoded messages, whichever comes first.
- **chan.join** works on public channels only (private → `forbidden`).
  **chan.add**: any member of a public channel; members of a private channel.
  DMs can't be joined, added to, or left.
- **hook.create**: caller must be a member of `chan` (else `forbidden`); DMs → `bad_request`.
  `name` names the hook's **bot user**. Bot names always end in `-bot`: the server appends
  it unless `name` already ends in it (`ci` → `ci-bot`); the result must follow the
  user-name rule (§9, so at most 32 bytes including `-bot`) or → `bad_request`. People can't
  register names ending in `-bot` (§4), so bots never squat a person's name. An existing bot
  with that name is reused only by its owner (the user whose `hook.create` created it) or an
  admin; anyone else → `conflict`. Otherwise a new bot is created (`user.new`) and owned by
  the caller; a human with that name → `conflict`. If the bot isn't a member of `chan` it is
  added (`chan.join`, actor = caller). The result's `name` is the bot's name. Caps
  (`conflict` with a message saying which): at most 10 hooks per creator, 20 per channel,
  and 100 **active** bots per server and 10 per owner. A bot is *active* while it has at
  least one hook; reusing an active bot doesn't take a slot. A *retired* bot (below) counts
  toward no cap, but creating a hook under its name reactivates it — by its owner or an
  admin only, and only if a slot is free (the caps are checked against the bot's owner).
  `url` is secret and returned only here. **hook.list** returns hooks in channels the caller
  currently belongs to (admins: all), filtered in the query. **hook.delete**: the creator, an admin, or any current
  member of the hook's channel (anyone else → `not_found`); the URL stops working at once. The
  bot user and its messages stay. When a bot's last hook is deleted the bot is *retired*:
  the user keeps its id and name (so old messages still render, and nobody else can take the
  name) and its owner, but frees its slots. Retirement is server state only — no event, and
  `User` is unchanged. Disabling a user (`user.disable`) deletes the hooks they
  created (and any posting as them) in the same transaction, retiring bots left without hooks. `hook.create`, `hook.list` and
  `hook.delete` count as write ops for rate limits.
- **dm.open** returns the existing DM for that exact member set (caller included)
  or creates it. DMs have `name = ""`.
- **group.set** replaces a group's members (sorted, duplicates dropped) and emits `group.update`;
  non-admins get `forbidden`. `name` follows the user/channel name rule (§9) and must not end in
  `-bot` (`bad_request`); it must not be a user's name (`conflict`), and registering a user whose
  name is a group's fails with BYE `name_taken`. Members must be existing, enabled, non-bot users
  (`not_found` / `bad_request`) and number at most 50 (`bad_request`); at most 200 groups
  (`conflict`). **Empty `members` deletes** the group. Setting what is already stored, or deleting
  a group that doesn't exist, is a no-op: ok `{seq: 0}`, no event. `group.set` counts as a write op
  for rate limits.
- **Mentions of a group are decided by clients.** A message containing `@<group>` as a whole word
  (the same rule as `@<user>`: not inside a longer name or an email address, case-insensitive)
  counts as a mention for every member, in every channel the member is in. Like any mention, a
  bot's message and one's own never count. The server neither parses bodies nor notifies.
- **remind.create** schedules a reminder for the caller. `at` is absolute Unix milliseconds, in
  `(0, now + 366 days]` (else `bad_request`; a time already past is delivered at once). `text` is
  validated like a message body (non-empty, no control characters; `bad_request`) and is at most
  4 KiB (`too_large`). `chan`, if set, must be readable by the caller (`forbidden`). At most 100
  pending reminders per user (`conflict`). The server owns delivery (§6.1): it works with no client
  connected. **remind.list** returns the caller's pending reminders; **remind.delete** removes one
  of the caller's (anyone else's or an unknown id → `not_found`). `remind.create` and
  `remind.delete` count as write ops for rate limits.

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
| `pin.add` / `pin.remove` | `{msg}` (cap `pins`; visible like reactions: members at that seq) |
| `chan.new` / `chan.update` | `Channel` |
| `chan.join` | `{user, chan: Channel}` |
| `chan.leave` | `{user}` |
| `user.new` / `user.update` | `User` |
| `group.update` | `Group` (cap `groups`; visible to everyone; no members = deleted) |
| `read.update` | `{chan, seq}` (visible only to the actor) |

### Visibility

An event on channel C is visible to user U iff U is a member of C **at that
event's seq**. Exceptions: `chan.join`/`chan.leave` about U are always visible
to U; `chan.new`/`chan.update` of public channels and all `user.*` are visible
to everyone, as is `group.update`. Live fan-out checks membership at send time; after U's `chan.leave`
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
   users, groups, visible channels, read markers and the last 50 messages per channel,
   then RES `{head: H, reset: true}`. With `reset` the client discards its local
   cache (but **not** its outbox) before applying the snapshot.
3. **Replay path** — otherwise replay visible events with since < seq ≤ H in
   order, then RES `{head: H, reset: false}`.
4. Then flush the buffer and stream live events in seq order.

Each session's outbound queue is bounded (1024 frames). On overflow the server
sends BYE `slow_consumer`; the client reconnects and syncs.

Clients apply events idempotently (`seq ≤ last_seq` → ignore) and persist
`last_seq` only after applying.

### 6.1 Reminders

Pending reminders live in the server's database, not in the event log. A scheduler owned by the
server sleeps until the earliest `at` (and wakes when a new reminder could be earlier); it does not
poll on a short tick. When a reminder is due it is posted, as if sent by its user, into the user's
private notes (the DM with only themselves, created if the user never opened it; not encrypted, the
server writes it): a `msg.new` (preceded by `chan.new` if the notes were created) whose `body` is
`⏰ ` + `text`, followed by ` (from #name)` if `chan` was set and the user can still read it
(` (from a DM)` for a DM; nothing otherwise). The message's `time` is the delivery time.

Delivery is exactly once: the pending row is deleted in the same transaction that inserts the
message (and the message nonce is derived from the reminder id as a second guard), so a crash or
restart can neither lose nor repeat it. A reminder that fell due while the server was down is
delivered as soon as the server starts. A reminder whose user is disabled is dropped.

A delivered reminder is an ordinary message by the user, so ordinary rules would skip it for
notifications and unread counts. Clients identify one as: authored by me, in my notes, body starting
with `⏰ `, and then notify and count it as unread like a DM from someone else.

## 7. Ephemeral

| kind | fields | notes |
|---|---|---|
| `typing` | `{chan, user}` | C→S at most every 3 s while typing; dropped unless sender is a member; fan-out to members |
| `presence` | `{user, state: "online"\|"away"\|"offline", status?: str}` | S→C on change; C→S sets own away/status |

The server overwrites `user` in every C→S EPHEMERAL with the authenticated user.

## 8. Objects

```
User    = {id, name, display, admin: bool, disabled: bool, created: i64, bot?: bool}
Channel = {id, name, kind: "public"|"private"|"dm", topic, members: [u64], created,
           e2e?: bool, epoch?: u32, creator?: u64, pins?: [u64]}   (e2e/epoch/creator: §13.3)
Message = {id, chan, author, body, thread: u64 (0 = top level), time,
           edited: i64 (0 = never), deleted: bool, reactions: {emoji: [u64]}}
```

`Channel.pins` (cap `pins`; omitted when empty) lists the ids of the channel's pinned messages, newest pin
first; snapshots and `chan.*` events carry it, and `pin.add`/`pin.remove` keep it current. The server
advertises cap `pins` in WELCOME; older servers simply lack the ops and the field.

`User.bot` (omitted when false) marks a webhook bot user (§12). Bots have no keys, never
log in (add-key invites can't target them) and never have presence. Clients MUST NOT treat a
bot's message as a mention or DM notification (its text comes from outside tc); it still
counts as unread.

```
Group = {name: str, members: [u64]}
```

A `Group` (cap `groups`) is a named set of users that `@name` mentions at once (§5 rules). The
server advertises caps `groups` and `remind` in WELCOME; older servers lack the ops.

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

## 12. Webhooks (HTTP)

Cap `hooks`. Lets scripts, CI jobs and agents post into a channel with a plain HTTPS
request — no tc install, no SSH key.

**URL.** `https://<public addr>/hooks/<token>`. The server's public address is validated
at startup as `host:port` (DNS name, IPv4 or bracketed IPv6 without zone, numeric port;
anything else, e.g. quotes or spaces, refuses to start). An empty host (`:7443`) becomes
`localhost` in URLs, with a warning in the log.

**Endpoint.** The TLS-over-TCP listener offers ALPN `tc/1` and `http/1.1` (in that
order of preference). Connections that negotiate `http/1.1` — or no ALPN at all, which a
tc client never does — are served by an HTTP/1.1 server; `tc/1` gets the frame protocol;
any other offer fails the TLS handshake. QUIC stays `tc/1` only, and tc clients offer only `tc/1`.

```
POST https://<host>:<port>/hooks/<token>
```

`token` = 16 random bytes, lowercase base32 without padding (like invites); the server
stores only its SHA-256. The URL **is** the credential.

HTTP clients are served a second long-lived self-signed certificate, **ECDSA P-256**
(`https.key`/`https.crt` in the data dir), because common TLS stacks (e.g. LibreSSL in
macOS curl) can't verify Ed25519. The server picks it for any ClientHello that doesn't
offer `tc/1`. Callers pin it like tc clients pin theirs: curl
`-k --pinnedpubkey 'sha256//<b64>'`, where `<b64>` is the SPKI SHA-256 of the HTTP
certificate in standard base64 with padding (`-k` only skips CA/hostname checks; the pin
is still enforced). `hook.create` returns `url` and `pin` (the `sha256//<b64>` form) as
separate fields and no command: a client that offers one builds it itself, after checking
`url` matches `^https://[A-Za-z0-9.:\[\]-]+/hooks/[a-z2-7]+$` and `pin` is `sha256//` plus
standard base64 of 32 bytes, so a hostile server can't smuggle shell into it. tc's command
reads the URL from `$TC_HOOK_URL` (set by a commented `export` line) to keep the secret out
of shell history and `ps`.

**Request.** `Content-Type: application/json` → body `{"text": str}`; any other type →
the raw body is the text. Reads at most 128 KiB (→ 413). The text must be valid UTF-8
(→ 400); ANSI CSI/OSC sequences and `
` are stripped, other control characters removed
(`sanitize.StripANSI`), trailing whitespace trimmed; then the normal message rules apply
(empty → 400, > 64 KiB → 413). Optional `Idempotency-Key` header (≤ 256 B): the send
nonce becomes the first 16 bytes of `SHA-256(SHA-256(token) || key)` (the token hash is
unique forever; hook ids are never reused either), so retries return
the original message (send idempotency, §5); the same key with different text → 409.

**Effect.** Exactly a `send` by the hook's bot user into the hook's channel: a
`msg.new` with `author` = bot, same event log, fan-out and visibility. The send is
charged to both the bot's and the hook creator's per-user send bucket (§9), so more hooks
never mean more throughput for one person. Inside the send's write transaction the server
re-checks that the hook still exists, that its creator is enabled and still a member of
the channel, and that its bot is enabled and still a member; otherwise the request gets
the same 404 as an unknown token (a hook whose creator left is dormant until they rejoin
or someone deletes it).

**Responses** (JSON, `Cache-Control: no-store`):

| status | body | when |
|---|---|---|
| 200 | `{"msg": u64}` | posted (or idempotent repeat) |
| 400 | `{"error": "bad_request", "msg": str}` | bad JSON, invalid UTF-8, empty |
| 404 | `{"error": "not_found"}` | anything but `POST /hooks/<live token>` — unknown and deleted tokens are indistinguishable |
| 409 | `{"error": "conflict"}` | Idempotency-Key reused with different text |
| 413 | `{"error": "too_large"}` | body or text over the limits |
| 429 | `{"error": "rate_limited"}` + `Retry-After: <s>` | per hook: burst 20, refill 1/s; all hooks on the server together: burst 50, refill 10/s; or the creator's/bot's send bucket (§9) |

Every HTTP request, whatever its path, first spends a token from a per-source bucket
(burst 30, refill 5/s; IPv6 sources grouped by /64) → 429 + `Retry-After`, before any
token lookup. The HTTP server sets header (10 s), read/write (30 s) and idle (10 s)
timeouts, caps headers at 16 KiB, and closes connections beyond 256 open ones, or beyond 16 open ones from one source (same
grouping as the bucket above), unserved.

## 13. End-to-end encryption (cap `e2e1`)

Design and threat model: `docs/E2E.md` ("Review r1 → r2" is authoritative).
Everything in this section exists only in sessions whose effective caps
(§3) include `e2e1`: without it the server answers the ops below with
`unknown_op`, never sends the events below, and omits the snapshot fields.
The server advertises `e2e1` in WELCOME.

**The server is not trusted for any of this.** It checks what it can (so an
honest server rejects garbage early), but clients verify every signed object
themselves against SSH keys they pinned; server-side checks are no evidence.

### 13.1 Signed objects

```
Signed = {b: bin, sig: bin}
```

`sig` is over `context || b`, where `context` is a NUL-terminated tag that
names the object type. Verify `sig` before decoding `b`; `b` is never
re-encoded (forward it byte for byte). `b` is a msgpack map in canonical form:
the exact encoding `internal/proto` produces (fields in the listed order,
smallest int encodings, no extra keys); anything else is rejected. In
particular an unknown field in `b` is rejected, not ignored (unlike the
additive frame fields of §11): a field one verifier ignores and another
honours would give one signature two meanings. Format extensions therefore
need a new `v` (e.g. `v: 2`), which older verifiers reject. H =
SHA-256; integers in binary layouts are big-endian. Ids are `bin16`, keys
`bin32`, times Unix ms.

SSH signatures use the §4 wire format and algorithm rules (no `ssh-rsa`/SHA-1;
`sk-*` signatures must have the user-presence flag).

### 13.2 Devices

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
  emits nothing. A low-order `x25519` point (any encoding; its shared secret with every
  key is zero) is malformed → `bad_request`. An `x25519` or `ed25519` key already used by any device →
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

### 13.3 Epoch bundles *(implemented: format, server rules)*

An encrypted channel (`Channel.e2e`) has a chain of epochs. **Epoch = roster
version**: each epoch fixes the members and a fresh 32-byte key K, sealed to
every device of the roster.

```
EpochBundle.b   = {v: 1, server_id: bin16, chan: u64, epoch: u32 (first = 1),
                   prev: bin32, roster: [u64], by: bin16, time: i64, kc: bin32,
                   eph: bin32, boxes: [{dev: bin16, ct: bin48}], back?: bin72}
EpochBundle.sig = Ed25519 by device `by`, context "tc-epoch-v1\x00"
eh = H(b)[:16]        ChainHash = H(b) (the next bundle's prev)
```

- `prev` = ChainHash of the previous epoch's `b`; all zero for epoch 1.
  `roster` is sorted ascending with no duplicates and non-empty; `boxes` is
  non-empty, at most 128, with distinct `dev`. `back` is REQUIRED for epoch
  > 1 and MUST be absent for epoch 1 (every e2e channel shares history).
  Anything else (including non-canonical msgpack or an unknown field,
  §13.1) is malformed; extending the bundle format needs `v: 2`.
- `kc = HMAC-SHA256(K, "tc-kc-v1" || server_id || chan || epoch)`; one
  ephemeral X25519 key `eph` per bundle; per box
  `ss = X25519(eph_priv, d.x25519)`,
  `bk = HKDF-SHA256(ss, salt = eph || d.x25519, info "tc-box-v1")`,
  `ct = XChaCha20-Poly1305(bk, nonce 0^24, K, AD = server_id || chan || epoch || d.dev || by)`.
- `back = nonce24 || XChaCha20-Poly1305(HKDF(K, "tc-back-v1"), nonce, K_prev, AD = server_id || chan || epoch)`.
- Two boxes to devices with the same X25519 key would reuse key and nonce:
  the server refuses to register such devices (§13.2) and clients MUST
  reject such bundles.

`Channel` gains `e2e: bool` and `epoch: u32` (both omitted when false/0;
`epoch` is 0 until the first `key.rotate`), and `creator: u64`, the user
who created it (omitted when 0: `#general`, and channels whose `chan.new`
predates the field get it from that event's actor). The fields are present in every
session's Channel objects; clients without `e2e1` ignore them (§11).

Server rules (honest case) for **key.rotate** `{chan, bundle}` (write op,
rate limited), checked in this order inside the write transaction (the
bundle is decoded only after the channel, membership and encryption checks,
so non-members can't make the server parse 16 KiB):

| check | error |
|---|---|
| `len(b) + len(sig)` ≤ 16 KiB | `too_large` |
| channel exists | `not_found` |
| caller is a current member | `forbidden` |
| channel is encrypted | `bad_request` |
| `b` well formed (canonical, §13.1) | `bad_request` |
| ≤ 128 boxes | `too_large` |
| `server_id` is this server's; `chan` equals the arg | `bad_request` |
| `by` is a registered, non-revoked device **of the caller** | `forbidden` |
| `sig` verifies under `by`'s Ed25519 key | `bad_request` |
| CAS: `epoch == current + 1` and `prev == ChainHash(current b)` | `stale_epoch` |
| epoch > 1: the caller (`by`'s user) is in the current bundle's roster; epoch 1: the caller is `Channel.creator` (DMs: any member) | `forbidden` |
| `roster` == current members that aren't disabled | `stale_epoch` |
| every box goes to a registered device of a roster user, each at most once | `bad_request` |
| no box goes to a device revoked after the current epoch's `key.epoch` seq (any revoked device at epoch 0): the rotator couldn't have seen that revoke | `stale_epoch` |
| no box goes to any other revoked device (its revoke came before the head the rotator built on) | `bad_request` |
| no two boxes share an X25519 key | `bad_request` |
| at least one box for every roster user who has an active device ("every member with a device needs at least one box") | `bad_request` |
| rate cap, below | `rate_limited` |

Rate cap: with each epoch the server stores `state = H("tc-epoch-state-v1\x00"
|| u32 len(roster) || roster ids as u64 || sorted active device ids of the
roster users)` as of that rotation. When the state now equals the current
epoch's stored state (nothing changed), a rotation is refused if the
channel's last rotation was < 2 s ago (whoever made it) or the caller's last
rotation of this channel was < 10 s ago. When it differs (a member joined,
left or was disabled, or one of their devices was registered or revoked),
neither cap applies. The comparison is with the server's record, not with
what the current bundle boxed, so never-boxed devices don't exempt
anyone. Epoch 1, and epochs stored before `state` existed, are uncapped.

On success the server stores the bundle, sets `Channel.epoch`, appends a
`key.epoch` event (actor = caller) and answers `{seq}`. Writes are
serialized and each write's events are fanned out before its RES, so the
loser of a concurrent rotation always receives the winner's `key.epoch`
before its own `stale_epoch` RES. `stale_epoch` means "your view of the
channel is behind": apply pending events (or `key.fetch`), rebuild, retry.
It is returned only for races (the CAS, a roster change, a boxed device
revoked after the head); a bundle that is wrong for the state the client
must already have seen is `bad_request`, so retrying it can't loop.
Boxes may be missing for some devices: clients seal only to devices they
verified, and the rest get the key via `key.share` (§13.7). But every roster
user who has an active device needs at least one box, so someone of theirs
can answer `key.need` for their other devices; a user with no devices yet
needs none.

**key.fetch** `{chan, from: u32, to: u32}`: current members only (else
`forbidden`, also for unknown channels); `1 ≤ from ≤ to` and `to − from < 256` (else `bad_request`).
Returns `{bundles: [Signed], seqs: [u64]}`: the stored bundles in that range in epoch
order, and (same length) the seq of each bundle's `key.epoch` event,
possibly stopping early at ~768 KiB: ask again from the last epoch + 1.

**Revocation vs epochs.** A `device.revoke` at seq r invalidates only
bundles whose `key.epoch` seq is after r (they should not have boxed that
device); bundles at or before r stay valid, and the revoked device's
earlier keys are expected history. Clients order the two by these seqs
(`key.fetch` `seqs`, snapshot `keys[].seq`, event seqs).

A channel's current bundle is **stale** (encrypted sends will need a
rotation first, §13.4) when there is none yet, its roster isn't the current
non-disabled members, or it boxes a revoked device. A newly registered
device doesn't make it stale (it gets K via `key.share`, §13.7).

Creating encrypted channels: `chan.create {…, e2e: true}` requires `e2e1`
and `private: true` (else `bad_request`). `dm.open` creates a new DM with
`e2e = true` if and only if the caller's session has `e2e1`; an existing DM
is returned unchanged, whoever reopens it. So a legacy caller (no `e2e1`)
creates a plaintext DM even when every other member speaks `e2e1`; it stays
plaintext until converted with `chan.encrypt` (step 9). Neither op creates
an epoch. Once encrypted sends land (step 7), a plaintext `send` into an
e2e channel is `bad_request` "upgrade tc", so legacy clients can read
channel metadata but not post into encrypted DMs or channels.

Client validity (step 6, not yet implemented): `sig`; `by` is a verified
device of a previous-roster user (epoch 1: the creator / any DM member);
the chain extends the pinned head (lower or forked → alarm); K matches
`kc`; boxes only to verified devices.

The chain link is about the signed bundle, not its keys: a bundle that
verifies and whose `prev` is the pinned head is accepted and becomes the
new head even if its box to this device is missing, fails to open or
fails `kc`. The client then has no K for that epoch and recovers it via
`key.need` / `key.share` (§13.7); it MUST NOT treat this as a fork. Only a
second, different bundle for the same epoch (or one that doesn't chain to
the pinned head) is a fork.

Epoch 1 must be created by the channel's creator (any member of a DM).
Clients rotate epoch 1 right after `chan.create`; an e2e channel whose
creator leaves before that can't be keyed.

### 13.4 Encrypted payload *(not yet implemented)*

`enc` layout and rules: `docs/E2E.md` §13.4. Until it lands, sends into
e2e channels are plaintext as before; with it, a plaintext `send` into an
e2e channel is `bad_request` "upgrade tc" (§13.3).

### 13.5 Ops

Implemented: `device.register`, `device.revoke`, `devices.list` (§13.2),
`key.rotate`, `key.fetch` (§13.3), `chan.create + e2e?`, `dm.open` (e2e
DMs). Not yet: `key.share`, `send/edit/delete/react + enc`, `chan.add +
bundle`, `chan.remove`, `chan.encrypt` (`docs/E2E.md` §13.5).

| op | args | data |
|---|---|---|
| `key.rotate` | `{chan, bundle: Signed}` | `{seq}` |
| `key.fetch` | `{chan, from: u32, to: u32}` | `{bundles: [Signed], seqs: [u64]}` |

### 13.6 Events

Implemented: `device.new`, `device.revoke` (§13.2), and:

| event kind | chan | data | visible to |
|---|---|---|---|
| `key.epoch` | the channel | `{bundle: Signed}` | `e2e1` sessions of users who are members at its seq (§6): after `chan.join` the joiner sees it, after `chan.leave` the leaver doesn't — identically live and on replay |

Snapshot (§6) adds `keys: [{bundle: Signed, seq: u64}]` for `e2e1` sessions: the current
bundle of every encrypted channel the user is a member of, with the seq of its
`key.epoch` event (channels at epoch 0 have none), possibly over several parts. Not yet: `key.share`,
`enc` in `msg.*`/`react.*`.

### 13.7 Key requests *(not yet implemented)*

Ephemeral `key.need` and op/event `key.share`: `docs/E2E.md` §13.7.

### 13.8 Errors & limits

- `stale_epoch` (RES error code): the bundle doesn't build on the channel's
  current epoch, roster or devices. Retryable after catching up.
- Limits: EpochBundle `len(b) + len(sig)` ≤ 16 KiB and ≤ 128 boxes;
  `key.fetch` ≤ 256 epochs per call; ≤ 1 `key.rotate` per channel per user
  per 10 s and ≤ 1 per channel per 2 s, both waived when the roster or its
  active devices changed since the current epoch (§13.3; plus the §9 write bucket).
  Signed device objects ≤ 4 KiB (§13.2).
