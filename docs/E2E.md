# End-to-end encryption design (M3)

Status: **implemented (steps 1–12)**. Draft r1 below was **rejected** by review (a malicious server
could read DMs: device certs, membership and key boxes were all server-controlled). **r2 = the
revisions at the end of this file ("Review r1 → r2") supersede anything above that conflicts.** The
implementation followed the PR plan there; step 12's security pass is at the end of this file.

## Goals / non-goals

- The server (and anyone who steals its database) cannot read DMs or private-channel messages.
- Members can't forge each other's messages.
- New members of a private channel can read its history (Slack expectation), by default.
- Non-goals for v1: hiding metadata (who talks to whom, when, sizes), encrypting public channels,
  per-message forward secrecy.

## Keys

Per **device** (stored in `TC_HOME`, 0600):

- X25519 encryption key (`crypto/ecdh`), Ed25519 signing key.
- `DeviceCert = {user, device_id, x25519_pub, ed25519_pub, created}` signed with the user's
  **SSH key** (SSHSIG, namespace `tc-device-v1`, works through ssh-agent and hardware keys — the
  agent can sign but can't do ECDH, hence separate device keys).
- The server publishes device certs; **clients verify every cert against the user's SSH key** and
  ignore anything that doesn't verify, so a malicious server can't inject devices to harvest keys.

## Group keys: per-channel epochs

Chosen over Signal sender keys and MLS: at < 15 users / ~30 devices, re-sealing a 32-byte key to
every device costs ~2.4 KB per rotation, and epochs make history-on-join trivial. No production-ready
Go MLS library exists.

- Encrypted channel state: `epoch` (u32), random 32-byte `K_epoch`.
- `key.rotate {chan, epoch: n+1, boxes: {device_id: seal(K)}}` — server accepts only if `n` is
  current (compare-and-set) and boxes cover exactly the current members' devices.
- Rotate on member removal, device removal, user disable. Lazily: the next sender rotates.
- Joins don't rotate: an existing member seals the current (and, if history is shared, past) epoch
  keys to the new member's devices.
- Sealing: `nacl/box` anonymous sealed boxes to each device's X25519 key.

## Messages

```
enc = {v: 1, epoch, dev, nonce: 24B, ct, sig}
ct  = XChaCha20-Poly1305(HKDF(K_epoch, "tc-msg-v1"), nonce,
        msgpack{body, ...}, AD = chan || epoch || dev || client_nonce)
sig = Ed25519(device signing key, header || ct)
```

- `sig` stops members forging as each other (the symmetric key alone would allow it).
- Edits/deletes must be signed by a device of the original author.
- Ciphertext padded to 256-byte buckets.

## What the server still sees

Membership, timestamps, sizes (bucketed), thread links, reactions, read markers, channel names and
topics. Search for encrypted channels is client-side over the local decrypted cache (SQLite FTS5);
the local cache is therefore sensitive — rely on disk encryption, documented.

## Multi-device & recovery

- `tc device add`: existing device confirms the new device's fingerprint and seals all epoch keys.
- Optional server-stored backup encrypted with an age passphrase recipient.
- Losing every device without a backup loses encrypted history — said plainly in the docs.

## Verification (implemented in step 10)

- **Safety number** (Signal-compatible-ish): 60 digits shown as 12 groups of 5. Input =
  `"tc-safety-v1" || server_id || (u64 user, u16 n, n × (u16 len, fingerprint))` for both users ordered
  by user id, fingerprints = `ssh.FingerprintSHA256` of every SSH key this device pinned for that user,
  sorted. `h = SHA-512(input)`, then 5200 rounds of `h = SHA-512(h || input)`; group i is bytes
  `h[5i:5i+5]` big-endian mod 100000. Like Signal it is iterated SHA-512 with 5-byte chunks mod 100000;
  unlike Signal it is one number over both parties (not two 30-digit halves). Both sides compute it from
  their own trust store, so it matches only if each pinned the same keys for both users. `/verify @bob`
  (TUI) and `tc verify @bob` (CLI) show it with both users' fingerprints; confirming re-computes it and
  marks exactly those pinned keys `Verified` (`trust.VerifyKeys`, refused if the pinned set changed since
  — `ErrPinsChanged`), audited in `trust_audit`. Devices whose certs those keys signed are thereby
  verified out of band.
- `/verify @bob --github bob` (and `tc verify @bob --github bob`) fetches `https://github.com/bob.keys`
  (net/http, 15 s timeout, 256 KiB cap, redirects only within the same origin — no http downgrade, no
  other host — only on that explicit command) and compares. A match is only shown: the user must still
  confirm (`v` / `y`: "verify @bob because you confirmed github.com/bob is theirs") before
  `ConfirmGitHub` marks the keys Verified ("github:bob" in the audit log). A mismatch changes nothing.
- A user counts as verified when some pinned key is `Verified` and none is only `TOFU`: accepting a key
  change pins the new key as TOFU, so the user is unverified again until re-verified.
- Invite carries the inviter's SSH fingerprint (TOFU anchored in the invite, like the TLS pin) — M4.
- **Key change** → one sticky red banner line in the encrypted channels shared with that user; sending
  there (send/edit/react, TUI and `tc send`) fails with `ErrKeyChange` until `/accept-key @bob`;
  receiving is unaffected. Queued outbox entries are re-checked when sent and held (failed with the
  key-change reason) until the change is accepted, which requeues them. **Deletes stay allowed**: they
  only remove, sealed under the message's own epoch, and never seal to the new key. The gate fails
  closed: if it can't look up an encrypted channel (no server id, channel not cached, trust store
  error) the send is refused. `/accept-key @bob [fingerprint]` shows the pending fingerprints and needs
  `y`; `AcceptKeyChanges` acknowledges exactly the shown ones (`trust.AcknowledgeKeyChange` each) and
  refuses with `ErrPendingChanged` (the TUI re-shows) if what's pending changed meanwhile. Trust
  overlays ignore keys other than Esc for 500 ms after opening (type-ahead). Plaintext channels aren't
  gated (nothing is sealed there).
- **Alarms** stay sticky; `/alarm` explains one and `/alarm-clear #name` (the channel's name typed to
  confirm) clears it through `trust.ClearChannelAlarm` / `ClearTrustAlarm`, which move the alarm (detail,
  raise time) to the `trust_audit` table with who cleared it and when — never a silent delete. Nothing
  in the sync code calls them (a test checks). Computed alarms (unconfirmed member) can't be cleared.
  Clearing the server-wide alarm says so ("server-wide — clears the alarm for all encrypted channels").

## Rollout

Cap `e2e1`. DMs and new private channels encrypted by default; `/encrypt` converts an existing
private channel. Public channels unchanged. Bots in encrypted channels get a bot device.

## Threat model (as of step 12)

What a malicious server can and cannot do today, and what closes each gap:

- **Trust on first use (TOFU), Signal-like.** A user's first SSH key the client sees in a valid
  device cert is pinned without confirmation. A server present at that first contact could
  substitute its own key and read what is sealed to it. Later key changes are caught (pending key
  change, nothing sealed until acknowledged). Clients therefore report every user whose pins are
  only TOFU (or endorsed by a TOFU key) as **unverified** (`Client.UnverifiedUsers`; the TUI marks
  them with `?`). Invite-anchored pins (the invite carries the inviter's SSH fingerprint) arrive in M4;
  `/verify` (safety numbers or GitHub keys, step 10) upgrades the pins to verified.
- **Server-asserted membership, creator and roster are trusted** until signed membership exists:
  the server says who is in a channel, who created it (who may sign epoch 1) and so who belongs in
  an epoch's roster. A server can add a member it controls (who then gets keys sealed to their
  verified devices) or hide a leave. Clients check that each rotator was in the previous roster
  and seal only to verified devices, but the roster itself comes from the server.
- **Whether a channel is encrypted** comes from the server for a channel this device has never
  seen: a server could present a new channel (one created by someone else) as plaintext, and a
  client would then send plaintext into it. Once the trust store has seen a channel encrypted, the
  flag is sticky (a later "plaintext" is an alarm and nothing is sent in the clear). DMs and
  channels we create are encrypted by our own choice (`dm.open` from an `e2e1` client, `chan.create
  e2e`; `CreateEncryptedChannel` refuses a plaintext answer). An existing DM returned by `dm.open`
  may legitimately be a legacy plaintext DM, so its flag is the server's word until step 9's
  `chan.encrypt`. Signed channel metadata would close the gap.
- **Plaintext in encrypted channels** is never authenticated: what was cached before the client
  first saw the channel encrypted is "legacy" (sticky cutoff), anything else "inserted by server";
  neither notifies or counts as a mention (step 7).
- **Server identity is pinned** per address + TLS pin; a changed `server_id` stops the client
  instead of starting trust over, and dropping `e2e1` after speaking it is a sticky alarm.
- **Revocation timing**: a bundle is rejected if the server's seq order or its signed time (with
  5 min clock skew allowed) put it after a revoke of its signer. This defends against a server
  acting alone; a revoked device colluding with the server can backdate its bundles.
- **Membership changes rotate atomically** (step 9): a removed member's devices hold no key for
  anything sent after the removal, even given the server's raw rows; a leaver's last epoch goes stale
  and the next sender rotates first. A malicious server can still add a member it controls (the roster
  is its word) — clients don't seal to anyone no verified epoch includes, so it needs an epoch-0
  channel or a colluding member — or withhold a removal; a converted channel's legacy cutoff comes
  from the server only for a device that cached none of the old plaintext, and only up to epoch 1.
  A removed member still sees deletes/reactions on pre-removal messages (sealed under their epochs).
- **Edits don't hide the original** from members holding that epoch's key: the original `enc` stays
  (the edit refers to its mid). Delete removes the ciphertext. (Step 8.)
- **Edits and reactions over time**: the server can withhold the newest edit from a device that never
  saw it (it shows an older verified one), and can replay a reaction its author since removed (adds
  carry no version). It can't forge, move or reorder edits a client has seen. (Step 8.)
- **Message times**: `Message.time` is the server's, Plain `ts` the author's (signed). When they differ by
  more than 5 min the client shows the signed time with a dim "⚠ server time differs" (step 12), so a
  server can shift a message's time only within the clock skew allowed. Plaintext messages and
  metadata (who talks to whom, when, sizes) stay the server's word.
- **A member can poison a rotation** (boxes that don't yield K, then never answer key.need). Since
  step 12 the other members wait 2 min for a key.share and then rotate past it, linking back to a
  random key: that epoch's messages are lost to them (a history gap), nothing else.
- **Stolen device**: once revoked (`device.revoke`, signed by the user's SSH key or another of their
  devices: `tc devices revoke <id>` or `/devices`; the revoking client owes every channel it is in a rotation, recorded
  before the revoke and retried until done, so it never depends on the server's seqs), every client
  rotates before sealing anything new in a channel whose current key it may hold (boxed, or by key.share; B5) (step 12, not relying on the
  server's `stale_epoch`) and ignores revokes the stolen device signs afterwards. Disabling the user
  (admin) also rotates them out. It keeps what it could already read. A device revoke does not remove
  an SSH login key: a stolen device that holds the user's SSH private key signs in again and registers
  as a new device, which every client trusts (its cert verifies under the pinned key). Clients announce
  a new device of the user to the user's other devices ("new device of yours"), including in the pass
  where a device that was revoked while offline re-registers. The only remedy today is a whole account:
  an admin disables it (`tc users disable @name`, op `user.disable`: its sessions close, none of its
  keys can sign in, its invites go, encrypted channels rotate it out) and invites the owner again as a
  new account (new name, new SSH key); the old account's encrypted history stays with the old devices.
  **Known gap:** there is no per-key removal, so the owner can't drop just the stolen key and keep the
  account. Members can't mint add-key invites (admins only, §4.1 of PROTOCOL.md), so a stolen key alone
  can't add a second key of the thief's that would survive this.
- **Lost devices**: K lives only on devices. Losing every device of a user loses their encrypted
  history (no backup in v1); a new device gets new messages, and older history only from a member's
  backlinks (the history of every epoch whose K a remaining member holds).
- **The local cache is sensitive**: `TC_HOME/cache.db` holds decrypted messages and the outbox, and
  `e2e.db` the device and epoch keys (both 0600, SQLite `secure_delete`). Rely on disk encryption; a
  thief with the disk reads what that device could.
- **Availability** is not defended: a server can drop, delay or withhold anything. Clients recover
  what they can fetch again (bundles by `key.fetch`, keys by `key.share`) and never fail open.

## Pitfalls checklist

Nonce reuse (random 24-byte XChaCha nonces) · cross-channel replay (chan/epoch in AD) · trusting the
server's device list (verify certs) · plaintext leaking into webhooks, logs, outbox, crash output ·
rotation races (CAS + retry) · retention must purge old epoch keys · no unaudited MLS.

Libraries (all pure Go, keeps `CGO_ENABLED=0`): `crypto/ecdh`, `crypto/ed25519`,
`x/crypto/{chacha20poly1305,hkdf,nacl/box,ssh}`.


---

# Review r1 → r2 (authoritative)

## Key decisions
- **Trust store** `TC_HOME/e2e.db` (0600), separate from the cache, never touched by `resetCache`:
  pinned SSH keys per user (first = TOFU, anchored by invite / `/verify`), endorsements, verified
  devices, per-channel chain head `(epoch, eh)`, sticky "server uses e2e" and per-channel e2e flags.
- A user's additional SSH keys need a **KeyEndorse** signed by an already-pinned key; otherwise a
  key-change banner, nothing sealed to it, sending blocked in shared e2e channels until acknowledged.
- **Epoch = roster version.** The roster lives inside a **signed, hash-chained EpochBundle**; every
  membership change (including joins) rotates. Clients seal only to `roster ∩ verified devices`.
- Bundles are Ed25519-signed by a rotator device that belongs to the previous roster; boxes are
  authenticated by AD; key commitment `kc` lets every recipient check it got the same K.
- History via **backlinks** (`back = AEAD(K_n, K_{n-1})`); all e2e channels share history in v1.
- Every signed object is `{b, sig}` over exact bytes (no re-encoding). `enc` is a fixed binary layout.
- Signed plaintext carries type, thread_ref, edit target, version, sender timestamp → the server
  can't move, replay, re-attribute or forge edits. Reactions become HMAC tags + signed enc.
- Device certs use the existing tc signature format with context `"tc-device-v1\x00"` (no SSHSIG).
  sk-* signatures must have the user-presence flag.
- Device private keys are never deleted from the trust store: keys replaced because TC_HOME is re-used
  for another user, or retired after a revoke, move to an archive table; `TC_HOME/device.key`
  (pre-trust-store) is removed only once the trust store holds exactly its keys.
- Cut from v1: age passphrase backup, emoji SAS, per-epoch history boxes, SSHSIG. (`--github` verify
  was cut here but landed in step 10.)
- Webhooks into e2e channels → `forbidden`. Server search excludes e2e; client FTS instead.
- **M1 bug found:** delete/edit never scrub `events.data`, so old bodies replay on sync (PR 0).

## Protocol §13 (to land in PROTOCOL.md with the protocol PRs)

### 13.1 Signed objects
`Signed = {b: bin, sig: bin}`; sig over `context || b`; verify before decoding b; H = SHA-256; big-endian.
b is canonical msgpack: unknown fields are rejected (not ignored), so any format extension needs `v: 2`.

### 13.2 Devices
- `DeviceCert.b = {v:1, server_id: bin16, user: u64, dev: bin16, x25519: bin32, ed25519: bin32, ssh: bin, created: i64}`;
  sig = SSH signature by `ssh` over `"tc-device-v1\x00" || b` (§4 algorithm rules; sk-* needs UP flag).
- `KeyEndorse.b = {v:1, server_id, user, new_ssh: bin, time}`; sig by an endorsed SSH key, context `"tc-endorse-v1\x00"`.
- `DeviceRevoke.b = {v:1, server_id, user, dev, time}`; sig by an endorsed SSH key (`"tc-revoke-v1\x00"`) or
  Ed25519 of another non-revoked device of the user (`"tc-revoke-dev-v1\x00"`).

### 13.3 Epoch bundles
```
EpochBundle.b = {v:1, server_id, chan: u64, epoch: u32 (first = 1), prev: bin32 (H(prev b), 0 for 1),
  roster: [u64] sorted distinct, by: bin16 (rotator dev), time: i64, kc: bin32, eph: bin32,
  boxes: [{dev: bin16, ct: bin48}], back?: bin72}   // back required for epoch > 1, absent for epoch 1
sig = Ed25519(by, "tc-epoch-v1\x00" || b);  eh = H(b)[:16]
K random 32B; kc = HMAC-SHA256(K, "tc-kc-v1" || server_id || chan || epoch)
box(d): ss = X25519(eph_priv, d.x25519); bk = HKDF(ss, salt = eph || d.x25519, info "tc-box-v1");
        ct = XChaCha20-Poly1305(bk, nonce 0^24, K, AD = server_id || chan || epoch || d.dev || by)
back = nonce24 || XChaCha20-Poly1305(HKDF(K, "tc-back-v1"), nonce, K_prev, AD = server_id || chan || epoch)
```
Client validity: sig; `by` is a verified device of a previous-roster user (epoch 1: creator / any DM
member); chain extends pinned head (lower/forked → alarm); K matches kc; boxes only to verified devices.
The server enforces the same `by` rule (`forbidden`), using additive `Channel.creator` (0 for #general;
backfilled from the `chan.new` actor). A bundle that chains correctly but whose box is unusable to a
recipient is still accepted as the new head; that recipient recovers K via `key.share`, not a fork alarm.

### 13.4 Encrypted payload
```
enc = 0x01 || epoch(u32) || eh(16) || dev(16) || nonce(24) || ct || sig(64)
mk  = HKDF(K_epoch, info "tc-msg-v1" || server_id || chan || epoch)
ct  = XChaCha20-Poly1305(mk, nonce, pad256(msgpack Plain), AD = "tc-msg-v1" || server_id || chan || enc[0:61])
sig = Ed25519(dev, "tc-sig-v1\x00" || server_id || chan || enc[0:61] || ct);  mid = H(enc)[:16]
Plain = {t: msg|edit|del|react, ts, body?, thread_ref?: bin16, ref?: bin16, v?: u32, emoji?, add?}
pad256 = Plain || 0x80 || 0x00* to a multiple of 256
```
Accept: sig by a verified device of the author; t matches op; thread_ref matches; edits same author
and higher v; first mid wins. Decrypted text must pass §10 checks. Undecryptable events are applied
(stored as ciphertext, last_seq advances) and decrypted when keys arrive.

### 13.5 Ops (cap `e2e1`)
| op | args | data |
|---|---|---|
| `device.register` | `{cert: Signed, endorse?: Signed}` | `{seq}` |
| `device.revoke` | `{revoke: Signed}` | `{seq}` |
| `devices.list` | `{users?: [u64]}` | `{certs, endorse, revoked}` |
| `key.rotate` | `{chan, bundle: Signed}` | `{seq}` |
| `key.fetch` | `{chan, from: u32, to: u32}` (≤ 256) | `{bundles, seqs}` (seqs[i] = key.epoch seq of bundles[i]) |
| `key.share` | `{chan, epoch, dev, eph, ct}` | `{}` |
| `send` | `+ enc?: bin` (exactly one of body/enc) | `{msg}` |
| `edit` / `delete` / `react` | `+ enc?` (e2e react emoji = "~"+b64url(HMAC(HKDF(mk,"tc-react-v1"),"tc-react"‖mid‖emoji)[:12])) | |
| `chan.create` | `+ e2e?: bool` | |
| `chan.add` | `+ bundle?` (required when e2e) | |
| `chan.remove` | `{chan, user, bundle}` (new; private chans) | |
| `chan.encrypt` | `{chan, bundle}` | `{seq}` |

Server (honest case): dedupe (author, nonce) first (conflict compares enc bytes), then `stale_epoch`
if enc.epoch ≠ current or bundle stale, then verify sig vs a registered device of the caller.
Plaintext into e2e → `bad_request` "upgrade tc" (step 7). DMs: `dm.open` makes a new DM e2e iff the caller
advertises `e2e1`; a legacy caller creates a plaintext DM, later convertible by `chan.encrypt` (step 9). `key.rotate` (channel/membership/e2e checked before decoding the bundle) CAS `epoch == cur+1 && prev == H(cur.b)`, roster ==
non-disabled members, boxes only for registered non-revoked roster devices (a subset is fine — clients seal
only to devices they verified, the remaining devices get K via `key.share`; but ≥ 1 box per roster user
who has an active device, else `bad_request`; decided in step 5 review; a box to an unknown, non-roster or
already-revoked device is `bad_request`, and `stale_epoch` is only for races: CAS, roster change, a
boxed device revoked after the current head's `key.epoch`); losing RES after the
winning `key.epoch` event; ≤ 1 rotate / chan / 10 s / user and ≤ 1 / chan / 2 s, both waived only if a
hash of (sorted roster ‖ sorted active device ids of roster users), stored per epoch at rotation time,
differs now (not judged by what the bundle boxed: an unboxed device must not exempt). Search
excludes e2e. Webhooks into e2e forbidden. Delete/edit scrub messages AND original event data.

### 13.6 Events
`device.new {cert, endorse?}` (all) · `device.revoke {revoke}` (all) · `key.epoch {bundle}` (members at
seq; after chan.join, after chan.leave) · `key.share` (members) · `msg.*`/`react.*` gain `enc?`.
`Channel += {e2e, epoch, creator?}`; `Message += {enc?}` (body "" when enc). Snapshot += `devs`, `revoked`, `keys: [{bundle, seq}]`. A device revoke invalidates only bundles whose seq is after the revoke event's seq, or whose signed `time` is at or after the revoke's signed `time` (a client rejects a bundle on either).

### 13.7–13.8
Ephemeral `key.need {chan, epoch, dev}` (≤ 1/chan/30 s) → members answer with `key.share`.
Error `stale_epoch` (retryable, re-encrypt with the SAME nonce). Optional BYE `upgrade_required`.
Limits: enc ≤ 64 KiB + 1 KiB; DeviceCert ≤ 4 KiB; ≤ 8 devices/user; bundle ≤ 16 KiB (≤ 128 boxes).

## PR plan (each: `go test -race ./...` + listed tests)
0. **M1 fix**: delete/edit scrub original `msg.new` (and older edit) event data; outbox match by nonce.
1. `internal/e2ecrypto` (pure): enc encode/parse, pad256, HKDFs, box seal/open, backlink, kc, mid.
   KATs in testdata, bit-flip/AD-swap failures, fuzzed parsing.
2. `identity.SignContext/VerifyContext` + DeviceCert/KeyEndorse/Revoke; all key types incl. sk-* UP flag; cross-context replay rejected.
3. Client trust store `e2e.db`: survives reset; concurrent processes.
4. Proto+server devices: `e2e1`, device ops/events, snapshot `devs`, limits (protocol-guardian).
5. Server epochs: `key.rotate` CAS/roster/staleness, `key.fetch`, `key.epoch`, ordering, rate cap.
   Decision (PR #13 review): `user.disable` does not emit device revokes; instead rosters exclude
   disabled users and their devices count as revoked for sealing and staleness. Low-order X25519 keys (all-zero or
   any other, which would make every box undecryptable and block rotations) are rejected in certs; clients drop unverified objects for another server_id and keep ≤ 64 per user per kind.
6. Client bundle logic: verify chain/kc/backlinks, lazy decrypt queue, `key.need/share`; reject bundles with duplicate recipient X25519 keys (one ephemeral + zero nonce per box means a duplicate would reuse key+nonce).
   Decisions (step 6 review): a bundle is invalid if EITHER the server seq order OR its signed `time` puts it
   after a signed revoke of its `by` device (time: more than 5 min after, for clock skew; defends against a
   server acting alone — a revoked device colluding with the server can backdate); `key.share` stays a logged,
   rate-limited event so offline devices get keys; TOFU on the first valid cert in arrival order is accepted
   until invite-anchored pins (M4).
7. Encrypted send e2e: outbox stores enc once, stale_epoch retry same nonce, DMs epoch 1, self-DM.
   Decisions (step 7): Plain = `{t: "msg", ts, body, thread_ref?}`; `thread_ref` = the root's mid, or 16
   zero bytes for a plaintext (legacy) root. "Same nonce" on stale_epoch = same send nonce AND same enc
   nonce (safe: the epoch key differs; same epoch → identical bytes). A message is encrypted at `Send`
   when the verified head's K is here (offline too), else by the outbox before the first send; only a
   stale_epoch re-encrypts (at most twice in a row, then retry later), never a reconnect, so a lost RES
   finds its bytes by nonce. The sender rotates first when there's no bundle (DM member / creator) or the
   roster changed, and after a stale_epoch at the server's current epoch (e.g. a boxed device was
   revoked). No K → failed "no key yet — waiting for a member" + key.need, requeued when K arrives.
   Receivers: undecidable messages (no K, unknown device, thread root not cached) wait as ciphertext and
   are re-judged by the e2e worker; invalid ones are never shown; first mid wins. Plaintext in an e2e
   channel is legacy below epoch 1's `key.epoch` seq (stored in the trust store, schema v8; unknown →
   legacy) and "inserted by server" at or above it — note step-6 clients sent plaintext after epoch 1,
   so such old messages also show as inserted. The server refuses plaintext `edit` in e2e channels too
   and the client refuses edits there until step 8. The cache (and outbox) holds plaintext: as
   sensitive as the conversation.
   Decisions (step 7 review): pre-step-7 plaintext after epoch 1 showing as "inserted by server" is accepted
   (nothing was deployed); thread_ref = 16 zero bytes for a plaintext root, and a reply to a deleted root seen
   only after a reset shows invalid (documented edge); sends failed for lack of a key are requeued when the key
   arrives; invalid/inserted messages count as unread but never notify or count as mentions.
   Security review fixes: legacy vs inserted is decided by a sticky per-channel **legacy cutoff** in the
   trust store (the newest message id cached when the client first saw the channel encrypted), not by epoch
   seqs; both are unauthenticated (no notifications, mentions or activity) and render loudly. Future
   improvement: carry a signed cutoff in the epoch-1 bundle so every member agrees on what is legacy. The
   lowest id wins for a mid (server: unique (chan, mid), replays → conflict); a verified row's chan/author/
   thread/bytes never change on refetch; revokes with unknown seq reject messages whose Plain.ts is > 5 min
   after the revoke time; the outbox never seals to a stale key, re-encrypts queued-but-unsent encs when
   the head or members changed, and refuses everything in alarm; e2e-scoped search stays local; edits of
   uncached messages and reminders into e2e channels are refused; the cache uses secure_delete.
8. Encrypted edit/delete/react, thread_ref, HMAC emoji, local FTS.
   Decisions (step 8): Plain types `edit {t, ts, body, ref, v}`, `del {t, ts, ref}`, `react {t, ts, ref,
   emoji, add}` (ref = the original's mid). Edits are sealed with the current epoch (stale_epoch like a
   send; the server dedupes a retry of the stored `edit_enc` first) and keep the original `enc` next to the
   latest `edit_enc`: the original anchors the mid, so **an edit doesn't hide the original text from
   members holding its epoch key** (only delete removes ciphertext); superseded edit encs are scrubbed
   from the log. `v` = last applied + 1, chosen by the editor; the server can't order edits, so clients
   do, the same way everywhere: the higher (v, Plain ts, mid of the edit's enc) wins and an equal tuple
   is the same edit, so two devices editing at once with the same v converge whatever the arrival
   order (coordinator decision). A server can still serve an older edit to a fresh device (rollback of edits is not
   detected without history). Deletes and reactions are sealed under the **message's own epoch** (the
   tag needs that mk anyway; nothing secret in them); the server checks their epoch/eh match the
   message's enc and the signer is the caller's active device, and caps their enc at one padding
   block (397 bytes) so reactions can't bloat a message past the frame limit. Only the author can sign a delete; an
   admin's delete is unsigned and shows "removed (not by author)"; deleted messages keep `del_enc`
   (`Message.del_enc`) so a fresh device can tell, comparing `ref` with the mid only when it knows it.
   Reaction tags are `"~" + base64url(HMAC(rk, "tc-react"‖mid‖emoji)[:12])` with `rk = HKDF(mk, "tc-react-v1")`; `Message.renc` carries each
   reaction's enc; removes need no proof (a server can always withhold a reaction), and a server can
   replay a reaction its author since removed (adds carry no version; accepted for v1). The server's
   `edited` and tag map are never shown for encrypted messages. Edits of a waiting message (or whose key
   or device is missing) and unjudgeable reactions are kept (cache `epend`, `rencs`) and retried by the
   e2e worker. Rejections go to a bounded in-memory log (`Client.E2ERejects`). Local search stays the
   step-7 LIKE scan over decrypted bodies (not FTS5): edits update the body, deletes clear it.
   Coordinator decisions (step 8 review): deletes and reactions into a channel in alarm are refused
   client-side like sends and edits (`ErrE2EAlarm`); `enc` on edit/delete/react isn't gated on `e2e1`
   (same as `send`).
   Security review fixes (step 8): "message deleted" only for an attributable delete (author-signed,
   or plaintext channel); "deleted (unverified)" while judging; anything else "removed (not by
   author)", incl. step-7 cached deletes (migration). The server keeps the original mid and sends it as
   `Message.mid`; a device that never saw the enc checks a delete's ref against it (only if no other
   cached message has it; never used for threads), and without a mid a delete isn't the author's.
   Reaction tags use `rk = HKDF(mk, "tc-react-v1")` so mk is only an AEAD key. The edit time shown is
   the signed Plain ts. A server copy without enc never replaces a cached encrypted message. React/
   delete of uncached messages is refused. ≤ 256 reactions per message (`conflict`); history pages
   always hold one message (trimmed `renc`). Pending work is bounded (16 reactions per message, 1000
   reactions/edits per channel), reactions of deleted/invalid messages are dropped, and pending
   items no longer wake the e2e worker (it runs on key/device events), so it can't spin.
9. `chan.add{bundle}`, `chan.remove`, `chan.encrypt`, disable → rotate; history via backlinks.
   Decisions (step 9): in an encrypted channel with an epoch, `chan.add`/`chan.remove` carry the next
   bundle and the server commits it in the same write as the membership change (events: chan.join /
   chan.leave, then key.epoch; the joiner sees both, the removed user only its chan.leave). The bundle is
   optional at epoch 0 (no key yet; the creator's epoch 1 includes everyone) and for removing a disabled
   user (in no roster). `chan.remove`: the creator (while a member) or an admin may remove others,
   anyone may remove themselves (= leave); private channels only (public: bad_request, DMs: forbidden);
   only a roster member can make the bundle, so a member remover must send it, while an admin who isn't
   a member removes without one (coordinator decision): the bundle goes stale and the next sender
   rotates, as after a leave. `chan.encrypt` deletes the channel's webhooks in the same write and
   retires orphaned bots (coordinator decision).
   Leaving can't carry a bundle (the leaver would know K): the bundle goes stale and the next sender
   rotates (unchanged step-7 path). `chan.encrypt` is cap-gated; only the creator of a private channel
   (any member of a DM with others) may convert, because epoch 1 must be signed by the creator / a DM
   member and clients already check exactly that — letting any member convert would need clients to
   accept epoch 1 from anyone. It records `Channel.legacy` (newest message id); a client uses its own
   newest cached id as the legacy cutoff, and the server's `legacy` only when it cached nothing (so the
   server's word only affects devices that never saw the plaintext; both classes are unauthenticated).
   A conversion marks cached plaintext legacy at once. `user.disable`: the admin's client rotates every
   encrypted channel it shares with the user (best effort). A client removed by someone else keeps the
   channel and its history (`ChannelView.Removed`, "you were removed") until `/leave` closes it; it
   keeps its old epoch keys (trust store) and gets nothing newer. `/add` refuses with the names of
   members who have devices but none verified.
   Security review fixes (step 9): a server-claimed legacy cutoff (`Channel.legacy`, used only with
   nothing cached) counts only if it is ≤ the seq of the channel's verified epoch 1 (else 0), and what it
   classifies shows "⚠ unauthenticated (legacy, server-claimed)" (trust store v9 records the claim);
   re-adding someone removed by another user (their last leave's actor ≠ themselves) to an encrypted
   channel needs the creator or an admin; the creator can't be removed at epoch 0 (only they can make
   epoch 1); clients seal and rotate only to members confirmed by the verified head's roster (a verified
   epoch is signed by a previous-roster member): a user present only through an unsigned chan.join is
   "unconfirmed", never sealed to, and an alarm ("membership not confirmed by a member") once the chain is
   caught up. Epoch-0 channels still trust the server's member list (the creator builds epoch 1 from it).
   Known limits (step 9): a removed member keeps the channel's history only until their next snapshot
   reset (it wipes the cache; their trust store keeps the old keys, but the server no longer serves
   them the messages). `Channel.legacy` is server-asserted and used only when a device cached none of
   the channel's plaintext; carrying a signed cutoff in the epoch-1 bundle (bundle `v: 2`) is a future
   improvement. Deletes and reactions are sealed under their message's own epoch (step 8), so for
   messages from before a removal they use a key the removed member holds: a removed member colluding
   with the server (or reading its database) can still see later deletes of, and reactions (real emoji)
   to, those old messages — never new bodies.
10. TUI: key-change banner + send block, fork alarm, "⚠ unencrypted", "waiting for key", `/verify`.
   Done (step 10): see "Verification" above. Decisions: the safety number covers only *pinned* keys
   (pending key changes are excluded until accepted, which changes the number); a teammate whose own
   device views differ (e.g. one side pinned another of their keys) gets a mismatch, which is the point;
   the key-change gate covers only encrypted channels and excludes my own pending keys; alarm clears are
   audited (trust schema v10, `trust_audit`) and re-raised if the server still presents the cause;
   the one-time `?` hint is per channel per run (not persisted).
11. Malicious-server suite (`Tamper` hook): injected devices/keys, fake joins, rollback, fork,
    forged bundles, swapped/withheld boxes, hidden leave, stripped cap/flag, plaintext injection,
    cross-channel/thread replay, author/thread/time rewrite, forged delete, reorder, forced reset,
    bad key.share; malicious member: inconsistent boxes, rotation spam, garbage enc, control chars.
   Done (step 11). **Hooks**: `internal/server/tamper_on.go`, compiled only with `-tags tctamper`
   (`tamper_off.go` is the default: empty stubs behind a `false` constant, so release builds hold no
   hook code; `tc server` refuses to start from a tagged build). `Server.SetTamper` installs
   `Welcome` (rewrite WELCOME per user) and `Out` (every outgoing frame of a user's sessions — EVENT,
   RES, SNAPSHOT, EPHEMERAL — rewritten, dropped, multiplied or held back to reorder);
   `TamperDB`/`TamperWrite`/`TamperAppendEvent`/`TamperAddMember` change the database and log as the
   server would, `TamperPush`/`TamperKick` inject frames and force reconnects. **Suite**:
   `internal/e2e/malicious_test.go` (+ `malicious_harness_test.go`, same tag), real clients over
   loopback; each case checks that no key or plaintext reaches anything the attacker controls (no
   box/share to its device in the server log, no epoch key in its trust store, no enc it can open,
   no plaintext row) and the client-visible signal. Run: `make test-malicious` or
   `GOTOOLCHAIN=local go test -race -tags tctamper ./internal/e2e -run Malicious` (`make test` and CI
   run it too). Results:

   | case | result |
   |---|---|
   | device cert under a server-added SSH key / add-key invite | detected: key-change banner, bob's sends `ErrKeyChange`, nothing sealed to it |
   | fake `chan.join` of mallory (plain / + bundle signed by mallory) | detected: alarm "membership not confirmed" / "rotated by someone not in epoch n", sends and rotations refused |
   | replayed old bundle; live claim of an older epoch | harmless: ignored, sends stay on the head epoch |
   | forced reset to a snapshot at an older epoch | detected: rollback alarm (trust store survives the reset) |
   | fork (different validly signed bundles to A and B) | detected: the other branch's messages invalid, fork alarm once both bundles are seen |
   | bundle signed by an unknown device / claiming a member's device | unknown: "unverified", the server's K never used; claimed: signature alarm |
   | box swapped with another's / bundle moved from another channel | swapped: signature alarm; moved: ignored |
   | withheld box (device hidden from the rotator) | recovered by key.need/key.share, no alarm |
   | hidden removal (`chan.remove`, leave event hidden from a member) | detected: alarm "membership not confirmed", nothing sealed to the removed user |
   | hidden self-leave (server keeps the leaver a member) | **accepted limit** (threat model: withheld removal) |
   | `e2e1` stripped from WELCOME / `e2e` cleared on a channel | detected: sticky alarm, sends refused, no plaintext |
   | channel presented as plaintext at first contact | **accepted limit** (threat model: e2e flag of a never-seen channel) |
   | plaintext `msg.new` in an encrypted DM | "inserted by server", no mention/notification |
   | enc replayed into another channel / a thread / as duplicate / as an edit | invalid (edit: rejected, text unchanged) |
   | rewritten author / thread | invalid |
   | rewritten time | step 11: accepted limit; **closed in step 12**: the signed time is shown with "⚠ server time differs" (`TestMaliciousRewrittenTime`) |
   | forged delete (unsigned / another message's signed delete) | "removed (not by author)" |
   | reordered events (seqs swapped) | message waits then opens; the newer edit wins |
   | dropped or skipped `key.epoch` | **bug found and fixed**: the client now `key.fetch`es (see below) |
   | different `server_id` in WELCOME | client stops ("server identity changed"), trust store untouched |
   | `key.share` with a server-chosen K | rejected by kc; the real K arrives later |
   | member: box with a different K for one recipient | kc rejects it; recovered by key.share, no alarm (§13.3) |
   | member: rotation spam | one rotation, the rest `rate_limited` |
   | member: well-signed garbage / control characters / wrong Plain type | invalid, attributed to the member |

   Decisions (step 11): **bug fixed** — a `key.epoch` event delivered after a later seq (or dropped)
   is skipped and never replayed, which wedged the channel for that client (messages waited forever,
   every send failed after a stale_epoch). Clients now `key.fetch` the epochs above their head when a
   message waits for an epoch above every bundle they hold (once per epoch per connection) and after
   a `stale_epoch` on a send or rotation (PROTOCOL §13.3). An inconsistent box from a malicious member
   is recovered silently rather than alarmed (kc already guarantees every member ends up with the
   same K; per §13.3 an unusable box is not a fork). A bundle from an unverifiable device doesn't
   block sending under the verified head.
12. Security pass + chaos during rotations + docs (threat model, device loss, local cache sensitivity).
   Done (step 12). **Chaos during rotations** (`internal/e2e/chaos_rotate_test.go`, skipped under
   `-short`): alice, bob, carol, dave in an encrypted private channel; alice/bob/dave send while alice
   adds carol and removes dave and members rotate keys, all through the chaos proxy (kills a connection
   every 150–500 ms, 0–15 ms latency per write; the server clock is skewed past the no-change rotation
   cap). Checked: every member converges to the identical set of decrypted messages (each once, none
   waiting, nothing pending, no alarm) and the identical verified head; dave can't open any row the
   server stored after his removal and holds no later epoch key. `-count=5` with `-race`, 5/5 green:

   | run | kills | explicit rotations | messages | dave removed at | head | converged after chaos |
   |---|---|---|---|---|---|---|
   | 1 | 28 | 6 | 50 | epoch 10 | 11 | 8.1 s |
   | 2 | 32 | 6 | 50 | epoch 8 | 11 | 15.9 s |
   | 3 | 36 | 5 | 50 | epoch 8 | 11 | 19.7 s |
   | 4 | 36 | 5 | 50 | epoch 9 | 10 | 17.0 s |
   | 5 | 27 | 6 | 50 | epoch 8 | 11 | 5.8 s |

   **Signed-time drift** (closes step 11's "rewritten time"): verified messages keep Plain `ts`
   (`Message.SignedTime`, cache column `msgs.sts`); `Message.TimeDrift()` when it is more than
   `client.MaxTimeDrift` (5 min) from the server's time; the TUI then shows the signed time, doesn't
   group the message under the previous one and adds a dim "⚠ server time differs" (also in the
   viewer's title). **Fuzzing**: `make fuzz` (`FUZZTIME`, default 30 s) runs 14 targets covering
   every decoder of untrusted E2E input (results below). **Security pass**: findings below; every
   medium or higher is fixed with a test that fails without the fix.

## Step 12 security pass

Fresh-eyes review of `internal/client` (e2e, enc*, membership, verify, devices), `internal/trust`,
`internal/server` (epochs, membership, keyshare, devices, encrypted send paths), `internal/e2ecrypto`
and the signed-object code in `internal/identity`, as a malicious server, a malicious member, a
removed member and a stolen-device holder. Things already listed as accepted limits above were not
re-reported. Each fix's test was run against the code without the fix and failed.

| # | sev | where | attacker · scenario | status |
|---|---|---|---|---|
| S1 | high | `client/e2e.go:801` (`poisonedLong`), `commitBundle` | **member**: rotates with a validly signed bundle whose every other box holds a wrong K, then goes silent. Honest clients accept it as head (§13.3); nobody has K, so nobody can rotate (the backlink needs K) or remove her: the channel is wedged, no alarm. | **fixed** (33a9c78): a box in the signed bundle that doesn't yield K proves the rotator's fault; after `poisonWait` (2 min) without a key.share the e2e worker rotates past it with a backlink to a random key (history gap for that epoch only). `TestEncRecoversFromPoisonedBox` |
| C1 | medium | `client/encmsg.go:221` (`noteE2EChan`) | **server**: shows a new DM encrypted, withholds `devices.list` so the e2e worker never marks it, then sends `chan.update e2e:false`: queued messages went out as plaintext. | **fixed** (7cace74): the sticky e2e flag is set when a channel is first stored encrypted. `TestEncStickyFlagBeforeWorker` |
| C2 | medium | `client/encops.go:121` (`checkSealer`) | **removed member + server**: keeps posting/editing under the last epoch it held; shown as verified. | **fixed** (1f4cb9e): messages and edits need the author in the named epoch's roster and (known seq) no later than the next epoch's key.epoch; earlier-judged ones are re-checked when that epoch verifies. `TestRemovedMemberCantPostUnderOldEpoch` |
| C3 | medium | `client/e2e.go:594` (`applyRevoke`), `client/devices.go:212` | **stolen, already revoked device + server**: signs revokes of its owner's healthy devices and the server gives them seq 0 or an early seq: peers revoke them for good, their messages turn invalid, the victim's own client retires its keys. | **fixed** (9380eac): a revoked signer counts only if both seqs are known and both seq and signed time (±5 min) put the revoke before its own; SSH-signed revokes are applied first; own devices ignore revokes signed by a device we revoked. `TestE2ERevokedDeviceCantRevokeOthers`, `TestOwnRevokedIgnoresRevokedSigner` |
| C4 | medium | `client/encmsg.go:756` (`boxedRevoked`), `trust` schema v11 | **stolen device + server**: after its revoke the server never answers `stale_epoch`, so senders kept sealing under an epoch boxed to the stolen device until the next membership change. | **fixed** (5fa066b): the trust store records each epoch's boxed devices; sends/edits rotate first when one is revoked. `TestEncRotatesAfterBoxedDeviceRevoked`, `TestEpochBoxed` |
| S2 | medium | `server/epochs.go:465`, `client/e2e.go:735` | **user being disabled**: `dispatch` checks "disabled" outside the write lock, so an in-flight `key.rotate` (or `chan.add`) committed; the disabled user held the new K, and the admin's follow-up rotation was rate-limited away. | **fixed** (3d7750a): `rotateTx` re-checks disabled under the lock and requires the caller in the new roster; clients alarm on a rotator outside its own roster. `TestKeyRotateDisabledInFlight`, `TestE2ERotatorNotInNewRoster` |
| M1 | medium | `trust/trust.go:1009` (`AdvanceEpoch`) | **none (two processes)**: the TUI and `tc send` share TC_HOME; the slower one offered an epoch the other had already recorded and passed: `ErrRollback`, a sticky alarm that blocked sending. | **fixed** (5893574): the same bundle below the head is a no-op, a different one a fork, only an unrecorded one a rollback. `TestEpochsAndKeys` |
| C5 | low | `client/encmsg.go:447` (`reopenMsgs`) | **member**: 1000 replies to a root others don't have stay waiting forever; the queue only ever re-checked the 1000 oldest, so later messages never opened. | **fixed** (27c3947): the queue walks waiting rows batch by batch with a cursor. `TestEncReopenQueueNoStarvation` |
| C6 | low | `client/encmsg.go:522`, `client/encops.go:240` | **race / server**: the e2e worker wrote a verdict/edit by id outside the event transaction; a delete or refetch applied meanwhile could get the old body written back. | **fixed** (6d0d868): the updates match what was judged (enc, chan, author, thread, not deleted). Covered by the existing suites (the window isn't reproducible deterministically). |
| C7 | low | `trust/trust.go:744` (`PutDevice`) | **server + member**: a verified twin device carrying another device's X25519 key made every honest rotation fail (`ErrDuplicateKey`). §13.2 asks clients to check. | **fixed** (27c3947): the trust store refuses a new device with another device's X25519 or Ed25519 key. `TestDevices` |
| S3 | low | `server/epochs.go:502` | **two colluding members**: leave / rotate / re-add churn changes the state each time, so the no-change rotation cap never applies: the chain grows at the write rate limit (~5/s); joiners must fetch and verify all of it. | documented: availability only (bounded by the write limit and `key.fetch` ≤ 256 per call); a cap on waived rotations is future work. |
| S4 | info | `server/ops.go:195` (`canRead`) | **member removed mid-request**: `history`/`pins.list` check membership, release the lock, then query, so a removal in between returns post-removal rows. | documented: those rows are ciphertext under epochs the removed member has no key for (C2/C4 hold); only metadata leaks. |
| I1 | info | `client/encmsg.go:338` and `encops.go` | **member**: Plain msgpack isn't checked for canonical form (duplicate/unknown keys, trailing bytes decode). | documented: a Plain is AEAD-sealed and signed by its one author and every client runs the same decoder, so it can't mean two things to two clients; `FuzzPlain` checks only valid text is shown. A canonical check needs Plain `v: 2`. |
| I2 | info | `identity/certs.go:131` | **member**: a cert with a small-order Ed25519 key lets anyone forge that device's signatures. | documented: only weakens the member's own device (they can already post anything); rejecting small-order keys is future work. |
| I3 | info | `client/e2e.go:492` | **server**: re-serves malleated ECDSA cert signatures (s → n−s); each is re-verified and reported as a change. | documented: costs a verify and a UI refresh. |
| I4 | info | `trust/trust.go:897` | `AdvanceChain` skips the gap check. | documented: used only by tests; production uses `AdvanceEpoch`. |
| C8 | info | `client/encmsg.go:348` | **server**: points a reply's thread at an invalid replayed copy of its root (same mid): the reply shows under a hidden root. | documented: the reply is genuine and in the right conversation; cosmetic. |
| C9 | info | `client/conn.go:122` | **server**: `session.call` has no per-call timeout and some calls run under `e2eMu`; a withheld reply stalls e2e work until reconnect. | documented: availability (the server can always withhold); C1 no longer depends on it. |

**Fuzzing** (`make fuzz FUZZTIME=60s`, 60 s each, 8 workers; no crashers, so no regression inputs to keep):

| target | decodes | execs |
|---|---|---|
| `proto` FuzzDecode | every frame, op args, events | 8.7 M |
| `proto` FuzzDevices | Device, revoke, devices.list, snapshot devs | 9.0 M |
| `proto` FuzzKeyOps | key.share / key.rotate / key.epoch / key.fetch / chan.add payloads | 7.1 M |
| `sanitize` FuzzSanitize | rendered text | 10.9 M |
| `e2ecrypto` FuzzParseEnc | enc framing | 12.0 M |
| `e2ecrypto` FuzzDecrypt | enc verify + AEAD + unpad (re-signed) | 10.2 M |
| `e2ecrypto` FuzzUnpad | padding (only pad256 output accepted) | 15.3 M |
| `e2ecrypto` FuzzOpenBoxes | epoch boxes, key.share boxes, backlinks | 3.1 M |
| `identity` FuzzDeviceCert | DeviceCert decode/verify | 3.4 M |
| `identity` FuzzKeyEndorse | KeyEndorse decode/verify | 2.5 M |
| `identity` FuzzDeviceRevoke | DeviceRevoke decode/verify (SSH and device) | 2.7 M |
| `identity` FuzzEpochBundle | EpochBundle decode/verify, CheckBoxKeys | 5.7 M |
| `client` FuzzPlain | Plain msg/edit/del/react through judge, applyEdit, deleteVerdict, judgeReact | 94 k |
| `client` FuzzKeyShare | key.share storage and application (never a key that misses kc) | 1.5 M |
