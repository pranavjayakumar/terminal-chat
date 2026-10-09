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

Commands: `/join #x` `/create #x [--private]` `/add @user` `/dm @a @b` `/topic …` `/name …` `/search …` `/pins` `/activity` `/highlight [word…]` `/unhighlight word…` `/away [status]` `/back` `/snippet file[:range]` `/share-pane [pane]` `/invite` (admin) `/webhook name` `/webhooks` `/webhook-delete id`.

Unsent text is kept per channel and thread (a dim `✎` in the sidebar marks channels with a draft) and
survives restarts. `/highlight deploy oncall` makes those words count like an @mention: marker, unread
`@` badge and notification.

From the shell:

```sh
tc send '#general' deploy finished          # message
kubectl logs pod/api | tc send @priya        # piped → code block (colors stripped)
(git diff) | tc send '#review'               # a diff block, colored in tc
tc send '#backend' --file pkg/db.go:40-60 look at this   # lines 40-60 as a code block + permalink
tc tail '#deploys'                           # stream a channel
```

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
