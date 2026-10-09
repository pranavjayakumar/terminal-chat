# Status & handoff

Last updated: 2026-10-10 (end of session 1). Read this first when resuming.

## Where we are

| Milestone | State |
|---|---|
| M0 scaffold, spec, agents, CI | ✅ done |
| M1 core chat (server, client, TUI, sync, offline outbox) | ✅ done, battle-tested (chaos + adversarial + live herdr) |
| M2 collaboration | ~60%: herdr/desktop notifications, unread badge, `/share-pane`, auto-away, `tc notify`, notes (`@me`), display names (server), thread history (server+client), webhooks + bots, privacy scrubbing — all merged |
| M3 end-to-end encryption | steps 1–3 merged (#9 crypto, #10 device certs, #11 trust store); **step 4 in PR** (`e2e/server-devices`) |
| M4 zero-setup onboarding | not started |
| M5 voice | not started |

Release tag: `v0.1.0` = M1. No newer tag yet.

## Resume checklist (do in order)

1. **Step 4 PR** (`e2e/server-devices`, worktree `.claude/worktrees/kiro-devices`): merge of main was being
   resolved at pause time (spec renumbered: webhooks = §12, E2E = §13). Check the branch builds, open/finish
   its PR, run the **protocol-guardian** review on it (worker ran its own reviewer instead), then merge.
   Follow-ups noted by the worker: device key lives in `TC_HOME/device.key` with a TODO — move it into
   `internal/trust` (`trust.Store.DeviceKeys`); registration errors are silent (no client logger); hardware
   keys prompt for a touch once per device in the background; no cap on unverified objects a server can push
   into the client cache; a revoked device only notices on next connect.
2. **E2E steps 5–12** per `docs/E2E.md` "PR plan": 5 server epochs (`key.rotate` CAS, roster, `key.fetch`,
   `key.epoch`), 6 client bundle verification (reject duplicate recipient X25519 keys), 7 encrypted send +
   outbox, 8 encrypted edit/delete/react + local FTS, 9 membership changes + `chan.remove` + backlinks,
   10 TUI warnings + `/verify`, 11 malicious-server test suite, 12 security pass + chaos during rotations.
   Each protocol step: update PROTOCOL.md §13 + protocol-guardian review.
3. **Finish M2** (TUI/client gaps): `/name` command (server op `user.update` exists), thread view fetching
   older replies (`client.ThreadHistory` exists), `?` on empty input steals messages starting with `?`
   (make help F1 / `/help` only), long-code-line viewer, activity view (Ctrl-A), per-channel drafts, keyword
   highlights, `/pin` `/pins`, `/remind`, user groups `@team`, diff and `file:line` snippets.
4. **Deploy**: waiting on the user's DigitalOcean droplet IP → follow `docs/DEPLOY.md`, then a real-network
   test (QUIC over the internet, TCP fallback behind UDP-blocking networks, webhooks via real curl).
5. **M4 onboarding** (install-command invites, `tc join`, brew, `tc update`, goreleaser, multi-use team
   invites) — see `docs/ROADMAP.md`.

## How we work (keep doing this)

- Workers run on **kiro-cli** to save Claude usage (owner's request). Template:
  ```sh
  git worktree add .claude/worktrees/<name> -b <branch> origin/main
  cd .claude/worktrees/<name>
  env -u HERDR_ENV -u HERDR_PANE_ID -u HERDR_TAB_ID -u HERDR_WORKSPACE_ID \
    kiro-cli chat --no-interactive --trust-all-tools --model claude-opus-5.5 "<prompt>" > <log> 2>&1
  ```
  (unset the herdr vars or the worker inherits/renames the coordinator's pane). Opus for risky work,
  `claude-sonnet-5.5` for merges/mechanical work. Prompts must say: read CLAUDE.md + relevant docs, run go
  with `GOTOOLCHAIN=local _ZO_DOCTOR=0`, small commits ending with the Co-Authored-By trailer, never push,
  gofmt + vet + `go test -race -count=1 ./...` green before finishing, print commit list + test output.
- Coordinator (Claude) reviews diffs, runs tests, pushes, opens PRs, merges after CI green.
- Every feature: a review agent pass (security / protocol / tui / breaker as CLAUDE.md says), then an
  **adversarial verifier** that tries to bypass each fix and keeps failing tests (this caught 2 webhook
  bypasses). New tests must be shown to fail without their fix.
- To update a pushed PR branch: `git merge origin/main` (no rebase, no force-push).
- Live testing: herdr tab "feat · terminal chat · servers": pane `tc server` runs
  `./bin/tc server --data <scratch>/live/srv --listen 127.0.0.1:17450`, panes `alice`/`bob` run `tc` with
  `TC_HOME=<scratch>/live/{a,b}` and `SSH_AUTH_SOCK=` (test keys). The scratch dir is session-specific; if
  it's gone, recreate: start a server with a temp `--data`, `tc login` two users with throwaway
  `ssh-keygen -t ed25519` keys and `--key`. Drive panes with `herdr pane send-text/send-keys`, read with
  `herdr pane read <id> --source recent`.

## Known issues / decisions log

- Go must run with `GOTOOLCHAIN=local` (deps pinned to versions whose go directive ≤ 1.25).
- herdr 0.9.1 CLI wants the positional pane id/title before options.
- macOS LibreSSL curl can't verify Ed25519 → HTTP side (webhooks) uses a separate ECDSA cert/pin.
- Bubble Tea delivers fast/IME/pasted input as multi-rune KeyMsg; always match keys via `keyName(k)`.
- Server log is scrubbed on edit/delete (not strictly append-only); SQLite `secure_delete` + FTS5
  secure-delete on.
- Bots are named `*-bot`, owned, capped (10 active per owner, 100 per server), retired when their last hook
  is deleted.
- E2E design r1 was rejected (malicious server could read DMs); r2 in `docs/E2E.md` is authoritative.

## Merged PRs (session 1)

#1 battle tests · #2 M2 core · #3 typed-words-as-keys fix · #4 E2E design r2 · #5 herdr + notifications ·
#6 deflake sync test · #7 webhooks (+ security fixes + verifier bypass fixes) · #8 privacy scrubbing ·
#9 e2ecrypto · #10 device certs · #11 trust store.
