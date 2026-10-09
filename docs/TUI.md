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
- Reactions on a line below: `👍 2  🎉 1` (your own reactions highlighted).
- Thread roots show `↳ N replies`; threads open in a right pane (or full width below 100 cols).
- Typing line above the input; nothing when nobody is typing.
- Input: single line that grows to max 6 lines; `Alt-Enter`/`Ctrl-J` inserts newline; `Enter` sends.

## Rendering rules

- Every user-controlled string goes through `sanitize.Text`/`sanitize.Line` before Lip Gloss.
- Markdown-lite only: ```` ``` ```` fenced blocks (left bar `┃`, no wrap reflow, horizontal truncation
  with `…`), inline `` `code` ``, `**bold**`, `_italic_`, URLs underlined. No headings, no tables.
- `@me` mentions highlighted; whole message line gets a subtle marker (`▎` in the gutter) when it
  mentions me or contains one of my highlight words.
- Author colors: stable hash of user id into an 8-color palette that reads on dark and light terminals.
  Respect `NO_COLOR` (no colors, keep bold/dim).
- Width math uses display width (runewidth), never byte or rune counts.

## Keys

| Key | Action |
|---|---|
| `Enter` | send |
| `Ctrl-K` | quick switcher (fuzzy over channels + people; Enter opens; DMs created on demand) |
| `Ctrl-A` | activity view (also `/activity`); shadows the textarea's "line start", use `Home` |
| `Alt-↑/↓` or `Ctrl-P/N` | previous / next channel |
| `Alt-Shift-↑/↓` | previous / next unread channel |
| `PgUp/PgDn`, mouse wheel | scroll history (fetches older via History at top) |
| `↑` on empty input | select messages mode (↑/↓ moves selection) |
| in select mode: `e` edit (own), `d` delete (own, confirm), `r` reply in thread, `+` react (prompt for emoji; `+1`, `:tada:` shortcuts), `y` copy body (OSC 52), `Esc` leave |
| `Esc` | close thread / switcher / select mode; never quits |
| `Ctrl-C` twice, or `/quit` | quit |
| `F1` or `/help` | help overlay (`?` is just text, so messages can start with it) |

## Slash commands

`/join #x`, `/leave`, `/create #x [--private]`, `/add @user`, `/topic text`, `/dm @a @b`,
`/invite` (admin; prints code), `/away [status]`, `/back`, `/search terms` (results list; Enter jumps),
`/activity`, `/highlight [word…]`, `/unhighlight word…`, `/share-pane [pane]` (herdr only), `/quit`.
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
- Edit text (`e`) is never a draft; abandoning an edit (`Esc`, switching channel) discards it.
- Leaving a channel, a login on another account or a change of server identity drops its drafts.

## Highlight words

- `/highlight deploy oncall` adds words; `/highlight` alone lists them; `/unhighlight deploy` removes one.
  Words are lowercase, a single word (no spaces), at most 64 bytes, at most 50 in all. Stored in the
  client cache (`highlights` table), local to this machine, kept across resets.
- A message **by a person** (not mine, not a bot's, not deleted) containing a word — case-insensitive,
  **whole word**: `deploy` matches "Deploy failed" and `deploy-prod` but not `redeploy` or `deployment` —
  is treated exactly like an `@mention`: a `▎` gutter marker in the log, `@` instead of a count on the
  channel's unread badge (`ChannelView.Mention`), and the notification rules below, and it appears in the
  activity view. Bot (webhook) messages never match, whatever they say.

## Activity view

`Ctrl-A` or `/activity` opens an overlay over the log with the last 100 things that concern me, newest
first, across all channels I'm in. Everything comes from the local cache (works offline; ~13 ms for the
whole list over 10k cached messages, see `client.Activity`).

```
 activity — 4 items · ↑/↓ move, Enter jumps, Esc closes
 10:49 · #general · carol · 🎉 on: nice, the deploy is out
 10:46 · #general · carol · ↳ thanks, merging now
 10:45 · @bob · bob · psst
 10:44 · #general · bob · @ hey @alice can you look?
```

| Kind | Marker | What counts |
|---|---|---|
| mention | `@ ` | a message by a person with `@me` |
| highlight | `* ` | a message by a person with one of my highlight words |
| reply | `↳ ` | a reply by someone else in a thread I started, or in one I had already replied to |
| reaction | `<emoji> on: ` | someone else reacted to my message; the preview is my message |
| DM | | any message by someone else in a direct message |

A message that fits several kinds is listed once, as the first of the order above. Never listed: my own
messages, bots, deleted messages, channels I'm not in. Keys: `↑/↓` (`Ctrl-P/N`, `k/j`) move, `PgUp/PgDn`,
`Home/End`, `Enter` jumps, `Esc`/`Ctrl-A`/`q` closes. **Enter** opens the channel (widening the loaded page
if the message is old), opens the thread when the item is a reply, and selects the message (select mode).
Every field is sanitized and the row is truncated to the width.

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
- **Auto-away**: after `TC_AWAY_MIN` minutes (default 15, `0` disables) without a key press the
  presence goes away; the next key brings it back. A manual `/away` is never overridden (only
  `/back` ends it).
