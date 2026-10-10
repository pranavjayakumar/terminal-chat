# Work log

Append-only, newest last. One line per event: time (IST) · what · where · result.
The current resume point always lives in [STATUS.md](STATUS.md) → "Handoff".

## 2026-10-10
- 08:30 · Claude limit reset; resumed step 10 review fixes · PR #30 · merged (CI unavailable → verified locally)
- 08:35 · GitHub Actions blocked (billing) · owner made the repo public · CI free again
- 08:40 · Droplet 159.65.152.35 (Ubuntu 24.04, x86_64, 512 MB) · key-only SSH, ufw 22/7443 · done
- 08:42 · Deployed tc (main 7c5b405) as systemd `tc` on :7443 · admin invite printed for the owner · live
- 08:44 · Internet smoke test on temp :7444 instance · QUIC + TCP, encrypted DM (server holds only ciphertext) · pass · instance + rules removed
- 08:50 · E2E step 11 (malicious-server suite) · Claude Opus subagent, worktree agent-ab2d5e801827b592f, branch e2e/malicious-server · running
- 08:55 · M4 onboarding · kiro Opus, worktree .claude/worktrees/kiro-onboard, branch m4-onboarding · running
- 09:00 · Owner: finish M3 + M4 end to end, then pause; kiro cap 55–60% (now 46.8%); Claude orchestrates only
- 10:30 · E2E step 11 merged · PR #32 (CI green — Actions back after repo went public) · 24 cases; bug fixed: dropped key.epoch left clients stuck
- 10:35 · E2E step 12 (chaos during rotations, signed-time drift, security pass, fuzz, docs) · kiro Opus, worktree .claude/worktrees/kiro-step12, branch e2e/final-pass · running · kiro 47.0%
- 11:40 · Switched Claude account; resumed from handoff
- 11:45 · M4 onboarding built (kiro) · PR #34 · security review (kiro Sonnet): H1 release trust root, M1–M4, L1–L7
- 11:50 · M4 security fixes · kiro Opus in .claude/worktrees/kiro-onboard (branch m4-onboarding) · running · signed releases with the owner's SSH key (signing step done locally by the coordinator at release time) · kiro 48.3%
- 12:30 · Owner: if kiro reaches the cap, run workers as Claude subagents · kiro 50.3%
- 13:20 · M4 security fixes done (kiro) + owner's signing key embedded · PR #34 merged (CI green) · M4 on main
- 13:20 · Waiting for E2E step 12 before tagging v0.2.0 (release must include M3 final pass)
- 13:50 · Repo cleanup: 37 merged remote branches deleted, local merged branches + obsolete stash removed, GitHub 'delete branch on merge' enabled · only main + e2e/final-pass (step 12, in flight) remain
