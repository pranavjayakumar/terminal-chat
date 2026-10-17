# tc: product review and roadmap proposal (v0.5 / M6, M7+)

PM report, 2026-10-17. Based on the docs (README, ONBOARDING, ROADMAP, TUI, STATUS, E2E, VOICE draft on
`feat/voice-huddles`), the code in `cmd/tc` and `internal/tui`, a hands-on run, and competitor research.
For the hands-on run I built the binary from the `chore/docs-cleanup` checkout, started a throwaway server
on 127.0.0.1:17999, used fresh `TC_HOME`/`HOME` dirs, set `SSH_AUTH_SOCK=` and `TC_NO_UPDATE_CHECK=1`,
and drove the TUI in tmux at 100, 80 and 60 columns.

---

## (a) Positioning

**tc is the group chat for a small team of terminal people who want their own place, private and
permanent, outside work.** It is one binary you already trust. You log in with the SSH key you already
have, it lives in the herdr pane you already have open, and your DMs and private channels are end-to-end
encrypted, so the person running the server can't read them. You keep your history forever, nobody counts
seats, and no vendor can delete your messages after 90 days. It is not "Slack in a terminal". It is
"our own room": calm by default, with pipes, diffs, `tc notify` and webhooks treated as first-class
because the users are developers and their agents. The pitch fits in three words: **yours, private,
instant**.

The competitive wedge is real, and every alternative misses at least one leg:

- **Slack Free** limits history to 90 days and has deleted content older than a year since August 2024
  ([Zulip on the exodus](https://blog.zulip.com/2022/08/26/why-slacks-free-plan-change-is-causing-an-exodus/),
  [AlternativeTo](https://alternativeto.net/news/2024/8/slack-will-delete-content-older-than-one-year-for-free-plan-users-starting-august-26th)).
- **Mattermost v11** caps its free tier at 10k messages
  ([forum](https://forum.mattermost.com/t/mattermost-v11-changes-in-free-offerings/25126)).
- **Discord** has age-verification backlash
  ([GamingOnLinux](https://www.gamingonlinux.com/2026/02/discord-is-about-to-require-age-verification-for-everyone/comment_id=289731)).
- **Matrix** has a self-hosting and "unable to decrypt" reputation
  ([HN 39368852](https://news.ycombinator.com/item?id=39368852),
  [yaky.dev](https://yaky.dev/2025-11-30-self-hosting-matrix/)).
- **Campfire** is free and self-hosted but has no E2E, isn't terminal-native, and got a muted reception
  ([HN 39048962](https://news.ycombinator.com/item?id=39048962)).

The structural weakness is the same one that killed IRC for normal people: **no phone, and nothing
happens when tc isn't open** ([Slashdot](https://tech.slashdot.org/story/18/08/24/1519229/irc-turns-30)).
Every terminal-first tool that survived solved this by attaching to something with a phone presence:
weechat relay plus Glowing Bear, soju plus Web Push, gurk riding on Signal. Fixing that is the most
important thing on this list.

---

## (b) Onboarding friction audit (current tc, main plus in-flight branches)

### What's already good, and better than every competitor I looked at

- Join is fast: `tc join` took **89 ms** to a loopback server, and `tc send` took **73 ms**.
- With no key around, join creates a tc-only key and asks nothing. That avoids the devzat
  "Permission denied (publickey)" and identity-leak complaints
  ([HN 30690988](https://news.ycombinator.com/item?id=30690988),
  [30695824](https://news.ycombinator.com/item?id=30695824)).
- The invite-as-install-command, signed checksums, and the PATH-shadowing warning in `install.sh` are best
  in class. The closest things are Delta Chat's QR onboarding and Snikket.
- Idle RSS is **26 MB** per client, within the 30 MB budget.
- The release binary is stripped (`-s -w`). The unstripped dev build is 23 MB.
- `#general` opened automatically for me because it had messages. The fix for empty servers is
  `fix/first-start-general` (#42).

### Friction points, in order from "I got an invite" to "I came back after a week"

| # | Moment | Friction (observed or from code) | Severity | Fix |
|---|---|---|---|---|
| 1 | Admin, before any invite exists | The real "one step" bottleneck is the server. You need a VM, ufw, UDP+TCP 7443, systemd, and a hardening checklist (DEPLOY.md is 129 lines). If UDP is blocked, nobody notices until QUIC times out. | High (only once, but it gates everything) | `tc server setup` to write a systemd unit and data dir, print the firewall commands, and self-test reachability over both UDP and TCP from outside (e.g. the client tests both and reports which works). Add `tc server doctor` and `tc server backup` (wrapping sqlite `.backup` plus `server.key`). Longer term, a cloud-init one-liner. |
| 2 | Paste the invite | Linux: `tc` collides with iproute2 `/usr/sbin/tc`. `install.sh` warns about shadowing, but a user who types `tc` later can get traffic control instead. | High on Linux | **Do the rename before v0.5 gets wide use** (STATUS already flags this). Every install after this point costs more to migrate. |
| 3 | Join with an existing key | A passphrase-protected `~/.ssh/id_ed25519` with no agent makes join stop with an error. RSA-only users silently get a tc key. That is fine, but `--github` verification then won't match their GitHub keys. | Medium | When the key is locked, offer "use a tc-only key instead? [Y/n]" rather than failing. Mention that `--github` checks need the GitHub key. |
| 4 | Join on the terminal-probe path | Under a pty with no OSC-11 reply, `tc join` emitted `ESC]11;?` and `ESC[6n` before the name prompt and swallowed the typed answer. My `script` harness hung at the prompt. Real terminals reply, so this is low risk, but it can bite in odd terminals and some SSH or tmux chains. | Low-Med | Don't initialise lipgloss or termenv in the `join` path. Plain stdout is enough there. |
| 5 | Server unreachable | The raw error was `tcp dial …: connection refused quic dial …: timeout: no recent network activity` after 5 s. | Medium | Map this to "Can't reach host:7443 over UDP or TCP. Is the server running, and are ports 7443/tcp and 7443/udp open? (`tc server doctor` on the host)". |
| 6 | **Second laptop** | Joining with the same key returns `key_exists: … log in without an invite`, and **no command does that**. The `add key` invite kind exists in the protocol (§4.1) but there's no CLI or TUI to mint one for yourself. `tc device add` from E2E.md isn't built. A new device also gets older encrypted history only through other members' backlinks. | **High**: a dead end, and the kind of thing that makes people leave | `tc join tc://host#pin` with no token should log in with a registered key (the server already knows it). Add `/device add` (or `tc device link`) that shows a short code on the existing device; the new device enters it, and the old device confirms the fingerprint and seals every epoch key it holds. Signal shipped "history on linked devices" in 2025 because an empty new device reads as broken ([Signal blog](https://signal.org/blog/a-synchronized-start-for-linked-devices/)). |
| 7 | First open of the TUI | No welcome or orientation inside the TUI. An empty channel is a blank pane, and `@me` notes aren't visible in the sidebar or `Ctrl-K`, even though `tc notify` and `/remind` post there. | Medium | One-time dim welcome card in `#general` (Ctrl-K, F1, `/dm`, the "?" means unverified, where notes live). Pin "notes" to the top of DIRECT. Empty channel: "No messages yet. Say hi, or `/topic` to describe it". |
| 8 | First DM | The `?` hint ("not verified yet — compare safety numbers with /verify …") shows on the first DM, which is right. But the 60-digit read-aloud number is the only in-band verification. | Low-Med | Keep TOFU as the default. Make `--github` the advertised path (one keystroke, no call needed). Consider "verify by both reading a short 6-word phrase" on a huddle once voice ships. Matrix gave up on per-device verification prompts and moved to per-user TOFU ([matrix.org](https://matrix.org/blog/2025/11/exclude-insecure-devices/)). Don't nag. |
| 9 | Away from the terminal | **Nothing reaches you when tc isn't running.** Notifications are herdr, OS or bell from a live process. Reminders post server-side, but nobody sees them until tc is opened. | **Highest** | Phone push bridge (see F1). |
| 10 | **Back after a week** | I posted 62 messages while the client was closed and reopened it at 80 columns. It **jumped to the bottom and marked everything read**. There's no "── new ──" divider, no jump to first unread, no cross-channel catch-up, and `↓ N new` only appears if you're already scrolled up. Activity (Alt-A) covers mentions, replies and DMs, but not "what did #general talk about". | **High** for a social, async group | F2: unread divider plus jump to first unread on open, and a catch-up view. Don't mark read until the viewport has actually shown the messages. |
| 11 | Lost laptop | No revoke command today (in flight on `feat/device-revoke`). No key backup, so losing every device loses encrypted history. | Medium (rare but catastrophic) | Revoke UX is shipping in v0.3. Add an optional recovery key (F4). |
| 12 | Update | `tc update` plus the dim sidebar hint is good. Brew tap is in flight. | n/a | Done. Don't add auto-update without consent. |

---

## (c) Prioritized features

Scoring: Reach is how many of the ~15 users it affects; Impact is 1–3; Confidence; Effort is S (≤1 week
of worker time with tests), M (2–3 weeks), L (>3 weeks). The RICE value is relative. Each feature still
has to clear the product bar: adversarial, chaos and live tests, and no known bugs.

### Proposed v0.5 / M6: "Never miss it, never lose it, catch up in 30 seconds"

M6's theme is retention after week one. v0.3 and v0.4 handle getting people in and voice. M6 makes the
team still be there in month two.

| ID | Feature | RICE | Effort |
|---|---|---|---|
| F1 | Phone push via ntfy (content-free for E2E) | 15×3×0.9/M = **highest** | M |
| F2 | Catch-up: unread divider, jump to first unread, `/catchup` | 15×3×0.9/S = **very high** | S–M |
| F3 | Second device that works: token-less login plus `/device link` with history | 10×3×0.8/M | M |
| F4 | Optional recovery key (encrypted key backup on server) | 15×2×0.7/M | M |
| F5 | Onboarding polish bundle (friction items 3, 4, 5, 7) | 15×2×0.9/S | S |
| F6 | `tc server setup` / `doctor` / `backup` | 1 admin×3×0.9/S, but it gates everyone | S–M |
| F7 | Per-channel notify levels (all / mentions / mute) plus "calm by default" | 15×2×0.9/S | S |
| F8 | Rename (before installs grow) | everyone, once | S–M (mostly mechanical, plus a migration of `TC_HOME` and the update channel) |

#### F1. Phone push via ntfy
- **Problem:** tc is invisible when the terminal is closed or the laptop is asleep. A friend group is
  asynchronous; most messages arrive when you're not in a pane.
- **User story:** "When someone DMs me or @mentions me and I haven't seen it within N minutes, my phone
  buzzes: 'priya mentioned you in #climbing'. If I don't act, I see it the next time I open tc."
- **Why it beats Slack for this team:** you get Slack-grade "I won't miss it" without installing a Slack
  app, and the push doesn't contain your message.
- **Design:**
  - `/notify-phone https://ntfy.sh/<random>` or a self-hosted ntfy URL, stored server-side per user.
  - The server sends a push for DMs, mentions and highlight words only when the user has no active
    session or has been idle or away for longer than X.
  - Pushes are coalesced, like the existing 5 s window, but per user and longer (2 min).
  - **E2E channels send only "New message from @priya" with no content.** The server can't read it anyway.
    Mentions in E2E channels are invisible to the server, so for E2E channels push on "any DM message",
    and for private channels on "any message, if the user opted into all".
  - Public channels can include a short preview if the user opts in.
  - ntfy is a plain HTTP POST: no new dependency, and it works on iOS and Android with UnifiedPush
    ([ntfy guide](https://ramnode.com/guides/ntfy)). Matrix users fall back to ntfy for the same reason
    ([yaky.dev](https://yaky.dev/2025-11-30-self-hosting-matrix/)).
  - Recommend self-hosting ntfy next to the tc server (one more binary), or a random 32-char topic on
    ntfy.sh, because guessable topics are readable.
- **Risks:**
  - Metadata leaves the box (who DMs whom, and when) to a third party if ntfy.sh is used. Default to
    "self-host ntfy", label ntfy.sh clearly, and never put E2E content in the push.
  - The server needs to know about "mentions" in public channels, which it already does.
  - No cost to client size or speed. The server needs one outbound HTTP worker with a rate limit.
  - Hostile-text risk is only in the title; sanitize it.

#### F2. Catch-up
- **Problem:** coming back after a day or a week, tc dumps you at the bottom and marks everything read
  (observed). The interesting conversation scrolls away silently. Slack charges for "Recap"
  ([Trailhead](https://trailhead.salesforce.com/content/learn/modules/slack-ai/get-to-know-slack-ai));
  IRCv3 added `read-marker` for exactly this ([spec](https://ircv3.net/specs/extensions/read-marker)).
- **User story:** "I open tc on Monday and see '── 47 new since Fri 18:02 ──' at the first unread message
  in #general, with the view positioned there. `/catchup` (or `Alt-U`) shows every channel with unread
  messages, oldest first, and I walk through them with `n`."
- **Why it beats Slack:** no AI subscription needed, it's instant from the local cache, and it works
  offline.
- **Design:**
  - Store the last-read position per channel. The server already syncs read markers, and opening a
    channel already marks it read via MarkRead.
  - Render a divider at the first unread message and open the channel there when unread > one screen.
  - Mark read only as the viewport passes messages, not on open.
  - `/catchup` is an overlay modelled on Activity: channels with unread counts, the first unread line
    previewed, and Enter jumps to the divider.
  - CLI version: `tc catchup` prints a compact digest for herdr agents or scripts.
- **Risks:** client-only, cache-backed and cheap. The risk is getting read-marker semantics right across
  several panes and devices. This needs a `breaker` pass.

#### F3. Second device that works
- **Problem:** friction #6 is a dead end today. Developers in this team have two machines (work laptop and
  home desktop) almost by definition.
- **User story:** "On my desktop I run `tc join tc://host#pin`. It says 'this key isn't registered; on a
  device that's logged in, run `/device link` and type the code 'K7-F3-QX'.' I do, my laptop shows the new
  device's fingerprint, I press `y`, and my desktop has my channels and history in 10 seconds."
- **Why it beats Slack:** Slack's multi-device is magic because it isn't E2E. This gives the same feel
  without giving up E2E, which Matrix still struggles with (UTDs, "delete 15 stale devices",
  [HN 46107368](https://news.ycombinator.com/item?id=46107368)). Keybase's per-device sigchain is the
  model tc already resembles ([Keybase](https://keybase.io/blog/keybase-new-key-model)).
- **Design:**
  - **Token-less login:** the server accepts a registered key, and the pin comes from the fragment.
  - **Device link:** a PAKE is overkill, so use a short code plus fingerprint confirmation on the old
    device. The old device mints an `add key` invite for its own user, so the protocol already exists,
    and seals every held epoch key to the new device's cert.
  - **History:** the server re-serves stored ciphertext.
- **Risks:** E2E-critical (key sealing to a new device). Needs `protocol-guardian`, `security-reviewer`,
  the malicious-server suite and `breaker`. No cost to size or speed.

#### F4. Optional recovery key
- **Problem:** losing every device means losing every encrypted DM, forever, which is said plainly but
  painful.
- **User story:** "In `/settings backup` I get a 24-word (or 64-char) recovery key, which I put in my
  password manager. If my laptop dies, `tc join … --recover` asks for it and my DMs come back."
- **Why it beats Slack:** Slack has no E2E to recover. Against Signal, Signal took ten years to ship this
  ([Signal Secure Backup](https://support.signal.org/hc/en-us/articles/9708267671322-Signal-Secure-Backup)).
  Against Matrix, "reset your identity" is the bad path to avoid
  ([Element docs](https://docs.element.io/latest/element-support/device-verification/how-to-ensure-you-have-a-recovery-key/)).
- **Design:**
  - Epoch keys are sealed to a recovery X25519 key derived from the recovery secret and uploaded as an
    opaque blob, kept up to date on each rotation.
  - E2E.md already planned an "age passphrase recipient". Prefer a high-entropy generated key over a
    passphrase, so the server can't brute-force it offline.
  - Opt-in, offered once after the first E2E message, never nagging.
- **Risks:**
  - A weak secret becomes a server-side brute-force target, so use only a generated key.
  - Adds blob storage per user. Small: tens of KB.

#### F5. Onboarding polish bundle
Covers audit items 3, 4, 5 and 7:

- a "use a tc-only key instead?" prompt when the key is locked;
- no terminal probing in `join`;
- human-readable connection errors;
- a first-run welcome card;
- notes pinned in DIRECT;
- an empty-channel hint.

Effort S. Risk: none. Needs a TUI review.

#### F6. `tc server setup | doctor | backup`
- **Problem:** "one-step onboarding" is true for joiners and false for the person who has to run the
  server. If that person gives up, nobody joins.
- **User story:**
  - "On a fresh VM I run the install line, then `sudo tc server setup --public chat.example.com:7443`. It
    writes the systemd unit, prints the ufw commands, starts the service, tests that 7443/udp and 7443/tcp
    are reachable from outside, and prints my admin invite."
  - "`tc server backup /root/tc-backup.tgz` makes a consistent DB snapshot plus `server.key`."
- **Why it beats Slack:** owning your data stays as boring as Campfire's "just run it"
  ([once.com/campfire](https://once.com/campfire)) while staying one Go binary.
- **Risks:** the setup code runs as root, so keep it small and auditable. Doctor needs an external probe:
  the client side can test both protocols on join and report which works.

#### F7. Per-channel notification levels, calm by default
- **Problem:** with 15 friends, all-channel notifications are noise. A real slice of users prefers
  "non-notification chat" ([HN 30691619](https://news.ycombinator.com/item?id=30691619)).
- **User story:** "`/mute #memes` keeps it in the sidebar, dim, with no badge and no notifications.
  `/notify #climbing all` makes every message there notify."
- **Default:** mentions, highlights and DMs only. That is what tc does now, so this adds mute and "all"
  per channel, synced across devices.
- **Effort and risk:** S, low.

#### F8. Rename
Not a feature, but it should be in M6. The cost grows with every install, and there's a Linux name clash.
It touches the binary name, `TC_HOME` default, the `tc://` scheme (keep accepting the old one), the brew
formula, and the update channel. Make sure `tc update` can migrate old installs: ship one release that
installs both names.

### M7+: "Richer, still light"

| ID | Feature | Problem → story | Why better than Slack here | Effort | Risk to light / fast / E2E |
|---|---|---|---|---|---|
| G1 | **Encrypted file attachments** (≤ 25 MB, save-to-disk, `o` opens with the OS) | People share screenshots, PDFs and logs that are too big for a code block. "`tc send @bob --attach trace.pprof`; bob presses `o`." | E2E files with no storage caps or vendor retention | M–L | Server storage growth; needs quotas, chunked transfer, sanitized filenames. Roadmap "Later" already lists this. Keep **no inline rendering** (respects the "inline images" cut). |
| G2 | **Reply from the phone notification** (ntfy action → signed one-shot reply token, public channels and opted-in DMs only) | "I can answer 'yes, 7pm works' from the bus." | Covers about 80% of phone needs without an app | M | A reply token is a credential. E2E content can't be composed on the phone without a client, so limit this to public channels or skip it. |
| G3 | **Retention / disappearing messages per channel** | "#venting deletes after 7 days." | Slack's retention is paid and admin-controlled. Here it's per channel and member-visible. | M | E2E: purge old epoch keys too (already noted in E2E.md). Interacts with search and the cache. |
| G4 | **Search upgrades**: `from:@x in:#y before: after: has:code` and the jump context | Slack-grade "find that link from March" | Unlimited history plus good search is the anti-Slack-Free pitch | S | Client-side FTS for E2E already exists. No cost. |
| G5 | **Read-only "peek" web view?** | See the next table: rejected for now. | | | |
| G6 | **Scheduled send** (`/later tomorrow 9am …`) | Friends in different timezones | Nice to have | S | Server holds plaintext until the send time in public channels. For E2E, encrypt at schedule time; it's fine. |
| G7 | **Local GitHub unfurls via `gh`** (client-side only, opt-in) | PR or issue links show title and status | No URL leak, unlike Slack's server unfurls | S | Exec of `gh`. Sanitize. Rate-limit. |
| G8 | **Presence-aware huddle nudges** (after M5): sidebar "🎧 3 in #general huddle" | Discord's ambient "pop in without calling" is what friend groups love ([Discord](https://apple.co/2lUG7za)) | Drop-in, no ringing | S on top of M5 | None beyond M5. |
| G9 | **Email digest** (Zulip-style, batched, only while inactive; [Zulip](https://api.zulip.com/help/email-notifications)) | Fallback for people who refuse to install ntfy | | M | SMTP config burden on the admin. Only do it if F1 adoption is poor. |
| G10 | **SSH guest mode** (public channels, read/write via `ssh chat.example.com`) | Onboard a curious friend with zero install, devzat-style ([HN 30688691](https://news.ycombinator.com/item?id=30688691)) | Zero install | M–L | Second front door, abuse/flooding ([HN 8744819](https://news.ycombinator.com/item?id=8744819)), more attack surface. Do it only if the team asks. |

### Already in flight (don't re-plan, just make sure they land clean)
v0.3: #42 first-start, any-member invites, github-key invites, device revoke UX, brew tap.
v0.4: voice huddles.

**On voice:** the `voice` build tag means a second, cgo, per-platform artifact. Make sure install.sh,
`tc update` and brew pick the right one. Measure idle RSS with voice compiled in but not in a huddle,
since the budget is 30 MB and the client is already at 26. If audio libs push past budget, load them
lazily or ship voice as a separate `tc-voice` helper process.

---

## (d) Don't build, and why

| Don't build | Reason |
|---|---|
| **Native mobile app** (for now) | It doubles the surface (two app stores, push certificates, a second E2E client to keep correct). The Matrix UTD saga came from client diversity ([Element talk](https://2025.matrix.org/documents/talk_slides/LAB4%202024-09-21%2014_30%20Kegan%20Dougal%20-%20Unable%20to%20decrypt%20this%20message.pdf)). F1 plus G2 cover most of the phone need. Revisit only if the team says ntfy isn't enough after two months. |
| **Web client** | Same reason. It also needs a browser E2E implementation and server-served JS, which means the server could serve malicious JS: that breaks the E2E threat model. |
| **Federation / bridges to Slack, Discord or Matrix** | Bridges leak plaintext, break E2E, and add a maintenance treadmill. This team wants out of Slack, not a mirror of it. |
| **AI summaries (server-side)** | The server can't read E2E content, and shouldn't. A deterministic `/catchup` (F2) gives 80% of the value. A local-LLM `tc catchup | llm` is something users can pipe themselves, so document the pipe and don't ship it. |
| **Inline images / sixel / kitty graphics** | Already cut. Devzat threads show image and escape rendering is a top complaint ([HN 41004380](https://news.ycombinator.com/item?id=41004380)). Attachments open externally (G1). |
| **Per-device verification prompts, or mandatory verification** | Matrix and Wire both found per-device verification fails with users, and the industry is moving to TOFU per user plus self-vouched devices ([matrix.org](https://matrix.org/blog/2025/11/exclude-insecure-devices/), [Wire](https://support.wire.com/hc/en-us/articles/207692235)). Keep `/verify` optional and per user, and never gate sending on it, except on a key change, which tc already does correctly. |
| **Multi-workspace / multi-server in one client** | For one team of fewer than 15, one server is the product. `TC_HOME=…` already lets power users run two. It would complicate the cache, notifications and the sidebar for near-zero reach. |
| **Mandatory Zulip topics, canvases, lists, workflows, polls, custom emoji, server-side unfurls** | Respect the existing cut list. All the reasons still hold. Polls via reactions are fine at 15 people. |
| **Video / screen share** | Huge cost to binary size, cgo and CPU. herdr `/share-pane` plus voice covers the developer use case ("look at my pane"). |
| **SSO / LDAP / roles beyond admin** | It's a friend group. SSH keys are the identity. |
| **Read receipts per message** | Social pressure in a friend group, and metadata. Unread counts are enough. |
| **Hosted "tc cloud"** | It changes the trust model (you'd become the server operator) and creates a support burden. Make self-hosting boring instead (F6). |

---

## (e) If we could only do five

1. **Phone push via ntfy (F1).** This is the IRC lesson. Without it, tc becomes "the chat we check when we
   remember", and the team slides back to WhatsApp or Slack. Send content-free pushes for E2E. It's one
   HTTP worker on the server.
2. **Catch-up done right (F2).** A "new since" divider, opening at the first unread message, marking read
   only when seen, and `/catchup`. For an asynchronous friend group this is the daily experience, and
   today it silently swallows the backlog (observed).
3. **Second device that just works (F3), plus recovery key (F4) as one "never lose it" epic.** Fix the
   `key_exists` dead end with token-less login and `/device link`, with history. Then add an opt-in
   generated recovery key. Matrix shows a single "unable to decrypt" or "your history is gone" moment
   drives people away for good ([HN 39368852](https://news.ycombinator.com/item?id=39368852)).
4. **Rename now, then F5 onboarding polish.** The rename gets more expensive with every install, and the
   Linux `tc` clash is a real first-run trap. Bundle it with the small join and TUI fixes: locked-key
   fallback, readable network errors, welcome card, visible notes, empty-channel hint.
5. **`tc server setup / doctor / backup` (F6).** "One-step onboarding" has to include the person who
   hosts. If the owner's server dies or a friend can't connect over UDP, the product fails for everyone.
   Plus a backup command, because the promise is "your history forever".

Explicitly not on the list: more chat features (attachments, retention, scheduled send). The team already
has more features than it can discover. M6 should be about reliability of attention (push, catch-up) and
reliability of identity (devices, recovery), which is exactly where Matrix, IRC and terminal chats have
lost users before.

---

### Hands-on notes (reproducible)

- **Build:** `GOTOOLCHAIN=local CGO_ENABLED=0 go build -o <scratch>/pm/tc ./cmd/tc`.
- **Server:** `tc server --data <scratch>/pm/srv --listen 127.0.0.1:17999 --public 127.0.0.1:17999` printed
  the admin invite. `tc server invite --uses 15 --ttl 24h --data …` worked while the server was running.
- **Join:** non-tty join with no prompts took 89 ms. A pty join hung at the name prompt after terminal
  probes (audit #4). Second-device join gave `key_exists` with no next step (audit #6).
- **TUI:**
  - At 60 columns, the sidebar hides and the header shows `· 1@`.
  - F1 help fits.
  - Ctrl-K then `bob` opened the E2E DM with the `@bob? 🔒` header and the `?` hint.
  - Backlog of 62 messages: opened at the bottom, no divider (audit #10).
- **Resources:** idle RSS was 26 MB per client. The release binary is built with `-s -w`.
