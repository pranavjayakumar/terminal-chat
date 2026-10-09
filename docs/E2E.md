# End-to-end encryption design (M3) — draft

Status: design draft. Must pass `protocol-guardian` and `security-reviewer` before implementation.

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

## Pitfalls checklist

Nonce reuse (random 24-byte XChaCha nonces) · cross-channel replay (chan/epoch in AD) · trusting the
server's device list (verify certs) · plaintext leaking into webhooks, logs, outbox, crash output ·
rotation races (CAS + retry) · retention must purge old epoch keys · no unaudited MLS.

Libraries (all pure Go, keeps `CGO_ENABLED=0`): `crypto/ecdh`, `crypto/ed25519`,
`x/crypto/{chacha20poly1305,hkdf,nacl/box,ssh}`.
