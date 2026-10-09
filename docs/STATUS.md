# Status & handoff

Last updated: 2026-10-10 (end of session 1; branch `m2-activity` adds drafts, highlights, activity view). Read this first when resuming.

## Where we are

| Milestone | State |
|---|---|
| M0 scaffold, spec, agents, CI | ✅ done |
| M1 core chat (server, client, TUI, sync, offline outbox) | ✅ done, battle-tested (chaos + adversarial + live herdr) |
| M2 collaboration | ~75%: herdr/desktop notifications, unread badge, `/share-pane`, auto-away, `tc notify`, notes (`@me`), display names (`/name`), thread history, webhooks + bots, privacy scrubbing, per-channel drafts, `/highlight`, activity view (Alt-A) — all merged |
| M3 end-to-end encryption | steps 1–5 merged (#9 crypto, #10 certs, #11 trust store, #13 server devices, #16 server epochs); **step 6 (client verification, key.need/share, RotateKey) in progress** on branch `e2e/client-bundles` |
| M4 zero-setup onboarding | not started |
| M5 voice | not started |

Release tag: `v0.1.0` = M1. No newer tag yet.

## Resume checklist (do in order)

1. **E2E step 6** (`e2e/client-bundles`, worktree `.claude/worktrees/kiro-verify`): client verifies certs
   (TOFU + endorsements), epoch chain (sig, prev, roster, by-authorization, rollback/fork alarms), opens boxes
   (kc), backlinks, `key.need`/`key.share`, `Client.RotateKey`, moves device key into `internal/trust`.
   Review with protocol-guardian + security-reviewer, then merge.
2. **E2E steps 7–12** per `docs/E2E.md`: 7 encrypted send/outbox (DMs created as e2e since step 5 still send
   plaintext until this lands — handle that history), 8 edit/delete/react + local FTS, 9 membership changes +
   `chan.remove` + `chan.encrypt` (legacy DMs), 10 TUI warnings + `/verify`, 11 malicious-server suite,
   12 security pass + chaos. Webhooks must be forbidden into e2e channels (step 7).
3. **Finish M2**: diff and `file:line` snippets. (Long-line viewer, `/pins`, `/remind` and user groups
   `@team` are done.)
4. **Deploy**: waiting on the droplet IP → `docs/DEPLOY.md`, then real-network tests.
5. **M4 onboarding** — see `docs/ROADMAP.md`. Note: the repo is private, so install-from-GitHub-releases needs
   auth; consider serving install.sh + binaries from the tc server itself (pinned TLS).

**Budget rule (owner):** kiro-cli may use at most 45–50% of its monthly credits (check with
`kiro-cli chat --no-interactive "/usage"`); it was 44.3% before step 6 started. Above ~47% use Claude only.

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
- Live testing: herdr tab "feat · terminal chat · servers" (recreated in session 2; herdr key names are
  `ctrl+n`, `alt+a`, `esc`, `enter`): pane `tc server` runs
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
#9 e2ecrypto · #10 device certs · #11 trust store. Session 2: #12 status docs · #13 E2E step 4 · #14 TUI gaps
(/name, thread history, ? fix) · #15 drafts/highlights/activity · #16 E2E step 5.
