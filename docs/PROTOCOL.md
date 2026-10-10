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
| `send` | `{chan, body: str, nonce: bin16, thread?: u64, enc?: bin}` | `{msg: u64}` (`enc`: e2e channels, §13.5) |
| `edit` | `{msg, body: str, enc?: bin}` | `{seq}` (`enc`: e2e channels, §13.5) |
| `delete` | `{msg, enc?: bin}` | `{seq}` (`enc`: e2e channels, §13.5) |
| `react` / `unreact` | `{msg, emoji: str, enc?: bin}` | `{seq}` (`enc`: e2e channels, §13.5) |
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
  `chan`/`body`/`enc`/`thread` → `conflict`. Clients retry from their outbox freely.
  This lookup comes before every other check except argument validation, so
  a repeat is found even if the channel's epoch moved on since (§13.5).
- **thread** must be a top-level message in the same `chan`; no nested threads.
- **edit**: author, while a member of the channel (else `not_found`). **delete**: the author (even after
  leaving) or any admin; deleted messages keep their
  id with `body = ""`, no `enc`/`edit_enc`, `deleted = true` (and `del_enc`, §13.5). Edit or react on a deleted message → `not_found`.
  A plaintext `edit` in an encrypted channel, or of a message with `enc`, is `bad_request`;
  encrypted channels use `edit`/`delete`/`react` with `enc` (§13.5).
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
| `msg.edit` | `{msg, body, edited: i64, enc?: bin}` |
| `msg.delete` | `{msg, enc?: bin}` |
| `react.add` / `react.remove` | `{msg, emoji, enc?: bin}` |
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
  state — latest `body` (or original `enc` and latest `edit_enc`) and
  `edited`, or `body = ""`, `deleted = true` (no `enc`/`edit_enc`; `del_enc`
  if the author signed the delete), without reactions;
- every superseded `msg.edit` of it (all of them once it is deleted) carries
  `body = ""` and no `enc`. A real edit never has an empty body without
  `enc`, so clients ignore such a `msg.edit`.

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
           edited: i64 (0 = never), deleted: bool, reactions: {emoji: [u64]}, enc?: bin,
           edit_enc?: bin, del_enc?: bin, renc?: [{user: u64, emoji: str, enc: bin}]}
```

`Message.enc` (omitted when absent) is the encrypted payload of a message in an
encrypted channel (§13.4); `body` is `""` then. Clients without `e2e1` see an
empty body. `edit_enc` (latest encrypted edit), `del_enc` (the author's
signed delete) and `renc` (one signed enc per encrypted reaction; `reactions`
then maps tags, not emoji) are §13.4's; all omitted when absent.

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

**Encrypted channels.** `hook.create` in an encrypted channel (§13) is `forbidden`; a live
hook whose channel is encrypted (e.g. converted later) gets 403 and posts nothing. The
server never encrypts on anyone's behalf.

**Responses** (JSON, `Cache-Control: no-store`):

| status | body | when |
|---|---|---|
| 200 | `{"msg": u64}` | posted (or idempotent repeat) |
| 400 | `{"error": "bad_request", "msg": str}` | bad JSON, invalid UTF-8, empty |
| 403 | `{"error": "forbidden", "msg": str}` | the hook's channel is end-to-end encrypted |
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

**Server identity and downgrade.** Clients key all trust state by
`server_id`, so they pin it: the first `server_id` seen after a successful
sync is recorded for the config's address and TLS pin, and a later WELCOME
with a different (or missing) `server_id` stops the client ("server
identity changed — log in again to trust it"); trust state is never reset
for it. `tc login` explicitly re-trusts (a new record for the invite's
address and pin). A client that ever saw a server with `e2e1` keeps that
fact; a later WELCOME without `e2e1` is a sticky alarm for the server and
every encrypted channel, and clients never send plaintext into
encrypted channels: queued messages for them fail instead (§13.4).

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
| `devices.list` | `{users?: [u64], after?: u64}` | `{certs: [Signed], endorse: [Signed], revoked: [Signed], next?: u64, revoked_seqs?: [u64]}` |

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
  revoke, ordered by user, and `revoked_seqs[i]`, the seq of `revoked[i]`'s
  `device.revoke` event (§13.3 "Revocation vs epochs"). Whole users only; when the next user would push
  the result past ~768 KiB the server stops and sets `next`: call again with
  the same `users` and `after = next`.
- **Snapshot** (§6) adds `devs: [Device]` (every device, revoked ones too),
  `revoked: [Signed]` (every DeviceRevoke) and `revoked_seqs: [u64]` (same
  length: each revoke's event seq), possibly over several parts.

Clients store what they receive and decide trust locally: a cert counts only
if it verifies under an SSH key pinned for its `user` (directly or via a
KeyEndorse chain from a pinned key), and a revoke only if it verifies under a
pinned key or a verified device of that user (not revoked before it), and
names a device the client knows to be that user's. Unverifiable objects are
kept but ignored. Pinning: a client pins its own login key; for anyone else
the key of the first valid (self-consistent) cert it sees for that user is
pinned on first use; a valid cert under any other key that no pinned key
endorses is a pending key change, not a verified device, until the user
acknowledges it. Users whose pins are only first-use (or endorsed by a
first-use key) are reported as unverified until confirmed out of band
(`docs/E2E.md` "Threat model").

Limits: `Signed` objects here ≤ 4 KiB; ≤ 8 non-revoked devices per user;
≤ 32 device registrations per user, ever.

### 13.3 Epoch bundles *(implemented: format, server rules, client checks)*

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
(`key.fetch` `seqs`, snapshot `keys[].seq`, event seqs). A seq is never 0:
the server backfills it from the `device.revoke` event and, if that is
impossible, leaves the revoke out of `devices.list` and the snapshot (and
logs it) rather than send 0. A client that has a revoke without a seq
(missing or 0 in `revoked_seqs`) treats the seq as **unknown**, not as
"before everything": it applies only the time rule below until a real seq
arrives, and an unknown seq never wins the "earliest" comparison.

A *client* additionally rejects a bundle (an alarm, §13.3 client validity)
whose own signed `time` is more than 5 minutes (allowed clock skew between
devices) after the signed `time` of a verified `DeviceRevoke` of its `by`
device, whatever the seqs say: a bundle is invalid if either the server's
seq order or the signed times put it after the revoke. This defends against
a server acting alone, which can misorder seqs but can't re-sign times; a
revoked device colluding with the server can still backdate its bundles.
With several valid revokes of one device the earliest known seq and the
earliest known time apply. (Boxes to revoked devices are still judged by seq alone.)

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
an epoch. A plaintext `send` (or `edit`) into an e2e channel is
`bad_request` "upgrade tc" (§13.5), so legacy clients can read channel
metadata but not post into encrypted DMs or channels.

Client validity: bundles are checked in epoch order, from epoch 1 (or the
epoch after the pinned head), fetching gaps with `key.fetch`. Each must:
decode (canonical, `server_id` and `chan` match, the epoch is the next one);
be signed by `by`, a device the client verified (§13.2) whose revoke, if
any, has a seq not before the bundle's seq; have `by`'s user in the previous
bundle's roster (epoch 1: `Channel.creator`, or for a DM any member, and in
the roster); have `prev` = ChainHash of the previous bundle; box no device
the client knows to belong to a non-roster user or revoked before the
bundle's seq, and no two boxes to one X25519 key among devices it knows.
Boxes to devices the client hasn't verified are allowed (the rotator may
have verified them). A failure, a second valid bundle for an epoch, or a
current epoch below the pinned head is an **alarm** for the channel: the
head doesn't move and the alarm is kept in the trust store. A bundle whose
rotator the client can't verify (yet) stops the walk without an alarm.

The chain link is about the signed bundle, not its keys: a bundle that
verifies and whose `prev` is the pinned head is accepted and becomes the
new head even if its box to this device is missing, fails to open or
fails `kc`. The client then has no K for that epoch and recovers it via
`key.need` / `key.share` (§13.7); it MUST NOT treat this as a fork. Until
it has K the client keeps the bundle (e.g. its device keys weren't loaded
yet) and retries its box on later passes. Only a
second, different bundle for the same epoch (or one that doesn't chain to
the pinned head) is a fork.

Epoch 1 must be created by the channel's creator (any member of a DM).
Clients rotate epoch 1 right after `chan.create`; an e2e channel whose
creator leaves before that can't be keyed.

### 13.4 Encrypted payload *(implemented)*

Messages in an encrypted channel carry `enc` instead of `body`:

```
enc   = 0x01 || epoch(u32) || eh(16) || dev(16) || nonce(24) || ct || sig(64)
mk    = HKDF-SHA256(K_epoch, info "tc-msg-v1" || server_id || chan || epoch)
ct    = XChaCha20-Poly1305(mk, nonce, pad256(msgpack Plain), AD = "tc-msg-v1" || server_id || chan || enc[0:61])
sig   = Ed25519(dev, "tc-sig-v1\x00" || server_id || chan || enc[0:61] || ct)
mid   = H(enc)[:16]
pad256(p) = p || 0x80 || 0x00* up to a multiple of 256
Plain = {t: "msg",   ts: i64, body: str, thread_ref?: bin16}
      | {t: "edit",  ts: i64, body: str, ref: bin16, v: u32}
      | {t: "del",   ts: i64, ref: bin16}
      | {t: "react", ts: i64, ref: bin16, emoji: str, add: bool}
tag   = "~" || base64url(HMAC-SHA256(mk, "tc-react" || mid || emoji)[:12])   // 17 chars, no padding
```

`eh` is the eh (§13.3) of the epoch's bundle, `dev` the sending device,
`nonce` 24 random bytes, `ts` the sender's clock (Unix ms). `thread_ref` is
present exactly when the message is a thread reply: the `mid` of the thread
root's `enc`, or 16 zero bytes if the root has no `enc` (a legacy plaintext
root, below). `ref` is the `mid` of the message an edit, delete or
reaction is about; `v` is an edit's version (the last applied version + 1);
`emoji` is a reaction's real emoji (§9 emoji rules) and `add` whether it adds
or removes it. An edit is sealed like a message, with the newest verified
epoch. A delete or reaction is sealed under the **message's own epoch**
(the `epoch` and `eh` of its `enc`), whose `mk` also makes the reaction
`tag` the server stores instead of the emoji: equal emoji on one message
give equal tags, so set semantics and the §5 caps keep working without the
server learning the emoji.

**Sending.** A client encrypts a message once, with the K of the newest
epoch it verified, and keeps the resulting `enc` (with the plaintext) in its
outbox; every retry sends the identical bytes, so `(author, nonce)`
idempotency (§5) holds across reconnects and rotations. Before encrypting it
catches up on the channel's chain, and rotates first (§13.3) when the
current bundle is stale as far as it can tell: there is none yet (epoch 1:
only the creator, or any DM member), or its roster isn't the current
non-disabled members. Only a `stale_epoch` RES (proof the message wasn't
stored) makes it encrypt again: it applies the events that came before the
RES, fetches missing bundles, rotates if it holds the current epoch yet the
server calls it stale (e.g. it boxes a revoked device), and re-encrypts the
same plaintext and `ts` with the **same send nonce and the same enc nonce**
(safe: the epoch key differs) — at most twice in a row, then it waits and
tries again later. If the channel still needs a rotation after its last
try, the message fails: it is never sealed to the old K. An entry encrypted
while queued and not yet handed to the server is re-encrypted if the
verified head or the members changed meanwhile. A client that can't get K for the epoch (no box, no
share yet, can't rotate) marks the message failed ("no key yet — waiting for
a member"), sends `key.need`, and requeues it when the key arrives; a failed
message the user retries is encrypted afresh. It never
sends plaintext into a channel that is, or that it once saw as, encrypted
(also not after a downgrade alarm, §13), refuses to send into (or flush
queued messages into) a channel with an alarm, never sends a plaintext
`edit` there (nor any edit of a message it hasn't cached), and refuses reminders
(`remind.create` `chan`) that point into one.

**Receiving.** A client accepts a message with `enc` only if: the enc
parses; it holds the verified K of `enc.epoch` and `enc.eh` is that epoch's
eh; `enc.dev` is a device it verified that belongs to `Message.author` (and
wasn't revoked before the message's id, nor — for a revoke whose seq it
doesn't know — signed with a Plain `ts` more than 5 minutes after the
revoke's signed time); the signature and AEAD check out;
Plain decodes with `t = "msg"`; `body` passes §10 (valid UTF-8, no control
characters but `\n`/`\t`, not blank, ≤ 64 KiB); `thread_ref` matches
`Message.thread` as above; and no message with a lower id carries the same
`mid` (the lowest id wins whatever arrives first: a later-id copy is a
replay and becomes invalid when the lower one shows up). Anything that fails is
kept and shown as invalid, never as text. A message it can't judge yet (no
K for the epoch, the sending device or the thread root not known yet) is
stored as ciphertext, still advances `last_seq`, shows as "waiting for
key", and is decrypted when what it lacked arrives. Once verified, a
message's channel, author, thread and bytes are fixed: a refetched copy
(history, snapshot) that differs is invalid and the stored one stays. Only
a verified edit (below) changes its shown text and edited time; the
server's `edited` and `reactions` are not shown for encrypted messages.

**Edits, deletes, reactions.** Each `enc` below is checked like a message's
(parses, K of its epoch and its `eh`, signature by a device the client
verified that wasn't revoked before the event's seq or — seq unknown, as in
a refetched copy — by the Plain `ts` rule, AEAD) and then:

- **edit** (`msg.edit.enc`, `Message.edit_enc`): signed by a device of the
  **original** message's author; `t = "edit"`; `ref` = the original's `mid`;
  `body` passes §10; `v ≥ 1`; and the edit is **newer** than the applied
  one: the higher `(v, ts, mid of the edit's enc)` wins, compared in that
  order (mid as bytes), and an equal tuple is the same edit. Every client
  thus ends on the same edit whatever order concurrent edits (e.g. two
  devices both sending `v = 2`) arrive in. Then the
  text and edited time change; otherwise nothing does. The original must be
  verified; an edit of a message still waiting is kept and judged once it
  opens (same for an edit whose own key or device is missing).
- **delete** (`msg.delete.enc`, `Message.del_enc`): the text goes in any
  case (the server can always delete). Signed by a device of the author,
  `t = "del"` and `ref` = the message's `mid` (when the client knows it; a
  message first seen deleted has no `mid` to compare) → "message deleted";
  no `enc`, or one that fails → **"removed (not by author)"** (an admin or
  the server deleted it). Plaintext at or below the legacy cutoff stays
  "message deleted". A deleted message keeps its `mid` locally (replies name it).
- **react** (`react.add.enc`, `Message.renc`): signed by a device of the
  **reacting** user (the event's actor / `renc.user`); sealed under the
  message's epoch and `eh`; `t = "react"`, `add`; `ref` = the message's
  `mid`; `emoji` passes §9; and `tag(emoji)` equals the tag the server
  stored. Only such reactions show, by their emoji; tags without a verified
  `enc` are ignored. A `react.remove` (or a refetch without the reaction)
  removes it without proof: the server can always withhold a reaction.

Anything rejected changes nothing and is logged locally (`E2ERejects`).
A client sends an encrypted edit only of its own verified message, signs
its own deletes (an admin's delete of someone else's message has no `enc`),
and reacts only to verified messages. Like sends and edits, it refuses
deletes and reactions in a channel with an alarm. Local search follows edits and
deletes (it searches the current decrypted text of undeleted messages).

**Plaintext in encrypted channels.** DMs created as encrypted before
encrypted sends existed may hold plaintext messages. When a client first
sees a channel as encrypted it records, once, a **legacy cutoff**: the
newest message id it had cached in it. A message without `enc` at or below
the cutoff is shown as "⚠ unauthenticated (legacy)"; any other can only have
been inserted by the server (honest servers refuse plaintext sends there)
and is shown as "⚠ inserted by server". Neither is authenticated: neither
notifies nor counts as a mention. The client's cache keeps decrypted
bodies (and the outbox plaintext): it is as sensitive as the conversation.
Search in encrypted channels is local, over the decrypted cache: a search
scoped to one never reaches the server, and a global search drops any
server result from an encrypted channel (the server never returns them).

### 13.5 Ops

Implemented: `device.register`, `device.revoke`, `devices.list` (§13.2),
`key.rotate`, `key.fetch` (§13.3), `key.share` (§13.7), `chan.create + e2e?`,
`dm.open` (e2e DMs; a DM with no other members — private notes — is never e2e, because the server
posts reminders into it), `send + enc`, `edit/delete/react/unreact + enc`. Not yet: `chan.add +
bundle`, `chan.remove`, `chan.encrypt` (`docs/E2E.md` §13.5).

The `enc` fields of `send`, `edit`, `delete` and `react`/`unreact` are
**not** gated on `e2e1` (the ops exist for every session): a session
without the cap that sends one gets the same checks as below. Only the
e2e-specific ops and events of this section are cap-gated.

| op | args | data |
|---|---|---|
| `send` | `{chan, nonce, thread?, enc: bin, body: ""}` (exactly one of `body`/`enc`) | `{msg}` |
| `edit` | `{msg, enc: bin, body: ""}` (exactly one of `body`/`enc`) | `{seq}` |
| `delete` | `{msg, enc?: bin}` (`enc`: the author's signed delete) | `{seq}` |
| `react` / `unreact` | `{msg, emoji: tag, enc: bin}` | `{seq}` |
| `key.rotate` | `{chan, bundle: Signed}` | `{seq}` |
| `key.fetch` | `{chan, from: u32, to: u32}` | `{bundles: [Signed], seqs: [u64]}` |
| `key.share` | `{chan, epoch: u32, dev: bin16, eph: bin32, ct: bin48}` | `{}` |

Server rules (honest case) for **send**, in this order:

| check | error |
|---|---|
| `nonce` is 16 bytes | `bad_request` |
| with `enc`: `body` is empty | `bad_request` |
| `len(enc)` ≤ 64 KiB + 1 KiB | `too_large` |
| `enc` framing parses (version, sizes, whole pad blocks) | `bad_request` |
| without `enc`: the §9/§10 body rules | `bad_request` / `too_large` |
| `(author, nonce)` already stored: same `chan`, `thread` and `body`/`enc` bytes (unless since edited or deleted) → the original `{msg}`, else | `conflict` |
| caller is a member | `forbidden` |
| a webhook into an encrypted channel (§12) | `forbidden` |
| encrypted channel without `enc` ("this channel is end-to-end encrypted — upgrade tc") | `bad_request` |
| `enc` into a plaintext channel | `bad_request` |
| `enc.epoch` is the channel's current epoch (epoch 0: never) | `stale_epoch` |
| the current bundle isn't stale (§13.3) | `stale_epoch` |
| `enc.eh` is the eh of the current bundle | `bad_request` |
| `enc.dev` is a registered, non-revoked device of the caller | `bad_request` |
| `enc`'s signature verifies under that device's Ed25519 key | `bad_request` |
| no message in the channel has the same `mid` (a replay under a new nonce) | `conflict` |
| then the usual thread and rate-limit checks (§5, §9) | |

The stored message and its `msg.new` carry `enc` byte for byte with
`body = ""`; `history`, thread history, `pins.list` and snapshots return it
as is. `enc` is never indexed for search, and `search` skips encrypted
channels altogether. `delete` clears `enc` with the body (and in the
`msg.new` event, §6).

Server rules (honest case) for **edit** with `enc`, in this order: `body`
empty (`bad_request`), `len(enc)` ≤ 64 KiB + 1 KiB (`too_large`), framing
parses (`bad_request`); the message exists, isn't deleted and the caller is
a member (`not_found`); the caller is its author (`forbidden`); the channel
is encrypted and the message has `enc` (`bad_request`; a plaintext edit
there is `bad_request` "upgrade tc"); `enc` equal to the stored `edit_enc`
→ ok `{seq: 0}` (a retry, found before any epoch check); then the send
rules from `enc.epoch` on (`stale_epoch` for another epoch or a stale
bundle, `eh`, signer = an active device of the caller, signature). The
server stores it as `Message.edit_enc` next to the original `enc`, sets
`edited`, emits `msg.edit {msg, body: "", edited, enc}` and scrubs the
previous edit's `enc` from the log (§6). It stores the latest edit it
received; it can't read `v`, so clients order
edits.

**delete** with `enc`: only the author may sign (`forbidden` for anyone
else, e.g. an admin, who deletes without `enc`); then `len(enc)` ≤ 397 bytes, one padding block (`too_large`),
framing (`bad_request`), the message has `enc` and `enc` names its `epoch`
and `eh` (`bad_request`), signer = an active device of the caller with a
valid signature (`bad_request`). Stored as `Message.del_enc`; `msg.delete`
carries it; `enc` and `edit_enc` are cleared.

**react/unreact** in an encrypted channel: `emoji` must be a tag (`~` and 16
base64url characters) and `enc` present (else `bad_request` "upgrade tc");
`enc` in a plaintext channel is `bad_request`; then the delete's `enc` checks
(message's epoch and `eh`, caller's active device, signature). Set semantics
and the caps (32 distinct tags per message, 10 per user) apply to tags. A
reaction's `enc` is stored with it (`Message.renc`) and carried by
`react.add`/`react.remove`. The server can check none of the contents.

### 13.6 Events

Implemented: `device.new`, `device.revoke` (§13.2), and:

| event kind | chan | data | visible to |
|---|---|---|---|
| `key.epoch` | the channel | `{bundle: Signed}` | `e2e1` sessions of users who are members at its seq (§6): after `chan.join` the joiner sees it, after `chan.leave` the leaver doesn't — identically live and on replay |
| `key.share` | the channel | the `key.share` args (actor = the sharer) | same as `key.epoch` |

Snapshot (§6) adds `keys: [{bundle: Signed, seq: u64}]` for `e2e1` sessions: the current
bundle of every encrypted channel the user is a member of, with the seq of its
`key.epoch` event (channels at epoch 0 have none), possibly over several parts.
`msg.new` (and snapshot messages) carry `Message.enc` (and `edit_enc`,
`del_enc`, `renc`) in encrypted channels (§13.4); `msg.edit`, `msg.delete`
and `react.*` carry the op's `enc` (§13.5).

### 13.7 Key requests

A device that has no K for an epoch it verified (no box for it, a box that
doesn't open, or `kc` doesn't match) asks the channel's members:

```
Ephemeral key.need = {kind: "key.need", chan, epoch: u32, dev: bin16}   // + user, set by the server
```

Clients send at most one per channel per 30 s. The server (honest case)
relays it to the `e2e1` sessions of the channel's current members, the
sender's other sessions included but not the sending one, and silently
drops it unless the session sent no `key.need` for the channel in the last
30 s (checked first, before any database lookup: a dropped `key.need` still
counts), the sender has `e2e1`, `dev` is an active device of the sender,
the channel is encrypted, `1 ≤ epoch ≤` its current epoch and the sender is
a member.

A member that holds K for `epoch` answers only if `dev` is a device it
verified (§13.2), not revoked, of the `key.need`'s `user`, and that user is
both a current member of the channel (as the client knows it) and in the
roster of that epoch's bundle (never seal K to a non-member, nor to a former
member asking for an epoch they once belonged to). It
answers at most once per (chan, epoch, dev) per 30 s, with
**key.share** `{chan, epoch, dev, eph, ct}`:

```
eph = fresh X25519 key per share
ss  = X25519(eph_priv, d.x25519);  bk = HKDF-SHA256(ss, salt = eph || d.x25519, info "tc-share-v1")
ct  = XChaCha20-Poly1305(bk, nonce 0^24, K, AD = "tc-share-v1" || server_id || chan || epoch || d.dev)
```

Anyone can build a share, so the recipient accepts K only if it matches
the `kc` of the epoch's verified bundle; a share for an epoch it hasn't
verified yet is kept until it has. Clients bound what they keep: only
shares to their own device from a current member, at most one per
(chan, epoch, device, sharer) (the newest event wins), and a global cap that
evicts the oldest share of whichever sharer holds the most, so junk from
one sharer never displaces another's legitimate share. Server rules (write op, rate limited):
`dev`, `eph`, `ct` of the right sizes and `epoch ≥ 1` (else `bad_request`);
channel exists (`not_found`); caller is a member (`forbidden`); channel is
encrypted and `epoch ≤` its current epoch (`bad_request`); `dev` is a
registered device (`not_found`), not revoked, of a current member
(`bad_request`). It appends a `key.share` event (§13.6).

### 13.8 Errors & limits

- `stale_epoch` (RES error code): the bundle doesn't build on the channel's
  current epoch, roster or devices; for `send`, the `enc` isn't for the
  channel's current epoch or that epoch's bundle is stale (§13.5). Nothing
  was stored. Retryable after catching up: a sender re-encrypts with the
  same nonces (§13.4).
- Limits: `enc` ≤ 64 KiB + 1 KiB (Plain ≤ the padded capacity), a delete's or reaction's `enc` ≤ 397 bytes (one padding block); EpochBundle `len(b) + len(sig)` ≤ 16 KiB and ≤ 128 boxes;
  `key.fetch` ≤ 256 epochs per call; ≤ 1 `key.rotate` per channel per user
  per 10 s and ≤ 1 per channel per 2 s, both waived when the roster or its
  active devices changed since the current epoch (§13.3; plus the §9 write bucket).
  Signed device objects ≤ 4 KiB (§13.2).
- Client-side bounds (what a server can make a client store or compute):
  ≤ 16 pending key changes per user (least recently seen dropped); a cert
  that fails verification is remembered by hash and not checked again;
  device verification triggered by `key.need` for an unknown device runs at
  most once per 10 s; unverified bundles and shares are kept only for
  channels cached as encrypted with the client as a member, only for epochs
  ≤ verified head + 64, and under per-channel and global caps. An unverified
  bundle never changes the cached `Channel.epoch` (only the server's
  Channel object or a verified bundle does).
