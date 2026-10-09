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
- `@me` mentions highlighted; whole message line gets a subtle marker when it mentions me.
- Author colors: stable hash of user id into an 8-color palette that reads on dark and light terminals.
  Respect `NO_COLOR` (no colors, keep bold/dim).
- Width math uses display width (runewidth), never byte or rune counts.

## Keys

| Key | Action |
|---|---|
| `Enter` | send |
| `Ctrl-K` | quick switcher (fuzzy over channels + people; Enter opens; DMs created on demand) |
| `Alt-↑/↓` or `Ctrl-P/N` | previous / next channel |
| `Alt-Shift-↑/↓` | previous / next unread channel |
| `PgUp/PgDn`, mouse wheel | scroll history (fetches older via History at top) |
| `↑` on empty input | select messages mode (↑/↓ moves selection) |
| in select mode: `e` edit (own), `d` delete (own, confirm), `r` reply in thread, `+` react (prompt for emoji; `+1`, `:tada:` shortcuts), `y` copy body (OSC 52), `Esc` leave |
| `Esc` | close thread / switcher / select mode; never quits |
| `Ctrl-C` twice, or `/quit` | quit |
| `?` on empty input | help overlay |

## Slash commands

`/join #x`, `/leave`, `/create #x [--private]`, `/add @user`, `/topic text`, `/dm @a @b`,
`/invite` (admin; prints code), `/away [status]`, `/back`, `/search terms` (results list; Enter jumps),
`/share-pane [pane]` (herdr only), `/quit`. Unknown command → inline error, input kept.

## Behavior

- Opening a channel marks it read up to the newest message (MarkRead) when the view is at the bottom.
  Scrolled up → don't mark read; show `↓ N new` pill.
- Unread counts in sidebar; `@` instead of count when there's a mention or it's a DM (bot
  messages never make a mention).
- Notifications: see "Notifications & herdr" below.
- Never block the UI on network: all client calls run in `tea.Cmd`s.
- Startup renders from cache immediately, before connecting.

## Notifications & herdr

- **When**: a DM or a message that @mentions me, not my own and not from a webhook bot
  (bot messages count as unread but never as mentions — their text comes from outside), and either the terminal is blurred
  (focus reporting) or the message is in a channel that isn't the active one.
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
