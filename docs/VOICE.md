# Voice huddles design (M5)

Status: **shipped in v0.4.0**; voice over the TCP fallback and the codec/quality changes below
since. Wire format: PROTOCOL.md §14 (caps `voice1`, `voice_tcp`).

## Goals / non-goals

- Drop-in voice per channel (public, private, DM): `/huddle` joins the channel's huddle,
  `/leave-huddle` leaves. No scheduling, no "call" object. A huddle started in a DM rings
  (docs/TUI.md, Notifications); channel huddles don't.
- Low latency over flaky networks: unreliable QUIC datagrams on the existing connection, Opus
  with in-band FEC, an adaptive jitter buffer. Mouth-to-ear target < 200 ms on a decent link.
- Voice in an encrypted channel (`Channel.e2e`) is end-to-end encrypted with keys derived from
  the channel's epoch key; the server relays bytes it cannot decrypt.
- The default `tc` stays a static `CGO_ENABLED=0` binary. Audio needs C (libopus, an audio
  device layer), so it lives behind the build tag `voice`; everything else (server relay,
  protocol, huddle state, TUI indicator) is pure Go and always built.
- Non-goals for v1: video, screen share, recording, server-side mixing, PSTN, more than 16
  people in one huddle, per-sender signatures on frames.

## Huddle model

- One huddle per channel, implicit: it exists while at least one person is in it.
- Joining needs current membership of the channel (the server's `members` map, same check as
  typing). Losing membership (`chan.leave`, `chan.remove`, `user.disable`), a revoked device and
  the channel becoming encrypted drop you at once.
- A **session** (one connection = one device) is in at most one huddle. A **user** is in at
  most one huddle: joining from another session or channel moves them (the old participation
  ends, the old session sees itself leave). This matches Slack: you can't be in two calls.
- Liveness: push-to-talk makes silence normal, so while a session is in a huddle the server
  PINGs it every 2 s and ends its participation (not the connection) after 10 s without any
  frame from it. A crashed, sleeping or unplugged client leaves everyone's roster within ~10 s
  instead of at the connection's 45 s idle timeout.
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
- **TCP fallback (cap `voice_tcp`, PROTOCOL.md §14.4).** Where UDP is blocked (corporate
  IPsec VPNs, some guest Wi-Fi) the connection falls back to TLS over TCP; voice then rides the
  frame stream as VOICE frames carrying the identical datagram bytes (same E2E encryption: the
  stream is only a carrier). The server relays between transports, so QUIC and TCP participants
  hear each other. The huddle bar shows `tcp` for the whole call and `/huddle` says `voice via tcp`.
  - A reliable stream turns loss into head-of-line stalls (a lost packet freezes every
    following frame for an RTO) and never drops late audio, so latency is bounded by dropping
    on purpose, at every hop: the client sends a frame only if the stream is free (never behind
    chat or a stuck write) and drops frames older than 60 ms; the server keeps at most 8 frames
    per TCP listener, ≤ 150 ms old, writes chat before them, and queues none while that
    session's output is backed up; `TCP_NOTSENT_LOWAT` (8 KiB) keeps the kernel from buffering
    seconds of it on Linux, and both ends skip voice while the socket already holds a backlog
    (`TCP_INFO` on Linux, `SO_NWRITE` on macOS); and the receiver's jitter buffer drops frames queued beyond its
    target + 80 ms. After a stall, the backlog is skipped, not played late.
  - Measured (`TestHuddleTCPSlowLink`: 22 KB/s of voice into a 12 KB/s link for 8 s): chat
    sent mid-call arrives in ~40 ms; voice latency stays at 0.2–0.24 s. Without the drops,
    chat took 1.4 s and voice latency grew to 3.5 s by the end.
  - FEC is off on TCP (nothing is lost, only late).
  - An old server (no `voice_tcp`): `/huddle` on TCP fails as before, "huddle needs UDP, and
    this connection fell back to TCP".
- Datagrams carry no stream ids or acks. Loss is concealed by Opus (FEC + PLC), never
  retransmitted.
- quic-go's `SendDatagram` blocks when its 32-entry send queue is full (congestion). The server
  therefore never sends from the reading goroutine: each receiving session has a small bounded
  channel (64 datagrams ≈ 1.3 s at one speaker) drained by a writer goroutine; full → drop.
  One slow listener can't slow anyone else.

## Codec

- Opus, 48 kHz mono, 20 ms frames (960 samples), `OPUS_APPLICATION_VOIP`.
- 32 kbit/s VBR (≈ 65–80 B/frame on speech), complexity 9, in-band FEC on. DTX off: push-to-talk already
  gates silence, and DTX's skipped frames would look like loss to the jitter buffer.
- **FEC follows measured loss.** Each client measures the loss on the streams it hears (an
  average over ~2 s of missing playout slots) and sets `OPUS_SET_PACKET_LOSS_PERC` to it every
  second (0 under 1 %, capped at 25 %); the paths in and out share the server and access links,
  so it is a fair estimate without a feedback channel. Until anything is heard it assumes 5 %.
  A clean link spends no bits on redundancy; on TCP it is always 0.
- Why (v0.4.0 → now): with 24 kbit/s and FEC sized for 10 % loss, Opus coded only **wideband
  (8 kHz audio bandwidth)** and used ~16.5 kbit/s; at 32 kbit/s it codes fullband.
  `TestOpusQuality` (synthetic speech-like signal) logs: log-spectral distance 31.9 → 6.0 dB
  clean and 32.0 → 8.0 dB at 10 % loss, segmental SNR 9.3 → 9.8 dB; encode ≈ 0.2 ms per 20 ms
  frame at complexity 9 (10 measured no better).
- Push-to-talk default: nothing is captured or sent while the key isn't held.
- The datagram limit (512 B, §14.3) leaves room for 120 ms-worth or a 128 kbit/s frame; v1 only
  ever sends 20 ms frames.

## Jitter buffer (per remote `src`)

- Frames are ordered by `seq`. Playout starts after a target delay (initial 80 ms), adapted
  between 60 and 200 ms from the observed inter-arrival jitter (RFC 3550 estimator, target ≈
  3 × jitter + 20 ms, re-evaluated at talk-spurt starts, shrinking at most one frame per spurt).
  Mid-spurt, a frame that arrives just behind the playout point grows the delay by one frame
  (one concealed slot) instead of being lost every time. The 60 ms floor (was 40) and the slow
  shrink stop a cycle where each spurt restarted too short, concealed and stretched again:
  device periods (10 ms, VPIO's ~10.7 ms) alone jitter frames by ±10 ms on each end.
- Frames buffered beyond target + 80 ms are dropped, oldest first (counted as late): a stall
  followed by a burst (Wi-Fi, and always on TCP) doesn't add its length to the delay.
- Missing frame at playout: if `seq+1` is buffered, decode its FEC data; else Opus PLC; after 5
  consecutive missing frames the stream goes silent and the buffer resets.
- Late frames (behind the playout point) are dropped. A flag bit marks the last frame of a
  talk spurt so the buffer can drain cleanly on push-to-talk release.
- Remote streams are mixed **on the client** into the playback device: summed, then a limiter
  (instant gain reduction to keep the peak under −0.2 dBFS, ~0.5 s release ramped within each
  frame) instead of hard clipping when talkers sum past full scale (`TestMixNoClipping`).

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
  own clock. This needs clocks within a minute (NTP). Rejections are counted per reason in the
  engine (`voice.Stats`); 25 or more clock-skew drops within one second raise a sticky TUI
  warning, once per participation (the ts check runs after the AEAD opens, so only authentic
  frames count).
- **Encrypted or not is decided by the client's trust state** (`E2EStatus`), never by the
  server's `Channel.e2e` flag alone: plaintext only in a channel never known as encrypted and
  not in alarm. A channel in alarm, or without a verified epoch we hold K for, refuses voice
  ("voice waits for the channel's keys"). Encryption changing mid-huddle ends the huddle.
- **Senders** seal only under a *fresh* head epoch, by the text path's rule (§13.4): its roster
  is who we'd seal to now and it boxed no device revoked since. Rotation in tc is lazy (the next
  sender rotates), so voice does what a text send does: at join, and whenever a once-a-second
  check finds the epoch stale (a member left, a device was revoked, an alarm), sending pauses,
  the client rotates via the same code path, and resumes under the new epoch. A removed member
  or a stolen device therefore hears at most ~1 s more, even if a malicious server keeps
  relaying to them.
- **Which keys a receiver accepts**: frames under the newest epoch it verified, or the one
  before it only within 10 s of seeing the head move (never on first sight, e.g. at join), so a
  rotation mid-sentence doesn't cut audio. Because frame keys are group-symmetric (anyone with
  K can speak as any roster member), an epoch is refused outright when someone outside the
  channel may hold its K: a roster user who is no longer a live member, a device revoked since
  (or a rotation we owe for our own revoke) — the text path's `revokedStale` rule. A malicious
  server refusing the rotation therefore gets silence, not an ex-member's forged voice. Within
  an accepted epoch: senders in its roster who are also current members, with `dev` a verified,
  non-revoked device of that user (trust store). The window is re-checked before cached keys
  are used. `ts` is checked after the AEAD opens. Anything else is dropped and counted;
  plaintext kinds in an e2e channel are always dropped (downgrade).

Public channels: voice is protected hop by hop (QUIC/TLS 1.3) but the **server can hear it**,
exactly like public-channel text. The TUI marks encrypted huddles with the lock it already uses.

## Limits

| What | Limit |
|---|---|
| participants per huddle | 16 (`huddle.join` → `conflict`) |
| concurrent huddles per server | 1024 (`conflict`; a user is in at most one, so this is ~users) |
| datagram size | ≤ 512 B including the header; larger dropped |
| per-session send rate | token bucket burst 100, refill 60/s (50/s nominal); excess dropped |
| `huddle.join` | counts as a write op for rate limits (§9); `huddle.leave` is exempt |
| per-receiver relay queue | 64 datagrams; full → drop newest |
| per-receiver VOICE frame queue (TCP) | 8 frames, ≤ 150 ms old; chat first; none while ≥ 64 KiB of output waits or a write is stuck 100 ms |
| client send queue | 16 frames ≤ 200 ms old (QUIC); 4 frames ≤ 60 ms old (TCP) |

## UX (TUI + herdr)

- `/huddle` — join the current channel's huddle (or start it). In a huddle, the status line
  starts with `🎧 ● 🔒#chan · bob* carol` — `●` (red) that our mic is on, `🔒` an encrypted
  channel, `*` who's talking right now; we're not listed. This head always comes first, before
  any error notice, so an open mic is never hidden. Viewing a channel whose huddle you're not in
  shows `🎧 3 in huddle — /huddle`. The sidebar marks every channel with a huddle going with a
  dim `🎧n` (n people) before the unread badge (the count, then the marker, is dropped before the
  name gets too short); without a sidebar (narrow panes) the header adds `🎧n` = huddles
  running in other channels.
- **Talk key `ctrl+t`** (bound only while in a huddle; unbound otherwise).
  Terminals don't report key-up, so: press starts talking; if key-repeats follow (you're
  holding it) talking stops 120–250 ms after the last repeat (3× the measured repeat gap) —
  push-to-talk. If no repeat arrives within 800 ms (longer than X11's 660 ms initial delay), the
  press was a tap and the mic latches open until the next tap — open mic.
- `/talk` and `/mute` open and close the mic where ctrl+t never arrives (Zellij) or key-repeat
  is off. Esc on an empty input closes an open mic.
- Not talking = muted. `/leave-huddle` leaves.
- `/huddle stats` (in a huddle) shows numbers to report from a real call, e.g.
  `udp · loss 0.4% · buf 60ms · 3 late · fec 0% · 27 kb/s`: the transport, recent loss on what we
  hear, the jitter buffer delay, frames dropped for coming too late, the loss our FEC protects
  against (UDP only), and our Opus bitrate while talking.
- A huddle that ends without `/leave-huddle` says why, once: `🎧 #chan ended: connection lost`
  (or `you were dropped`: membership, revoke, the server's liveness check; or `encryption
  changed`), plus `· /huddle to rejoin` only when that would rejoin it (online, viewing that
  channel, no ring up); `🎧 #chan moved to your other device`. It never replaces another error
  notice and stays quiet when tc stopped (revoked, disabled), whose banner says more.
- Builds without voice: `/huddle` says "no voice: tc update --voice"
  (switches to the voice build in place, same signed-release path); the roster indicator
  still works.
- Repeated clock-skew drops (25+ authentic frames a second outside ±60 s) show a sticky
  "clock skew: some huddle audio dropped — check NTP on both machines", once per participation.
- herdr: while you're in a huddle the pane's metadata title starts `tc · 🎧` (`tc · 🎧●` with an
  open mic), followed by the unread summary, so you can see which pane holds the call.

## Echo

Field report (v0.4.0, macOS ↔ Linux): the Linux user heard himself. The relay never returns a
sender's frames and a client never plays its own capture, so the cause is acoustic: the Mac's
speakers play his voice, the Mac's open mic picks it up, and the Mac sends it back. Headphones
fix it; for everyone else tc picks one of three modes per huddle (`/huddle` in your huddle
shows which):

| Mode | When | What |
|---|---|---|
| `system` | macOS (the voice-processing unit starts); Linux with an echo-cancel source **and** sink loaded | The OS cancels echo. On macOS: `kAudioUnitSubType_VoiceProcessingIO` (Apple's AEC + noise suppression; its AGC is turned off because it pumps the level and dulls the voice, `TC_VOICE_AGC=on` keeps it), via a small cgo shim (`vpio_darwin.c`) because miniaudio only opens the plain HAL unit. The unit runs our 48 kHz int16 mono format on both sides, so tc adds no resampling. Other apps' audio is ducked as little as macOS 14+ allows. |
| `gate` | no platform canceller (most Linux setups), or forced | Half-duplex gate in the engine: while remote audio plays (above ≈ −42 dBFS and 6 dB above the far end's learned noise floor, or above ≈ −26 dBFS regardless) and for 200 ms after, an **open** mic is muted (2.5 ms attack, 20 ms release ramps). A ctrl+t press is gateable until a key-repeat proves it held; a held key is never gated: you are deliberately talking (barge-in = hold ctrl+t). |
| `off` | `TC_VOICE_AEC=off`, or `system` asked for and unavailable | Nothing. |

`TC_VOICE_AEC=auto|system|gate|off` (default `auto` = system if available, else gate; `system`
= system or nothing, for testing; an unknown value fails `/huddle` with a message). Without
system echo cancellation the join notice starts with `no echo cancellation: use headphones`, once
per run.

- **Linux: enabling system echo cancellation.** tc never loads modules or changes audio settings;
  it looks (on the PulseAudio API, which PipeWire serves through pipewire-pulse) for a source and
  sink named like the modules' defaults (`echo-cancel-source`/`-sink`, `<master>.echo-cancel`,
  descriptions `Echo-Cancel …` or `… (echo cancelled with …)`; monitors and hardware that merely says
  "Echo Cancelling" excluded) and opens that pair; with several cancellers only a same-stem pair, else
  the gate; both are needed, since the canceller only hears the far end through its sink.
  To enable one:
  - PipeWire: `~/.config/pipewire/pipewire.conf.d/echo-cancel.conf` with
    `context.modules = [ { name = libpipewire-module-echo-cancel args = { library.name = aec/libspa-aec-webrtc } } ]`,
    then `systemctl --user restart pipewire pipewire-pulse`.
  - PulseAudio: `pactl load-module module-echo-cancel aec_method=webrtc` (for every login: add
    `load-module module-echo-cancel aec_method=webrtc` to `~/.config/pulse/default.pa`).
- **The gate's limits.** It is half-duplex: in open mic you aren't heard while others talk (hold
  ctrl+t to talk over them), and the far end's background noise above the threshold keeps your
  open mic shut only until its noise floor is learned (a few seconds: the floor follows quieter
  frames at once and rises ≈ 3 dB/s, so pauses between words keep it at the noise). The floor is
  capped at ≈ −30 dBFS and anything above ≈ −26 dBFS always counts, so loud continuous audio
  (music, a talker who never pauses) can't train it open; continuous far audio between ≈ −36 and
  −26 dBFS can, after a few seconds. Far speech less than 6 dB over steady far noise doesn't count.
  Bluetooth output adds 150–250 ms of latency the 200 ms hangover may not cover.
  Our own cues (join/leave, mic clicks) don't trigger it. It runs on the audio thread with no
  allocation (`TestGateNoAllocs`).
- **Why not speexdsp's echo canceller.** Considered (BSD, small, could be built static like
  libopus). Not added: macOS already gets Apple's canceller, PipeWire/PulseAudio's WebRTC
  canceller is better than speex MDF on Linux, and MDF needs tuning (tail length, drift between
  input and output clocks, nonlinear laptop speakers) that we can't validate without many real
  setups; a mis-tuned canceller leaks echo *and* garbles double talk. The gate is predictable and
  costs nothing. Revisit if Linux users without an echo-cancel module report echo in practice.
- **Never a monitor as the microphone.** A PulseAudio/PipeWire "Monitor of …" source records the
  speakers; as tc's input it would send everyone's voice straight back to them (no digital
  self-echo exists otherwise: the relay skips the sender and the engine never plays our own `src`).
  If the default input is a monitor (by name `….monitor`, or a description "Monitor of …" and its
  common translations), tc uses the first real microphone instead and says so on join and in
  `/huddle`; with no real microphone it joins listen-only (captures silence). `TC_VOICE_INPUT=<text>`
  picks the first non-monitor input whose name or description contains `<text>`
  (case-insensitive; a monitor is never picked, even by name); it skips the Linux echo-cancel pair
  and, on macOS, the voice-processing unit (which only opens the default devices), so the gate
  applies.
- Platform echo cancellation lives entirely on the client; the protocol is unchanged.
- The VPIO shim passes only the microphone samples that arrived each period. It used to pad a
  short read with zeros, which cut gaps into the voice (crackle) whenever input and output
  periods differ (a Bluetooth headset mic, a resampled device).
- Devices not at 48 kHz go through miniaudio's linear resampler with its steepest low-pass
  (order 8; the default is 4).

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
- Releases ship a second artifact per target, `tc-voice_<os>_<arch>.tar.gz` (binary named
  `tc`), built by `scripts/build-voice.sh` on native runners (macos-14 builds darwin arm64 and,
  with `clang -arch x86_64`, amd64; ubuntu-22.04 and ubuntu-22.04-arm build Linux). libopus
  1.5.2 is built from the pinned, sha256-checked xiph.org tarball and linked **statically**; the
  script fails if the binary links libopus dynamically. Runtime dependencies: macOS — system
  frameworks only; Linux — glibc ≥ 2.35 (libc/libm/libdl/libpthread) plus the audio stack
  miniaudio `dlopen`s (PulseAudio, PipeWire-pulse or ALSA). It is listed in the same SSHSIG-
  signed `checksums.txt`. `install.sh --voice` (or `TC_VOICE=1`) installs it; `tc update`
  keeps the flavor that's running (`voice.Available()`, fixed by the build tag), `tc update
  --voice` switches a plain install to it (after checking the new binary starts here), and
  `tc version` prints `(voice)` for it. CI builds and tests `-tags voice` on Ubuntu and macOS
  and runs the static release build for the runner's target on every push.

## Threat model

Attackers: a malicious or compromised server; a network attacker; a malicious channel member;
a removed member; a hostile client spamming datagrams.

| Threat | Mitigation |
|---|---|
| Network attacker reads/injects audio | QUIC (TLS 1.3, pinned SPKI) on every hop. |
| Server listens to an encrypted channel's huddle | Frames are ChaCha20-Poly1305 under keys from `K_epoch`, which the server never has. |
| Server alters, splices or relabels frames | Header is AEAD AD; per-sender key binds `dev`+`stream`; `dev` must be a verified device of the named user, so relabeling breaks decryption. |
| Server replays frames within a session | 64-frame replay window per `src`, kept for the whole participation (a roster that drops and re-adds a `src` can't reset it). |
| Server replays an old recording later | `ts` in the AD, ±60 s acceptance; window per stream. Within 60 s of recording a replay into a *new* stream is possible (accepted risk, documented). |
| Server downgrades an e2e huddle to plaintext | The client decides encryption from its sticky trust state, not the server's flag; refuses in alarm; ends the huddle if encryption changes. Receivers drop plaintext kinds in e2e channels. |
| Removed member / stolen (revoked) device keeps listening | Senders only seal under a fresh epoch and rotate first when it's stale (checked at join and every second), so the old `K` decrypts at most ~1 s more (plus ≤ 2 s of cached keys). Honest servers also drop them at once and discard their queued datagrams, to and from them. |
| Ex-member / revoked device injects audio under an old epoch | Receivers refuse any epoch whose K someone outside the channel may hold (roster user no longer a member, device revoked since, rotation owed), whoever the frame claims to be from, so a server that blocks the rotation gets silence. The previous epoch is accepted only 10 s after the head moves, never on first sight, and only if it passes the same test. |
| Alarm raised mid-huddle | Sending pauses while the channel isn't `E2EOK`. |
| **Malicious member impersonates another member's voice** | **Not prevented in v1.** Any roster member can derive any sender's `sk` (group-symmetric, like SFrame with a shared key). Text messages are signed per message; per-frame Ed25519 would add 64 B × 50/s = 25.6 kbit/s. Future: per-sender keys sealed to the roster with signed announcements. |
| Metadata | The server sees who is in which huddle, when, and frame sizes/timing (talk patterns; VBR sizes can leak some content). VBR leaks more than CBR; CBR is a possible opt-in later. |
| Datagram flood / amplification | Only huddle participants can send; per-session token bucket; fixed max size; relay fan-out ≤ 15; bounded per-receiver queues with drop. A client can't make the server send to non-participants. |
| Hostile datagram parser input | Fixed-layout header parsed with bounds checks, fuzzed (`FuzzParseDatagram`); the server never touches payloads. |
| Audio-device privacy | Mic is opened only while in a huddle and (push-to-talk) only captured while talking; leaving closes the device. |
