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
printf 'pbs.*\nvendor/\n' > .gitignore
git add source.go .gitignore
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
echo extra > pbs.go
if sh "$check" "$revision" 2>/dev/null; then exit 1; fi
rm pbs.go
mkdir vendor
echo generated > vendor/modules.txt
sh "$check" "$revision"
printf corrupt > .git/index
if sh "$check" "$revision" 2>/dev/null; then exit 1; fi
echo 'PASS: clean/vendored source accepted; wrong revision, tracked edits, untracked/ignored source and broken Git index rejected'
