# Status & handoff

Last updated: 2026-10-10 (end of session 1; branch `m2-activity` adds drafts, highlights, activity view). Read this first when resuming.

## Handoff (always current — read this first when switching accounts)

**Goal of the current push (owner, 2026-10-10):** finish M3 (E2E steps 11–12) and M4 (onboarding) end to end,
cut release v0.2.0, redeploy the droplet, hand the owner the one-line install/invite command, then PAUSE.

**In flight**
- E2E step 11 malicious-server suite — Claude subagent; worktree `.claude/worktrees/agent-ab2d5e801827b592f`,
  branch `e2e/malicious-server` (not pushed yet). If that worktree is gone/unfinished: re-run step 11 per
  `docs/E2E.md` PR plan on a fresh branch.
- M4 onboarding — kiro; worktree `.claude/worktrees/kiro-onboard`, branch `m4-onboarding`; prompt covers release
  workflow, install.sh, `tc join`, invite one-liner, multi-use invites, `tc update`.

**Next (in order):** review + merge step 11 → review + merge M4 → E2E step 12 (security pass + chaos during
rotations) on kiro → tag v0.2.0 (release workflow builds binaries) → redeploy droplet with the release binary
(`docs/DEPLOY.md`) → test the one-liner from a clean HOME → give the owner the command → update this file → pause.

**Rules:** kiro hard cap 55–60% of monthly credits (check `kiro-cli chat --no-interactive "/usage"`; stop at 55%).
Claude only orchestrates (review/merge/log); workers on kiro, launched with herdr env vars unset. Merge only with
`gh pr checks <n> --watch` exit 0 (CI works again since the repo is public). Log every event in `docs/WORKLOG.md`.

**Production server:** root@159.65.152.35 (key-only SSH from the owner's Mac key), systemd unit `tc`,
data `/var/lib/tc`, public addr 159.65.152.35:7443, pin `kizIIb-LUvMblyz18Ovf52VftSZnISNdJAEUz56LoU4`.
The owner's one-time admin invite was printed in `journalctl -u tc` on first start (don't consume it).
The owner should change the droplet root password (it was shared in chat; password SSH is disabled).

## Where we are

| Milestone | State |
|---|---|
| M0 scaffold, spec, agents, CI | ✅ done |
| M1 core chat (server, client, TUI, sync, offline outbox) | ✅ done, battle-tested (chaos + adversarial + live herdr) |
| M2 collaboration | ✅ essentially done: notifications, herdr badge, `/share-pane`, auto-away, `tc notify`, notes, `/name`, threads, webhooks + bots, privacy scrubbing, drafts, `/highlight`, activity (Alt-A), message viewer (`v`), pins (`p`, `/pins`), user groups (`@oncall`), reminders (`/remind`), diff/snippets (`tc send --file`, `/snippet`) |
| M3 end-to-end encryption | steps 1–7 merged; step 9 (membership: chan.add+bundle, chan.remove, chan.encrypt) on branch `e2e/membership`; (#9, #10, #11, #13, #16, #25 client verification, #26 encrypted send/receive) — **encrypted DMs and private channels work end to end**; step 8 (encrypted edit/delete/react) in PR #28; step 10 (trust UX: safety numbers, `/verify`, `--github`, key-change banner, `/accept-key`, `/alarm`, audited `/alarm-clear`, `?` marks, `tc verify`) on branch `e2e/trust-ux` |
| M4 zero-setup onboarding | not started |
| M5 voice | not started |

Release tag: `v0.1.0` = M1. No newer tag yet.

## Resume checklist (do in order)

1. **E2E step 7** (branch `e2e/encrypted-send`): `send`/`Message` + `enc`; server refuses plaintext
   send/edit in e2e channels, dedupes before `stale_epoch`, checks enc signer = caller's active device;
   search skips e2e; webhooks forbidden (403). Client encrypts once into the outbox, stale_epoch → re-encrypt
   with the same nonces, rotates when stale, "no key yet" failure + key.need + requeue; decrypts/verifies
   incoming (lazy queue for missing keys/devices/roots), legacy vs server-inserted plaintext (trust schema
   v8: epochs.seq), local search, `Update.New` is `[]client.Message` with `Sec`. TUI: 🔒 header + key
   state, per-message markers, no sends in alarm. `tc send/notify/tail` covered. Decisions in `docs/E2E.md`
   step 7. Review with protocol-guardian + security-reviewer + tui-reviewer + breaker, then merge.
2. **E2E steps 8–12** per `docs/E2E.md`: 8 edit/delete/react + local FTS, 9 membership changes +
   `chan.remove` + `chan.encrypt` (legacy DMs), 10 TUI warnings + `/verify`, 11 malicious-server suite,
   12 security pass + chaos.
3. **Finish M2**: diff and `file:line` snippets. (Long-line viewer, `/pins`, `/remind` and user groups
   `@team` are done.)
4. **Deploy**: waiting on the droplet IP → `docs/DEPLOY.md`, then real-network tests.
5. **M4 onboarding** — see `docs/ROADMAP.md`. Note: the repo is private, so install-from-GitHub-releases needs
   auth; consider serving install.sh + binaries from the tc server itself (pinned TLS).

**Budget rule (owner):** kiro-cli may use at most 45–50% of its monthly credits (it reached 46.6% on 2026-10-10,
so remaining work that day ran on Claude subagents) (check with
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
- Merge only with `gh pr checks <n> --watch` exiting 0, then `gh pr merge <n> --merge` (a hand-rolled
  pending-loop once merged a PR with checks still pending).
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
(/name, thread history, ? fix) · #15 drafts/highlights/activity · #16 E2E step 5 · #17 status ·
#18 message viewer · #19/#22 deflakes · #20 pins · #21 snippets/diffs · #23 groups + reminders.
