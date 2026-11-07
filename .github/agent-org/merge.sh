#!/bin/sh
# Merges agent-org PRs that are safe to land without the owner: labelled
# agent-org, opened by the owner's token, not a draft, CI green on the
# current head, mergeable, and touching no sensitive path. Everything else
# stays open for the owner. Decisions use the PR's real file list, never
# the agent's own labels or claims.
set -eu

: "${REPO:?}"
OWNER=pranavjayakumar
MAX_LINES=1500

# Crypto, identity, wire format, transport, release and install paths, CI,
# dependencies and this pipeline itself always need the owner.
SENSITIVE='^(internal/e2ecrypto/|internal/trust/|internal/identity/|internal/proto/|internal/transport/|internal/release/|internal/server/(auth|invite|devices|keys)|docs/PROTOCOL\.md|docs/E2E\.md|install\.sh|Formula/|scripts/|Makefile|go\.mod|go\.sum|\.github/)'

gh pr list --repo "$REPO" --state open --label agent-org --author "$OWNER" \
	--json number,isDraft,mergeable,additions,deletions,labels \
	--jq '.[] | select(.isDraft | not) | select(.mergeable == "MERGEABLE")
		| select([.labels[].name] | (index("agent-org:hold") or index("agent-org:needs-owner")) | not)
		| select(.additions + .deletions <= '"$MAX_LINES"') | .number' |
while read -r n; do
	files=$(gh pr view "$n" --repo "$REPO" --json files --jq '.files[].path')
	if printf '%s\n' "$files" | grep -Eq "$SENSITIVE"; then
		echo "#$n touches a sensitive path: left for the owner"
		gh pr edit "$n" --repo "$REPO" --add-label agent-org:needs-owner >/dev/null || true
		continue
	fi
	if ! out=$(gh pr checks "$n" --repo "$REPO" 2>&1); then
		# CI skips docs-only changes (ci.yml paths-ignore), so no checks
		# is green only when every file is Markdown.
		if ! { printf '%s' "$out" | grep -q 'no checks reported' &&
			! printf '%s\n' "$files" | grep -qv '\.md$'; }; then
			echo "#$n: checks not green yet"
			continue
		fi
	fi
	echo "merging #$n"
	gh pr merge "$n" --repo "$REPO" --squash --delete-branch || echo "#$n: merge failed"
done
