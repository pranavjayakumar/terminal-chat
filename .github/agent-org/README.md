# Agent org

An autonomous Claude Code agent that improves this repo on a schedule, with no
one at a keyboard. Every 4 hours `.github/workflows/agent-org.yml`:

1. **merge** (`merge.sh`): squash-merges open PRs labelled `agent-org` that
   are green, mergeable, under 1,500 changed lines and touch no sensitive path
   (crypto, identity, protocol, transport, auth, invites, release, install,
   CI, dependencies). The rest get `agent-org:needs-owner` and wait for you.
2. **work** (`PROMPT.md`): picks one task (red main, then agent PRs needing
   fixes, then issues you labelled `agent-ok`, then `docs/BACKLOG.md`, then
   self-directed fixes), builds it with tests and reviews, opens one PR and
   waits for CI. If nothing is worth doing, it opens nothing.

Commits and PRs are made as the owner, so they count on the owner's GitHub
contribution graph.

## Setup (once)

1. **Claude token.** On your machine run `claude setup-token` and add the
   result as the repo secret `CLAUDE_CODE_OAUTH_TOKEN`
   (Settings › Secrets and variables › Actions). Runs use your Claude
   subscription's limits.
2. **GitHub token.** Create a fine-grained personal access token
   (Settings › Developer settings › Fine-grained tokens) for
   `pranavjayakumar`, repository access *only this repo*, permissions:
   Contents read/write, Pull requests read/write, Issues read/write,
   Actions read, Metadata read. **Don't** grant Workflows: without it the
   agent can't change anything in `.github/workflows/`. Add it as the secret
   `AGENT_GH_TOKEN`. Give it an expiry and put a reminder in your calendar.
3. Start it from the Actions tab (agent-org › Run workflow) to watch a first
   run, or wait for the schedule.

## Steering it

- Label an issue `agent-ok` to queue it (only your label counts).
- Edit "Owner's list" in `docs/BACKLOG.md`.
- Label a PR `agent-org:hold` to stop it being merged.
- Turn it off: Actions › agent-org › Disable workflow.
