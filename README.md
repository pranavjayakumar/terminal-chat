# tc — terminal chat

Team chat that lives in your terminal (and your herdr panes). One small binary, Mac + Linux.

- Instant start from a local cache, works offline, sends when you're back
- Channels, private channels, DMs, threads, reactions, edits, search, pinned messages
- Sign in with your existing SSH key — no passwords, no accounts to manage
- Pipe anything into chat: `make test 2>&1 | tc send '#backend'`

## Quickstart

**Server** (once, on any machine teammates can reach — see [docs/DEPLOY.md](docs/DEPLOY.md)):

```sh
tc server --public YOUR_IP:7443       # prints a one-time admin invite on first run
tc server invite                       # one invite per teammate
```

**Everyone:**

```sh
tc login 'tc://host:7443/abc…#pin…'   # uses ssh-agent or ~/.ssh/id_ed25519
tc                                    # chat
```

## Using it

| | |
|---|---|
| `Enter` / `Alt-Enter` | send / newline |
| `Ctrl-K` | jump to any channel or person |
| `Alt-A` | activity: mentions, replies to you, reactions, DMs (Enter jumps) |
| `Alt-↑/↓` | previous / next channel (`Alt-Shift` for unread) |
| `↑` on empty input | select a message: `e` edit · `d` delete · `r` thread · `+` react · `y` copy · `v` view full · `p` pin/unpin |
| `Esc` | back out of anything |
| `F1` or `/help` | all keys and commands |
| `Ctrl-C` twice | quit |

Commands: `/join #x` `/create #x [--private]` `/add @user` `/dm @a @b` `/topic …` `/name …` `/search …` `/pins` `/activity` `/highlight [word…]` `/unhighlight word…` `/group name @a @b` (admin) `/groups` `/remind <when> <text>` `/reminders` `/remind-delete id` `/away [status]` `/back` `/snippet file[:range]` `/share-pane [pane]` `/invite` (admin) `/webhook name` `/webhooks` `/webhook-delete id` `/verify @user` `/accept-key @user` `/members` `/alarm`.

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
- A stolen device reads what it could already read. Once it is revoked, tc rotates keys before
  anything new is sent (there is no `tc` command to revoke a device yet: log in with a new SSH key and
  ask an admin to disable the old account, or see docs/E2E.md). Losing every device loses your
  encrypted history: there is no backup.
- Your local cache (`~/.config/tc`) holds decrypted messages and your keys. Use disk encryption.

Design and threat model: [docs/E2E.md](docs/E2E.md).

## herdr

Run tc in a herdr pane and it notices (`HERDR_ENV=1`):

- mentions and DMs pop up as herdr notifications (outside herdr: macOS/Linux desktop notifications, else the bell)
- the pane title shows your unread count, e.g. `tc · 3 unread · 1@`
- `/share-pane` picks another pane in the workspace and, after a preview, posts its last 60 lines as a code block

`TC_NOTIFY=auto|herdr|os|bell|off` picks the notification backend; `TC_AWAY_MIN=15` sets
auto-away (0 disables). Details in [docs/TUI.md](docs/TUI.md).

## Webhooks

CI jobs, scripts and agents can post into a channel with a plain HTTPS POST — no tc
install or SSH key needed. In the channel, run `/webhook ci` (any member can). tc
creates a bot user `ci-bot` (bot names always end in `-bot`), adds it to the channel,
and copies a ready-made curl command to your clipboard. The secret URL is only in the
commented `export` line, never on screen, and the command reads it from the environment
so it stays out of your shell history and `ps`:

```sh
# export TC_HOOK_URL='https://chat.example.com:7443/hooks/abcdefghijklmnopqrstuvwxyz'
curl -fsS -k --pinnedpubkey 'sha256//BASE64PIN=' -H 'Content-Type: application/json' -d '{"text":"hello from a webhook"}' "$TC_HOOK_URL"
```

- The URL is the secret — treat it like a password: put it in a secret store or run the
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

GitHub Actions — store the URL and pin as secrets `TC_HOOK_URL` and `TC_HOOK_PIN`
(`sha256//…`):

```yaml
- name: Notify tc
  if: always()
  env:
    TC_HOOK_URL: ${{ secrets.TC_HOOK_URL }}
    TC_HOOK_PIN: ${{ secrets.TC_HOOK_PIN }}
    # Via env, not inline ${{ }}, so branch names can't inject shell.
    MSG: "${{ github.repository }}@${{ github.ref_name }}: ${{ job.status }} — ${{ github.server_url }}/${{ github.repository }}/actions/runs/${{ github.run_id }}"
    KEY: "${{ github.run_id }}-${{ github.run_attempt }}"
  run: |
    printf '%s' "$MSG" | curl -fsS -k --pinnedpubkey "$TC_HOOK_PIN" \
      -H 'Content-Type: text/plain' -H "Idempotency-Key: $KEY" \
      --data-binary @- "$TC_HOOK_URL"
```

Wire details: [docs/PROTOCOL.md §12](docs/PROTOCOL.md).

## Development

See [CLAUDE.md](CLAUDE.md). `make test` runs everything with the race detector; the wire
protocol is specified in [docs/PROTOCOL.md](docs/PROTOCOL.md).
