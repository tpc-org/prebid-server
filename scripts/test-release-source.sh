#!/bin/sh
set -eu

check=$(cd "$(dirname "$0")" && pwd)/check-release-source.sh
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
cd "$fixture"
git init -q
git config user.name 'Release test'
git config user.email 'release-test@example.invalid'
echo original > source.go
git add source.go
git commit -qm baseline
revision=$(git rev-parse HEAD)
sh "$check" "$revision"
if sh "$check" 0000000000000000000000000000000000000000 2>/dev/null; then exit 1; fi
echo modified >> source.go
if sh "$check" "$revision" 2>/dev/null; then exit 1; fi
git checkout -- source.go
echo extra > extra.go
if sh "$check" "$revision" 2>/dev/null; then exit 1; fi
rm extra.go
sh "$check" "$revision"
echo 'PASS: clean source accepted; wrong revision, tracked edits and untracked source rejected'
