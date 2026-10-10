# End-to-end encryption design (M3) — draft

Status: draft r1 below was **rejected** by review (a malicious server could read DMs: device certs,
membership and key boxes were all server-controlled). **r2 = the revisions at the end of this file
("Review r1 → r2") supersede anything above that conflicts.** Implementation follows the PR plan there.

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

## Verification

- Safety number = hash of both users' SSH key fingerprints; `/verify @bob` shows 6 emoji or 30 digits.
- `/verify @bob --github bob` compares against `https://github.com/bob.keys`.
- Invite carries the inviter's SSH fingerprint (TOFU anchored in the invite, like the TLS pin).
- Key change → sticky banner, sending blocked until acknowledged.

## Rollout

Cap `e2e1`. DMs and new private channels encrypted by default; `/encrypt` converts an existing
private channel. Public channels unchanged. Bots in encrypted channels get a bot device.

## Threat model (as of step 6)

What a malicious server can and cannot do today, and what closes each gap:

- **Trust on first use (TOFU), Signal-like.** A user's first SSH key the client sees in a valid
  device cert is pinned without confirmation. A server present at that first contact could
  substitute its own key and read what is sealed to it. Later key changes are caught (pending key
  change, nothing sealed until acknowledged). Clients therefore report every user whose pins are
  only TOFU (or endorsed by a TOFU key) as **unverified** (`Client.UnverifiedUsers`; the TUI marks
  them). Invite-anchored pins (the invite carries the inviter's SSH fingerprint) arrive in M4;
  `/verify` (safety numbers, step 10) upgrades a pin to verified.
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
- Cut from v1: age passphrase backup, `--github` verify, emoji SAS, per-epoch history boxes, SSHSIG.
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
| `edit` / `delete` / `react` | `+ enc?` (e2e react emoji = "~"+b64url(HMAC(mk,"tc-react"‖mid‖emoji)[:12])) | |
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
   from the log. `v` = last applied + 1, chosen by the editor; the server can't order edits, so two
   devices editing at once can diverge (the higher-v one wins where both arrive; a refetch shows the
   server's latest) and a server can serve an older edit to a fresh device (rollback of edits is not
   detected without history). Deletes and reactions are sealed under the **message's own epoch** (the
   tag needs that mk anyway; nothing secret in them); the server checks their epoch/eh match the
   message's enc and the signer is the caller's active device, and caps their enc at one padding
   block (397 bytes) so reactions can't bloat a message past the frame limit. Only the author can sign a delete; an
   admin's delete is unsigned and shows "removed (not by author)"; deleted messages keep `del_enc`
   (`Message.del_enc`) so a fresh device can tell, comparing `ref` with the mid only when it knows it.
   Reaction tags are `"~" + base64url(HMAC(mk, "tc-react"‖mid‖emoji)[:12])`; `Message.renc` carries each
   reaction's enc; removes need no proof (a server can always withhold a reaction), and a server can
   replay a reaction its author since removed (adds carry no version; accepted for v1). The server's
   `edited` and tag map are never shown for encrypted messages. Edits of a waiting message (or whose key
   or device is missing) and unjudgeable reactions are kept (cache `epend`, `rencs`) and retried by the
   e2e worker. Rejections go to a bounded in-memory log (`Client.E2ERejects`). Local search stays the
   step-7 LIKE scan over decrypted bodies (not FTS5): edits update the body, deletes clear it.
9. `chan.add{bundle}`, `chan.remove`, `chan.encrypt`, disable → rotate; history via backlinks.
10. TUI: key-change banner + send block, fork alarm, "⚠ unencrypted", "waiting for key", `/verify`.
11. Malicious-server suite (`Tamper` hook): injected devices/keys, fake joins, rollback, fork,
    forged bundles, swapped/withheld boxes, hidden leave, stripped cap/flag, plaintext injection,
    cross-channel/thread replay, author/thread/time rewrite, forged delete, reorder, forced reset,
    bad key.share; malicious member: inconsistent boxes, rotation spam, garbage enc, control chars.
12. Security pass + chaos during rotations + docs (threat model, device loss, local cache sensitivity).
