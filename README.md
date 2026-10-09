# tc — terminal chat

Team chat that lives in your terminal (and your herdr panes). One small binary, Mac + Linux.

- Instant start from a local cache, works offline, sends when you're back
- Channels, private channels, DMs, threads, reactions, edits, search
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
| `Alt-↑/↓` | previous / next channel (`Alt-Shift` for unread) |
| `↑` on empty input | select a message: `e` edit · `d` delete · `r` thread · `+` react · `y` copy |
| `Esc` | back out of anything |
| `?` or `/help` | all keys and commands |
| `Ctrl-C` twice | quit |

Commands: `/join #x` `/create #x [--private]` `/add @user` `/dm @a @b` `/topic …` `/search …` `/away [status]` `/back` `/invite` (admin).

From the shell:

```sh
tc send '#general' deploy finished          # message
kubectl logs pod/api | tc send @priya        # piped → code block (colors stripped)
tc tail '#deploys'                           # stream a channel
```

## Development

See [CLAUDE.md](CLAUDE.md). `make test` runs everything with the race detector; the wire
protocol is specified in [docs/PROTOCOL.md](docs/PROTOCOL.md).
