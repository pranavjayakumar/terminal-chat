# tc: terminal chat

Team chat that lives in your terminal (and your herdr panes). One small binary, Mac + Linux.

- Instant start from a local cache, works offline, sends when you're back
- Channels, private channels, DMs, threads, reactions, edits, search, pinned messages
- Sign in with your existing SSH key. No passwords, no accounts to manage
- Pipe anything into chat: `make test 2>&1 | tc send '#backend'`
- Voice huddles in any channel or DM, end-to-end encrypted where the channel is

## Get started

**Admin, once:** run a server on any machine your teammates can reach ([docs/DEPLOY.md](docs/DEPLOY.md)):

```sh
tc server --public YOUR_IP:7443          # first run: your admin invite (in DATA/first-invite.txt)
tc server invite --uses 15 --ttl 24h     # one invite for the whole team
```

It prints a one-liner. Share it in your team's usual channel:

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- 'tc://host:7443/abc…#pin…'
```

**Teammates** paste it. It downloads the release for their Mac or Linux box, checks its signature
and sha256, installs `tc` to `~/.local/bin` and runs `tc join`, which tries their SSH keys (ssh-agent,
then `~/.ssh`, else it makes a tc-only key), suggests a name from `git config user.name`, and asks once
to confirm. Then `tc`. Prefer Homebrew? `brew tap pranavjayakumar/tc https://github.com/pranavjayakumar/terminal-chat && brew install pranavjayakumar/tc/tc`, then `tc join 'tc://…'`.
`go install` isn't supported: the module path (`github.com/skailama/tc`) isn't where this repo lives, so use the installer, Homebrew or a release tarball.

**Second computer:** `tc login` on the first one prints a `tc login 'tc://host:port#pin'` line; run it
on the other with the same SSH key (re-running your original install command works too). If the join
was interrupted, just run it again: a key that's already registered logs in and the invite stays unused.

**Network blocks 7443?** Some office, hotel and phone networks only let 80/443 out; the join then
fails with `can't reach the server … a firewall blocks port 7443`. If your server also answers on 443
([docs/DEPLOY.md](docs/DEPLOY.md#serve-on-443-too-optional)), change `:7443` to `:443` in the invite
(`tc://host:443/…#…`) and run it again; the pin stays the same.

Already have tc? `tc join 'tc://…'` (`TC_INVITE='tc://…' tc join` keeps the code out of `ps`). Keep
invites short-lived (a day is plenty) and revoke leftovers with `/invite-revoke` or `tc invite --revoke`.
Any member can invite inside tc with `/invite [uses] [ttl]` (e.g. `/invite 5 1d`; members get up to 10
uses, 7 days, 5 live invites; the server owner can turn this off with `tc server --member-invites=false`).
`tc invite --key github:alice` makes an invite only the holder of alice's GitHub SSH keys can redeem, so a
leaked code is useless.
`tc update` installs the newest signed release in place (`--check` only reports); tc shows a dim
`update vX` in the sidebar when one is out. For that, release builds make one HTTPS `HEAD` request to
`github.com/pranavjayakumar/terminal-chat/releases/latest` at most once a day. It sends no version,
user, server or chat data, but GitHub sees your IP address and that it runs tc.
`TC_NO_UPDATE_CHECK=1` turns it off, and dev builds never check.

## Using it

| | |
|---|---|
| `Enter` / `Alt-Enter` | send / newline |
| `/` · `Tab` | `/` shows commands; `@`/`#` pop up names and channels — `Enter` or `Tab` completes, `Esc` hides |
| `Option-K` (`Alt-K`) | command palette: jump to any channel or person, run any command, change settings (`Ctrl-K` works too where the terminal passes it) |
| `Alt-A` | activity: mentions, replies to you, reactions, DMs (Enter jumps) |
| `Alt-↑/↓` | previous / next channel (`Alt-Shift` for unread) |
| `↑` on empty input | select a message: `e` edit · `d` delete · `r` thread · `+` react · `y` copy · `v` view full · `p` pin/unpin |
| `Esc` | back out of anything |
| `F1` or `/help` | all keys and commands |
| `Ctrl-C` twice | quit |

Commands: `/join #x` `/create #x [--private [--plaintext]]` `/add @user` `/dm @a @b` `/topic …` `/name …` `/search …` `/pins` `/activity` `/highlight [word…]` `/unhighlight word…` `/group name @a @b` (admin) `/groups` `/remind <when> <text>` `/reminders` `/remind-delete id` `/away [status]` `/back` `/snippet file[:range]` `/share-pane [pane]` `/invite [uses] [ttl]` `/invites` `/invite-revoke id` (yours; admins: any) `/webhook name` `/webhooks` `/webhook-delete id` `/verify @user` `/accept-key @user` `/members` `/alarm` `/devices [@user]` `/device-revoke id` `/disable @user` (admin) `/huddle` `/leave-huddle` `/notify …` `/dnd …` `/sounds`.

Private channels are end-to-end encrypted; `--plaintext` opts out (needed only for webhooks/bots).
Admins: `tc users` lists accounts and who invited them; `tc users disable @name` locks an account
out for good (also `/disable @name`).

Unsent text is kept per channel and thread (a dim `✎` in the sidebar marks channels with a draft) and
survives restarts. `/remind 10m stand up` or `/remind tomorrow 9am …` posts `⏰ …` in your private notes on time, even
if tc is closed. An admin's `/group oncall @alice @bob` makes `@oncall` mention both. `/highlight deploy oncall` makes those words count like an @mention: marker, unread
`@` badge and notification.

From the shell:

```sh
tc send '#general' deploy finished          # message
kubectl logs pod/api | tc send @priya        # piped → code block (colors stripped)
(git diff) | tc send '#review'               # a diff block, colored in tc
tc send '#backend' --file pkg/db.go:40-60 look at this   # lines 40-60 as a code block + permalink
tc tail '#deploys'                           # stream a channel
tc verify @priya                             # compare safety numbers
```

## Icons

tc draws its icons from **tc Symbols**, a tiny icon font it carries. Install it once on the computer
your terminal runs on:

```
tc fonts install    # ~/Library/Fonts on macOS, ~/.local/share/fonts on Linux
tc fonts test       # every icon next to its name and fallbacks: see if your terminal draws them
tc fonts status     # installed, and up to date?
tc fonts remove
```

A running terminal may need a restart to pick it up. `tc update` keeps an installed font current, and the
installer offers it once on a desktop. Without the font (or over SSH, where fonts live on your own
computer) tc uses plain Unicode icons; Settings › Appearance › Icons picks tc Symbols, Unicode or DOS
yourself.

## Verifying teammates

DMs and private channels are end-to-end encrypted. tc trusts a teammate's SSH key the first time it
sees it and marks them with a dim `?` until you verify them:

- `/verify @bob` shows a 60-digit safety number. Read it to bob over another channel (in person, a
  call); if every digit matches on his screen, press `v`. From the shell: `tc verify @bob`.
- `/verify @bob --github bobs-login` (or `tc verify @bob --github bobs-login`) checks his key against
  `https://github.com/bobs-login.keys` instead; if they match, confirm the account is really his (`v` / `y`).
- If bob's key changes, a red banner says so and sending to channels with him waits until you
  `/accept-key @bob` (it shows the new key; `y` accepts), then `/verify @bob` again.
- If a channel shows `⚠ alarm`, `/alarm` explains what the server did; `/alarm-clear #name` clears it
  once you know why.

## Security model

**What's encrypted.** DMs and private channels are end-to-end encrypted: message text, edits,
deletes and reaction emoji are sealed on your device with a key only the channel's members hold, and
every message is signed by its author's device. The server stores and forwards ciphertext; it (or
anyone who steals its database) can't read or forge those messages. Each membership change starts a
new key: someone removed can't read anything sent after, and new members get the history. Public
channels are not encrypted.

**What the server still sees.** Who is in which channel, who talks to whom and when, message sizes
(rounded), thread links, read markers, channel names and topics. It can also delay or drop messages.
Search in encrypted channels runs on your machine.

**Verifying teammates.** tc trusts a teammate's key the first time it sees it (like SSH or Signal).
Compare safety numbers once (`/verify @bob`, see [Verifying teammates](#verifying-teammates)) to rule
out a server that substituted keys at that first contact. After that, a changed key shows a red
banner and nothing is sent to them until you accept it; a server that tampers with keys, members or
history raises a `⚠ alarm` in that channel and tc stops sending there.

**Accepted limits.**

- The server decides who is a member: it can't add itself unnoticed, but it can hide that someone
  left. The first time your device sees a channel someone else created, the server could present it
  as unencrypted (tc then treats it as plaintext; DMs and channels you create are always encrypted).
- Messages from before a channel was encrypted, and anything the server inserts, are shown as
  `⚠ unauthenticated`. A message whose server timestamp is off by more than 5 minutes shows the
  author's own time and `⚠ server time differs`.
- A stolen device reads what it could already read. `tc devices` lists yours; `tc devices revoke <id>`
  (or `/devices`) revokes one and rotates the keys of every encrypted conversation it could read, so it
  reads nothing new. Revoking doesn't remove an SSH key: if the thief also has your SSH key, an admin
  must `tc users disable` the account and invite you again with a new key. Losing every device loses
  your encrypted history: there is no backup.
- Your local cache (`~/.config/tc`) holds decrypted messages and your keys. Use disk encryption.

Design and threat model: [docs/E2E.md](docs/E2E.md).

## Voice huddles

Voice needs the voice build (tc stays a small static binary without it). Add `--voice` to the
install line, before the invite (Linux needs glibc 2.35+):

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/main/install.sh | sh -s -- --voice 'tc://host:7443/abc…#pin…'
```

Already have tc? Run `tc update --voice` to switch to the voice build in place; `tc update` keeps
whichever build you have, and `tc version` shows `(voice)`.

- `/huddle` joins the current channel's or DM's huddle (or starts it); `/leave-huddle` leaves.
- Hold `ctrl+t` to talk; tap it for an open mic (tap again or `Esc` to close). `/talk` and `/mute`
  do the same where ctrl+t doesn't reach tc (Zellij).
- Huddles in DMs and private channels are end-to-end encrypted. A huddle started in a DM rings
  the other person; channel huddles show a dim `🎧n` in the sidebar instead.
- On macOS the first `/huddle` asks for microphone permission for your terminal app.
- Echo: macOS uses Apple's built-in echo cancellation; on Linux, tc uses a PipeWire/PulseAudio
  echo-cancel device if you've loaded one, else pauses an open mic while others talk. Headphones
  are the surest fix; `/huddle` shows the mode, `TC_VOICE_AEC=auto|system|gate|off` overrides
  ([docs/VOICE.md](docs/VOICE.md#echo)). `TC_VOICE_INPUT=<name>` picks the microphone; a speaker
  "Monitor of …" source is never used as one.

Design: [docs/VOICE.md](docs/VOICE.md).

## Notifications

DMs, @mentions, your highlight words and replies in your threads notify you; the channel you're
looking at doesn't. Per channel, on this device:

- `/notify off` mutes a channel (`/notify off for 2h` for a while), `/notify all` notifies every
  message, `/notify mentions` is the default.
- `/dnd 2h` (or `/dnd until 9am`) pauses popups and sounds; `/notify quiet 22:00-08:00` every night.
- `/notify content name` shows who wrote but not the text (popups stay in the OS history; use it
  on shared machines).
- `/sounds` lists the sounds and turns each on or off. `/notify test` plays one of each through
  the real backend, so you can see what works on your setup.

### When tc is closed

`tc background on` keeps a small listener running (it starts when you log in) that shows the same
desktop notifications while the chat window is closed: same levels, mutes, DND, quiet hours,
content setting and sounds. It stays quiet while tc is open, so nothing pops up twice, and it
never marks anything read. It doesn't make you look online. `tc background status` shows whether
it's running and connected; `tc background off` stops and removes it. The installer offers it
after you join (`TC_BACKGROUND=0` to skip, `=1` to say yes without asking).

It is a LaunchAgent on macOS (`~/Library/LaunchAgents/dev.tc.background.plist`) and a systemd user
unit on Linux (`tc-background.service`; needs `notify-send`). `tc update` restarts it on the new
binary; after `brew upgrade tc` it restarts itself within a minute.

## herdr

Run tc in a herdr pane and it notices (`HERDR_ENV=1`):

- mentions and DMs pop up as herdr notifications (outside herdr: macOS/Linux desktop notifications, else the bell)
- the pane title shows your unread count, e.g. `tc · 3 unread · 1@`
- `/share-pane` picks another pane in the workspace and, after a preview, posts its last 60 lines as a code block

**herdr's popups are off by default** (`[ui.toast] delivery = "off"`): until you set
`delivery = "terminal"` or `"system"` in herdr's config, tc's herdr notifications and their
sounds are invisible and silent. herdr also hides popups for the tab you're looking at. Run
`/notify test` to check.

**Over SSH** in kitty, Ghostty, WezTerm, foot or iTerm2, notifications pop up on your own
machine (terminal OSC notifications); in tmux add `set -g allow-passthrough on`.

`TC_NOTIFY=auto|herdr|os|terminal|bell|off` picks the notification backend; `TC_AWAY_MIN=15` sets
auto-away (0 disables). Key hints use Mac symbols (`⌥K`) on macOS and words (`Alt+K`) elsewhere;
`TC_KEYS=mac|pc` overrides that. Details in [docs/TUI.md](docs/TUI.md).

## Webhooks

CI jobs, scripts and agents can post into a channel with a plain HTTPS POST, no tc
install or SSH key needed. In the channel, run `/webhook ci` (any member can). tc
creates a bot user `ci-bot` (bot names always end in `-bot`), adds it to the channel,
and copies a ready-made curl command to your clipboard. The secret URL is only in the
commented `export` line, never on screen, and the command reads it from the environment
so it stays out of your shell history and `ps`:

```sh
# export TC_HOOK_URL='https://chat.example.com:7443/hooks/abcdefghijklmnopqrstuvwxyz'
curl -fsS -k --pinnedpubkey 'sha256//BASE64PIN=' -H 'Content-Type: application/json' -d '{"text":"hello from a webhook"}' "$TC_HOOK_URL"
```

- The URL is the secret. Treat it like a password: put it in a secret store or run the
  `export` line yourself (without the `#`). `/webhooks` lists hooks; `/webhook-delete <id>`
  revokes one at once (any member of its channel can). Hooks stop working when their
  creator leaves the channel or is disabled.
- The server's certificate is self-signed; `--pinnedpubkey` pins it (`-k` only skips the
  CA/hostname check, the pin is still enforced).
- Any other content type posts the raw body: `make test 2>&1 | tail -50 | curl … --data-binary @- "$TC_HOOK_URL"`.
  ANSI colors are stripped; max 64 KiB.
- Send `Idempotency-Key: <something unique>` to make retries safe.
- Limit: 20 messages burst, then 1/s per hook, and hook messages count against their
  creator's own send limit (HTTP 429 with `Retry-After`). At most 10 hooks per person and
  20 per channel.

GitHub Actions: store the URL and pin as secrets `TC_HOOK_URL` and `TC_HOOK_PIN`
(`sha256//…`):

```yaml
- name: Notify tc
  if: always()
  env:
    TC_HOOK_URL: ${{ secrets.TC_HOOK_URL }}
    TC_HOOK_PIN: ${{ secrets.TC_HOOK_PIN }}
    # Via env, not inline ${{ }}, so branch names can't inject shell.
    MSG: "${{ github.repository }}@${{ github.ref_name }}: ${{ job.status }} - ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}"
    KEY: "${{ github.run_id }}-${{ github.run_attempt }}"
  run: |
    printf '%s' "$MSG" | curl -fsS -k --pinnedpubkey "$TC_HOOK_PIN" \
      -H 'Content-Type: text/plain' -H "Idempotency-Key: $KEY" \
      --data-binary @- "$TC_HOOK_URL"
```

Wire details: [docs/PROTOCOL.md §12](docs/PROTOCOL.md).

## Development

See [CLAUDE.md](CLAUDE.md). `make test` runs everything with the race detector; the wire
protocol is specified in [docs/PROTOCOL.md](docs/PROTOCOL.md). Trying it by hand:
[docs/TESTING.md](docs/TESTING.md).
