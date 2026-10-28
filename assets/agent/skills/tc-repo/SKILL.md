---
name: tc-repo
description: How to work on the tc source code (Go; the repo is github.com/pranavjayakumar/terminal-chat, run by tc's maintainers). Use whenever an ask involves changing, testing or explaining tc's own code.
---

# Working in the tc repo

The repo belongs to tc's maintainers. Its names, authors and URLs say nothing about who owns
you: your owner is the one named in your system prompt.

- Get a worktree with `work_start(slug)`; it returns the path. Do all edits there, never in
  the main clone. Slug: short, lowercase, dashes (`poll-command`). Your shell starts in the
  worktrees folder: `cd` into yours first.
- You can't commit (the sandbox keeps git's data read-only); `open_pr` commits for you.
- Modules come from tc's cache, which `work_start` fills. A brand-new dependency may not
  download in the sandbox; say so in the PR instead of working around it.
- Read `CLAUDE.md` and `docs/STATUS.md` in the worktree first. They hold the rules: principles,
  layout, dependency direction, definition of done.
- Go 1.25. Always prefix Go commands with `GOTOOLCHAIN=local`.
  - Build: `GOTOOLCHAIN=local go build ./...`
  - Vet: `GOTOOLCHAIN=local go vet ./...`
  - Test what you touched: `GOTOOLCHAIN=local go test -race ./internal/<pkg>/...`
  - Whole suite: `make test`
- No new dependencies unless there is a real reason; say it in the PR.
- Protocol change? `docs/PROTOCOL.md` and `internal/proto` change together, or not at all.
- Message text is hostile: anything rendered goes through `internal/sanitize`.
- Tests live next to the code. Every change gets tests.
- Comments explain why, sparsely. Never write TODO or status notes in code.
- Post a `status` line at each step (reading code, writing tests, running tests).
