# Can tc work without a hosted server?

Research report, 2026-10-17. Read-only research; no repo changes. Sources are at the end. Where a number is
my estimate and not measured or cited, it says so.

## TL;DR verdict

**Yes, but only once you decide what "no hosted server" means.** Every option that delivers messages while
the sender's and the reader's laptops are asleep at different times needs *something always on*. That can
be a server you run (today), a service someone else runs (an object-storage bucket, Tailscale's coordination
and DERP relays, iroh's public relays), or a teammate's machine that stays on. Pure laptop-to-laptop P2P
can't deliver to someone who is offline. It also still needs a rendezvous/relay server to get through NAT
(go-libp2p hole punching succeeds ~70% of the time, ~83% on the common NAT type; the rest is relayed).

Ranked for tc (team under 15, Mac + Linux terminals, small/fast/one-step, E2E already built):

1. **Recommended now: "host anywhere".** Keep the protocol and the server as they are, but make `tc server`
   trivial to run on *any* member's always-on box (desktop, Mac mini, Pi, NAS, home server). Add an
   optional embedded tailnet (tsnet) so it's reachable with no port forwarding and no public IP. Add
   signed host migration/backup so the host is replaceable. About 1–3 weeks. Almost no risk to the
   message guarantees.
2. **Recommended if the owner wants "nothing to run at all": "the bucket is the server".** Replace the
   server process with an E2E-encrypted, append-only event log in a free object-storage bucket (Cloudflare
   R2 / S3 / B2). **Conditional put-if-absent keeps a global `seq`.** Every client checks every event
   itself, which the malicious-server design mostly does already. Messages arrive after the next poll
   (about 2–5 s, not instant), with optional direct P2P pushes for speed. About 6–10 weeks. It replaces
   the server's authority with signed membership/admin operations, which also closes today's biggest E2E
   gap (the roster is the server's word).
3. **Not recommended: full P2P mesh** (libp2p / iroh / Holepunch / CRDT logs). It's a near-total rewrite
   of sync, ordering and membership. It still needs relays and an always-on peer for offline delivery.
   Binary-size and maturity costs are high in Go (iroh has only community, cgo, Linux-only Go bindings;
   a minimal libp2p program is reportedly 20–30 MB, against our 20 MB cap with tc already at 16 MB).

## What the server does today (and what each replacement must cover)

From README.md, docs/PROTOCOL.md §2–§9, §12, §13, docs/E2E.md:

| # | Server job | Hard to replace without a server? |
|---|---|---|
| 1 | **Reachability**: a stable address + pinned TLS key in the invite (`tc://host:7443#pin`) | Medium: needs a relay, tunnel or tailnet |
| 2 | **Store-and-forward** while laptops sleep; snapshot + replay sync | **Hard**: needs always-on storage |
| 3 | **Global total order**: one `seq` across an append-only log, gap-tolerant replay, idempotent apply | Hard without one writer, unless storage has compare-and-set |
| 4 | **Authority**: auth, invites, admin, channel membership-at-seq visibility, permission checks, rate limits, `stale_epoch` compare-and-set on key rotation, dedupe of idempotent sends | Hard: becomes signed ops that every client checks |
| 5 | **Rewriting the log** on edit/delete so replay never brings back deleted text | Medium |
| 6 | **Webhooks / bots** (inbound HTTPS on :7443) | Needs a public endpoint, so a host or a third-party relay |
| 7 | **Reminders** fire while tc is closed (server scheduler) | Medium: fire on any online device of the owner |
| 8 | **Search** of plaintext channels (e2e search is already client-side FTS) | Easy: local FTS |
| 9 | **E2E plumbing**: publishes device certs, stores epoch bundles, relays `key.need`/`key.share` | Easy: just storage. Clients already verify everything |

Key observation: **docs/E2E.md already treats the server as untrusted for confidentiality and integrity**.
Clients verify device certs against SSH keys, check epoch rotations, verify signed messages and treat
availability as not defended. What clients still take on the server's word is **ordering, membership
roster, channel encrypted-flag, admin/invite authority and timestamps**. Any serverless design has to
turn exactly those into signed, client-checked state. That is the bulk of the work in options 2 and 3.

## Options evaluated

### A. Pure P2P meshes / NAT traversal

- **go-libp2p** (DCUtR hole punching, Circuit Relay v2, AutoNAT, gossipsub). Native Go. Measured hole-punch
  success is **70% ± 7%** across 4.4M attempts (IMC '26), **82.9% for port-restricted cone NATs**, and
  around 40% for symmetric NATs. About 36% of NAT'd peers never even get the relay reservation needed to
  try. You must run or borrow relays and bootstrap peers. **Offline delivery: none built in.** Ordering:
  none, so you'd build a log or CRDT on top. Binary: "even the simplest libp2p Go program is roughly
  20–30 MB" (anecdotal), which breaks our 20 MB budget. It brings a large dependency tree. Mature
  (IPFS, Filecoin, Ethereum).
- **iroh** (n0; QUIC with hole punching, relays, iroh-gossip, iroh-docs). n0 says about 9/10 connections
  go direct (vendor claim; ProbeLab notes no study backs ">90%" claims). It runs free, rate-limited
  public relays (no SLA), and you can self-host `iroh-relay`. **Rust-only in practice**: Go is a
  *community* binding (`iroh-go`) that is experimental, **cgo-required, Linux x86_64/aarch64 musl
  only, no macOS**. That rules it out for tc's pure-Go, `CGO_ENABLED=0`, Mac + Linux build. Delta Chat
  uses it for realtime only, with relays hosted on chatmail servers, so it's still infrastructure.
- **Tailscale `tsnet`** (embed a whole tailnet node in the Go binary; userspace netstack). Pure Go. It has
  `Listen`/`ListenPacket` (UDP works, so our QUIC works unchanged inside the tailnet). WireGuard
  everywhere, direct paths in "most environments", DERP relay fallback. **It needs a coordination
  server**: Tailscale's SaaS (`ControlURL` defaults to it; the free Personal plan is per-user, reportedly
  6 users since April 2026, earlier 3, so a 15-person team likely needs a paid plan or Headscale). Self-
  hosting **Headscale** is "hosting a server" again, though a tiny one. **Offline delivery: none**; it
  only makes a host reachable. Binary-size cost is **not measured**. I estimate +10–15 MB (gVisor
  netstack + magicsock), which needs a build tag or a separate `tc-mesh` binary. Measure before deciding.
  Funnel (public ingress) is TCP-only on ports 443/8443/10000 and terminates TLS at the node, which
  doesn't fit our pinned-TLS QUIC. Tailnet-only access is the right shape.
- **Plain WireGuard / Yggdrasil**. WireGuard needs one reachable endpoint (a server) or a coordinator.
  Yggdrasil is an experimental overlay that still needs public peers. Neither solves offline delivery.
- **Hyperswarm / Holepunch / Pear / Keet**. A DHT plus hole punching, Autobase multi-writer logs. JS/Bare
  only, no Go. Notably **Keet solves offline delivery with "blind peers"**: always-on peers that store a
  room's encrypted data without the key. That's exactly our "untrusted server", by another name.
- **Matrix P2P (Pinecone + Dendrite)**. Experimental, not production. Pinecone's last push was early 2025.
  Not viable.

### B. Local-first replicated logs / messaging systems

- **Automerge / Yjs**: CRDT documents. Go support is weak (`automerge-go` wraps the Rust core via cgo; Yjs
  has no serious Go port). Chat is an append-mostly log, so a full CRDT is overkill. A per-author signed
  log plus a deterministic merge order is simpler and is what we'd build anyway.
- **Willow / Earthstar, p2panda**: good ideas (capabilities, per-author logs, causal order). p2panda is
  Rust, built on iroh, and says its APIs are "not yet stable for production" before 1.0. It has a
  group-encryption crate with post-compromise security (PCS). Useful as **design references** for signed
  membership and per-author logs, not as dependencies.
- **Secure Scuttlebutt**: per-author append-only signed feeds plus "pubs" (always-on peers) for offline
  delivery. Largely dormant. The lesson again: offline delivery came from always-on pubs.
- **Briar / Bramble, Cwtch**: Tor onion services. Briar delivers only when both are online (or via
  "mailbox", an always-on device). Cwtch uses untrusted relay servers. Slow, heavy (Tor), not "blazing
  fast".
- **Delta Chat**: email (IMAP/SMTP) as the store-and-forward transport, Autocrypt E2E, chatmail relays.
  It's "serverless" only in that you use someone's mail server. Interesting but a huge protocol shift.
- **SimpleX**: relays that hold per-queue messages with no user identities. Still relay servers (theirs
  or self-hosted). Haskell.
- **Nostr**: signed events on dumb relays. Same pattern: always-on relays (free public ones exist but are
  unreliable, with spam limits and retention policies) plus client-side verification. Our E2E layer
  would sit on top. Public relays leak metadata to strangers.

### C. "Serverless but not P2P"

- **A teammate's machine as host + tunnel.** Works today with zero protocol change. Reachability: port
  forwarding, tsnet/Tailscale (UDP OK), or Cloudflare Tunnel (TCP/HTTP only, no UDP, so we'd be on our
  TLS/TCP fallback, and quick tunnels change URL each run). **Laptop sleep is the real problem**: if the
  host is a laptop, the team is offline when it sleeps (outboxes keep messages, nothing is lost, but
  nothing is delivered). A desktop, Mac mini, Pi or NAS fixes it.
- **Rotating host election** (whoever's online hosts). This breaks the single-writer global `seq`: two
  partitions each elect a host and you get split-brain logs that need merging. That means CRDT-style
  ordering, so it's effectively option B's work without B's always-on storage. Not recommended as the
  primary design.
- **Object storage as an encrypted mailbox (S3/R2/B2).** Always on, cheap or free (R2 free tier: 10
  GB-month, 1M Class A writes, 10M Class B reads per month, no egress fees). S3 (since 2024) and R2 support
  **conditional writes** (`If-None-Match: *` put-if-absent). That gives a compare-and-set primitive, so a
  **single global log with `seq` = object key is possible** without a server. Verify R2's exact
  conditional-PUT semantics and billing in a spike (not confirmed from docs in this research). No push:
  clients poll, so latency is about the poll interval. Credentials are shared or per-member, and a
  member with write credentials can delete objects (availability, not confidentiality).
- **GitHub repo as transport**: per-author files or commits, pushed and pulled. Rate limits (5k req/h per
  token), latency of seconds to minutes, ToS gray area for chat traffic, history is permanent (bad for
  deletes). Fine for an audit log, poor for chat.

## Comparison table

| Option | Offline delivery | Ordering | Membership / removal + our E2E | Needs infra? | NAT | Laptop sleep | Go / size / CPU | Maturity | Rewrite of tc |
|---|---|---|---|---|---|---|---|---|---|
| **Today** (droplet) | Server store | Global `seq` | Server roster + epochs (roster trusted) | Our VPS ($) | n/a (public IP) | Fine | 16 MB, pure Go | Shipped | — |
| **A1 Host anywhere** (member box + port fwd) | Host store | Global `seq` | Unchanged | Member's always-on box | Port forward / public IP | Fine if host is a desktop; dead if host sleeps | +0 | Shipped | ~5% |
| **A2 Host + embedded tsnet** | Host store | Global `seq` | Unchanged (+tailnet ACL as an extra layer) | Tailscale SaaS or Headscale + DERP | Direct "most", DERP fallback (always connects) | Same as A1 | Pure Go; est. +10–15 MB (unmeasured) | Tailscale: high | ~10% |
| **B Bucket is the server** (R2/S3 log + CAS) | Bucket (always on) | Global `seq` via put-if-absent | Signed membership/admin ops checked by clients; epochs unchanged; removal = rotate + revoke bucket creds | Free-tier bucket (3rd party) | None needed (HTTPS out) | Fine: nobody needs to be on | Pure Go S3 client (small); polling CPU/net low | Storage: high; design: new | ~35–45% (server logic moves into a shared, deterministic validator) |
| **B+ Bucket + P2P push** | Bucket | Global `seq` | Same as B | Bucket + relay (tsnet/DERP or none on LAN) | Direct or relay | Fine | +tsnet size if used | Mixed | B + ~2 wk |
| **go-libp2p mesh + log** | None (needs always-on peer) | Build HLC/causal | Build signed membership | Relays + bootstrap | ~70% punch, else relay | Bad | Est. 20–30 MB, heavy deps | High (lib) / new (app) | 60–80% |
| **iroh** | None (docs/gossip need online peers) | iroh-docs (range-based set reconciliation) | Build | n0 public relays or self-host | Claims ~90% direct | Bad | **cgo, Linux-only Go binding, no macOS** | Lib good; Go binding experimental | 60–80% + FFI |
| **Holepunch/Keet-style** | Blind peers (= server) | Autobase | Build | DHT bootstrap + blind peers | DHT punching | Blind peer covers it | No Go | JS only | 100% |
| **Nostr relays** | Public/self relays | Per-event timestamps | Build on top | Relays (3rd party) | n/a | Fine | Pure Go libs exist | Medium | ~50% |
| **Delta Chat / email** | Mail server | Weak (mail order) | Autocrypt groups | Mail server | n/a | Fine | Big change | High (DC) | ~90% |
| **Rotating host election** | Only while some host is up | Split-brain, needs merge | Hard | None / tunnel | Needs tunnel | Partial | Small | New | 50%+ |
| **GitHub repo transport** | GitHub | Commit order | Build | GitHub | n/a | Fine | Small | Hacky | ~40% |

## Recommended architecture 1: "Host anywhere" (do this first)

Goal: no droplet, no ops knowledge. Any member with an always-on machine hosts. Zero protocol change for
messages.

1. **`tc host` UX** (wraps `tc server`). Runs as a launchd (macOS) or systemd `--user` service on the
   member's machine, prints the admin invite. Same binary, same SQLite, same pinned TLS. On a laptop it
   warns that delivery stops while the machine sleeps, and can use `caffeinate -i` / systemd-inhibit
   while plugged in.
2. **Reachability, pick per team:**
   - (a) public IP / router port forward / IPv6: nothing new.
   - (b) **embedded tsnet, build-tagged** (`-tags mesh`): the server and every client join a tailnet with
     an auth key carried in the invite (`tc://100.x.y.z:7443#pin&ts=<authkey>` or a separate `tskey`).
     Our pinned TLS + QUIC run unchanged over WireGuard (tsnet `ListenPacket` supports UDP). It needs a
     Tailscale account (paid above the free user cap) or a Headscale instance. **Measure the binary
     delta first.** If tc + tsnet is over 20 MB, ship it as an optional variant or keep (a) as default.
   - (c) document Cloudflare Tunnel (TCP fallback path only, no QUIC) as a no-account-ish option.
3. **Host migration and backup** (this is what makes the host replaceable, not a pet):
   `tc server export` produces an encrypted, signed snapshot of the SQLite DB (age to admin keys). Then
   `tc server import` on a new box. The **TLS key and `server_id` move with it**, so invites, pins and
   the E2E trust stores stay valid, and clients only need the new address (a signed "host moved" record
   published before shutdown, or `tc join --rehost`). Optional periodic encrypted backup to R2/S3.
4. Webhooks, reminders, search and E2E all keep working because the server still exists.

Effort: (1)+(3) about 1–1.5 weeks. (2b) about 1–1.5 weeks more (tsnet integration, invite format,
build tag, live NAT tests). Protocol: invite format only (PROTOCOL.md §4.1), plus protocol-guardian
review. Risk: low. The battle-tested server is unchanged.

## Recommended architecture 2: "The bucket is the server" (if the owner wants nothing running)

Goal: no process anyone runs. An E2E-encrypted log in a free bucket. Keep tc's event model, `seq`,
outbox, epochs and TUI.

**Storage layout (one bucket per team):**
```
log/00000000000000000123      one event per object (or small batches), key = seq (zero-padded)
head                          advisory hint for the latest seq (not authoritative)
snap/<seq>                    periodic signed snapshots for fast cold start
blobs/<hash>                  snippets/files
```
- **Append = put-if-absent on `log/<head+1>`.** A 412 means someone else won that seq: re-read and retry
  at the next one. That gives the same global total order as today, and `sync since` becomes "list after
  `log/<since>`". At under 15 people contention is negligible. Idempotency: the client nonce is inside
  the signed event, and readers dedupe by (device, nonce), as the server does today.
- **Every event is signed by the author's device key and encrypted.** Private channels and DMs use the
  existing epoch keys (unchanged §13 formats). Public channels and metadata use a **team epoch key**,
  rotated on member removal, so the bucket provider sees only ciphertext, sizes and timing.
- **No server authority, so a deterministic validator runs in every client.** Each client replays the log
  through the same pure function (`state, event -> state | reject`). That's today's server handler
  rules (permissions, membership, admin ops, invite redemption, `stale_epoch` compare-and-set, edit/
  delete authorship), moved into a new package shared by client and server (e.g. `internal/ledger`, so
  `client` and `server` still don't import each other). Invalid events are skipped by everyone the
  same way. Seq gives a total order, so there are no merge conflicts. This is the "malicious server"
  stance taken to its end: the bucket can drop or withhold (as today) but can't forge, reorder silently
  (seq is in the key, and events link to the previous seq/hash), or add members.
- **Membership/admin become signed ops** (`chan.join` signed by an admin or inviter, `chan.remove`, admin
  grants, invites as signed capabilities). This **closes today's E2E gaps** ("server-asserted roster",
  "encrypted flag is the server's word") as a side effect.
- **Edits/deletes:** the author overwrites their own `msg.new` object with a tombstone (keeps the "replay
  never resurrects" rule), or uses a crypto-erasure key per message if overwrite rights can't be scoped.
- **Removal:** rotate the team and channel epochs (existing machinery) and revoke the member's bucket
  credentials. R2 API tokens are per bucket. Per-prefix write scoping needs S3 IAM policies, so on R2 a
  member can delete objects they shouldn't. Mitigate with object versioning/lock and client-side
  replicas (every client already caches the log).
- **Latency:** clients poll `LIST log/ start-after=<last>` every ~2 s while the TUI is focused and ~30 s in
  the background. Cost check (my arithmetic): 15 clients × 1 poll/2 s × 8 h × 22 days ≈ 4.8M Class B
  per month, inside R2's 10M free. Background polling adds more, so tune it. Writes: thousands per day,
  far below 1M Class A per month. **Optional fast path:** when peers are reachable (same LAN, or tsnet),
  send a direct "new seq N" nudge so others fetch immediately. That gives sub-second delivery without
  relying on the nudge for correctness.
- **What's lost or changed:** inbound webhooks need a public endpoint, so they're dropped, or kept via a
  tiny optional bot host or a Cloudflare Worker. Reminders fire from the owner's own device when it's
  next online (wording change: "on time if any of your devices is on"). Server-side search goes away
  (local FTS already exists). Presence/typing need a live channel (P2P nudge or nothing). Server time
  is replaced by signed author time with the existing skew rules.
- **Onboarding** stays one step: the invite carries the bucket endpoint, a per-member scoped token (or a
  wrapped one) and the team key fingerprint. The creator needs a Cloudflare/S3 account once. That's
  the new "admin, once" step, replacing "rent a droplet".

**Migration path from today:**
1. Extract the server's state machine into a pure `internal/ledger` reducer that the existing server
   uses unchanged. Property-test that server and ledger give identical results on recorded event logs.
2. Add a `client.Transport` interface with two implementations: the existing QUIC/TCP connection, and a
   `bucket` backend (append via CAS, list/poll, snapshot). The outbox, cache and apply stay.
3. Add the signed membership/admin ops (PROTOCOL.md + E2E.md changes, protocol-guardian +
   security-reviewer). Ship them first on the *server* path too (they close E2E gaps there).
4. Add `tc server export --to bucket` to convert an existing team's log (re-signing what we can and
   marking older history "legacy, server-attested").
5. Breaker/chaos: concurrent appends racing on the same seq, partial uploads, LIST eventual-
   consistency, credential revocation mid-write, a malicious member deleting objects, clock skew.

Effort: about 6–10 weeks with tc's review bar (ledger extraction 1.5–2 wk, bucket transport 1.5 wk,
signed membership/admin + spec 2–3 wk, migration + chaos/adversarial tests 1.5–2 wk, P2P nudge optional
+1–2 wk).

## Why not a full P2P mesh

- It doesn't remove infrastructure: libp2p needs relays (and 30% of punches fail), iroh needs relays,
  tsnet needs a coordinator plus DERP, and Holepunch needs a DHT plus blind peers for offline delivery.
- It doesn't solve offline delivery: teammates on laptops are rarely all online. Every production P2P
  messenger adds an always-on store (Keet blind peers, SSB pubs, Briar mailbox, Cwtch servers, Delta
  Chat mail servers).
- It costs the global `seq`. That means causal/HLC ordering, CRDT merges, membership races under
  partition (a removal concurrent with a send), and rewriting the chaos-tested sync and E2E rotation
  logic. That's months of work and the main source of message-loss/duplication bugs, against principle #2.
- The Go ecosystem is thin: no maintained pure-Go iroh, Automerge or Yjs. libp2p fits but is big and
  heavy relative to the 20 MB / 30 MB RSS budgets.

## Risks

- **Binary/RSS budget:** tsnet and libp2p could each push tc past 20 MB (tc is 16 MB today). Measure.
  Use a build tag or a separate artifact.
- **Third-party dependence:** bucket and Tailscale are still someone's servers (outage, ToS, pricing
  changes; Tailscale's free tier changed in 2026). Mitigation: the bucket backend speaks plain S3, so
  R2, B2, AWS, MinIO or Garage on a NAS are interchangeable.
- **Bucket semantics:** conditional-write and LIST consistency differ by provider. Must be spiked and
  chaos-tested per provider. Only support providers that pass.
- **Write-capable members can damage availability** (delete objects) on providers without prefix
  scoping. Use versioning/object lock plus client replicas.
- **Latency regression** (polling) against "blazing fast". The P2P nudge mitigates it when peers are
  reachable.
- **Security model shift:** moving authority into clients needs a careful spec and a security review.
  Upside: it closes known E2E gaps.
- **Laptop hosts** (architecture 1) silently stop delivery when asleep. UX must make this obvious.

## What the owner should decide

1. **What does "without a hosted server" mean to you?**
   (a) "No VPS to rent/patch, but a teammate's always-on box is fine" → Architecture 1 (1–3 wk).
   (b) "Nothing anyone runs, but a free third-party bucket is fine" → Architecture 2 (6–10 wk).
   (c) "No third party at all, ever" → only possible if some member's machine is always on (that's 1),
   or with LAN-only/when-both-online delivery. There's no free lunch here.
2. **Is 2–5 s delivery latency acceptable** when peers aren't directly reachable (Architecture 2)?
3. **Are webhooks/bots a must-have?** If yes, Architecture 2 needs an optional host or a Worker for them.
4. **Is a Tailscale account (or Headscale) acceptable** for zero-config reachability, and is a
   build-tagged larger binary OK?
5. **Should the signed membership/admin ops be done anyway?** They're needed by Architecture 2 and
   independently close the top remaining E2E threat-model gap. They're a good first step whatever is
   chosen.

Suggested sequencing: ship Architecture 1 (export/import + `tc host`) now, then measure the tsnet binary
size. Do the signed-membership work as an E2E hardening step. Revisit Architecture 2 only if running
any box is still a dealbreaker after that.

## Sources

- ProbeLab, "Can libp2p punch through NATs?" (per-NAT-class rates, relay reservation bottleneck): https://probelab.io/blog/can-libp2p-punch-through-nats/
- Trautwein et al., "Challenging Tribal Knowledge – Large Scale Measurement Campaign on Decentralized NAT Traversal" (70% ± 7.1%, 4.4M attempts): https://arxiv.org/html/2510.27500v1 · https://probelab.io/publications/large-scale-measurement-of-nat-traversal-for-the-decentralized-web-a-case-study-of-dcutr-in-ipfs/
- go-libp2p relay daemon (archived; relay v2 built into go-libp2p): https://github.com/libp2p/go-libp2p-relay-daemon
- libp2p Go binary size anecdote (20–30 MB): https://git.coopcloud.tech/decentral1se/pear
- iroh relays (public relays, ~9/10 direct, self-hosting): https://docs.iroh.computer/concepts/relays · https://docs.iroh.computer/iroh-services/relays/public · https://docs.iroh.computer/add-a-relay
- iroh Go bindings (community, experimental, cgo, Linux musl only): https://docs.iroh.computer/languages/go · https://git.coopcloud.tech/decentral1se/iroh-go · https://github.com/n0-computer/iroh-ffi
- tsnet package (ControlURL, ListenPacket, ListenFunnel): https://pkg.go.dev/tailscale.com/tsnet · https://tailscale.com/docs/features/tsnet.md
- Tailscale connection types and DERP: https://tailscale.com/docs/reference/connection-types · https://tailscale.com/kb/1232/derp-servers
- Tailscale Funnel (ports 443/8443/10000, TCP, TLS terminated on node): https://tailscale.com/kb/1223/funnel
- Tailscale free-tier changes 2026 (secondary): https://www.ssdnodes.com/learn/is-tailscale-free-plan-limits · https://thisisanitsupportgroup.beehiiv.com/p/tailscale-s-free-tier-just-doubled-if-you-ve-been-putting-it-off-today-s-the-morning
- Headscale: https://github.com/juanfont/headscale
- Small Tailscale builds: https://tailscale.com/kb/1207/small-tailscale/
- Cloudflare R2 pricing / free tier: https://developers.cloudflare.com/r2/pricing/
- Cloudflare quick tunnels (secondary; no UDP): https://stackharbor.com/en/knowledge-base/cf-quick-tunnel-trycloudflare-guide/ · https://pinggy.io/blog/best_cloudflare_tunnel_alternatives/
- Keet / Pear blind peering (always-on encrypted peers): https://docs.pears.com/how-to/blind-peering/add-blind-peering-to-a-chat-app/
- Matrix Pinecone (experimental): https://matrix.org/blog/2021/05/06/introducing-the-pinecone-overlay-network/ · https://awesome.ecosyste.ms/projects/github.com%2Fmatrix-org%2Fpinecone
- p2panda (pre-1.0, iroh-based, group encryption crate): https://docs.rs/p2panda · https://github.com/jelmer/p2panda
- Delta Chat realtime P2P via iroh, relays on chatmail servers: https://alternativeto.net/news/2024/11/delta-chat-introduces-real-time-p2p-networking-with-hole-punching-and-forward-secret-e2ee · https://fosdem.org/schedule/event/fosdem-2025-5853-delta-chat-from-e-mail-messaging-to-peer-to-peer-realtime-networking
- Automerge Go status: https://pkg.go.dev/github.com/savaki/automerge (no maintained pure-Go port found)
- From general knowledge, not re-verified in this session: S3 conditional writes (`If-None-Match`, AWS 2024); SimpleX (https://simplex.chat), Nostr (https://nostr.com), SSB (https://scuttlebutt.nz), Briar (https://briarproject.org), Cwtch (https://cwtch.im), Yggdrasil (https://yggdrasil-network.github.io), Willow (https://willowprotocol.org). **Spike R2's conditional PUT before committing to Architecture 2.**
