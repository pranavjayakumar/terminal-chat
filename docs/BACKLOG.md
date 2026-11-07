# Agent org backlog

The agent org takes the first unchecked item under "Owner's list" when no
issue labelled `agent-ok` is waiting (.github/agent-org/PROMPT.md §1). The owner edits that list;
agents only tick items they finished and add new findings under "Discovered".

## Owner's list

- [ ] `tc version` for `go install` builds: fill the version from
      `debug.ReadBuildInfo()` when it is `dev`, and make `tc update` tell a
      go-installed binary to re-run `go install` instead of replacing itself.
- [ ] Docs: "Bots are shell scripts", a short guide with working examples
      (`tc tail '#x' | while read …; do tc send …; done`, webhooks, `tc notify`),
      each example tested by a Go test that runs it against a local server.
- [ ] Phone push without an app: a `notify_cmd` hook (ROADMAP.md) that runs a
      user command for each notification, with an ntfy.sh example in the docs.
      No shell interpolation of message text; pass it on stdin or as argv.
- [ ] Theme presets for Catppuccin (Mocha, Latte), Tokyo Night and Gruvbox in
      the existing theme system, with frame-fit tests.
- [ ] Docker image for `tc server`: a small multi-stage `Dockerfile` and a
      `docker-compose.yml` with a volume for the data dir, plus a DEPLOY.md
      section. (Don't touch `.github/`; the owner wires up publishing.)
- [ ] README: a "How it's built" section (agents, adversarial breaker passes,
      the malicious-server test suite) and a security-model summary in six
      bullets linking docs/E2E.md. Keep claims accurate to the code.
- [ ] VHS tapes under `demo/` (`hero.tape`, `pipe.tape`, `join.tape`) that
      script a two-user demo against a local server, plus a `demo/README.md`
      explaining how to record them. Don't commit generated GIFs.
- [ ] Message retention: an optional per-server `--retention 90d` that deletes
      old plaintext messages and blobs, documented and tested.

## Discovered

<!-- Agents append follow-ups here: one line each, with a link to the PR or code. -->
