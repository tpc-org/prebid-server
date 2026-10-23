#!/bin/sh
set -eu

revision=${1:?expected full source revision}
case "$revision" in *[!0-9a-f]*|'') echo 'Expected a full source revision' >&2; exit 1;; esac
test "${#revision}" -eq 40
test "$(git rev-parse HEAD)" = "$revision" || { echo 'Source HEAD differs from release revision' >&2; exit 1; }
status=$(git status --porcelain --untracked-files=normal)
test -z "$status" || { echo 'Release source is not clean' >&2; exit 1; }
