#!/bin/sh
# The agent org's review-and-merge gate.
#
#   merge.sh list            PRs ready for review: "<number> <head sha> auto|owner"
#   merge.sh prepare <file>  fetch each listed PR's diff and metadata for the reviewer
#   merge.sh land <file>     post each verdict; merge approved "auto" PRs
#
# A PR is "auto" when it may land without the owner: labelled agent-org plus
# agent-org:fix or agent-org:refactor, opened by the owner's token, not a
# draft, mergeable, CI green, under MAX_LINES and touching no sensitive path.
# Everything else that is green is "owner": reviewed once per commit, never
# merged here. Decisions use the PR's real file list, never the agent's own
# claims, and a merge needs an "approve" verdict that the independent
# reviewer wrote in this same job for the exact head commit.
set -eu

: "${REPO:?}"
OWNER=pranavjayakumar
MAX_LINES=1500
REVIEW_DIR=${REVIEW_DIR:-review}
MARK='<!-- agent-org-review'

# Crypto, identity, wire format, transport, release and install paths, CI,
# dependencies and this pipeline itself always need the owner.
SENSITIVE='^(internal/e2ecrypto/|internal/trust/|internal/identity/|internal/proto/|internal/transport/|internal/release/|internal/server/(auth|invite|devices|keys)|docs/PROTOCOL\.md|docs/E2E\.md|install\.sh|Formula/|scripts/|Makefile|go\.mod|go\.sum|\.github/)'

# checks_green <n> <files>: CI passed, or CI skipped a Markdown-only change
# (ci.yml paths-ignore).
checks_green() {
	if out=$(gh pr checks "$1" --repo "$REPO" 2>&1); then return 0; fi
	printf '%s' "$out" | grep -q 'no checks reported' &&
		! printf '%s\n' "$2" | grep -qv '\.md$'
}

# reviewed <n> <sha> <verdict regex>: a review comment for this commit exists.
reviewed() {
	gh pr view "$1" --repo "$REPO" --json comments --jq '.comments[].body' |
		grep -Eq "$MARK sha=$2 verdict=($3) -->"
}

list() {
	gh pr list --repo "$REPO" --state open --label agent-org --author "$OWNER" \
		--json number,headRefOid,isDraft,mergeable,additions,deletions,labels \
		--jq '.[] | select(.isDraft | not) | select(.mergeable == "MERGEABLE")
			| select([.labels[].name] | index("agent-org:hold") | not)
			| "\(.number) \(.headRefOid) \(.additions + .deletions) \([.labels[].name] | join(","))"' |
	while read -r n sha size labels; do
		files=$(gh pr view "$n" --repo "$REPO" --json files --jq '.files[].path')
		checks_green "$n" "$files" || { echo "#$n: checks not green" >&2; continue; }
		kind=auto
		case ",$labels," in
		*,agent-org:needs-owner,* | *,agent-org:feature,*) kind=owner ;;
		*,agent-org:fix,* | *,agent-org:refactor,*) ;;
		*) kind=owner ;;
		esac
		[ "$size" -le "$MAX_LINES" ] || kind=owner
		printf '%s\n' "$files" | grep -Eq "$SENSITIVE" && kind=owner
		if [ "$kind" = owner ]; then
			gh pr edit "$n" --repo "$REPO" --add-label agent-org:needs-owner >/dev/null || true
			# Owner PRs get one review per commit, to help the owner.
			reviewed "$n" "$sha" 'approve|changes' && continue
		else
			# A fix is waiting on the worker; don't review the same commit again.
			reviewed "$n" "$sha" changes && continue
		fi
		echo "$n $sha $kind"
	done
}

prepare() {
	mkdir -p "$REVIEW_DIR"
	while read -r n sha kind; do
		git fetch -q origin "pull/$n/head:review/pr-$n"
		[ "$(git rev-parse "review/pr-$n")" = "$sha" ] || { echo "#$n moved; skipped" >&2; continue; }
		gh pr diff "$n" --repo "$REPO" >"$REVIEW_DIR/$n.diff"
		gh pr view "$n" --repo "$REPO" --json number,title,body,labels,files \
			>"$REVIEW_DIR/$n.json"
		echo "$n $sha $kind" >>"$REVIEW_DIR/queue"
	done <"$1"
}

land() {
	[ -f "$1" ] || return 0
	while read -r n sha kind; do
		verdict=$(head -n 1 "$REVIEW_DIR/$n.verdict" 2>/dev/null || true)
		case "$verdict" in approve | changes) ;; *)
			echo "#$n: no valid verdict" >&2
			continue
			;;
		esac
		{
			if [ "$verdict" = approve ]; then
				echo "**agent-org review: approved** for ${sha}."
			else
				echo "**agent-org review: changes requested** for ${sha}."
			fi
			echo
			cat "$REVIEW_DIR/$n.md" 2>/dev/null || true
			echo
			echo "$MARK sha=$sha verdict=$verdict -->"
		} >"$REVIEW_DIR/$n.comment"
		gh pr comment "$n" --repo "$REPO" --body-file "$REVIEW_DIR/$n.comment" >/dev/null
		if [ "$kind" = auto ] && [ "$verdict" = approve ]; then
			echo "merging #$n"
			gh pr merge "$n" --repo "$REPO" --squash --delete-branch \
				--match-head-commit "$sha" || echo "#$n: merge failed"
		fi
	done <"$1"
}

case "${1:-}" in
list) list ;;
prepare) prepare "$2" ;;
land) land "$2" ;;
*)
	echo "usage: merge.sh list | prepare <file> | land <file>" >&2
	exit 2
	;;
esac
