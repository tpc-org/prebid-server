# Build a pinned PBS image

Production releases use one image built from a clean, approved `master` revision,
then load that same image in both regions. The legacy host source tree is not a
release context: its bare-repository checkout can update files while leaving an
unrelated `.git` directory behind. October 23 inspection found all 6,391 source
files matching `794274d8602b9a443db4c790ec6e97eccaeec372` in both regions, while
the embedded Git metadata still named September's `b64d6382` and marked it dirty.

Create a fresh full clone (not a linked worktree) and detach at the approved
40-character revision. Fetch and verify it is on `origin/master`; run
`sh scripts/check-release-source.sh "$revision"` in that clone. Pass that revision
as Docker's `REVISION` build argument and set `VERSION` from the same clone.
Build for `linux/amd64`. The Dockerfile checks source identity after vendoring
and tests, before compilation; a stale or dirty Git checkout must fail.

The build intentionally does not run `go mod tidy`. Dependency edits belong in
reviewed source commits. `go mod vendor` uses the committed dependency versions.
Unversioned builds remain available for development but are not release artifacts.

Record the source SHA, image ID and OCI revision label. Copy the binary out of
a stopped candidate container and inspect `go version -m`: require the approved
`vcs.revision` and `vcs.modified=false`. Record its SHA-256. Export the image once
with `docker save` and record the archive checksum; neither region rebuilds it.
Verify those identifiers after loading the archive and after activation.

Before production activation, preserve each region's private config, mounts,
network settings and previous image. Rehearse startup and native auction handling
with controlled demand and a private test configuration. Activate one region,
verify health/error/latency and publisher behavior, then activate the other using
the same image. A failed canary restores the previous image and regional runtime
settings. Configuration equality alone is not a completed application release.

This build correction does not authorize the separate Thrad v2 cutover in PR20.
The existing `thrad` v1 and `thradx` v2 test adapters retain their behavior.
