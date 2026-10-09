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
`/quit`. Unknown command → inline error, input kept.

## Behavior

- Opening a channel marks it read up to the newest message (MarkRead) when the view is at the bottom.
  Scrolled up → don't mark read; show `↓ N new` pill.
- Unread counts in sidebar; `@` instead of count when there's a mention or it's a DM.
- Notifications (M2 adds herdr): for M1, ring the terminal bell (`\a`) on a mention or DM message while
  that channel isn't focused. Configurable off with `TC_BELL=0`.
- Never block the UI on network: all client calls run in `tea.Cmd`s.
- Startup renders from cache immediately, before connecting.
