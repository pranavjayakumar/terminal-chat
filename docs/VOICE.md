# Voice huddles design (M5)

Status: **draft r1** (target release v0.4.0). Wire format: PROTOCOL.md §14 (cap `voice1`).

## Goals / non-goals

- Drop-in voice per channel (public, private, DM): `/huddle` joins the channel's huddle,
  `/leave-huddle` leaves. No ringing, no scheduling, no "call" object.
- Low latency over flaky networks: unreliable QUIC datagrams on the existing connection, Opus
  with in-band FEC, an adaptive jitter buffer. Mouth-to-ear target < 200 ms on a decent link.
- Voice in an encrypted channel (`Channel.e2e`) is end-to-end encrypted with keys derived from
  the channel's epoch key; the server relays bytes it cannot decrypt.
- The default `tc` stays a static `CGO_ENABLED=0` binary. Audio needs C (libopus, an audio
  device layer), so it lives behind the build tag `voice`; everything else (server relay,
  protocol, huddle state, TUI indicator) is pure Go and always built.
- Non-goals for v1: video, screen share, recording, server-side mixing, voice over the
  TCP fallback, PSTN, more than 16 people in one huddle, per-sender signatures on frames.

## Huddle model

- One huddle per channel, implicit: it exists while at least one person is in it.
- Joining needs current membership of the channel (the server's `members` map, same check as
  typing). Losing membership (`chan.leave`, `chan.remove`, `user.disable`) drops you at once.
- A **session** (one connection = one device) is in at most one huddle. A **user** is in at
  most one huddle: joining from another session or channel moves them (the old participation
  ends, the old session sees itself leave). This matches Slack: you can't be in two calls.
- The server assigns each participation a `src` (u32, unique on the server while it lives,
  never 0, never reused within a server process) that tags its datagrams.
- Huddle state is **ephemeral** (never stored): a server restart ends every huddle and clients
  rejoin by hand. State travels as EPHEMERAL `huddle {chan, parts}` (§14.2) to the channel's
  members on every change, and once per huddle right after `sync` (like presence).
- Who's talking is a client-side fact (frames arriving from a `src` in the last ~300 ms). The
  server never sends "speaking" events: it would be a second, laggier source of the same truth.
- Mute is client-side too (a muted client sends nothing). There is no mute op in v1: whether
  someone is talking is visible from their frames, which is all a listener needs.

## Transport

- **QUIC datagrams (RFC 9221)** on the same QUIC connection as the frame stream. Both sides set
  `EnableDatagrams`; it's a transport parameter, so old peers simply don't get datagrams.
- **TCP fallback: no voice in v1.** A reliable stream turns loss into head-of-line stalls (a
  dropped packet freezes every following frame for an RTO), which is worse than silence, and it
  would share the 8 MiB session queue with chat. `huddle.join` on a TCP session fails with
  `bad_request` "voice needs UDP"; the TUI says "huddle needs UDP — your network blocks it".
  TCP sessions still see huddle rosters, so you know a huddle is happening. Frame type 0x40
  stays reserved for a possible framed fallback later.
- Datagrams carry no stream ids or acks. Loss is concealed by Opus (FEC + PLC), never
  retransmitted.
- quic-go's `SendDatagram` blocks when its 32-entry send queue is full (congestion). The server
  therefore never sends from the reading goroutine: each receiving session has a small bounded
  channel (64 datagrams ≈ 1.3 s at one speaker) drained by a writer goroutine; full → drop.
  One slow listener can't slow anyone else.

## Codec

- Opus, 48 kHz mono, 20 ms frames (960 samples), `OPUS_APPLICATION_VOIP`.
- 24 kbit/s VBR (≈ 60 B/frame), in-band FEC on with expected loss 10 %, DTX on (silence costs
  ~1 frame / 400 ms). Complexity 5 (cheap on laptops).
- Push-to-talk default: nothing is captured or sent while the key isn't held.
- The datagram limit (512 B, §14.3) leaves room for 120 ms-worth or a 128 kbit/s frame; v1 only
  ever sends 20 ms frames.

## Jitter buffer (per remote `src`)

- Frames are ordered by `seq`. Playout starts after a target delay (initial 60 ms), adapted
  between 40 and 200 ms from the observed inter-arrival jitter (RFC 3550 estimator, target ≈
  3 × jitter + 20 ms, re-evaluated at talk-spurt starts so it never stretches mid-sentence).
- Missing frame at playout: if `seq+1` is buffered, decode its FEC data; else Opus PLC; after 5
  consecutive missing frames the stream goes silent and the buffer resets.
- Late frames (behind the playout point) are dropped. A flag bit marks the last frame of a
  talk spurt so the buffer can drain cleanly on push-to-talk release.
- Remote streams are mixed (summed with clipping) **on the client** into the playback device.

## Server: SFU relay

- The server forwards; it never decodes, mixes or transcodes, and the datagram payload is
  opaque to it (and ciphertext in encrypted channels).
- For each datagram from a session: parse the fixed header (§14.3); drop unless the session is
  in a huddle, `src` equals the session's assigned `src`, the kind matches the channel (only
  encrypted kinds in an e2e channel), the size is within limits and the session's token bucket
  has a token. Then queue the **identical bytes** to every other participant's session.
- Nothing is buffered or logged per frame. Counters only (relayed, dropped by reason).
- Cost: with push-to-talk usually 1 speaker → N-1 sends. Worst case 16 open mics → 240
  datagrams per 20 ms; the per-sender rate limit bounds it.

## E2E for voice (encrypted channels)

Keys come from the channel's newest **verified** epoch key `K` (§13.3); nothing new is sealed
or distributed.

```
vk    = HKDF-SHA256(K, info "tc-voice-v1" || server_id || chan || epoch)          32 B
sk    = HKDF-SHA256(vk, info "tc-voice-send-v1" || dev(16) || stream(16))         32 B, per sender stream
nonce = 0x00000000 || u64be(seq)                                                    12 B
ct    = ChaCha20-Poly1305(sk, nonce, opus_frame, AD = header)                      header = bytes 0..18
```

- `dev` is the sender's device id, `stream` 16 random bytes the sender picks on every
  `huddle.join` and announces in it; the server relays both in `parts`. Per-sender keys mean two
  senders never share a (key, nonce) even if a malicious member copies someone's `stream`.
- `seq` starts at 0 and increases by one per frame; a stream stops at 2³² − 1 (≈ 2.7 years).
- The header (kind, flags, src, seq, epoch, ts) is the AD, so the server can't alter any of it.
- **Replay**: per stream, a 64-frame sliding window over `seq` (DTLS-style) drops duplicates and
  frames older than the window. **Cross-session replay** (the server re-announcing an old
  `stream` and replaying a recording): every encrypted header carries `ts`, the sender's Unix
  time in seconds, inside the AD; receivers drop frames whose `ts` is more than 60 s from their
  own clock. This needs clocks within a minute (NTP); the TUI shows "voice rejected (clock?)"
  when it happens repeatedly.
- **Which keys a receiver accepts**: frames under the channel's newest verified epoch and the
  one before it (so a rotation mid-sentence doesn't cut audio), with `dev` a verified,
  non-revoked device of the user the roster names (trust store), and only from senders in that
  epoch's roster. Anything else is dropped and counted; plaintext kinds in an e2e channel are
  always dropped (downgrade).
- **Senders** use the newest verified epoch, and switch as soon as a new one verifies. A
  removed member keeps the old `K` but stops getting new keys, so after the remaining senders
  switch (sub-second after the rotation verifies) they can't decrypt, even if a malicious server
  keeps relaying to them.
- A channel with no verified epoch yet (or an unconfirmed-member alarm, §13.4) refuses voice:
  "voice waits for the channel's keys".

Public channels: voice is protected hop by hop (QUIC/TLS 1.3) but the **server can hear it**,
exactly like public-channel text. The TUI marks encrypted huddles with the lock it already uses.

## Limits

| What | Limit |
|---|---|
| participants per huddle | 16 (`huddle.join` → `conflict`) |
| concurrent huddles per server | 64 (`conflict`) |
| datagram size | ≤ 512 B including the header; larger dropped |
| per-session send rate | token bucket burst 100, refill 60/s (50/s nominal); excess dropped |
| `huddle.join`/`huddle.leave` | count as write ops for rate limits (§9) |
| per-receiver relay queue | 64 datagrams; full → drop newest |

## UX (TUI + herdr)

- `/huddle` — join the current channel's huddle (or start it). In a huddle, the status line
  shows `🎧 #chan · ana bo* cy` — `*` marks who's talking right now, `(m)` muted, and the lock
  for encrypted channels. Channels with a huddle you're not in show a small `🎧3` in the list.
- **Talk key `ctrl+t`** (bound only while in a huddle; unbound otherwise).
  Terminals don't report key-up, so: press starts talking; if key-repeats follow (you're
  holding it) talking stops 600 ms after the last repeat — push-to-talk. If no repeat arrives
  (a tap), the mic latches open until the next tap — open mic. The 600 ms covers typical OS
  initial repeat delays (macOS default ≈ 375–500 ms).
- Not talking = muted. `/leave-huddle` leaves.
- Builds without voice: `/huddle` explains "this tc build has no voice — install the voice
  build (see docs/VOICE.md)"; the roster indicator still works.
- herdr: the pane's metadata badge gets `🎧` while you're in a huddle; no notifications for
  huddle starts in v1 (could be noisy; owner decision, see open questions).

## Audio I/O library

Choice: **miniaudio via `github.com/gen2brain/malgo`** for capture/playback, **libopus via a
~100-line cgo wrapper** in `internal/voice`.

- miniaudio is a single C file vendored inside malgo: no system package for audio I/O on either
  OS. CoreAudio on macOS; on Linux it `dlopen`s PulseAudio/PipeWire-pulse or ALSA at runtime, so
  the binary doesn't hard-link them. Full-duplex, per-callback low latency, MIT-0/public domain.
- PortAudio needs a system library on both OSes (not installed via brew here), adds a link
  dependency, and has no advantage for two mono streams.
- libopus (BSD) is the reference codec; there is no production pure-Go Opus encoder. We wrap
  only what we use (`opus_encoder_create/ctl/encode`, `opus_decoder_create/decode`), avoiding a
  binding dependency (`hraban/opus` also wants libopusfile).
- Pure-Go-only alternatives (e.g. a Go Opus decoder) don't encode, and a raw PCM fallback would
  be 768 kbit/s; not worth it.

## Builds and releases

- `go build ./...` (default, `CGO_ENABLED=0`) → `internal/voice` compiles its stub
  (`voice_stub.go`, `//go:build !voice`): `Available() == false`, every call returns
  `ErrNoVoice`.
- `make build-voice` → `CGO_ENABLED=1 go build -tags voice`, needs `pkg-config opus`
  (macOS: `brew install opus`; Debian/Ubuntu: `libopus-dev`).
- Release plan (not in this PR; release.yml is owned by the coordinator): build a second
  artifact `tc-voice_<os>_<arch>` on native runners (macos-14 arm64, macos-13 amd64,
  ubuntu-24.04 amd64 and arm64), linking libopus **statically** (`libopus.a`) so the only
  runtime dependencies are libc and the dlopen'd audio stack. It's listed in the same SSHSIG-
  signed `checksums.txt`. `tc update` keeps the flavor: a voice build downloads `tc-voice_*`.
  The default `tc` artifact and `install.sh` behavior are unchanged; `install.sh --voice` picks
  the voice artifact.

## Threat model

Attackers: a malicious or compromised server; a network attacker; a malicious channel member;
a removed member; a hostile client spamming datagrams.

| Threat | Mitigation |
|---|---|
| Network attacker reads/injects audio | QUIC (TLS 1.3, pinned SPKI) on every hop. |
| Server listens to an encrypted channel's huddle | Frames are ChaCha20-Poly1305 under keys from `K_epoch`, which the server never has. |
| Server alters, splices or relabels frames | Header is AEAD AD; per-sender key binds `dev`+`stream`; `dev` must be a verified device of the named user, so relabeling breaks decryption. |
| Server replays frames within a session | 64-frame replay window per stream. |
| Server replays an old recording later | `ts` in the AD, ±60 s acceptance; window per stream. Within 60 s of recording a replay into a *new* stream is possible (accepted risk, documented). |
| Server downgrades an e2e huddle to plaintext | Receivers drop plaintext kinds in e2e channels; senders never send them there. |
| Removed member keeps listening | Senders move to the new epoch as soon as it verifies; old `K` decrypts nothing new. Honest servers also drop them from the huddle at once. |
| **Malicious member impersonates another member's voice** | **Not prevented in v1.** Any roster member can derive any sender's `sk` (group-symmetric, like SFrame with a shared key). Text messages are signed per message; per-frame Ed25519 would add 64 B × 50/s = 25.6 kbit/s. Future: per-sender keys sealed to the roster with signed announcements. |
| Metadata | The server sees who is in which huddle, when, and frame sizes/timing (talk patterns; VBR sizes can leak some content). DTX + VBR leak more than CBR; CBR is a possible opt-in later. |
| Datagram flood / amplification | Only huddle participants can send; per-session token bucket; fixed max size; relay fan-out ≤ 15; bounded per-receiver queues with drop. A client can't make the server send to non-participants. |
| Hostile datagram parser input | Fixed-layout header parsed with bounds checks, fuzzed (`FuzzParseDatagram`); the server never touches payloads. |
| Audio-device privacy | Mic is opened only while in a huddle and (push-to-talk) only captured while talking; leaving closes the device. |
