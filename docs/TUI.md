# tc TUI design (M1)

Minimal, keyboard-first, works in a 40-column herdr split. Bubble Tea + Lip Gloss.

## Layout

```
┌──────────────┬───────────────────────────────────────────────┐
│ tc · online  │ #general — team-wide chatter                  │
│              │                                               │
│ CHANNELS     │ 10:42 alice  morning! anyone seen the deploy  │
│ # general  3 │              logs from last night?            │
│ # backend    │ 10:43 bob    yep, pasting:                    │
│ # secret  @  │              ┃ panic: nil map write           │
│              │              ┃   at handler.go:42             │
│ DIRECT       │ 10:44 alice  👍 1   ↳ 2 replies                 │
│ ● bob        │                                               │
│ ○ carol      │ bob is typing…                                │
│              ├───────────────────────────────────────────────┤
│              │ > message #general                            │
└──────────────┴───────────────────────────────────────────────┘
```

- Sidebar: 18 columns; hidden automatically below 70 columns total (toggle `Ctrl-B`).
- Header: channel name + topic (one line, truncated). Status (Connecting/Offline/Reconnecting) shows in
  the sidebar title, dim, only when not Online.
- Messages: `HH:MM author  body`. Consecutive messages from the same author within 5 min omit the
  time/author. Day separators (`── Tue 9 Oct ──`). Wrapped bodies align under the body column.
- Pending (outbox) messages render dim; failed ones show `! failed — r to retry, d to discard`.
- Edited: dim ` (edited)`. Deleted: dim italic `message deleted` (encrypted channels: `deleted (unverified)` until judged). In encrypted channels `(edited)` shows
  only for an edit the author signed, reactions only when signed by the reactor, and a delete the author
  didn't sign (an admin, the server) reads `removed (not by author)`. `e` refuses messages that aren't
  verified (waiting, invalid, legacy or inserted plaintext).
- Pinned messages end with a dim `📌` on their footer line; the header shows a dim `📌 N` when the channel has pins.
- Reactions on a line below: `👍 2  🎉 1` (your own reactions highlighted).
- Thread roots show `↳ N replies`; threads open in a right pane (or full width below 100 cols).
- An empty channel shows a dim `no messages yet — say hi · F1 help · Ctrl-K channels & people` (shorter when the message pane is under 64 or 26 columns; in my notes `your notes — only you see them; /remind lands here`) once a history fetch said it is empty.
- Typing line above the input; nothing when nobody is typing.
- Input: single line that grows to max 6 lines; `Alt-Enter`/`Ctrl-J` inserts newline; `Enter` sends.

## Rendering rules

- Every user-controlled string goes through `sanitize.Text`/`sanitize.Line` before Lip Gloss.
- Markdown-lite only: ```` ``` ```` fenced blocks (left bar `┃`, no wrap reflow, horizontal truncation
  with `…`), inline `` `code` ``, `**bold**`, `_italic_`, URLs underlined. No headings, no tables.
- `@me` mentions highlighted; the message gets a marker in the gutter: `▎` when it mentions me, `*`
  when it only contains one of my highlight words.
- Author colors: stable hash of user id into an 8-color palette that reads on dark and light terminals.
  Respect `NO_COLOR` (no colors, keep bold/dim).
- Fenced blocks: an info string (```` ```go ````) shows a dim right-aligned label on the block's first line
  when it fits next to that line's text (no syntax highlighting). A ```` ```diff ```` block, or an unlabeled
  block that looks like a unified diff (`diff --git`, or `---`/`+++` headers plus `@@ -` hunk), colors `+` green,
  `-` red, `@@` dim cyan, file headers dim bold; under `NO_COLOR` only the `+`/`-` markers remain. The full-message
  viewer (`v`) colors diffs the same way. `git diff | tc send` produces a diff block automatically.
- Width math uses display width (runewidth), never byte or rune counts.

## Keys

| Key | Action |
|---|---|
| `Enter` | send (completes instead while the completion popup is open, unless the `@`/`#` word already equals the highlighted item) |
| `/`, `@`, `#` | completion popup above the input (see below) |
| `Tab` | complete from the popup; reopens it after `Esc` |
| `Ctrl-K` | quick switcher (fuzzy over channels + people; Enter opens; DMs created on demand) |
| `Alt-A` | activity view (also `/activity`); `Ctrl-A` stays the textarea's line start |
| `Alt-↑/↓` or `Ctrl-P/N` | previous / next channel (`Ctrl-P/N` move the completion popup's highlight while it is open) |
| `Alt-Shift-↑/↓` | previous / next unread channel |
| `PgUp/PgDn`, mouse wheel | scroll history (fetches older via History at top) |
| `↑` on empty input | select messages mode (↑/↓ moves selection) |
| in select mode: `e` edit (own), `d` delete (own, confirm), `r` reply in thread, `+` react (prompt for emoji; `+1`, `:tada:` shortcuts), `y` copy body (OSC 52), `p` pin / unpin (notice "pinned" / "unpinned"), `Esc` leave |
| `Esc` | close the completion popup / thread / switcher / select mode; never quits |
| `Ctrl-C` twice (the second while `press Ctrl-C again to quit` shows, 4 s), or `/quit` | quit |
| `F1` or `/help` | help overlay (`?` is just text, so messages can start with it) |

## Slash commands

`/join #x`, `/leave`, `/create #x [--private [--plaintext]]`, `/add @user`, `/topic text`, `/dm @a @b` (`/dm @me` or my own name: my notes, also in `Ctrl-K`; `@me` is always me, even if an account is named `me`; after `/dm` the popup offers `@me` and my name),
`/invite` (admin; prints code), `/away [status]`, `/back`, `/search terms` (results list; Enter jumps), `/pins` (overlay of this channel's pinned messages, newest pin first, as `author · preview`; Enter jumps, Esc closes),
`/activity`, `/highlight [word…]`, `/unhighlight word…`, `/group name @a @b…` (admin), `/groups`,
`/remind <when> <text>`, `/reminders`, `/remind-delete id`, `/snippet path[:range] [message]`,
`/share-pane [pane]` (herdr only), `/verify @user [--github login]`, `/accept-key @user`, `/members`,
`/alarm`, `/alarm-clear #name`, `/devices [@user]`, `/device-revoke id`, `/disable @user` (admin), `/quit`.
`/invites` and `/webhooks` open a list, one per row. The invite and webhook viewers take the full width (no sidebar) so a mouse selection copies only the command.
`/webhook name` copies the curl command; when no system clipboard tool took it (only OSC 52, which Terminal.app and tmux without `set-clipboard on` ignore) a viewer opens with the command hidden: `s` shows it (it holds the secret URL), `y` copies again, Esc closes and forgets it.
`/topic` or `/name` alone shows the current topic or display name; `--clear` removes it. Commands that change something confirm it in the status line (`added @bob`, `topic set`, `away: lunch`).
Unknown command → inline error, input kept.

### Completion popup

While the input is a bare `/command` (no space yet) a popup of up to 4 rows just above the input lists the
matching commands (prefix matches first, then substring; an exact name first) with their argument hint and a
dim one-line description; more matches scroll (`↑`/`↓` marks at the right). An `@` or `#` word at the cursor
does the same for people (this channel's members first, then everyone else, then user groups like
`@oncall`) and channels (joined ones first); a name equal to what was typed always comes first. For
`@`/`#` a substring match needs two letters and never comes from digits alone, so `fixed in #12` isn't offered `#release-12`. The popup covers the bottom of the
message list, above the status line (notices and connection state stay visible), with a dim `▏` gutter;
it is never drawn outside normal mode.

- `↑`/`↓` or `Ctrl-P`/`Ctrl-N` move the highlight (wrapping). It stays on the same item when users or channels change under the open popup.
- `Enter` or `Tab` puts the highlighted item into the input followed by a space (a whole `@`/`#` word is
  replaced, cursor mid-word included) and closes the popup. On the `/` menu `Enter` **never runs** the
  command — even when the text already equals the highlighted command; the next `Enter`, with the popup
  closed, runs it as usual. For an `@`/`#` word that already equals the highlighted item, ignoring case (`thanks @bob`
  with `@bob` highlighted) there is nothing to complete, so `Enter` closes the popup and sends on the first
  press; `Tab` still completes (adds the space).
- The popup follows edits, not cursor moves: moving the cursor (←/→, ↑/↓ in a multi-line draft, Home/End)
  hides it and never opens it, so those keys and `Enter` keep their usual meaning. It also doesn't follow a
  draft into another channel or thread.
- `Esc` closes it without touching the input; it stays closed until the input changes. `Tab` reopens it, completing at once when there is a single match. `Esc` order: popup, then the
  open mic, then a ring, then the usual chain.
- Pastes and multi-rune bursts never open it, so pasted text is sent by the first `Enter`. `//text`, text
  being edited (`e`) and plain messages never get the command menu.
- Admin-only commands are hidden from non-admins; `/talk`, `/mute` and `/leave-huddle` are hidden in builds
  without voice (`/huddle` stays: it explains how to get voice).
- The command list and `/help` come from one table (`commandTable`, internal/tui/commands.go).

`/create #x --private` makes an end-to-end encrypted channel (🔒; the server never sees its text).
`--plaintext` opts out, e.g. for a private channel webhooks post into (webhooks can't post into encrypted
channels). Private channels made by older tc versions (whose `/create --private` didn't encrypt) stay unencrypted
until someone runs `/encrypt` there (earlier messages are then marked legacy); their header has no 🔒.

## Verifying people (encrypted channels)

- **`?` marks**: a member trusted only on first use (their SSH key was pinned the first time this device
  saw it, or a changed key was accepted since the last verification) gets a dim `?`: right after the name
  in a DM's header (`@bob?`), as `?N unverified` after the 🔒 in a channel header, and `? @bob` in
  `/members` (`!` = key changed). The first time a channel with such members opens in a run, a dim
  notice suggests `/verify @bob`. Plaintext channels show none of this.
- **`/verify @bob`** opens an overlay: the 60-digit safety number (12 groups of 5, 4 per row; 2 below 26
  columns), then both users' SSH key fingerprints and the instructions. The title always says `v: they
  match · Esc: cancel`; ↑/↓/PgUp/PgDn scroll when it doesn't fit. `v` marks bob's pinned keys verified
  (notice `✓ @bob verified`; if his keys changed meanwhile the client refuses and says to compare again).
  All trust overlays ignore keys except Esc for 500 ms after opening, so type-ahead can't confirm.
- **`/verify @bob --github login`** fetches `github.com/login.keys` (only on this command) and shows a
  green `✓ every key … is on github.com/login` plus "Press v to verify @bob because you confirmed
  github.com/login is theirs" (title `v: verify · Esc: cancel`), or a red `✗ MISMATCH` with each pinned key
  marked `✓`/`✗` and the account's keys listed. Nothing changes without `v`, or on a mismatch.
- **Key-change banner**: when a member of the active encrypted channel presents a new SSH key that no
  pinned key endorsed, one red line under the header reads `⚠ /accept-key @bob, then /verify @bob —
  @bob's key changed` (the command first, so it survives truncation; several: `⚠ /accept-key, then
  /verify each — keys changed: @bob, @carol`). Enter, edits and reactions are refused (input kept, red
  notice); deletes and receiving go on. `/accept-key @bob [fingerprint]` opens an overlay listing the
  pending fingerprints; `y` accepts exactly those (if the pending set changed meanwhile it refuses and
  shows the new set), Esc cancels. Accepting pins the new key unverified, so bob gets his `?` back.
- **Alarms**: the header shows `⚠ alarm: <detail>` in red (fork, rollback, plaintext downgrade, server
  dropped e2e, unconfirmed member) and sending is blocked. `/alarm` opens a list with the detail, when it
  was raised, what it means and how it can happen, and how to clear it. `/alarm-clear` alone or with a
  wrong name only says what to type; `/alarm-clear #name` (`@bob` in a DM) clears it, recorded in the
  trust store's audit log. For the server-wide alarm the list, prompt and result say "server-wide —
  clears the alarm for all encrypted channels". Alarms derived from current state (unconfirmed member)
  can't be cleared.

## Devices

- **`/devices [@user]`** opens an overlay listing the user's devices (default: yours), cached at once
  and refreshed from the server in the background (the title says `· refreshing…`, or `· cached (…)`
  when the refresh failed): `* ab12cd34  this device  added 2026-10-01`, then `active`,
  `revoked 2026-10-17` and `unverified` (a cert no pinned key of the user signed: only the server's
  claim) rows. `old id of this device, …` (`old id, …` under 50 columns) prefixes a verified id with
  the keys this device had before it was revoked and re-registered. The signing key's fingerprint is
  added only when it fits whole. ↑/↓ (k/j), PgUp/PgDn, Home/End move, `r` refreshes, Esc, `q` or
  Enter closes. Another user's list is read-only.
- In your own list, `d` on an active, verified device that isn't this one puts the question in the
  title, in red (`revoke device ab12cd34 for good? y: revoke · any other key: cancel`; under 60 columns
  `revoke ab12cd34? y: yes · other: no`). Unlike the trust overlays, any key other than `y` cancels
  ("not revoked"). `y` revokes it and the list refreshes. The notice (10 s) is `✓ revoked device
  ab12cd34 — new keys in N encrypted channels · had your SSH key? an admin must disable your account`,
  without the channel count when the line is too narrow: a revoke doesn't remove an SSH key (docs/E2E.md
  "Stolen device"). `d` on this device, a revoked or an unverified one says why not instead. The
  overlay ignores keys except Esc for 500 ms after opening, like the trust overlays.
- **`/device-revoke id`** (any unique prefix of the id `/devices` shows) opens the list with that
  device's question already up. Revoking this device is only possible from the CLI
  (`tc devices revoke id --self`).
- Notices: when a verified revoke of a device this client already trusted arrives — in red `this device
  was revoked — it re-registers with new keys when it reconnects`; when it was revoked while offline
  and has re-registered already, `this device re-registered — /devices (was ab12cd34)`, or, if a new
  device of yours appeared meanwhile, `new device of yours: dd34ef56 — not you? /devices to revoke it ·
  this device re-registered (was ab12cd34)`; `your device ab12cd34 was revoked — /devices`; otherwise
  `@bob revoked a device (ab12cd34) — encrypted channels get a new key before the next message` (not
  over an error notice). A newly verified device of yours shows in red `new device of yours: ab12cd34 —
  not you? /devices to revoke it`. Several at once add `(+N more)`. Nothing is announced on a first
  sync (before any device of yours is verified).

## Disabling an account (admins)

- **`/disable @user`** shuts an account out for good, e.g. after its SSH key or a laptop holding it was
  stolen. The question takes over the body: two red lines first, so they survive any size
  (`disable @eve for good?` and `y: disable · any other key: cancel`, under 40 columns `y: yes · other:
  no`), then `It can't be undone.` and what it does: every session signed out now, none of their keys
  can sign in again, their unused invites and webhooks deleted, encrypted channels they were in
  re-keyed; to let them back in, invite them as a new account (new key and name). A background notice
  can't replace the question. Keys in the first 500 ms are ignored (Esc still cancels); `y` disables
  (`disabling @eve…`, then `✓ disabled @eve — signed out everywhere; none of their keys can sign in`),
  any other key cancels (`not disabled`). One disable runs at a time (`a disable is still running`).
  Members get `only admins can disable users`; yourself, an unknown or an already disabled user is an
  inline error with the input kept. From a shell: `tc users disable @eve`.

## Behavior

- Opening a channel marks it read up to the newest message (MarkRead) when the view is at the bottom.
  Scrolled up → don't mark read; show `↓ N new` pill.
- Unread counts in sidebar; `@` instead of count when there's a mention or it's a DM (bot
  messages never make a mention).
- Notifications: see "Notifications & herdr" below.
- Never block the UI on network: all client calls run in `tea.Cmd`s.
- Startup renders from cache immediately, before connecting.

## Drafts

- The input keeps its text per place to type: each channel, and each thread of it. Switching channel,
  opening a thread (`r`, a search or activity jump) or closing it saves what is in the input and
  restores the target's draft. A draft is cleared by sending it (or `Ctrl-U`, or `Esc` twice).
- Drafts live in the client cache (`drafts` table, local only, never sent anywhere) and survive restarts
  and snapshot resets. They are written at once on a switch, on clear/send and on quit, and 1 s after
  typing pauses otherwise; a crash can lose at most that last second of typing.
- A dim `✎` after a channel's name in the sidebar (and in `Ctrl-K`) means it has a draft that the input
  isn't showing: another channel's, or the channel's own while one of its threads is open.
- Edit text (`e`) is never a draft. Starting an edit puts the input's text (for instance the draft a
  jump left there) aside; saving, cancelling (`Esc`), switching channel, `Alt-A` or a jump ends the edit
  and brings the draft back.
- Several `tc` processes can share one cache: the slot a switch lands in is re-read from the cache
  unless it has unsaved changes here, and the `✎` marks follow the cache on every channel update.
  Draft text read from the cache goes through `sanitize.Text`. A failed write keeps the draft pending
  and is retried.
- Leaving a channel, a login on another account or a change of server identity drops its drafts.

## Highlight words

- `/highlight deploy oncall` adds words; `/highlight` alone lists them; `/unhighlight deploy` removes one.
  Words are lowercase, a single word (no spaces), at most 64 bytes, at most 50 in all. Stored in the
  client cache (`highlights` table), local to this machine, kept across resets.
- A message **by a person** (not mine, not a bot's, not deleted) containing a word — case-insensitive,
  **whole word**: `deploy` matches "Deploy failed" and `deploy-prod` but not `redeploy` or `deployment` —
  is treated like an `@mention`: a `*` gutter marker in the log (an actual `@mention` keeps `▎`), `@`
  instead of a count on the channel's unread badge (`ChannelView.Mention`), and the notification rules
  below, and it appears in the activity view. Bot (webhook) messages never match, whatever they say, and
  neither does text inside a fenced code block (pasted logs would match everything).
- The words are re-read from the cache on every channel update, so a `/highlight` made in another pane
  takes effect here without a restart; `/highlight` and `/unhighlight` work on the fresh list.

## User groups

- `/group oncall @alice @bob` (admin) sets the group `@oncall` to exactly those people; `/group oncall` with
  no people deletes it; `/groups` lists them in a read-only overlay (`↑/↓` move, `Esc` closes; it reads the
  cache, so it works offline). Names follow the channel/user name rule, can't be a user's name and can't end
  in `-bot`; at most 50 members.
- A message **by a person** containing `@oncall` (whole word, like `@name`) counts as an `@mention` for each
  member: `▎` gutter marker, `@` unread badge, activity view "mention" item, and the notification rules
  below. Bots' messages and my own never do. The groups come from the client cache and follow
  `group.update` events live.

## Reminders

- `/remind <when> <text>` asks the **server** to post `⏰ <text>` in my private notes (the DM with only
  me) at that time, so it fires with tc closed; a reminder due while the server was down arrives when it
  restarts. If set from a channel (not from the notes) the text gets ` (from #channel)` while I can still
  read it.
- `<when>`, optionally after `in` or `at`: `10m` `2h` `1d` `1w` `1h30m` (from now); `15:30`, `9:05`, `9am`,
  `9:30pm` (the next time my wall clock shows that: today, or tomorrow if it has passed; `12am` is
  midnight); `tomorrow 9am` / `tomorrow 15:30`. Times are the local zone's wall-clock time, converted to an
  absolute time on this machine before sending; "tomorrow" is the next calendar day even across a
  daylight-saving change. At most a year ahead, 4 KiB of text, 100 pending per user.
- `/reminders` lists mine with ids in an overlay (`id  when  text (from #chan)`); `/remind-delete id`
  cancels one.
- A delivered reminder is my own message, which never notifies, with one exception: a message of mine in my
  notes that starts with `⏰ ` notifies (`⏰ Reminder`, the text) unless I'm looking at the notes, and
  counts as unread (`@` badge) until read.

## Activity view

`Alt-A` or `/activity` opens an overlay over the log with the last 100 things that concern me, newest
first, across all channels I'm in. Everything comes from the local cache (works offline; ~13 ms for the
whole list over 10k cached messages, see `client.Activity`).

```
 activity — 4 items · ↑/↓ move, Enter jumps, Esc closes
•🎉 10:49 · #general · carol · on: nice, the deploy is out
 ↳  10:46 · #general · carol · thanks, merging now
 »  10:45 · @bob · bob · psst
 @  10:44 · #general · bob · hey @alice can you look?
```

The reason marker comes first. A dim `•` in the first column marks items newer than the last time the
overlay was open (the time of the newest item shown is kept in the cache's `meta` table as
`activity_seen`; nothing is dotted the very first time). Below 60 columns rows drop the time, below 48
the author as well, and the channel name is cut to 10 cells; the title shrinks to
`activity N · Enter · Esc`.

| Kind | Marker | What counts |
|---|---|---|
| mention | `@` | a message by a person with `@me` |
| highlight | `*` | a message by a person with one of my highlight words |
| reply | `↳` | a reply by someone else in a thread I started, or in one I had already replied to |
| reaction | the emoji (`on: …`) | someone else reacted to my message; the preview is my message |
| DM | `»` | any message by someone else in a direct message |

A message that fits several kinds is listed once, as the first of the order above. Never listed: my own
messages, bots, deleted messages, channels I'm not in. Keys: `↑/↓` (`Ctrl-P/N`, `k/j`) move, `PgUp/PgDn`,
`Home/End`, the mouse wheel (3 rows a notch), `Enter` jumps, `Esc`/`Alt-A`/`q` closes. Closing and
reopening (also `Alt-A` from select mode after a jump) keeps the selection while the list is unchanged.
**Enter** opens the channel, opens the thread when the item is a reply, and selects the message (select
mode). An old message widens the loaded page only while the selection needs it (Esc, moving back into a
normal page or leaving the channel restores the old limit); a thread reply whose history is still
loading is selected when it arrives. Every field is sanitized and the row is truncated to the width.

While open the list is re-read only when a message arrives in a channel I'm in (or the channels change),
at most once a second. The mention, reply and DM scans cover the newest 5000 messages within 30 days of
the newest one; a non-ASCII highlight word is matched in SQL (`tc_lower`) instead of decoding every row.

Limit: reactions are only known from `react.add` events this client has seen (live or replayed). A server
snapshot carries who reacted but not when, so reactions that predate the cache can't be placed in time
and are not listed; they appear in the log as usual. Rows are kept for the newest 1000 reactions to me.

## Notifications & herdr

- **When**: a DM or a message that @mentions me or contains one of my highlight words, not my own and not from a webhook bot
  (bot messages count as unread but never as mentions — their text comes from outside),
  and either the terminal is blurred (focus reporting) or the message is in a channel that
  isn't the active one.
- **What**: title `#chan · alice` or `alice (DM)`, body = first non-empty line. Title ≤ 60 runes,
  body ≤ 160, both through `sanitize.Line`.
- **Coalescing**: the first message in a channel notifies at once and opens a 5 s window; more
  messages in that window become one `N new messages` notification when it closes. At most one
  notification per 2 s overall (a message over the cap waits for its window). Opening the channel
  drops what's pending for it.
- **Backend** (`internal/notify`), chosen by `TC_NOTIFY=auto|herdr|os|terminal|bell|off` (default `auto`):
  auto = `herdr notification show` when `HERDR_ENV=1` and `herdr` is in PATH; else, **over SSH**
  (`SSH_CONNECTION`/`SSH_TTY`) with a terminal known to support it, the `terminal` backend; else
  the OS (`osascript` on macOS — text passed as argv to an `on run argv` script, never
  interpolated; `notify-send` on Linux), else the terminal bell. `TC_BELL=0` still turns the bell
  off. Every exec backend runs via argv (no shell), async, 2 s timeout, one at a time (extra ones
  dropped); a ring has its own slot so a hung popup can't swallow it.
- **Terminal backend** (OSC; the local terminal shows it, so it works over SSH): OSC 99 for
  kitty (`TERM=xterm-kitty`), OSC 777 for Ghostty/WezTerm/foot, OSC 9 for iTerm2
  (`LC_TERMINAL=iTerm2`, forwarded by macOS ssh); `TC_NOTIFY_OSC=9|777|99` forces one. Inside
  tmux it's wrapped for passthrough (`set -g allow-passthrough on`) plus a BEL; never inside herdr.
  Text is filtered to printable characters (no ESC/BEL/C1), ≤ 256 bytes; OSC 9 text starting with
  a digit gets a `tc · ` prefix (Windows Terminal/ConEmu read `9;4` as progress).
- **Sounds**: one event, one sound, played by whatever shows the popup — herdr `--sound`
  (`request` for mentions and rings, `done` for channel huddle starts, `none` for level-`all`
  messages), macOS `sound name` (Glass, Tink, Submarine, Pop), notify-send `sound-name` hints
  (`message-new-instant`, `phone-incoming-call`, `service-login`) and categories, the terminal's
  own, or one BEL. tc never starts a second player and ships no audio files. In a huddle (voice
  build) short synthesized cues play through the call's audio: someone joined / left (not in
  huddles of 8+, not right after you join, at most one a second) and mic on/off clicks.
- **Levels** (`/notify`, per channel, this device only): `mentions` (default: DMs, @mentions,
  highlights, reminders, replies in threads I started or replied in, DM huddle rings), `all`
  (every message — 30 s coalescing, soft sound — and channel huddle starts), `off` (muted: no
  popup, sound or ring; the sidebar dims it and drops its count, keeping `@` for a direct mention;
  its unread is left out of the header/herdr summary; the header shows `🔕`, or `🔕 until 14:30`
  for a timed mute). `/notify off for 2h` lapses back to the default.
- **Huddles**: a huddle someone starts in a DM or group DM **rings**: one popup with the ring
  sound (sent again once after 10 s), and the status line shows `📞 alice — /huddle to join · Esc
  dismiss` (compact `📞 alice /huddle` in narrow panes) until you join (from any device), it ends,
  you open that DM, Esc, `/dnd`/mute, or 30 s. A DM rings at most once per 2 min however the ring
  ended (a peer flapping join/leave), any DM at most once per 10 s, and a dismissed or unanswered
  ring keeps that DM quiet for 5 min. A second DM huddle while one rings gets a plain popup. The
  starter must be a member of the DM (rosters aren't end-to-end authenticated). Never for rosters
  a sync or channel join hands us (reconnects don't ring), nor for a late roster that already has
  several people. Esc closes an open mic first, then dismisses the ring. Channel huddle starts are silent at `mentions` (the sidebar
  `🎧n` shows them); at `all`, one popup per channel per 5 min.
- **Privacy**: `/notify content full|name|none` (`TC_NOTIFY_CONTENT`): `name` shows who but not
  the text, `none` shows just `tc` / `new message`. Popups persist in the OS notification history
  (Notification Center, GNOME's tray): use `name` on shared machines.
- **DND and quiet hours**: `/dnd [30m|2h|until 9am|off]` (bare `/dnd` toggles 1 h) and
  `/notify quiet 22:00-08:00|off` (`TC_QUIET`, local time, may wrap midnight) silence popups,
  sounds and rings but not badges or the activity view; the status line shows a dim `🔕 09:00`
  or `🔕 quiet` (times beyond today show the day). Nothing is replayed afterwards. A mention in a
  level-`all` channel never waits out its 30 s window.
- **Commands**: `/notify` (current settings, one per row in a list with the command that changes each, hints dropped under 50 columns; Esc closes; lists that don't fit end with `↑ N · ↓ N more`), `/notify all|mentions|off [#chan] [for 2h]`,
  `/notify default [#chan]`, `/notify global all|mentions|off`, `/notify content …`,
  `/notify quiet …`, `/notify test` (one sample per sound through the real backend, 2.5 s apart —
  shows whether herdr toasts, macOS permission or OSC support work), `/dnd …`, `/sounds`
  (list), `/sounds on|off` (`TC_SOUND`), `/sounds mention|message|ring|huddle|join|mic on|off`,
  `/sounds volume 0-100` (huddle cues only). `/mute` stays the mic. Env vars win over stored
  settings. Settings live in the local cache (`notify_prefs`, `meta`), never on the server.
- **Unread badge** (herdr only): the pane's herdr title is set to `tc · 3 unread · 1@` (same
  counts as the header summary) with `herdr pane report-metadata <pane> --source tc --title …`,
  or `--clear-title` when nothing is unread. Reported only when the text changes, 1 s after the
  last change, off the UI loop; cleared on exit.
- **`/share-pane [pane]`** (herdr only; otherwise an error): no argument opens a picker of the
  other panes in this workspace (current tab first, my pane excluded; label/agent/title + id);
  an argument matches a pane id or label. The pane's last 60 lines (`herdr pane read <id>
  --source recent --lines 60`) are stripped of escape sequences and shown in a preview that is
  exactly the message to be sent: ``📋 `label` `` + a fenced block. Enter sends to the current
  channel (or open thread), Esc cancels. Over 64 KiB keeps the tail with a `… (N bytes truncated)` note.
- **`/snippet path[:start[-end]] [message]`**: reads the file (relative to the directory tc was started in),
  `:N` = line N +/- 3, no range = whole file (error over ~63 KiB, binary or non-UTF-8). Builds the same message as
  `tc send --file`: optional text, a header `` `path:40-60` `` plus a GitHub/GitLab permalink at HEAD (or
  `(uncommitted)` and no link if the file is untracked or modified), and a fenced block with the language from the
  extension. Shown in the /share-pane preview: Enter sends, Esc cancels.
- **Auto-away**: after `TC_AWAY_MIN` minutes (default 15, `0` disables) without a key press the
  presence goes away; the next key brings it back. A manual `/away` is never overridden (only
  `/back` ends it).
