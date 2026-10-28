# tc TUI design (v4)

Minimal, keyboard-first, works in a 40-column herdr split. Bubble Tea + Lip Gloss; every frame is drawn
into a cell canvas (`internal/tui/canvas.go`) and encoded once for the terminal's color profile.

## Layout

```
 tc · 3 online          │        # general  team-wide chatter                       ⌕   ◉ 3
                        │        ▎ ♪ huddle 4:18  bob ▂▅▇▅  carol ▁▁▁▁            join
 ⌕ Jump to…        ⌥K   │
                        │        │ bob  21:46
 Channels               │        │ yep, the api pod crashed around 02:14. pasting:
 # backend           3  │        │  panic.log                                  copy
▎# general              │        │  panic: assignment to entry in nil map
 # deploys          @3  │
 ◆ incident-0214     4  │                 @bob nice catch, I will patch it after standup │
                        │                          ↳ 3 replies   ▐ 🚀 1 ▌  ▐ 👀 1 ▌        │
 Direct                 │                                                       21:49 ✓
 ● bob               1  │
 ◐ dave away            │        carol is typing •··
                        │        ╭─ #general ───────────────────────────────── public ─╮
                        │        │ message #general                                    │
 ● alice online      ⚙  │        ╰────────────── ⌥K commands · ⌥⏎ newline · ⏎ send ─╯
 tc  #general                                      F1 keys · ◆ e2e · ● quic · ◐ graphite
```

- Sidebar: 24 columns (`normal`), 32 (`wide`) or 7 (`icons`, names show as tooltips on hover). `Ctrl-B`/`F2`
  cycle normal → wide → icons; clicking its right edge does the same, dragging it picks a size. Below 90
  columns it shrinks to icons, below 60 it is hidden; there `Ctrl-B` cycles icons → normal → hidden (40–59:
  icons ↔ hidden) for the session without changing the saved size. The size is kept in `TC_HOME/ui.json`.
- Message column: centered, at most 86 columns. Threads open in a pane on the right (sliding in)
  when there is room, full width otherwise; the input moves into the pane while it is open. The pane is 44
  columns by default; drag its left edge (the grip lights up on hover, like the sidebar's) or press `⌥[` /
  `⌥]` to move the edge 4 columns left / right. It stays at least 32 wide and leaves the message column at
  least 48; the width is kept in `ui.json` (`"thread_w"`). Below 94 free columns the thread is full width.
- Status bar (bottom row): ` tc ` pill, the channel, then `F1 keys`, `◆ e2e` in encrypted channels, the
  connection (`● quic`/`● tcp`; the state when not online) and `◐ theme` (click: themes). No server name.
- Header: channel name + topic (one line, truncated); `◉ N` members and `⌕` (palette) on the right. Status
  (Connecting/Offline/Reconnecting) shows in the sidebar title, dim, only when not Online.
- Messages: everyone on the left (Slack style), name in that person's color and time over the text, a
  thin `│` bar in their color; mine the same in my color, with time and `◌`/`✓` under the text. Settings ›
  Appearance › Messages: Split (or the palette's Settings tab) puts mine on the right against an accent
  bar instead (`"split"` in `ui.json`). Each theme has eight name colors of clearly different hues.
  Identity is the name only. Consecutive messages from the same author within 5 min omit
  the name row. Day dividers (`──── Today ────`). Cozy density leaves exactly one blank row between groups
  (a new author, or more than 5 min apart) and one above and below a day divider or the thread's `N replies`
  divider, never two; none inside a group, code blocks and diffs included. Compact leaves none. A message draws at most 120 rows (code blocks 40) in the log; `v` shows all of it.
- Pending (outbox) messages render dim; failed ones show `! failed — r to retry, d to discard`.
- Edited: dim ` (edited)`. Deleted: dim italic `message deleted` (encrypted channels: `deleted (unverified)` until judged). In encrypted channels `(edited)` shows
  only for an edit the author signed, reactions only when signed by the reactor, and a delete the author
  didn't sign (an admin, the server) reads `removed (not by author)`. `e` refuses messages that aren't
  verified (waiting, invalid, legacy or inserted plaintext).
- Pinned messages end with a dim `📌` on their footer line; the header shows a dim `📌 N` when the channel has pins.
- Reactions as pills on a row below: `▐ 👍 2 ▌` (yours in the accent; up to 3 rows when they don't fit, the rest as `+N`); click one to toggle it. The row ends
  with a `☺+` pill that opens the emoji picker for that message. Hovering a pill shows who reacted above it
  (`you, bob and 3 others reacted with :tada:`; five names at most).
- Thread roots show `↳ N replies`; threads open in a right pane (or full width below 100 cols).
- An empty channel shows a dim `no messages yet — say hi · F1 help · ⌥K channels & people` (shorter when the message pane is under 64 or 26 columns; in my notes `your notes — only you see them; /remind lands here`) once a history fetch said it is empty.
- Typing line right above the input, no blank row between (`bob is typing •··`); its row stays reserved when
  nobody types, so the log never jumps. Errors, rings and
  lasting hints share that row; passing notices ("copied", "pinned") show as a toast pill there instead
  (fades in and out, click dismisses; on the right of the row while someone types or a ring shows, shortened
  if need be; in a narrow pane it covers the typing line while it lasts, and only a ring or our huddle sends
  it one row higher). Notices raised while a dialog is open show over its bottom edge.
- Input: a rounded box (channel in the top edge, `public`/`private`/`◆ e2e` on its right, key hints in the
  bottom edge) that grows to max 6 lines; `Alt-Enter`/`Ctrl-J` inserts newline; `Enter` sends.
- Huddle bar: one row under the header while a huddle runs in the open channel:
  `♪ huddle m:ss  you ⣀⡠⠔⠊⢁ ● live  bob ⣀⣀ …  mute leave` (in it; `⊘ muted` while the mic is closed) or
  `join`. Meters are a braille line (one dot per sample, two per cell): yours rolls over 10 samples, others'
  over 4, on the animation tick while you are in the huddle. Voice reports who is talking, not how loud,
  so a meter moves while that person talks and lies on the floor otherwise.
- Startup: a short logo intro while tc connects (see Skins › Intro).

## Skins

A skin is the chrome; a theme is the colors. They are independent: every skin works with every theme. A skin
holds no colors of its own; everything it draws takes the theme's tokens (`internal/tui/skin.go`), so a new
theme gets it for free. Settings › Appearance › Skin, the row under Theme (Settings opens on Theme, so ←/→ try themes and never
switch the skin), or the palette's Settings tab, group Skin:

- **Modern** (default): rounded boxes, flat panels, everything above.
- **Retro** (Borland Turbo Vision, matching the mockup's `Look_borland`):
  - double-line boxes `╔═╗║╚╝` for the input (one padding row under its text), the thread reply box, popups,
    the emoji picker and dialogs (content stays inside the frame);
  - a drop shadow 2 columns right and 1 row below every framed box: toward black on dark themes, a soft
    toward-ink gray on light ones; it covers the desktop pattern, like the mockup's;
  - a `░` desktop in the empty, non-clickable gutters beside the message column and around the thread pane,
    the theme's line color 45% of the way from each cell's background (the mockup's), pushed further where
    needed so it always stands at least 2:1 off what it sits on;
  - a menu bar on row 0 (terminals of 16+ rows): ` ≡  Chat View Huddle Theme ? Help`, hotkey letters in a
    popping color, the clock and `◉ N online` ending 5 columns from the right. ≡ opens the palette, Chat
    (`⌥C`) its Channels tab, View (`⌥V`) its Settings tab, Huddle (`⌥H`) joins or leaves the open channel's
    huddle, Theme (`⌥T`) the palette's themes, Help the keys sheet; each lights up under the pointer. When
    room runs out the middle menus go first. It takes the blank row 0 of a roomy layout, so nothing moves;
    a tight layout (under 30 rows) moves down a row. The clock wakes the screen once a minute while it
    shows; nothing else redraws while idle.
  - Surfaces are pulled apart from the theme's own tokens so frames, bars and shadows read on every theme:
    panels at least 1.1:1 off the background, bars and selections 1.5:1, frames 3:1 off their panel, quiet
    text kept readable on them, and a near-black background lifted toward its line color just until a
    shadow shows (1.25:1). Turbo already meets all of these, so it is exactly the mockup's.
  - A dark theme on a neutral near-black (sky, mint, rose, lilac, ember, phosphor) gets a deep colored
    ground in its accent's hue, every hue at the same depth as turbo's navy, with panel, bars, frames and
    the menu bar tinted to match; graphite keeps its charcoal (`plainGround` in the theme table).
  - Per-theme overrides live in the theme table: qbasic's gray dialog colors also color the menu bar (DOS
    menus); macintosh's hotkeys use its info blue, as its accent is black.
- Choosing Retro switches to the **turbo** theme only if no theme was ever picked; a picked theme (even
  graphite) stays. `ui.json` keeps `"skin"` and `"theme"`. A skin switch cross-fades the colors and swaps
  the chrome halfway.
- Adding a skin is one entry in `skins`: box glyphs, frame, shadow, pattern, menu bar, input padding, intro.

### Intro

Every start (both skins) opens with a short intro while tc connects: the tc logo in the theme's accent over
`connecting…` (the connection error, if any, in its place). It holds at least 0.7 s and until the cache or
the connection is ready, 1.2 s at most, then opens in 0.5 s: Retro shows the logo in a shadowed double-line
dialog on its desktop and zooms a frame from the dialog to the whole screen with the UI inside; Modern shows
it on a plain screen: the logo eases in (cubic, 0.3 s), and the opening fades it out and the chat in, each
half smoothstepped so nothing jumps. Frames tick at ~30 fps only while something moves; a still hold just
waits for its deadline. A key or a click ends it (the key still types). Settings ›
Appearance › Startup: `Animated` · `Instant` (`"startup": "instant"`); Animations off is instant too. On a
first run with no cache, the same logo screen stays until the first channels arrive.

### Icons

Every icon the UI draws for a role (channel, private, person, huddle, mic, muted, agent, e2e, search, pin,
settings, react, reply, copy, more, thread, file, image, code, bell, away, online, notes, talking, edit, delete, and the
tc mark) comes from one table, `internal/tui/icons.go`, in three packs:

- **tc Symbols**: tc's own 27-icon font (`internal/iconfont`, generated from `assets/iconfont/icons.txt` by
  `scripts/iconfont.py`), at U+E400–U+E41B: Private Use, in the gap Nerd Fonts leave between Weather (to
  U+E3E3) and Seti-UI (from U+E5FA), so Nerd Font users still get these. Each glyph is one column, like
  every Private Use character in our width function.
- **Unicode** and **DOS**: plain characters every terminal has.
- Settings › Appearance › Icons: `Auto` · `tc Symbols` · `Unicode` · `DOS`, with a live preview row
  (`"icons"` in `ui.json`). Auto is tc Symbols when the font is installed on this machine, the terminal is
  one known to find a glyph in another installed font (`TERM_PROGRAM` or `TERM`: iTerm2, Terminal.app,
  Ghostty, WezTerm, kitty) and isn't inside a multiplexer (tmux, screen, herdr, zellij), we aren't over
  SSH and the locale isn't CJK (ambiguous characters drawn wide); otherwise Unicode, or DOS with the Retro
  skin. The font is looked for once at start (drawing never touches the disk), and an installed font older
  than this build is refreshed then, quietly. When the font is missing (and not over SSH, where the row says
  to install it on your own computer), an **Install tc icons** button installs it; the icons switch at the
  next start, since a running terminal often can't draw a font installed after it started. Elsewhere (herdr,
  Zed's or VS Code's terminal) the Private Use glyphs draw as boxes, so Auto stays Unicode; picking tc
  Symbols by hand is always honored. Someone without the font, in a terminal that could show it, sees
  "New: tc icons · Settings › Icons" once (`"icons_hint"`). Away is `◐` (DOS `◑`), so it
  never looks like an offline `○` without color.
- Touches: on terminals known to draw them (WezTerm, kitty, Ghostty, iTerm2; never inside tmux, screen or
  herdr), `@you` mentions get a wavy accent underline and `#channel` links a dotted info one (SGR 4:3 / 4:4
  with SGR 58). A `#name` is a link only when it names a channel that exists (the server's name rule; never
  `#1234`, a hex color or a URL fragment); it is drawn in the info color and opens the channel on a click. Settings › Appearance › Underlines: `Auto` · `On` · `Off` (`"touches"`); never under
  NO_COLOR. Code blocks show a code icon, diffs a file icon; whoever talks in a huddle gets a pulsing
  talking icon.

## Rendering rules

- Every user-controlled string goes through `sanitize.Text`/`sanitize.Line` before it reaches the canvas.
- Markdown-lite only: ```` ``` ```` fenced blocks (a panel with a header row, no wrap reflow, horizontal
  truncation with `…`), inline `` `code` ``, `**bold**`, `_italic_`, URLs underlined. No headings, no tables.
- `@me` mentions in the accent; a message that mentions me gets an accent bar and a faint accent tint, one
  that only contains a highlight word a warn-colored bar.
- Themes: fifteen built in, for any skin (graphite default, sky, mint, rose, lilac, ember, phosphor, the light paper,
  teletype, macintosh, and the DOS-era turbo, norton, qbasic (gray dialogs), night and the light paper-retro), picked in Settings, the palette or `/theme name`, kept in `TC_HOME/ui.json`. Name
  colors are a stable hash of the user id into the theme's four muted name colors. Truecolor, degraded to
  256/16 colors by the terminal's profile; `NO_COLOR` drops colors (quiet text becomes faint, selections reverse).
- Fenced blocks: the header row shows the info string (```` ```go ````; `code` without one) and `copy`. A
  ```` ```diff ```` block, or an unlabeled block that looks like a unified diff (`diff --git`, or `---`/`+++` headers
  plus `@@ -` hunk), says `diff +N −M` and tints rows: `+` green, `-` red, `@@` in the hunk color, file headers dim;
  under `NO_COLOR` only the `+`/`-` markers remain. The full-message viewer (`v`) colors diffs the same way.
  `git diff | tc send` produces a diff block automatically.
- Width math uses display width (runewidth), never byte or rune counts.
- The input line and the palette/react prompts never draw bidi controls (they would reorder the line on screen).
- Motion (Settings › Animations, on by default; off = instant): send pop with sparkles, arrival fade and
  slide, reaction flash with a floating `+1`, thread pane slide, dialog fade, theme cross-fade, typing dots,
  mention badge pulse. A frame tick (~30 fps) runs only while something moves (a slow one for typing dots,
  talking meters and the huddle timer); the cursor blinks for 5 s after a key. An idle screen has no timer.
- 256-color terminals get each theme's surfaces on distinct grey steps; 16-color terminals keep their own
  background and mark selections in reverse. A selected message's bar becomes `▌`; a mention's bar is `┃`
  and a highlight word's `┇`, so they read without color too.
- Mouse: click channels, messages (select), reactions, `☺+`, `↳ replies`, the hover toolbar (`☺ ↩ ⧉ ⋯`; `⋯` opens the message menu), emoji picker cells and tabs, huddle
  join/leave, palette rows and tabs, settings controls, toasts, the theme in the status bar, the Retro menu bar; drag the sidebar or thread pane edge to size it; the wheel
  scrolls the pane under the pointer. A pointer move that changes nothing reuses the last frame.

## Keys

**Key labels.** On macOS hints use the key symbols (`⌥K`, `⌃P`, `⌥⇧↑`, `⏎`, `⇥`); everywhere else
they are words (`Alt+K`, `Ctrl+P`, `Alt+Shift+↑`, `Enter`, `Tab`; a pair like `⌥↑ ⌥↓` becomes
`Alt+↑/↓`, while `⌃C ⌃C` stays `Ctrl+C Ctrl+C`). `TC_KEYS=mac|pc` overrides the OS (e.g. ssh from a
Mac into Linux). The layout above shows the Mac form; with words the input box's bottom edge reads
`Alt+K commands · Alt+Enter newline · Enter send`, drops `newline` when that doesn't fit (under 53
columns of input box; `⌥⏎ newline` under 39 on a Mac), then `send` (under 33; 26 on a Mac),
keeping `Alt+K commands` down to 26, and nothing below that. The sidebar's `⌥K` beside "Jump to…"
is hidden in a sidebar too narrow for it; the palette's `⇥ next tab` beside the tabs is hidden when it
would touch them (always with words: the palette's footer says `Tab next tab`). The emoji picker's
bottom hint falls back to `←↑↓→ move · Tab category · esc` (its preview row already says
`Enter insert`). Every binding works with Alt; no key is ⌘-only.

| Key | Action |
|---|---|
| `Enter` | send (completes instead while the completion popup is open, unless the `@`/`#` word already equals the highlighted item) |
| `/`, `@`, `#`, `:` | completion popup above the input (see below; `:` under Emoji) |
| `Tab` | complete from the popup; reopens it after `Esc` |
| `Option-K` (`Alt-K`; also the `˚` macOS sends when Option types characters; `Ctrl-K` still works as a fallback) | command palette: tabs All · Channels · People · Commands · Settings (`Tab`/`Shift-Tab`); fuzzy search keeps the quick switcher's behavior (Enter opens the top channel or person; DMs created on demand); command rows run the slash command, or type it when it needs arguments; theme rows switch the theme |
| `Alt-A` | activity view (also `/activity`); `Ctrl-A` stays the textarea's line start |
| `Option-E` (`Alt-E`; on macOS set Option as Meta: Option-E alone is the accent dead key) | emoji picker: inserts at the cursor; in select mode it reacts like `+`; again closes it |
| `Alt-↑/↓` or `Ctrl-P/N` | previous / next channel (`Ctrl-P/N` move the completion popup's highlight while it is open) |
| `Alt-Shift-↑/↓` | previous / next unread channel |
| `PgUp/PgDn`, mouse wheel | scroll history (fetches older via History at top) |
| `↑` on empty input | select messages mode (↑/↓ moves selection) |
| in select mode: `e` edit (own), `d` delete (own, confirm), `r` reply in thread, `+` react (emoji picker; typing filters, so `+ +1 Enter` and `+ :tada: Enter` still work), `y` copy body (OSC 52), `p` pin / unpin (notice "pinned" / "unpinned"), `m` or `Enter` message menu (below), `Esc` leave |
| `Esc` | close the completion popup / thread / palette / dialog / select mode; never quits |
| `Ctrl-C` twice (the second while `press ⌃C again to quit` (`Ctrl+C` off macOS) shows, 4 s), or `/quit` | quit |
| `F1` or `/help` | keyboard sheet: Navigate, Messages, Write, Huddle, App, then every command (`?` is just text, so messages can start with it) |
| `Ctrl-B`, `F2` | sidebar size: normal → wide → icons |
| `⌥[` / `⌥]` (thread pane open) | its edge 4 columns left (wider) / right (narrower), saved; macOS Option-as-text sends `“` `‘`, which count only on an empty input |
| `/settings` (or `Ctrl-,` where the terminal reports it) | Settings: Appearance (skin, theme, density, sidebar, messages, animations, startup), Notifications (runs `/notify`, `/sounds`, `/dnd`), Keys, Account & devices, About; `↑/↓` move, `←/→` change, `Enter` toggles, `Tab` next page |

**Message menu** (`⋯` on the hover toolbar, or `m` / `Enter` in select mode): a small floating panel by the
button, in the skin's frame (Modern rounded, Retro double line with a shadow) with the icon pack's icons.
Its rows run the select-mode keys and show them on the right: `↩ Reply in thread` (r), `☺ React` (+),
`✎ Edit` (e, own), `⧉ Copy text` (y), `¶ Pin` / `Unpin` (p), `✕ Delete` (d, own, error color, still asks to
confirm). Only rows that do something show: a deleted or unsent message gets `Copy text`, a failed one
`Retry send` (r), `Copy text` and `Discard` (d). `↑/↓` move, `Enter` runs, the letters run directly, `Esc`,
`m` or a click outside closes it (the click still acts), `⋯` again closes it; any other key closes it and
does what it does in select mode (`v`, `Option-K`, `PgUp`); the pointer lights a row and a
click runs it. It sits right-aligned under the button, above it when there is no room below; when neither
side holds every row, the roomier side gets a shorter menu that scrolls. It works in the thread pane too.

## Slash commands

`/join #x`, `/leave`, `/create #x [--private [--plaintext]]`, `/add @user`, `/topic text`, `/dm @a @b` (`/dm @me` or my own name: my notes, also in `Option-K`; `@me` is always me, even if an account is named `me`; after `/dm` the popup offers `@me` and my name),
`/invite` (admin; prints code), `/away [status]`, `/back`, `/search terms` (results list; Enter jumps), `/pins` (overlay of this channel's pinned messages, newest pin first, as `author · preview`; Enter jumps, Esc closes),
`/activity`, `/highlight [word…]`, `/unhighlight word…`, `/group name @a @b…` (admin), `/groups`,
`/remind <when> <text>`, `/reminders`, `/remind-delete id`, `/snippet path[:range] [message]`,
`/share-pane [pane]` (herdr only), `/verify @user [--github login]`, `/accept-key @user`, `/members`,
`/alarm`, `/alarm-clear #name`, `/alarm-recover [#name]`, `/devices [@user]`, `/device-revoke id`, `/disable @user` (admin), `/quit`.
`/invites` and `/webhooks` open a list, one per row. The invite and webhook viewers take the full width (no sidebar) so a mouse selection copies only the command.
`/webhook name` copies the curl command; when no system clipboard tool took it (only OSC 52, which Terminal.app and tmux without `set-clipboard on` ignore) a viewer opens with the command hidden: `s` shows it (it holds the secret URL), `y` copies again, Esc closes and forgets it.
`/topic` or `/name` alone shows the current topic or display name; `--clear` removes it. Commands that change something confirm it in the status line (`added @bob`, `topic set`, `away: lunch`).
Unknown command → inline error, input kept.

### Completion popup

While the input is a bare `/command` (no space yet) a popup of up to 6 rows lists the
matching commands (prefix matches first, then substring; an exact name first) with their argument hint and a
dim one-line description; more matches scroll (`↑`/`↓` marks at the right). An `@` or `#` word at the cursor
does the same for people (this channel's members first, then everyone else, then agents as `@rocky ▟▙`
with a dim `Fizan's agent` or `your agent` (a prefix match on a handle still beats a person who only
contains the letters), then user groups like `@oncall`) and channels (joined ones first); a name equal to what was typed always comes first (my own name too, so `as @alice said` sends on Enter). For
`@`/`#` a substring match needs two letters and never comes from digits alone, so `fixed in #12` isn't offered `#release-12`. The popup is an
opaque floating panel on the input's left edge, one blank row above it (that row keeps showing notices and
typing), at most 72 columns and never wider than the column: a rounded frame with the kind (`Commands`,
`People`, `Channels`) and `i/N` in its top edge and `↑↓ move · ⇥ complete · ⏎ run · esc` in its bottom edge;
rows in columns (completion, its arguments, the description), the selected or hovered row in the accent;
a click picks a row. It fades and rises in unless animations are off, and is never drawn outside normal mode.

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

## Emoji

- **Picker** (`+` or `Option-E` in select mode, the toolbar's `☺`, a reaction row's `☺+`: react to that
  message; `Option-E` or the palette's "Insert emoji" in the input: insert at the cursor). A floating panel
  like the completion popup: rounded frame, `React to bob` / `Insert emoji` and `✕` in the top edge,
  `←↑↓→ move · ⇥ category · ⏎ react · esc` in the bottom edge. Inside: a search box, category tabs
  (frequently used, smileys, people, nature, food, activities, travel, objects, symbols; no flags), a grid
  of 10 per row that scrolls as one list with a title per category, and a preview row (the emoji,
  `:shortcode:`, its other names and keywords, and what Enter does: `⏎ react`, `⏎ remove` for one of
  mine, `⏎ insert`). Reacting sits right-aligned below the message (above it when there is no room, over
  its body under the name row when neither fits), never under the sidebar; inserting sits one blank row
  above the input's left edge. Its frame's cells take the pane's own
  background, never a message's tint.
- Keys: arrows move (←/→ run on through the rows), `PgUp`/`PgDn` a page, `Tab`/`Shift-Tab` jump to the
  next/previous category (and leave a search), typing filters (shortcodes first, then keywords; more words narrow it,
  `thumbs up`; a pasted emoji is offered as is), `Backspace`/`Ctrl-U` edit the search, `Enter` picks, `Esc` closes. Mouse: hover
  moves the one highlight (keys move it on from there), click picks, a tab click switches category, the
  wheel scrolls (the highlight follows into view), `✕` closes, and a click outside closes it and acts.
  The input's bottom edge says `esc close emoji` while it is open.
- Fit: 10 columns at 46 cells; narrower terminals get fewer columns (always 4 cells each with even 2-cell
  margins), and tabs that don't fit scroll with the selection. Under 9 rows the tabs and preview go; under
  4 rows or 10 columns the frame goes. It fades and rises in and out like the popup; instant with
  animations off. A channel switch closes it.
- **`:` completion**: `:` plus two shortcode letters at the start of a word opens the completion popup
  (`Emoji`, 44 columns at most) with emoji whose shortcode matches: an exact name first, then frequently
  used (`frequent`), then prefixes (shorter first), then names containing it. The typed letters show in
  the accent. `Tab`/`Enter` put the emoji in (and a space) and close the popup; `Enter` never sends from
  it. Never inside a fenced block or an inline code span, or after a letter (`http://`, `10:30`).
- **Shortcodes on send**: each known `:name:` in a message or an edit becomes its emoji when sent
  (`:fire:` → 🔥, `(:tada:)` → (🎉), `:tada::rocket:` → 🎉🚀). Only as a word of its own: after a space,
  the line's start, an opening bracket or quote, or another converted shortcode, so `12:100:3`, `?q=:fire:`
  and URLs stay as typed; unknown ones and ones in code stay too. An edit converts only shortcodes the
  message didn't already have. Slash commands are left alone. A send saves the frequently used list once.
- **Frequently used**: picks, completions and converted shortcodes count; kept in `TC_HOME/ui.json` as
  `"emoji"` (most used first, the newest first among equals, 40 at most). A fresh install offers
  👍 🚀 👀 🎉 🔥 😂 🙏 💯 👏 🤔. The picker shows two rows of them.
- Data: GitHub gemoji (MIT), generated into `internal/tui/emoji_data.go` by `go generate ./internal/tui`
  (`internal/tui/emojigen`). Only emoji terminals draw as one wide cell are kept: one code point with
  `Emoji_Presentation`, or a base plus U+FE0F that measures 2 cells (after one, the frame encoder moves
  the cursor to the next cell, for terminals that advance one column); no ZWJ sequences, skin tones,
  keycaps or flags, nothing newer than Unicode 13.0. Shortcodes are Slack's (`:ok:` is 🆗); `:check:`
  stays for ✅.

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
  can't be cleared. For a rollback or fork `/alarm` says clearing won't help while the server still
  shows it and points at `/alarm-recover` instead.
- **After a server restore**: `/alarm-recover` (online) previews accepting the server's older key chain
  in a list: which epochs are discarded, who gets the key afterwards, a `⚠` line per user who wasn't in
  this device's newest epoch (removed after the backup?), and "To accept: /alarm-recover #name". If it
  can't be accepted (a fork that isn't a member's recovery, a chain sharing nothing with ours…) it says
  why and keeps nothing. `/alarm-recover #name` accepts exactly the last preview of that channel, once;
  without a preview it says to preview first, and if the chain moved since, it says to preview again.

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
- Unread counts in the sidebar (dim; a DM's in the accent); `@N` in the accent when there's a mention, pulsing
  briefly when it grows (bot messages never make a mention). Both stop at `99+`.
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
- A dim `✎` after a channel's name in the sidebar (and in the palette) means it has a draft that the input
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

## Agents

Personal agents (v0.6.0, PROTOCOL.md §15). An agent is one more teammate in the room: a glyph, a `⟡` badge
and one status row. Code: `internal/tui/agent.go` (icons, state), `agentlive.go` (status rows, folded runs),
`approval.go`, `agentchrome.go` (sidebar, DM header, palette), `agentpanel.go` + `agentpanel_draw.go`.

- **Identity.** A message with `Message.Agent` set draws `<icon> Rocky ⟡ · Fizan's agent  hh:mm` on the
  first message of a group and a dashed `┆` bar where people get `│`; icon, name and bar take the agent's
  color. Below 50 columns the badge drops `· Fizan's agent`. An agent's messages never group with its
  owner's, and my agent's messages are not "mine" (no edit, no split side): `Message.FromAgent()` decides.
  Name, icon and color are the author's agent now (`AgentOf`), so a rename shows on old messages too; a
  retired agent keeps the handle its message carries. A reply starting `✓ ` (a milestone) gets its `✓` in
  the ok color. A message an outside Claude session posted for someone (`Via == "mcp"`, `tc mcp`) shows a
  faint `⇢` before their name (or before the time, for mine on the right) and doesn't group with their own.
- **Icon and color** live in `proto.Agent.Icon`: `"<preset>"` or `"<preset>:<n>"`, n the theme name color
  0–7 the owner picked (no color: a hash of the handle). Presets: pebble, orb, spark, owl, hive, moon,
  meter, kite, each with idle / thinking / working / done / asleep frames (1–2 cells). Anything else in
  the field shows as a static glyph. Timings: thinking 280 ms frames with a 1.12 s breath, working
  110 ms, streaming 220 ms (working frames), done flashes twice in the ok color (0.29 s each), asleep
  1.6 s at half strength (kept readable on its background). The frame tick runs only while a glyph on
  screen moves (the breath in 140 ms steps), never for an asleep agent or while the terminal is unfocused;
  Animations off shows still frames.
- **Live reply.** `Update.AgentStatus` reloads the channel's live statuses (`AgentThreads` +
  `AgentStatus`); each live agent gets a row at the bottom of the channel (top level) or of the open
  thread: `<icon> Rocky is thinking…`, then its status line (`is reading the thread…`), then
  `is writing…  live · not saved` with the preview streaming dim and italic behind a faint `┆` (its last
  10 rows). Asleep says `is asleep 💤`, an error `is stuck: <why>` in the error color. The row goes as
  soon as a message from that agent newer than its last status lands (or after done's flash). Narrow panes
  truncate it from the right. Scrolled up, the view stays put while the rows grow and shrink.
- **Work runs** (a run with 3+ distinct working steps; never my agent in Talk mode) fold under the reply
  as `▸ 5 steps · 2m 41s · $0.42` (cost only for my agent, from its audit log); a click, or `o` / ⏎ / space
  on the selected reply, unfolds the steps.
  What tc saw of the run is kept for this session only.
- **Sidebar.** Agents sit under their owner's DM row as `└ <icon> Name` (asleep: faint name, half-strength
  glyph, `💤`); mine sits under the "you" row (taking the roomy layout's blank row, or moving "you" up
  one). Its glyph moves with whatever it is doing anywhere. My agent's row: a click opens our DM, a click
  on its glyph (or on the row while the DM is open) opens the panel; it shows the DM's unread count, or `✋`
  while an approval waits (a click then opens that card). Someone else's agent: a click puts `@handle ` in
  the input. The agent DM is never listed as a channel.
- **Agent DM** (`ChanAgentDM`): header `<icon> Rocky · your agent · only you see this chat`, with the mode
  on the right (click: the panel), or `✋ waiting in #chan` while an approval waits; the input box says
  `only you` and `tell Rocky anything…`. The palette lists it as `@ Rocky (your agent)`, with its icon.
  The palette's People group lists every other agent too (`@juno · Bob's agent`): picking one types
  `@juno ` into the composer; my agent without a DM yet opens it. The agent speaks
  first (the harness); tc only draws it. Without the DM yet, the panel's `d` creates it (`OpenAgentDM`).
- **Approvals.** The harness posts `✋ @owner approve: <action>? react ✅ / ❌  ·  #code` + detail and waits
  for the owner's ✅ / ❌. tc draws the detail, then `<action>?  1 yes  2 yes, ok today  3 no` (`PRs ok today`
  when the action names a PR) and `only you can answer · keys 1–3`; everyone else sees `waiting for Fizan`.
  Keys 1–3 answer only a card selected on purpose (select mode, where the footer reads `1 yes · 2 PRs ok
  today · 3 no`); digits typed in the composer are always text. `⌥O` (or the palette's `Approval waiting ·
  #chan` row) opens the newest waiting card and selects it. A click on a choice answers too. 1 reacts ✅,
  2 reacts ✅ then ♾️ (the harness reads the ✅ as yes), 3 reacts ❌; a toast says what was answered. Only a verified card from my own agent takes an answer. Answered, it collapses to
  `✓ Fizan said yes · <action>?` (`✗ … said no`); a card the harness gave up on (`⌛ no answer on #code`)
  reads `⌛ no answer`. A card that arrives while I look elsewhere notifies me once, as `Rocky needs your
  ok`: it replaces the mention alert its `@owner` would raise. A card in a thread that isn't open counts
  as elsewhere.
- **Panel** (`⌥K` › Agent, the glyph in the sidebar, the mode in the DM header): one page, no tabs. State
  (awake / working / paused / asleep, idle time, sleep-after, on this or another computer), Spend today (`█` accent,
  `░` faint, `$3.40 of $20  17%`), Asks today per person, the Mode radio (Talk / Work / Crew: `←/→`, `1–3`
  or a click; `h`/`l` move it too), the note line, then Reads, Folders, Limits, Memory, Activity, Personality ("tell me in our
  DM": opens the DM). `⏎` opens a row's page: Reads (channels it may read for anyone's asks, toggled on
  the server with `agent.access`, plus "your DMs when you ask"), Folders (`a` adds a full path, `x`
  removes after `y`), Limits (`←/→`: daily cap, per-run cap, asks per person, rollover %, sleep after), Memory
  (`⏎` reads a note, `f` forgets one, `u` undoes today; both ask `y`), Activity (the audit log, newest
  first). Keys `p` pause / resume, `d` open DM, `L` logs, `r` re-read, Esc back / close. A short terminal scrolls
  the page to keep the focused row in view (`↑`/`↓` marks show more). Settings are the
  agent home's `config.json` (`internal/agent/home`); spend, asks, status and the log come from its
  `state/` files, written by the supervisor; nothing is read while drawing. On a computer that isn't the
  agent's home (`agent.HomeHere`) the panel shows what the server knows and changes nothing.
- **Setup.** With no agent the panel is the setup: handle (`@rocky`), the icon sheet, a color (auto or one
  of the 8), a preview of the name row, `Set up Rocky`, and `or from a shell: tc agent setup`. It calls
  `RegisterAgent`, then makes the agent home as `tc agent setup` does (`home.EnsureWith` with the server's
  host, config with this machine's id, `client.LoginAgent` for its device unless it is signed in already);
  `tc background` runs it. A refused handle shows the server's reason.
- **Follow-ups and quiet** (harness, `internal/agent/engage.go`; nothing in tc draws them). After an agent
  replies in a thread (or a channel's top level), people's messages there reach it without an @mention
  until nobody has written there for `engage_minutes` (default 10). "Be quiet for 5 min, then …" makes it
  call `quiet`: until then only an @mention reaches it there (an answered mention ends the quiet,
  `leave_thread` drops it); when it ends (checked on every update and each minute) it listens again and
  gets one turn if it was told what to do then or someone wrote meanwhile. Quiets are kept in
  `state/queue.json`, so a restart keeps them; one that ended more than 15 min (or `engage_minutes`) before
  the agent came back just ends. A follow-up that replies counts as one of the writer's asks per person,
  and past that limit their follow-ups don't run at all. Only an @mention restarts the `MaxFollowUps`
  (20) count of a conversation. A wake answers what someone said during the quiet as that person's ask,
  never with the trust of whoever asked for the quiet.
- **Others' asks** (harness, `internal/agent`; nothing in tc draws them). An ask by anyone but the owner:
  runs in Talk from `<agent home>/guest` (only a link to the skills; files in the working directory are
  readable without a prompt) and, in any mode, with deny rules for `memory/`, `state/`, `dream/` and
  `me.md`: its memory is `recall`, which leaves private notes out. It never resumes the owner's engine
  session of the same thread or reads its handoff (sessions, handoffs and frozen snapshots are kept per
  owner / others). It reads a channel on the Reads list only when that channel is public and not encrypted
  or the asker is in it; `search` without a channel searches each readable one. Its posts, reminders and
  `ask_agent` questions may not @here / @channel / @all / @everyone or @ a group, and its reply has those
  @ dropped. A ♾️ applies to the owner's own asks only (on a stranger's card it isn't kept) and, for a tool
  call, to that exact call. Others share `guest_cap_usd` of the daily cap (config only; 0 = half), so they
  can't spend the owner's headroom. A dream may lower a note's trust or make it private, never the
  reverse, and may not copy private text into a public note or the index, or lower-trust text into a
  higher-trust note.

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

- **When**: a DM or a message that @mentions me (outside fenced code blocks, like highlight words) or contains one of my highlight words, not my own and not from a webhook bot
  (bot messages count as unread but never as mentions — their text comes from outside),
  and either the terminal is blurred (focus reporting) or the message is in a channel that
  isn't the active one.
- **What**: title `#chan · alice` or `alice (DM)`, body = first non-empty line. Title ≤ 60 runes,
  body ≤ 160, both through `sanitize.Line`.
- **Coalescing**: the first message in a channel notifies at once and opens a 5 s window; more
  messages in that window become one `N new messages` notification when it closes. A message shown alone when its window closes is re-read first: deleted meanwhile, nothing pops up; edited, the popup shows the new text. A mention in a level-`all` window opens its own window; soft messages not yet announced are counted in it. At most one
  notification per 2 s overall (a message over the cap waits for its window). Opening the channel
  drops what's pending for it.
- **Backend** (`internal/notify`), chosen by `TC_NOTIFY=auto|herdr|os|terminal|bell|off` (default `auto`):
  auto = `herdr notification show` (a title starting with `-` gets a `tc · ` prefix so it can't read as an option) when `HERDR_ENV=1` and `herdr` is in PATH; else, **over SSH**
  (`SSH_CONNECTION`/`SSH_TTY`) with a terminal known to support it, the `terminal` backend; else
  the OS (`osascript` on macOS — text passed as argv to an `on run argv` script, never
  interpolated; `notify-send` on Linux, body markup-escaped (`&`, `<`, `>`) since servers may render it as markup), else the terminal bell. `TC_BELL=0` still turns the bell
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
  `♪n` shows them); at `all`, one popup per channel per 5 min, within the 2 s cap.
- **Echo control** (docs/VOICE.md "Echo"): the join notice puts the most important thing first,
  because narrow panes truncate it. That is a device note if there is one (`mic: Built-in Audio ·
  default input is a speaker monitor · TC_VOICE_INPUT picks another`, or `no microphone, listening
  only · …`). Otherwise, without the platform's echo canceller (mode `gate` or `off`), it is `no echo
  cancellation: use headphones` (`use headphones` under 60 columns), once per run. Then comes
  `left #x` when switching, then the talk help. A note and the hint don't both fit at 80 columns,
  so after a note the hint follows as its own notice when the note's expires. All of these use the
  normal info style, for 8 s. `/huddle` in the huddle you're in answers about echo first instead of
  an error: `echo cancellation on (system)`, `echo: gate only, use headphones` or `echo cancellation
  off, use headphones`, then any device note, then `/leave-huddle to leave`. The mic is gateable
  (open) from a fresh ctrl+t press until a key-repeat proves it held; a held key is never gated.
  `/talk` is open mic.
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
  The `/notify` list ends with `when tc is closed: on|off (shell: tc background off|on)` (whether
  `tc background` is installed; the hint is a shell command, dropped under 50 columns like the
  others; the row is left out when the state can't be read). Only `/notify` mentions it.
- **Policy code**: everything above (levels, coalescing, the 2 s cap, rings, privacy) lives in
  `internal/alert` (an `Engine` that returns the timers its caller schedules); the TUI answers its
  `World` from the loaded model and turns timers into ticks. `tc background` runs the same engine.
- **When tc is closed: `tc background on|off|status`** (`internal/background`). `on` installs and
  starts a LaunchAgent (`dev.tc.background`, `~/Library/LaunchAgents`) or a systemd user unit
  (`tc-background.service`, `systemctl --user enable` + `restart`), both running
  `tc background run` with KeepAlive/`Restart=always` and 30 s between restarts. A non-default
  `TC_HOME` gets its own entry (`dev.tc.background.<8 hex>`, `tc-background-<8 hex>`). The entry
  carries `TC_HOME` (absolute) and copies `TC_SOUND`, `TC_NOTIFY_CONTENT`, `TC_QUIET`, `TC_NOTIFY`
  and `SSH_AUTH_SOCK` (not launchd's own per-boot socket on macOS); re-run `on` after changing
  them (a forwarded or per-login `/tmp/ssh-*` agent socket isn't copied). Homebrew installs run the
  prefix's `bin/tc` link, not the versioned Cellar path. `on` refuses a binary another local user
  could replace (it or a parent directory world-writable or owned by someone else, e.g. `/tmp`). `on` is
  idempotent (rewrites the file and reloads); `off` stops it and removes the file, status and logs.
  - `run` connects with the normal client (cache, reconnect with backoff, local E2E decryption) as
    a **passive** session (PROTOCOL.md §4.3: not counted for presence, no typing, no huddles),
    feeds `Update.New`/`HuddleStarted` to the engine and shows popups through the OS backend only
    (`osascript`/`notify-send`; `TC_NOTIFY=off` silences it). It never calls `MarkRead` or
    changes unread state.
  - **No double popups**: every chat window holds a shared `flock` on `TC_HOME/ui.lock` for its
    lifetime (the OS drops it on exit or crash). The notifier probes it (exclusive, non-blocking)
    before handling anything and again right before each popup, and once a second: while it's
    held the notifier drops what was pending and closes its connection (status `paused`), so the
    window alone talks to the server and the cache. When the last window closes it reconnects
    within a second and syncs; what arrived after the window's last sync notifies. `tc join` and
    `tc login` hold the same lock while they log in (they wipe the cache for the new account), and
    a session that sees `config.json` change reconnects as the new login with a fresh engine.
  - One notifier per `TC_HOME` (`background.lock`, exclusive; a second instance waits). It
    writes `background.json` (state, transport, last error, count of popups; never text) for
    `status`, and logs state changes to `background.log` (rotated at 1 MiB). Not logged in, or
    shut out (revoked), it waits for `config.json` to change. When its binary is replaced
    (checked every 30 s) it exits so the service manager starts the new one; `tc update` also
    restarts it right away.
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
