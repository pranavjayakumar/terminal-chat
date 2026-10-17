# Notifications and sounds for v0.4.0 (research + design)

Status: research r1, 2026-10-17. Target: v0.4.0, built alongside voice huddles (PR #47).
Scope: **only the gaps**. Everything tc already does stays as it is (§1).

## TL;DR

- Keep the current pipeline: trigger rules, focus rules, 5 s / 2 s coalescing, the herdr → OS → bell
  backends, badges and the activity view. Add a small **policy layer** in front of it and a **cue**
  (sound class) on each notification.
- **Per-channel levels**: `all | mentions (default) | off`, plus a timed mute (`for 2h`). Stored
  locally in `cache.db`, like highlights. No protocol change and no server sync in v0.4.0.
- **Sounds**: no new player binaries and no audio files. Notification sounds travel inside the
  backend call that shows the popup: herdr `--sound`, osascript `sound name`, the notify-send
  `sound-name` hint, or the BEL on terminals. **In-huddle cues** (join, leave, mic on/off, ring loop,
  dropped) are synthesized PCM mixed into the voice playback stream. That only exists in voice
  builds, the only builds that can be in a huddle.
- **Rule: one event, one sound.** tc never pairs a popup with a second player.
- **Huddles**: a huddle started in a DM or group DM **rings** (Matrix MSC4075-style rules: once per
  huddle, never if you're already in it, never on a reconnect snapshot, 30 s cap). A channel
  huddle start is **silent by default**: it shows the 🎧 sidebar marker, and a popup only at level `all`.
- **Terminal notifications over SSH**: a new `terminal` backend sends OSC 9 / 777 / 99 (with tmux
  passthrough, like the existing OSC 52). `auto` picks it over SSH instead of the remote host's OS
  notifier.
- **Privacy**: `/notify content full|name|none` (Signal's three options). The default is `full`.
- **DND**: `/dnd 2h`, plus quiet hours `/notify quiet 22:00-08:00`. These are local, and they
  silence popups, sounds and rings, but not badges.
- Phone push (ntfy), cross-device sync of levels, per-thread mute and custom sound files are deferred to M6 (§3).

---

## 1. Already done, unchanged

These stay exactly as they are. The design below only adds to them.

| What | Where |
|---|---|
| Triggers: DMs, @mentions (including groups), highlight words, my own `⏰` reminders. Bots never notify, and neither does text that isn't verifiably its author's (`Sec.Trusted`). | `internal/tui/herdr.go` `shouldNotify`, `notifyFor` |
| Focus rule: no notification for the active channel while the terminal is focused (`tea.WithReportFocus`). | `shouldNotify`, `model.go` Focus/Blur |
| Coalescing: the first message in a channel notifies at once, then a 5 s window produces "N new messages". Global cap of 1 per 2 s. Opening the channel drops what's pending. | `queueNote`, `flushNote` |
| Backends: `TC_NOTIFY=auto\|herdr\|os\|bell\|off`, `TC_BELL=0`. herdr `notification show … --sound request`, osascript with argv text (`on run argv`), `notify-send --app-name=tc`, BEL. Exec uses argv, a 2 s timeout, one call in flight, `sanitize.Line`, and 60/160-rune limits. | `internal/notify` |
| herdr unread badge `tc · 3 unread · 1@`, debounced, cleared on exit. | `badgeTick`, `reportBadge` |
| Activity view: mentions, replies in my threads, reactions, DMs. | `internal/client/activity.go`, `Alt-A` |
| Auto-away (`TC_AWAY_MIN`), `/away`, `/back`. | `herdr.go` auto-away |
| Highlight words (`/highlight`), stored locally in `cache.db`. | `internal/client/highlights.go` |
| Huddle roster indicator, `/huddle`, `/leave-huddle`, `/talk`, and **`/mute` = mic mute** (voice branch). | `origin/feat/voice-huddles` `internal/tui/huddle.go` |

**Existing-behaviour caveat to document (no code change):** herdr's `[ui.toast] delivery`
**defaults to `"off"`**, and `--sound` "play[s] … only when the notification is shown". On a
fresh herdr config, tc's herdr backend is therefore silent and invisible
([herdr config reference](https://raw.githubusercontent.com/herdrdev/herdr/v0.9.5/docs/next/website/src/data/config-reference.json),
[herdr CLI reference](https://raw.githubusercontent.com/herdrdev/herdr/v0.9.5/docs/next/website/src/content/docs/cli-reference.mdx)).
herdr also "suppresses popups for the active tab"
([herdr configuration](https://raw.githubusercontent.com/herdrdev/herdr/v0.9.5/docs/next/website/src/content/docs/configuration.mdx)).
Fix: a README line ("set `[ui.toast] delivery = "terminal"` or `"system"`") and `/notify test`
(§2.9) so users can check their setup. This was checked against herdr 0.9.1 locally: `--sound
none|done|request`, and the sounds are user-configured mp3s (`[ui.sound] done_path/request_path`)
played by the **local herdr client**, so they work over herdr's SSH remote attach too.

---

## Research summary

### Chat apps

| App | Levels | Per-channel | Sounds | Privacy | Other |
|---|---|---|---|---|---|
| Slack | Everything / Mentions & DMs; per-channel exceptions; keywords (not in unfollowed threads) | yes | a separate sound per type: messages, VIP, **huddles**, DM sent, DM while in the conversation; "Mute all sounds" | "Include a preview of the message" toggle | notification schedule (quiet hours); mobile push only when inactive on desktop (default 1 min after lock or 10 min idle) ([Slack](https://slack.com/help/articles/201355156-Configure-your-Slack-notifications)) |
| Discord | All / Only @mentions / Nothing, plus mute for a duration; channel overrides category, which overrides server | yes | 22 toggleable sounds incl. Message, Mute, Deafen, User Join, User Leave, Incoming Ring; global only, never per channel (a long-standing request) | — | "Nothing" still badges mentions ([Discord support](https://support.discord.com/hc/en-us/articles/21084266106775), [sounds list](https://androidauthority.com/disable-discord-notifications-3147128), [per-channel join sound request](https://support.discord.com/hc/ko/community/posts/360072187072)) |
| Zulip | per-channel visual / audible / mobile / email; followed topics; muted topics | yes | one chosen sound, or None = no audible notifications | "Include content of direct messages in desktop notifications" | notifies when unfocused or off-screen ([channel](https://zulip.com/help/channel-notifications), [desktop](https://zulip.com/help/desktop-notifications), [DMs](https://www.zulip.com/help/dm-mention-alert-notifications)) |
| Mattermost | All / Mentions / Nothing per channel; "Ignore @channel/@here/@all"; mute greys the channel but still bolds a direct @mention | yes | global sounds; per-channel sound since v10.1 | — | mute also silences incoming call rings ([Mattermost](https://docs.mattermost.com/end-user-guide/preferences/manage-your-channel-specific-notifications.html)) |
| Element / Matrix | per room: All messages (Noisy) / All / Mentions & keywords / Mute (mute beats mentions) | yes | "Noisy" = with sound | — | [Element docs](https://ems-docs.element.io/books/element-support/page/notification-settings), [push rules](https://patrick.cloke.us/posts/2023/05/08/matrix-push-rules-notifications/) |
| Matrix RTC ringing (MSC4075) | ring vs notify; never ring in a silent room; ignore repeat notifies for the same call; don't ring if already a member of the call | — | ring | — | [MSC4075 draft](https://git.sudo.is/matrix/matrix-doc/src/branch/toger5/matrixrtc-call-ringing/proposals/4075-call-notify-event.md) |
| Signal Desktop | — | per chat mute | — | **Name, content, and actions / Name only / No name or content** (enum `message / name / count`) | [guide](https://geekrewind.com/how-to-change-notification-content-settings-in-singal-on-windows-11), [source mirror](https://git.uninsane.org/colin/Signal-Desktop/raw/commit/36e21c0134607d7cc8d29eaa6ef06fe9e9a385f9/ts/services/notifications.ts) |
| Telegram | per-chat `mute_until`, `show_previews` | yes | per chat | previews toggle | [API peerNotifySettings](https://core.telegram.org/constructor/peerNotifySettings) |
| iamb (TUI) | on/off | — | `via = "desktop\|bell"`, `sound_hint = "message-new-instant"` | `show_message` | closes notifications on read receipt ([iamb](https://iamb.chat/configure.html)) |
| weechat | scripts (notify-send, sound on highlight or private message) | via script | via script | — | [scripts](https://weechat.org/scripts/tag/notify/) |

Takeaways: three levels plus a timed mute is the common denominator. Sounds are global (not
per channel) everywhere except Mattermost 10.1. The huddle/call sound is distinct from the
message sound. Rings carry dedupe rules. Every serious client has a content-privacy switch.

### Terminal notifications

| Sequence | Format | Supported by |
|---|---|---|
| OSC 9 | `ESC ] 9 ; text ST` (message only) | iTerm2 ([docs](https://iterm2.com/3.6/documentation-escape-codes.html)), kitty (legacy), Ghostty, WezTerm. ConEmu and Windows Terminal reuse `OSC 9;N` for other things (`9;4` = progress), so a payload starting with digits + `;` is ambiguous ([tcell #499](https://github-redirect.dependabot.com/gdamore/tcell/issues/499)) |
| OSC 777 | `ESC ] 777 ; notify ; title ; body ST` | WezTerm ([notification_handling](https://wezterm.org/config/lua/config/notification_handling.html), which can suppress from the focused pane), Ghostty (`desktop-notifications = true` by default; [config](https://ghostty.org/docs/config/reference)), foot (`inhibit-when-focused = yes`; [foot.ini](https://codeberg.org/dnkl/foot/raw/branch/master/doc/foot.ini.5.scd)), VTE-based terminals |
| OSC 99 | `ESC ] 99 ; i=id:d=0:p=title:e=1 ; base64 ST`. Keys: `u` urgency 0–2, `s` sound name (base64; `system/silent/error/warn/info/question`, freedesktop names on Linux), `o=always\|unfocused\|invisible`, `w` expiry, chunks ≤ 2048 B | kitty ([spec](https://sw.kovidgoyal.net/kitty/desktop-notifications/)), foot |
| BEL | `\a` | everywhere. Ghostty's `bell-features` default is `attention,title`; foot can `urgent`/`notify` on BEL |
| tmux | the sequence is wrapped in `ESC P tmux; … ESC \` with inner ESCs doubled; needs `allow-passthrough on` (off by default since 3.3) | [tmux FAQ](https://github.com/tmux/tmux/wiki/FAQ). zellij and screen don't pass these through ([pi-notify table](https://github.com/ferologics/pi-notify)) |

Terminal.app and Alacritty support none of them (they get the bell). These notifications are produced
by the *local* terminal, so they **work over SSH**. That is the case the current backends get wrong:
`osascript`/`notify-send` would fire on the remote host.

### Platform

- **macOS**: `osascript display notification … sound name "Glass"` takes a name from
  `/System/Library/Sounds` (Basso, Blow, Bottle, Frog, Funk, Glass, Hero, Morse, Ping, Pop, Purr,
  Sosumi, Submarine, Tink, checked locally) or `~/Library/Sounds`. An unknown name plays the alert
  sound ([code-maven](https://code-maven.com/display-notification-from-the-mac-command-line)).
  Notifications are attributed to **Script Editor** and need that app's permission first
  ([macscripter](https://macscripter.net/t/trying-to-use-terminal-for-display-notification/76593)).
  Notification Center applies Focus/DND to them, including their sound. `afplay` ignores Focus.
  Reading the Focus state needs Full Disk Access for `~/Library/DoNotDisturb/DB`
  ([focus-cli](https://pkg.go.dev/github.com/legnoh/focus-cli)), so don't. `terminal-notifier`
  isn't installed by default; herdr itself prefers it and falls back to osascript.
- **Linux**: notify-send / freedesktop hints `urgency`, `category`, `sound-name`, `sound-file`,
  `suppress-sound`, `transient`, `desktop-entry`
  ([hints](http://specifications.freedesktop.org/notification/latest/hints.html)). Sound names
  ([naming spec](http://specifications.freedesktop.org/sound-naming/latest/)):
  `message-new-instant`, `phone-incoming-call`, `phone-hangup`, `service-login`/`-logout`,
  `bell-terminal`. Whether `sound-name` plays depends on the notification server (GNOME Shell and
  Plasma honour it; others may not). `canberra-gtk-play`, `paplay` and `pw-play` are not guaranteed,
  and none are here.
- Kenney's "Interface Sounds" are CC0 ([repo](https://github.com/Calinou/kenney-interface-sounds)),
  and herdr ships two of them, but they're OGG/MP3 and would need a decoder. See §4.3 for why tc
  synthesizes instead.

---

## 2. Design for v0.4.0 (gaps only)

### 2.1 Events and cues

The existing path is a notification with title and body. It gains a **cue**, which is the sound
class. The policy (§2.2) decides `popup bool, cue Cue` for each event.

| Event | Trigger (new parts in bold) | Popup | Cue |
|---|---|---|---|
| DM message | unchanged | yes | `mention` |
| @mention / highlight / reminder | unchanged | yes | `mention` |
| **Reply in a thread I started or replied in** | **new**: same set as `ActReply` in the activity view; counts as a mention-class trigger | yes | `mention` |
| **Any message in a level-`all` channel** | **new** | yes (30 s coalescing window) | `message` (soft) |
| **DM / group-DM huddle started by someone else** | **new**: see §2.6 | yes | `ring` |
| **Channel huddle started** | **new**: level `all` only | yes | `huddle` (soft) |
| In-huddle: someone joins / leaves | **new**, voice build | no | `join` / `leave` (synthesized) |
| In-huddle: my mic on / off | **new**, voice build | no | `mic-on` / `mic-off` click |
| In-huddle: I was dropped (network, revoked, removed) | **new**, voice build | no (status line) | `drop` |

### 2.2 Notification levels

Model: one **global default** plus **per-channel overrides**. DMs are channels, so they can
be muted too. Per-thread levels are not included (§3).

| Level | Popup and sound for | Sidebar |
|---|---|---|
| `all` | every message except bots' (they never notify), and huddle starts | normal |
| `mentions` (**default**, = today) | DMs, @mentions, highlights, reminders, replies in my threads, DM huddle rings | normal |
| `off` (muted) | nothing: no popup, no sound, no ring | name dim, **no unread count**. A direct @mention still shows `@` (as Mattermost and Discord do). It stays in the activity view and still counts in the herdr badge's `@` figure. |

- **Timed mute** (Telegram's `mute_until`): `/notify off 2h` stores `until`. When it passes,
  the channel reverts to the default. The check is lazy (whenever a decision is made); no timer is needed.
- A new DM defaults to `mentions`. In a DM, every message is mention-class anyway.
- `@here`/`@channel` don't exist in tc, so there's no "ignore @channel" option.
- The herdr unread badge is unchanged, except that muted channels' plain unread counts are left
  out of the summary, as they are from the sidebar.

### 2.3 Sound mechanisms per context

| Context | Notification cues (`mention`, `message`, `ring`, `huddle`) | In-huddle cues |
|---|---|---|
| **herdr** (backend `herdr`) | `--sound request` for `mention` and `ring`; `--sound done` for `huddle`; `--sound none` for `message` (popup only). tc never plays its own audio for these. Users who mute herdr (`HERDR_DISABLE_SOUND`, `[ui.sound] enabled=false`) get silence, as they asked. | voice-build mixer |
| **macOS local** (`osascript`) | add `sound name` to the existing script as argv item 3: `mention`=Glass, `ring`=Submarine, `huddle`=Pop, `message`=Tink. Taken from a fixed allowlist, never from user text. Focus/DND applies automatically. | voice-build mixer |
| **Linux local** (`notify-send`) | `--hint=string:sound-name:<name>` with `message-new-instant` (mention, message), `phone-incoming-call` (ring), `service-login` (huddle); `--category=im.received` or `call.incoming`; `--urgency=critical` for ring only; `--hint=string:x-dunst-stack-tag:tc-<chan>` and `x-canonical-private-synchronous:tc-<chan>` so a channel's "N new messages" replaces its previous popup where supported | voice-build mixer |
| **Terminal** (`terminal`, new, §2.4) | the terminal's own notification sound. OSC 99 gets `u=2` for ring. No extra BEL. | voice-build mixer |
| **Bell** | one BEL, as today (`TC_BELL=0` still disables it) | voice-build mixer |

No afplay, paplay, pw-play or canberra in v0.4.0. Every notification sound is played by the
system that shows the popup, so Focus/DND, the user's notification settings and volume all apply,
and the sound can't double up.

### 2.4 New `terminal` backend (OSC), and SSH-aware `auto`

- `TC_NOTIFY=terminal` forces it. In `auto`, the order becomes **herdr → terminal (only when
  `SSH_CONNECTION`/`SSH_TTY` is set and a supporting terminal is detected) → os → bell**. Local
  sessions keep today's choice. The change only stops `auto` from firing osascript or notify-send
  on a remote box. Inside herdr, tc never emits OSC; herdr owns delivery.
- Picking the dialect by environment, with no terminal queries (Bubble Tea owns stdin):
  `TERM=xterm-kitty` → 99. `TERM=xterm-ghostty`, `TERM=foot*`, `TERM_PROGRAM=WezTerm|ghostty` → 777.
  `LC_TERMINAL=iTerm2` (iTerm2 sets it, and macOS ssh forwards `LC_*` by default) or
  `TERM_PROGRAM=iTerm.app` → 9. Anything else means no OSC: `auto` falls through to os/bell.
  `TC_NOTIFY_OSC=9|777|99` overrides.
- Encoding (a pure function, fuzzed):
  - 99: two chunks `i=tc<n>:d=0:p=title:e=1` and `i=tc<n>:p=body:e=1`, payloads base64, so no
    escaping problems. `o=always`, because tc already applied its focus rules and `unfocused` would
    drop "message in another channel while focused". `u=2` only for ring.
  - 777: `ESC]777;notify;<title>;<body>ESC\`. Replace `;` in the title with `,`.
  - 9: `ESC]9;<title>: <body>ESC\`. If the text starts with a digit, prefix `tc · ` to avoid the
    `9;4` progress collision.
  - All text goes through `clean()` (already strips controls). Additionally assert that the payload
    contains no ESC, BEL or C1 bytes, and cap it at 256 bytes.
  - tmux: wrap in DCS passthrough exactly as `osc52()` does (`Options.Tmux`). If passthrough is
    off, the sequence is swallowed silently, so in tmux also send a BEL (tmux turns it into
    `bell-action`). Outside tmux, no BEL with OSC.
- Write through `Options.Out` as one `Write` call per sequence, the same contract as BEL and OSC 52
  today (a single short write between frames).

### 2.5 Anti-double-sound and coalescing rules

1. **One event → one backend call → at most one sound.** Never exec a player beside a popup.
2. In herdr, notification sounds come only from herdr. The voice-build mixer plays only in-huddle
   cues and the ring loop, and the ring loop never plays in herdr (rule 5).
3. Global cap: the existing "1 notification per 2 s" applies to cued popups. A `ring` **bypasses
   the queue and the 2 s cap** (it's time-critical) and has its own in-flight slot in `Notifier`,
   so a hung `message` popup can't drop it (today's single `busy` flag would).
4. Level-`all` channels use a **30 s** coalescing window instead of 5 s, and the summary popup uses
   cue `message`. Mention-class messages in an `all` channel keep the 5 s window and the `mention` cue.
   The stronger class wins within a window.
5. **Ring**: one ring per huddle instance. In herdr: one `request` popup, re-sent once after 10 s if
   still unanswered. Local voice build: a synthesized ring loop (≈ 0.4 s two-tone every 3 s, ≤ 30 s)
   plus **one silent popup** (osascript without `sound name`, notify-send with `suppress-sound`,
   OSC 99 `s=silent`). Non-voice build: one cued popup, and the body says "needs the voice build to
   join" (§2.6).
6. In-huddle cues: join/leave are coalesced per huddle in a 1 s window ("3 joined" plays one
   `join`). They're skipped entirely when the roster is ≥ 8 (big huddles are noisy) and never
   played for my own join. Mic clicks are debounced to 150 ms (the `ctrl+t` repeat heuristic must
   not chatter). No join/leave cues for 2 s after my own join, while the roster snapshot settles.
7. Channel huddle start (level `all`): at most one popup per channel per 5 min, so flapping
   rejoins don't spam.

### 2.6 Huddle notifications (input to the owner decision noted in VOICE.md)

The client derives everything from `huddle {chan, parts}` ephemerals (PROTOCOL §14.2). No
protocol change is needed.

- **Started** = this client saw the roster for `chan` go from empty to non-empty **live**. The
  first roster after `sync`, and the one after `chan.join`, are a **baseline** and never ring or
  notify. That covers reconnects, and it's the MSC4075 "don't re-ring" rule.
  - Optional later protocol addition: `started` (unix seconds) on `huddle`, so a client that
    connects 5 s after the start can still ring. It isn't needed for v0.4.0.
- **Ring** when all of these hold: the channel is a DM or group DM; the first participant isn't me;
  none of `parts` is me (on any device); the channel level isn't `off`; not DND or quiet hours;
  ring cue enabled. **Stop** on: I join (any device), the huddle ends, I open that DM while
  focused, `Esc` on the status-line prompt, or 30 s. Status line while ringing:
  `📞 alice — /huddle to join · Esc dismiss`.
- **Channel huddle start**: no popup at `mentions` (the default). The 🎧 sidebar marker after the
  channel name is the signal, as VOICE.md's follow-up already plans. At `all`, a popup "🎧 bo
  started a huddle in #dev" with cue `huddle`.
- **Encrypted channels**: the popup content is the same (no message text involved).
- herdr badge: append `🎧` while I'm in a huddle (VOICE.md follow-up, unchanged).

### 2.7 Privacy (content shown in popups)

`/notify content full|name|none`, env `TC_NOTIFY_CONTENT`. Global, applied to every backend:

| Mode | Title | Body |
|---|---|---|
| `full` (default) | `#chan · alice` / `alice (DM)` | first line (today) |
| `name` | same | `new message` / `N new messages` |
| `none` | `tc` | `new message` / `N new messages` |

- The default stays `full`, as Signal's does. E2E protects against the server; the popup is shown
  on the user's own device. The doc and `/notify` output still say that popups persist in OS
  notification history (macOS Notification Center keeps a local database, and GNOME keeps a tray
  history), and recommend `name` on shared machines.
- Ring and huddle popups under `none` read `tc` / `incoming huddle`.
- Not per channel in v0.4.0. One switch is easier to reason about.

### 2.8 DND and quiet hours

- `/dnd [30m|2h|until 09:00|off]`, and `/dnd` alone toggles for 1 h. Quiet hours:
  `/notify quiet 22:00-08:00|off`, env `TC_QUIET`, local time; a range may wrap midnight.
- Effect: no popups, no notification sounds, no ring. Badges, unread counts, the activity view
  and in-huddle cues (you chose to be in a huddle) stay. Nothing is replayed when DND ends; the
  activity view is the catch-up.
- **Local only**: not shown to others in v0.4.0 (that would need a presence field). The
  header/status line shows a dim `🔕 09:00`.
- OS Focus is respected implicitly by the os backend. tc doesn't try to read the Focus state.

### 2.9 Commands, env and config surface

| Command | Does |
|---|---|
| `/notify` | shows the effective settings: this channel's level (and whether it's inherited), the global default, content mode, quiet hours, DND, backend + OSC dialect, sounds on/off |
| `/notify all\|mentions\|off [#chan] [for 2h]` | sets the level (current channel when no `#chan` is given) |
| `/notify default [#chan]` | removes the override |
| `/notify global all\|mentions\|off` | sets the global default |
| `/notify content full\|name\|none` | privacy (§2.7) |
| `/notify quiet HH:MM-HH:MM\|off` | quiet hours |
| `/notify test` | fires one sample notification for each cue through the real backend, so herdr's `delivery=off`, macOS permission and missing OSC support become visible |
| `/dnd [dur\|until HH:MM\|off]` | DND |
| `/sounds` | lists the cues and their state |
| `/sounds on\|off` | master switch (popups keep showing, silently) |
| `/sounds <mention\|message\|ring\|huddle\|join\|mic> on\|off` | per cue (Discord-style, global) |
| `/sounds volume 0-100` | volume of the synthesized in-huddle cues only (default 60). Everything else follows system or herdr volume. |

- `/mute` keeps its voice-branch meaning (**mic**). Slack and Discord use "mute" for the mic in
  calls; a channel mute is `/notify off`. Ctrl-K channel palette: `Alt-M` toggles the selected
  channel between `off` and default, if a key is wanted (tui-reviewer to decide).
- Env vars (override persisted settings, for scripts and SSH): `TC_NOTIFY` (gains `terminal`),
  `TC_NOTIFY_OSC`, `TC_NOTIFY_CONTENT`, `TC_QUIET`, `TC_SOUND=on|off`. `TC_BELL` is unchanged.
- **UI markers** (each 1–2 cells, with a NO_COLOR-safe fallback):
  - Muted channel: dim name, no count, `@` only for a direct mention; the header shows dim `🔕`.
  - Timed mute: the header shows `🔕 until 14:30`.
  - Live huddle: `🎧` after the name (VOICE.md follow-up).
  - Ringing: the status line as in §2.6. DND: dim `🔕 HH:MM` at the right of the status line.

### 2.10 What's persisted where

| Data | Where | Why |
|---|---|---|
| per-channel level + `until` | `cache.db`, new table `notify_prefs(chan INTEGER PRIMARY KEY, level TEXT NOT NULL, until INTEGER NOT NULL DEFAULT 0)`, listed in the per-server table set like `highlights` | per server and per device, like highlights; no protocol change |
| global default, content mode, quiet hours, DND-until, sound switches, cue volume | `cache.db` `meta` keys `notify.default`, `notify.content`, `notify.quiet`, `notify.dnd_until`, `sound.<cue>`, `sound.volume` | same |
| huddle ring state | memory only | ephemeral by nature |

**Not synced in v0.4.0.** Syncing needs a protocol op (protocol-guardian, PROTOCOL.md), and a
plain server-side table would tell the server which E2E channels and DMs each user muted. That's
minor metadata, but new. A team of < 15 rarely has multi-device users who care. Highlights are
already local, so this stays consistent. M6 (ntfy push) needs the server to know *some* policy to
decide pushes. Design both together then: a user-private blob sealed to the user's own devices,
plus a coarse server-visible push policy.

---

## 3. Not in v0.4.0

- **Phone push via ntfy** (PRODUCT F1) → M6, with server-side policy and content-free pushes for E2E.
- Cross-device sync of levels and sound settings → M6 (§2.10).
- Per-thread levels and follow/unfollow (Slack, Zulip followed topics). v0.4.0 only adds "replies in
  my threads notify".
- Per-channel sounds (only Mattermost does this) and custom sound files (`TC_SOUND_<CUE>=path`). These
  would need a decoder or a player exec.
- `afplay`/`paplay`/`pw-play`/`canberra-gtk-play` playback, `terminal-notifier`, and click-to-focus
  actions (OSC 99 `a=focus` reports back on stdin; Bubble Tea would need to parse it).
- Reading macOS Focus or GNOME DND state. Showing DND to others as presence.
- A "huddle started" `started` timestamp in the protocol (§2.6). Ringing for channel huddles.
  Decline and busy signalling.
- Notification actions (reply from the popup), email digests, Windows support.
- Querying the terminal for OSC 99 capability (`p=?` + DA1). Env detection is enough for now.

---

## 4. Implementation notes (Go)

### 4.1 Where code goes (additive)

- `internal/notify`:
  - `type Cue uint8` (`CueNone, CueMessage, CueMention, CueRing, CueHuddle`), plus
    `func (n *Notifier) NotifyCue(title, body string, c Cue) bool`. The existing
    `Notify(title, body)` becomes `NotifyCue(…, CueMention)` (same argv, so today's tests keep
    passing).
  - `argv()` gains the per-backend sound mapping from §2.3, from fixed tables. osascript script:
    `on run argv … display notification (item 2 of argv) with title (item 1 of argv)` plus, when
    `count of argv > 2`, `sound name (item 3 of argv)`.
  - A separate in-flight slot for `CueRing` (`busyRing atomic.Bool`), and `Silent bool` for the
    ring's silent popup.
  - `osc.go`: `func oscSeq(dialect int, title, body string, urgent bool, tmux bool) []byte`
    (pure) and the `terminal` backend writing to the bell writer. `pick()` gains the SSH + dialect
    logic; `Env` already fakes `Getenv`/`LookPath`.
- `internal/tui`:
  - `notifypolicy.go`, a pure function:
    `decide(p prefs, now time.Time, ev noteEvent) (popup bool, cue notify.Cue)`, where
    `noteEvent{kind, chan, chanKind, mentionClass bool}` and prefs carries levels, DND, quiet hours
    and sound switches. `notifyFor`/`queueNote` call it *after* `shouldNotify` for mention-class
    messages, and *instead of* returning false for level-`all` channels. `shouldNotify` stays the
    single place for the bot, trusted and focus rules (unchanged).
  - `ring.go`: a ring state machine driven by huddle ephemerals, focus and keys, with tea.Tick
    timers and generation counters, like `pendingNote`.
  - Commands `/notify`, `/dnd`, `/sounds`; sidebar and header markers.
- `internal/client`: `notify_prefs` table + getters/setters (mirror `highlights.go`). The
  activity's `ActReply` logic is reused to classify "reply in my thread" (expose a
  `client.InMyThread(msg)` helper rather than duplicating it).
- `internal/voice` (build tag `voice`): `cues.go` synthesizes PCM at init and adds a `PlayCue(c)`
  that enqueues into the existing client-side mixer, below voice gain. The stub returns nil.

### 4.2 Safe exec (unchanged pattern, extended)

The current pattern stays: argv only (never `sh -c`), `exec.CommandContext` + 2 s timeout +
`WaitDelay`, no stdin, output discarded or bounded, and all remote text through `clean()` (sanitize,
one line, rune caps). New rules:

- Sound names and categories come only from constant tables. User text never selects a sound, a
  hint key or a flag.
- Keep the `--` before positional text for osascript and notify-send. Hints go before the `--`.
- OSC payloads: base64 for 99; for 9/777, `clean()` plus an explicit byte filter (reject ESC,
  `0x07`, `0x80–0x9f`) and a length cap. Fuzz it.

### 4.3 Sound assets: synthesize, don't embed

- Embedded audio needs a decoder. The voice build only has an Opus **encoder/decoder** for 48 kHz
  frames: workable for Opus-encoded cues, but each one adds an encoding and licence step. Raw PCM
  is ~96 KB per second.
- **Synthesized cues** cost ~60 lines of Go: sine partials with an ADSR envelope, generated once at
  init into `[]int16`, 0 bytes in the binary, no licence. Suggested shapes (48 kHz mono, peak −12 dBFS):
  - `join`: 660 Hz → 880 Hz, two 80 ms notes, 10 ms attack, 60 ms release.
  - `leave`: 880 Hz → 660 Hz.
  - `mic-on`: a 1.2 kHz, 20 ms tick. `mic-off`: a 700 Hz, 20 ms tick.
  - `ring`: 523 Hz + 659 Hz, a 400 ms dyad, repeated every 3 s.
  - `drop`: 660 → 440 → 330 Hz descending.
- Non-voice builds don't need any of these: every notification cue there is played by herdr, the
  OS or the terminal. The default binary's size doesn't change. **No new dependencies.**
- If designed sounds are wanted later, Kenney's CC0 Interface Sounds are the source (herdr already
  ships them). They'd be transcoded to Opus at build time and decoded with the voice build's libopus.

### 4.4 Testing

- **Policy**: a table test over level × event kind × channel kind × focus × DND/quiet × timed-mute
  expiry → (popup, cue), with a fake clock. Quiet hours that wrap midnight, and DST.
- **notify**: extend `TestPick` (SSH + TERM/LC_TERMINAL/TMUX/herdr combinations). Golden argv
  tests per backend × cue (sound name, hints, urgency, silent ring). The existing fake-binary
  approach records argv from stub executables in a temp PATH dir; also check that a hung stub
  doesn't block a ring (separate slot).
- **OSC**: golden bytes per dialect, tmux-wrapped variants, `FuzzOSCSeq` added to `make fuzz`
  (the invariant: output contains exactly one introducer and one terminator, no other control bytes).
- **TUI**: the fake `Notifier` records `(title, body, cue)`. Scenarios: a muted channel with a direct
  mention (no popup, `@` badge); an `all` channel burst (one popup, then a 30 s summary); a ring
  lifecycle (start, then stop on join, end, focus, Esc or timeout); a reconnect baseline doesn't ring;
  the narrow-pane status line fits; `/notify` output fits.
- **Voice**: cue PCM length and peak, mixer clipping with cues + 4 voices, and that `PlayCue` doesn't
  run on the audio callback's allocation path.
- **breaker**: a flood of flapping huddle ephemerals (cue and popup rate ≤ the caps); 1000 messages
  per second into an `all` channel; hostile display names and channel names in OSC and osascript paths.
- **Live checklist** (must be done before release): iTerm2, kitty, Ghostty, WezTerm, foot,
  Terminal.app (bell), each locally and over SSH, inside tmux with and without passthrough, inside
  herdr with `delivery = terminal|system|off`. Also macOS with Focus on, and GNOME plus a dunst session.

---

## 5. Risks

| Risk | Mitigation |
|---|---|
| herdr `delivery = "off"` by default, so tc notifications vanish silently (already true today) | README note + `/notify test`. Don't second-guess herdr config. |
| herdr "suppresses popups for the active tab", and possibly sound too, so a DM ring is lost while you're in another pane of the same tab | Use the status-line ring prompt and the herdr badge. Verify herdr's behaviour live; if the ring is suppressed, ask herdr upstream for an `--urgent` flag. |
| The local ring loop (voice build) bypasses macOS Focus, because tc plays it itself | Ring only in DMs, respect tc DND/quiet hours, 30 s cap, `/sounds ring off`. Document it. |
| OSC 9 misread as ConEmu/Windows Terminal `9;N` commands | Digit-prefix guard. Prefer 777/99 when detected. |
| OSC swallowed (tmux without passthrough, zellij, unknown terminal) | Env-based detection falls back to bell. In tmux, send BEL too. `/notify test`. |
| Interleaving OSC with Bubble Tea frames | One `Write` per sequence between frames, the same contract OSC 52 already relies on. |
| notify-send `sound-name` ignored by some servers (dunst, Xfce) | Accept silence there rather than add a player (no double sounds elsewhere). Document `TC_NOTIFY=bell` as the fallback. |
| osascript notifications blocked until Script Editor is allowed (Sequoia) | `/notify test` + README note. terminal-notifier is a later option. |
| Ring spam via a malicious member who joins and leaves a DM huddle repeatedly | One ring per huddle instance + a 5 min per-DM ring cooldown after a dismissed or timed-out ring. Muting the DM stops it. |
| Popups with E2E content stored in OS history | `/notify content name\|none`, documented. |
| Local-only prefs surprise multi-device users | `/notify` says "this device". Sync is in M6. |
| Scope creep vs the voice PR | §4.1 is additive; the only voice touch point is `PlayCue` plus calls from the huddle UI. Ship policy, levels and privacy first (no voice dependency), then OSC, then the huddle cues and ring. |
