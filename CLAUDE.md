# tc — terminal chat

A lightweight team chat that lives in the terminal (and in herdr panes).
One Go binary is both client (`tc`) and server (`tc server`). Mac + Linux.

## Product bar (from the owner)

Lightweight · feature-rich but shipped iteratively (every feature lands with no known bugs) ·
battle-tested (adversarial + chaos + live tests, not just unit tests) · seamless to use ·
one-step onboarding · end-to-end encrypted · blazing fast. When these conflict, prefer
correctness and speed over adding another feature.

## Principles

1. **Small and fast.** Cold start < 50 ms, idle RSS < 30 MB, binary < 20 MB. No new
   dependency without a reason written in the PR/commit.
2. **Never lose or duplicate a message.** Outbox + idempotent sends + seq-based sync.
3. **Message text is hostile.** Everything rendered goes through `internal/sanitize`.
4. **The spec is the contract.** `docs/PROTOCOL.md` describes the wire format;
   `internal/proto` implements it. Change both together, or neither.
5. **Keyboard first, minimal UI.** No mouse required, no chrome that doesn't earn its space.

## Layout

```
cmd/tc/              entrypoint + subcommands (tc, tc server, tc login, tc send, tc tail)
internal/proto/      frame codec, frame/op/event types (pure, no I/O deps)
internal/transport/  QUIC + TLS-over-TCP listeners/dialers, Conn interface, cert pinning
internal/identity/   SSH key loading, ssh-agent signing, auth signature payloads
internal/server/     hub (sessions, fan-out), handlers, auth, store (SQLite)
internal/client/     connection manager, reconnect, outbox, local cache (SQLite), event apply
internal/tui/        Bubble Tea UI
internal/sanitize/   terminal-safe text rendering
internal/e2e/        real client against real server over loopback
docs/PROTOCOL.md     wire protocol (source of truth)
```

Dependency direction: `tui → client → proto/transport/identity`; `server → proto/transport/identity`.
`client` and `server` never import each other. `proto` imports nothing internal.

## Stack

- Go 1.25, `github.com/quic-go/quic-go`, `github.com/vmihailenco/msgpack/v5`
- SQLite via `modernc.org/sqlite` (pure Go — keep text builds **CGO_ENABLED=0**)
- TUI: `charmbracelet/bubbletea`, `lipgloss`, `bubbles`
- SSH: `golang.org/x/crypto/ssh` (+ `/agent`)
- Voice (M3 only, cgo, build-tagged `voice`): libopus + miniaudio

## Commands

Always run Go with `GOTOOLCHAIN=local`. The module targets the installed Go 1.25. Without
it, a dependency that needs a newer Go makes `go get` download a whole toolchain and
appear to hang. Pick dependency versions whose `go` directive is ≤ 1.25.

```
make build        # ./bin/tc
make test         # go test -race ./...
make fuzz         # short fuzz run on proto decoder + sanitizer
make lint         # go vet + staticcheck (if installed)
make dev          # local server on :7443 with ./dev-data
```

Local two-user test: `make dev`, then in two panes
`TC_HOME=/tmp/tc-a ./bin/tc login <invite>` / `TC_HOME=/tmp/tc-b ...`.

## Conventions

- Errors: wrap with `fmt.Errorf("doing x: %w", err)`; protocol errors use the stable codes in PROTOCOL.md §5.
- Context-first signatures; every goroutine has an owner that can cancel it.
- No global state except in `main`. Config via `TC_HOME` (default `~/.config/tc`) and flags.
- Comments explain *why*, not what. Keep them sparse.
- Tests live next to code. Integration tests (real server + several clients over
  loopback, both QUIC and TCP) go in `internal/server/integration_test.go`.
- TUI tests drive the model directly through a fake `Backend` (no network) and assert every frame fits the terminal.

## Definition of done (per change)

- `go build ./... && go vet ./... && go test -race ./...` pass (hooks enforce build/vet).
- Protocol touched → `protocol-guardian` reviewed and PROTOCOL.md updated.
- Network/state logic touched → `breaker` run against it.
- Auth/invites/rendering touched → `security-reviewer` run.
- TUI touched → `tui-reviewer` run.

## Milestones

- **M0** scaffold, spec, agents, CI  ✓
- **M1** core text chat: server, auth, channels, DMs, sync, reconnect, TUI  ← current
- **M2** threads, reactions, search, notifications, herdr integration, `tc send/tail`, webhooks
- **M3** voice huddles (QUIC datagrams, Opus)
- **M4** distribution: goreleaser, install.sh, brew tap, `tc update`, deploy guide

## Herdr

Users run tc inside herdr. Integration uses the herdr CLI only (never assume it exists —
detect `HERDR_PANE_ID`): `herdr notification show` for mentions,
`herdr pane report-metadata` for unread badges, `herdr pane read` for `/share-pane`.
