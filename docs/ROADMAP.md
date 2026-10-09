# Roadmap

Product bar: lightweight · feature-rich, shipped iteratively with no known bugs · battle-tested ·
seamless · one-step onboarding · end-to-end encrypted · blazing fast.

Every milestone ships behind green CI, adversarial + chaos tests, and a live herdr pass.

## M2 — collaboration in the terminal (in progress)

| Feature | Notes |
|---|---|
| herdr notifications, unread badge, `/share-pane` | preview before sending a pane |
| Desktop notifications (macOS/Linux), auto-away | argv-only exec, coalesced |
| Webhooks (`curl` → channel), bot users | same port via ALPN, pinned cert |
| Display names (`/name`) | |
| Thread history from server | older replies, not just the cache |
| `tc notify -- <cmd>` | exit code, duration, output tail → channel/self, herdr notification |
| Activity view (`Ctrl-A`) ✓ | mentions, replies in your threads, reactions to you, DMs |
| Per-channel drafts ✓, keyword highlights ✓ | local only |
| `/pin` `/pins`, `/remind`, user groups `@oncall` | |
| Diff + `file:line` aware snippets | `git diff \| tc send`, `tc send --file a.go:40-60` |
| UI gaps from M1 | `?` no longer eats messages, long-line viewer |

## M3 — end-to-end encryption

DMs and private channels encrypted by default; public channels stay server-readable (search,
webhooks, bots). Design: [E2E.md](E2E.md).

## M4 — zero-setup onboarding

- Invite = install command (`curl … install.sh | sh -s -- '<invite>'`), brew tap, `tc update`
- `tc join` wizard: picks an agent key or generates one, name from git config, ≤ 2 prompts
- Team invites (multi-use, TTL), any-member invites, `--key github:user` pre-authorized invites
- Releases: goreleaser, checksums + signatures

## M5 — voice huddles

QUIC datagrams + Opus, push-to-talk, drop-in per channel.

## Later / maybe

Scheduled send · retention / disappearing messages · local GitHub unfurls via `gh` ·
phone push via `notify_cmd` hook (ntfy) · encrypted file attachments · SSH guest mode (public
channels only).

## Deliberately cut

Canvases, lists, workflow builder, polls (use reactions), server-side link unfurls (leak URLs,
break E2E), inline images, custom emoji, mandatory Zulip-style topics.
