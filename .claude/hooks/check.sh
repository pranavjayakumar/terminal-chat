#!/usr/bin/env bash
# Stop hook: block finishing a turn while the tree doesn't build or vet.
cd "$(dirname "$0")/../.." || exit 0
ls ./**/*.go ./*/*/*.go >/dev/null 2>&1 || exit 0
out=$(go build ./... 2>&1 && go vet ./... 2>&1)
if [ $? -ne 0 ]; then
  echo "go build/vet failed — fix before finishing:" >&2
  echo "$out" | head -40 >&2
  exit 2
fi
exit 0
