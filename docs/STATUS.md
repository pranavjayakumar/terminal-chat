# Status & handoff

Last updated: 2026-10-17 (session 4). Read this first when resuming.

## Handoff (always current — read this first when switching accounts)

**v0.3.0 RELEASED (2026-10-17, signed).** Droplet upgraded to v0.3.0 (DB backup at
/root/tc-pre-v030-2026-10-17.db, old binary /root/tc-v0.2.0). Fresh invites printed for the owner (admin single-use
+ 15-use team, both expire 2026-10-18 12:05 UTC; regenerate with `tc server invite` on the droplet).

**ACTIVE: v0.4.0 = M5 voice huddles, draft PR #47** (worktree agent-a4fce39fe1475bfe8). Coordinator defaults for
the owner decisions (owner may override): no voice over TCP fallback; shared frame keys in encrypted huddles for v1;
separate signed `tc-voice_<os>_<arch>` artifact (default `tc` stays CGO_ENABLED=0); ±60 s clock window; no
huddle-start notification. Remaining: merge main, CI + release jobs for the voice build, `install.sh --voice`,
UX follow-ups, budgets, reviews; then real two-machine mic test by the owner, tag v0.4.0.

Follow-ups after v0.4: #59 admin add-key invite command; per-key removal (`key.remove`, proposed in #52); M6 from
`docs/research/PRODUCT-2026-10.md` (owner to approve); rename.

Done this session: #43 docs, #44 brew tap, #45 first-start (#42), #46/#49 chaos deflakes, #48 member + github-key
invites, #50 device list/revoke, #52 B1–B3 (add-key invites admin-only, `tc users [disable]`, invited_by), #51 UX
polish, #53 B4/B7/B8, #58 `tc login` + join fixes, #60 CI flakes, #61 B5/B11/B12 device revoke gaps.

**How the parallel run works:** one Claude subagent per area in its own worktree (`isolation: worktree`), each
owns named files; overlaps are called out in the agent prompt. Reviewer agents (tui/security/protocol/breaker)
check each PR before merge. Merge one PR at a time; later branches `git merge origin/main`. `gh` must use the
personal account (`gh auth status` → pranavjayakumar active).

- **Released:** v0.2.0 (signed with the owner's SSH key; https://github.com/pranavjayakumar/terminal-chat/releases/tag/v0.2.0).
- **Production:** root@159.65.152.35 (key-only SSH), systemd `tc` running v0.2.0 on :7443 (UDP+TCP), data
  `/var/lib/tc`, pin `kizIIb-LUvMblyz18Ovf52VftSZnISNdJAEUz56LoU4`. The Oct 10 invites expired; make new ones
  after the v0.3.0 redeploy. Owner should change the droplet root password (it was shared in chat).
- **Name:** stays `tc` until v0.3/v0.4 are done, then rename (owner). `tc` clashes with iproute2's `tc` on Linux.

**Rules:** kiro hard cap 55–60% of monthly credits (check `kiro-cli chat --no-interactive "/usage"`); once reached,
run workers as Claude subagents. Claude orchestrates (review/merge/log). Merge only with `gh pr checks <n> --watch`
exit 0. Log every event in `docs/WORKLOG.md`. Delete worktrees/local branches after merge.

## Where we are

| Milestone | State |
|---|---|
| M0 scaffold, spec, agents, CI | ✅ done |
| M1 core chat (server, client, TUI, sync, offline outbox) | ✅ done, battle-tested (chaos + adversarial + live herdr) |
| M2 collaboration | ✅ essentially done: notifications, herdr badge, `/share-pane`, auto-away, `tc notify`, notes, `/name`, threads, webhooks + bots, privacy scrubbing, drafts, `/highlight`, activity (Alt-A), message viewer (`v`), pins (`p`, `/pins`), user groups (`@oncall`), reminders (`/remind`), diff/snippets (`tc send --file`, `/snippet`) |
| M3 end-to-end encryption | ✅ done: steps 1–12 (PRs #9–#13, #16, #25, #26, #28–#30, #32, step 12 PR) incl. malicious-server suite, chaos during rotations, fuzzing, security pass |
| M4 zero-setup onboarding | ✅ done (PR #34): signed releases, verified install.sh, `tc join`, invite one-liner, team invites + list/revoke, `tc update`. Deferred: brew tap, any-member invites |
| M5 voice | not started |

Release tags: `v0.1.0` = M1, `v0.2.0` = M2 + M3 + M4 (signed). Next: `v0.3.0` (#42, member/github-key invites, device revoke, brew), `v0.4.0` (M5 voice).

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
   `chan.remove` + `chan.encrypt` (legacy DMs), 10 TUI warnings + `/verify`, 11 malicious-server suite
   (done: `make test-malicious`; results table and decisions in `docs/E2E.md` step 11),
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
- Malicious-server tests need `-tags tctamper` (server hooks in `internal/server/tamper_on.go`, absent from
  default/release builds; `tc server` refuses to run from a tagged build). `make test` runs them; plain
  `go test ./...` does not. GitHub Actions was unavailable (billing) during step 11: local
  `go test -race` (both builds) was the gate.

## Merged PRs (session 1)

#1 battle tests · #2 M2 core · #3 typed-words-as-keys fix · #4 E2E design r2 · #5 herdr + notifications ·
#6 deflake sync test · #7 webhooks (+ security fixes + verifier bypass fixes) · #8 privacy scrubbing ·
#9 e2ecrypto · #10 device certs · #11 trust store. Session 2: #12 status docs · #13 E2E step 4 · #14 TUI gaps
(/name, thread history, ? fix) · #15 drafts/highlights/activity · #16 E2E step 5 · #17 status ·
#18 message viewer · #19/#22 deflakes · #20 pins · #21 snippets/diffs · #23 groups + reminders.
