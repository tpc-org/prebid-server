#!/bin/bash
# Run before pushing a branch for an upstream PR. Catches fork-only files
# that should never be contributed upstream.
set -e

git fetch upstream 2>/dev/null || {
  echo "ERROR: no 'upstream' remote configured. Add it with:"
  echo "  git remote add upstream https://github.com/prebid/prebid-server.git"
  exit 1
}

DIFF=$(git diff upstream/master --name-only)
PROBLEMS=$(echo "$DIFF" | grep -E '^(FORK_NOTES\.md|\.gitattributes|scripts/check-upstream-pr-scope\.sh|adapters/tpc)' || true)

if [ -n "$PROBLEMS" ]; then
  echo "ERROR: branch contains fork-only files that should not go upstream:"
  echo "$PROBLEMS" | sed 's/^/  /'
  exit 1
fi

CHANGES=$(echo "$DIFF" | wc -l | tr -d ' ')
echo "OK — $CHANGES file(s) diverge from upstream, none are fork-only."
