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
- Edited: dim ` (edited)`. Deleted: dim italic `message deleted`.
- Pinned messages end with a dim `📌` on their footer line; the header shows a dim `📌 N` when the channel has pins.
- Reactions on a line below: `👍 2  🎉 1` (your own reactions highlighted).
- Thread roots show `↳ N replies`; threads open in a right pane (or full width below 100 cols).
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
| `Enter` | send |
| `Ctrl-K` | quick switcher (fuzzy over channels + people; Enter opens; DMs created on demand) |
| `Alt-A` | activity view (also `/activity`); `Ctrl-A` stays the textarea's line start |
| `Alt-↑/↓` or `Ctrl-P/N` | previous / next channel |
| `Alt-Shift-↑/↓` | previous / next unread channel |
| `PgUp/PgDn`, mouse wheel | scroll history (fetches older via History at top) |
| `↑` on empty input | select messages mode (↑/↓ moves selection) |
| in select mode: `e` edit (own), `d` delete (own, confirm), `r` reply in thread, `+` react (prompt for emoji; `+1`, `:tada:` shortcuts), `y` copy body (OSC 52), `p` pin / unpin (notice "pinned" / "unpinned"), `Esc` leave |
| `Esc` | close thread / switcher / select mode; never quits |
| `Ctrl-C` twice, or `/quit` | quit |
| `F1` or `/help` | help overlay (`?` is just text, so messages can start with it) |

## Slash commands

`/join #x`, `/leave`, `/create #x [--private]`, `/add @user`, `/topic text`, `/dm @a @b`,
`/invite` (admin; prints code), `/away [status]`, `/back`, `/search terms` (results list; Enter jumps), `/pins` (overlay of this channel's pinned messages, newest pin first, as `author · preview`; Enter jumps, Esc closes),
`/activity`, `/highlight [word…]`, `/unhighlight word…`, `/snippet path[:range] [message]`, `/share-pane [pane]` (herdr only), `/quit`.
Unknown command → inline error, input kept.

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
- **Backend** (`internal/notify`), chosen by `TC_NOTIFY=auto|herdr|os|bell|off` (default `auto`):
  auto = `herdr notification show` when `HERDR_ENV=1` and `herdr` is in PATH, else the OS
  (`osascript` on macOS — text passed as argv to an `on run argv` script, never interpolated;
  `notify-send` on Linux), else the terminal bell. `TC_BELL=0` still turns the bell off.
  Every backend runs via exec argv (no shell), async, 2 s timeout, one at a time (extra ones dropped).
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
