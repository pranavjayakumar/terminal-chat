# tc onboarding

tc is a lightweight team chat that lives in your terminal and your herdr panes. One small
binary, Mac and Linux, end-to-end encrypted DMs and private channels, signed releases.

## 1. Join in one command

Your admin sends you a line like this. Paste it into a terminal:

```sh
curl -fsSL https://raw.githubusercontent.com/pranavjayakumar/terminal-chat/v0.3.0/install.sh | sh -s -- 'tc://host:7443/token#pin'
```

What happens:

1. `install.sh` downloads the release for your OS and CPU, checks the maintainer's SSH signature on
   the checksums, and installs `tc` into `~/.local/bin` (it tells you if that's not on your PATH).
2. `tc join` tries your SSH keys (ssh-agent, then `~/.ssh`, or it creates one for tc), suggests a
   name from your git config, and asks at most two questions. If the invite is tied to your GitHub
   keys, it picks the matching one.
3. You're in `#general`. Run `tc` to open the chat.

Already have tc? `tc join '<invite>'`. On a Mac with Homebrew:
`brew tap pranavjayakumar/tc https://github.com/pranavjayakumar/terminal-chat && brew install pranavjayakumar/tc/tc`.
Keep it current with `tc update` (signed, atomic) or `brew upgrade tc`.

Interrupted? Run the same command again: a key that's already registered just logs in, and the
invite isn't used up.

**Second computer:** run `tc login` on the first one. It prints `tc login 'tc://host:port#pin'`; run
that on the other computer with the same SSH key there (or re-run your original install command).

**Lost a laptop?** `tc devices` lists your devices, `tc devices revoke <id>` (or `/devices` in chat)
revokes one and re-keys every encrypted conversation it could read. If the thief may also have your
SSH key, ask an admin to `tc users disable` the account and invite you again with a new key.

## 2. Everyday use

| Key | Does |
|---|---|
| `Enter` / `Alt-Enter` | send / newline |
| `Ctrl-K` | jump to any channel or person |
| `Alt-↑/↓` | previous / next channel (`Alt-Shift` for unread) |
| `↑` on empty input | select a message: `e` edit, `d` delete, `r` thread, `+` react, `y` copy, `v` view, `p` pin |
| `Alt-A` | activity: mentions, replies, reactions, DMs |
| `F1` or `/help` | every key and command |
| `Esc` | back out of anything |
| `Ctrl-C` twice | quit |

Common commands: `/dm @a @b`, `/create #x [--private]` (private = encrypted), `/join #x`, `/add @user`, `/topic …`,
`/name …`, `/search …`, `/pins`, `/highlight word`, `/group oncall @a @b`, `/remind 2h text`,
`/snippet file.go:40-60`, `/away`, `/back`.

From the shell:

```sh
tc send '#general' deploy finished          # a message
make test 2>&1 | tc send '#backend'          # piped output becomes a code block
tc send '#review' --file api.go:40-60        # code with a GitHub permalink
tc notify -- make build                      # runs it, then posts ✓/✗, time and output tail to your notes
tc tail '#deploys'                           # stream a channel
```

## 3. herdr

Run tc in a herdr pane and it notices automatically:

- mentions and DMs pop up as herdr notifications (outside herdr: desktop notifications)
- the pane title shows your unread count, e.g. `tc · 3 unread · 1@`
- `/share-pane` picks another pane and posts its last lines after a preview
- `TC_NOTIFY=auto|herdr|os|bell|off`, `TC_AWAY_MIN=15` (auto-away)

## 4. Encryption and verifying people

- DMs and private channels are end-to-end encrypted: the server stores only ciphertext. The header
  shows 🔒. Public channels stay readable by the server (so search, webhooks and bots work there).
- Keys are trusted on first use. To make sure nobody is in the middle, verify each teammate once:
  `/verify @name` shows a 60-digit safety number. Compare it with them over another channel (call,
  in person) and press `v` if it matches. Or `/verify @name --github theirlogin` to check against
  their GitHub SSH keys.
- If someone's key changes you'll see a red banner and sending to shared encrypted channels pauses:
  `/accept-key @name` (shows the new fingerprints), then `/verify @name`.
- Red alarms in the header mean the client detected something the server shouldn't do (a forked or
  rolled-back key chain, a downgrade, an unconfirmed member). `/alarm` explains it.
- Private channels: `/add @user` and `/remove @user` rotate the channel key automatically; `/encrypt`
  converts an existing private channel.

## 5. For admins

```sh
tc server invite --uses 15 --ttl 24h        # one line for the whole team
tc server invite --admin                     # single-use, 24 h max
tc server invite --list / --revoke ID        # see and cancel live invites
tc invite --key github:alice                 # only alice's GitHub SSH keys can redeem it
tc users                                     # accounts and who invited them
tc users disable @name                       # lock an account out for good
```

Members can invite too (`/invite 5 1d`; up to 10 uses, 7 days, 5 live invites each); turn that off
with `tc server --member-invites=false`. In chat: `/invite 15 24h` shows and copies the one-liner,
`/invites`, `/invite-revoke ID`, `/disable @name`, `/webhook name` (CI and agents can then post with
curl; webhooks need a plain channel: `/create #ci --private --plaintext`), `/group …`.

Running the server: see [docs/DEPLOY.md](docs/DEPLOY.md). The production server runs as a systemd
unit `tc` with its data in `/var/lib/tc`. Back up `tc.db` with `sqlite3 … ".backup …"`, and keep
`server.key`, because losing it invalidates every client's pin.

## 6. How it's built (for contributors)

- One Go binary is both client and server. QUIC with TLS-over-TCP fallback on one port, its own
  msgpack protocol ([docs/PROTOCOL.md](docs/PROTOCOL.md)), SQLite on both sides, Bubble Tea TUI.
- Packages: `internal/proto` (wire), `transport`, `server`, `client` (cache, outbox, sync, E2E),
  `tui`, `e2ecrypto`, `trust` (pins, epochs, alarms), `identity` (SSH keys, signed objects),
  `notify`, `release`, `snippet`, `sanitize`.
- Encryption design and threat model: [docs/E2E.md](docs/E2E.md).
- Rules of the road: [CLAUDE.md](CLAUDE.md). Current state and next steps: [docs/STATUS.md](docs/STATUS.md).
- `make test` (race detector), `make test-malicious` (malicious-server suite), `make fuzz`.
- Releases: tag `vX.Y.Z` on main → workflow builds a draft → `make release-sign TAG=… KEY=…` signs
  the checksums with the maintainer's SSH key and publishes.
