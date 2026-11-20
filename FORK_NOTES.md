# tpc-org/prebid-server — fork notes

This file documents fork-specific conventions and is intentionally not in
upstream prebid/prebid-server. **Do not include this file in any upstream PR.**

## What this fork contains beyond upstream

Three files:

- `FORK_NOTES.md` — this file
- `.gitattributes` — marks fork-only files as `export-ignore`
- `scripts/check-upstream-pr-scope.sh` — pre-flight check for upstream PR branches
- `.github/workflows/release-package.yml` — builds the pinned release package (image, source, manifest) from an approved `master` revision in CI (see "Deploy mechanism")

TPC-specific adapters live under `adapters/tpc*`, and TPC-specific hook
modules live under `modules/tpc/*`, per the conventions below.

## Custom adapters

Custom adapters built specifically for TPC live alongside upstream adapters in
`adapters/<bidder>/`, but follow a strict naming convention to mark them as
fork-only.

**Naming:** TPC-only adapters are prefixed with `tpc` — e.g. `adapters/tpc/`
(the tpc-adapter-parity stub bidder, used to give the Mobile SDK path
equivalent behavior to what `tpcBidAdapter.js` does client-side on desktop
— see `docs/architecture/aws-resources.md`'s Prebid Cache section and
`docs/integration/mobile-sdk-integration.md` in `tpc-org/docs`). The
prefix makes divergence from upstream visually obvious in directory
listings and greppable in scripts.

**Do not include `adapters/tpc*` directories in any upstream PR.**

If a fix to an upstream adapter (e.g. `adapters/adform/`) is suitable for
upstream contribution, follow the "Upstream PR workflow" below.

## Custom hook modules

Same convention, one level down: TPC-only PBS hook modules live under
`modules/tpc/<module-name>/`, registered under vendor `"tpc"` in
`modules/builder.go`'s `builders()` map — e.g. `modules/tpc/profanityfilter/`
(rejects an auction before any bidder is called if the AI-chat conversation
text contains a banned word; see `docs/architecture/profanity-filter.md`
in `tpc-org/docs`).

**Do not include `modules/tpc/*` directories in any upstream PR.** The
one-line registration entry added to `modules/builder.go` for each such
module is also fork-only in intent, even though `builder.go` itself is an
upstream file — `scripts/check-upstream-pr-scope.sh` flags the whole
`modules/tpc` path, but can't detect a partial in-file diff inside
`builder.go` itself, so double-check that file's diff by eye before
including it in any upstream branch.

## Upstream files with fork edits

- `config/structlog.go` (2026-10-31) — the startup config dump redacted only
  names containing `password`, so `adapters[*].extra_info` (which carries
  Thrad/thradx/Gravity/Imprezia API keys in this fork) and any `*_secret`,
  `*_token`, `*_key` field were printed in plain text. The blocklist now
  redacts those too; `TestCredentialsRedacted` locks it in. This is a genuine
  upstream issue (`app_secret`, `recaptcha_secret`, `override_token`,
  `client_key`), so it could go upstream — but not with the fork-specific
  comment as-is.

## Upstream PR workflow

When contributing a fix or improvement to prebid/prebid-server upstream:

```bash
# 1. Branch from upstream/master, NOT from fork master
git fetch upstream
git checkout -b upstream-pr/<short-description> upstream/master

# 2. Cherry-pick or hand-apply ONLY the file(s) you want to contribute
git checkout master -- <path/to/file>
git add <path/to/file>
git commit -m "Adapter: clear description per upstream conventions"

# 3. Verify the branch contains nothing fork-only
./scripts/check-upstream-pr-scope.sh

# 4. Push and open PR from tpc-org/prebid-server@<branch> to prebid/prebid-server@master
git push origin upstream-pr/<short-description>
```

**Never PR from fork master directly to upstream master.** That's how
fork-only files leak into upstream PRs (which the script in step 3
will catch).

## Pulling upstream changes into this fork

The opposite direction from the workflow above. Full procedure,
including a real production incident from the 2026-08-18 sync (a new
upstream bidder-info file with an unresolved config placeholder crashed
both PBS regions — `go build`/`go test` passing gave false confidence,
since neither exercises PBS's own startup config validation) is in
`tpc-org/docs`'s `runbooks/upstream-sync.md` — **read it before
syncing**, especially the step that boots the real binary against real
config before deploying, and the note on not naively re-`git merge`ing
a branch after reverting its earlier merge (silently drops content —
revert-the-revert + cherry-pick instead).

## How this fork relates to the rest of the TPC stack
tpc-org/prebid-server (this fork — the Go source)
│ pinned image released per region (release-uploads + deploy.py)
▼
deploy.py prepare / activate swaps the container
│
▼
Docker container 'prebid-go' on each EC2 (us-east-1, eu-central-1)
▲
│ bind-mounted at runtime
│
tpc-org/pbs-settings (runtime config — pbs.yaml, stored imps,
stored requests, deploy.sh)
PBS runtime config is **not** in this repo. It lives in `tpc-org/pbs-settings`
and is released per region through its own trusted release tooling (see
its `CLAUDE.md` and `scripts/README.md`). An ordinary settings push no
longer installs `deploy.sh`.

Companion repos:
- **tpc-org/Prebid.js** — fork of `prebid/Prebid.js` (client-side library)
- **tpc-org/pbs-settings** — PBS runtime config + deploy.sh + post-receive hooks
- **tpc-org/prebid-deployments** — per-client bundle pipeline (CI/CD, client configs)
- **tpc-org/docs** — architecture overview and publisher integration docs

## Deploy mechanism

**Do not `git push production` (or `us-east` / `eu-central`) from this
repo.** Since November 2026, PBS images are released as an uploaded, pinned
image, one region at a time with a separate approval for each. A push to a
deploy remote's `main` still triggers the old post-receive rebuild on the
host, which would replace the released image with one built from git on the
host, outside the release checks.

Release outline (exact commands live with the release owner). Steps 1–3's
build and packaging run in CI: dispatch **Release package (pinned PBS image)**
(`.github/workflows/release-package.yml`) with the approved revision and use
its artifact (`image.tar.gz`, `source.tar.gz`, `manifest.json`):

1. Merge to `master`. Build from a **clean clone** at the merge commit:
   `docker buildx build --platform linux/amd64 --load --build-arg TEST=false
   --build-arg REVISION=<full sha> --build-arg VERSION=<git describe>`.
   `scripts/check-release-source.sh <sha>` must pass on the packaged source.
2. Smoke-test the image locally with `--network none` (`/status` and
   `/version` return 200, plus any feature check).
3. Package the image archive, source archive, manifest, `deploy.py` and
   `probe.py` into the host's release-uploads directory. Then run
   `deploy.py self-test` and `deploy.py prepare` (network-isolated startup;
   production unchanged).
4. `deploy.py activate` swaps the container and keeps
   `prebid-go-rollback-<release>` for rollback. Verify, observe, then repeat
   for the other region.
5. Afterwards, bring the hosts' git deploy refs up to date **without
   triggering the hook**. Push the commit to a non-`main` branch (the hook
   skips it), move `main` on the host with `git update-ref`, then delete
   the temporary branch.

Regions can intentionally run different images during a rollout. Check the
running container's `org.opencontainers.image.revision` label, not the git
ref, to see what is live.

The local `master` vs remote `main` naming is unchanged: the bare repos on
the hosts use `main`.

## Docker build — IMPORTANT

The Dockerfile runs upstream's `validate.sh` test suite during build:

```dockerfile
ARG TEST="true"
RUN if [ "$TEST" != "false" ]; then ./validate.sh ; fi
```

Setting `TEST=true` (the default) means every deploy runs the full
upstream test suite. **This is flaky on the upstream codebase** — at
least one test in `endpoints/openrtb2` times out at 3 minutes
intermittently, causing the build to abort and the container to fail
to start.

The deploy.sh in pbs-settings sets `--build-arg TEST=false` for exactly
this reason:

```bash
docker build --build-arg TEST=false -t ...
```

Upstream tests pass elsewhere (their own CI). Skipping during your
deploy doesn't reduce safety since you're not modifying prebid-server
code (only adapters and config), and upstream already vetted master.

## PBS runtime config — Stored Imp vs Stored Request

The naming is genuinely confusing. PBS uses the same JSON key (`storedrequest`)
for two different concepts:

| Where it appears in OpenRTB request    | PBS treats as | Files looked up in |
|-----------------------------------------|---------------|---------------------|
| `imp[].ext.prebid.storedrequest.id`     | Stored Imp    | `stored_imps/`      |
| `ext.prebid.storedrequest.id` (root)    | Stored Request| `stored_requests/`  |

**Stored Imp** holds per-impression config — bidder params, sizes, banner format.
The file content is merged directly into the corresponding `imp` object.

**Stored Request** holds auction-level config — currency, targeting, cache.
The file content is merged into the top-level request `ext.prebid` block.

A typical Path C production pattern uses both:
- Per-placement Stored Imp (e.g. `sayhola-60b4e117.json`) under `stored_imps/<client>/`
- Per-client Stored Request (e.g. `sayhola-auction-cafc718f.json`) under `stored_requests/<client>/`

The client-side bundle references both:
```js
ortb2Imp: { ext: { prebid: { storedrequest: { id: 'sayhola-60b4e117' } } } }     // per imp
ortb2:    { ext: { prebid: { storedrequest: { id: 'sayhola-auction-cafc718f' } } } }  // top
```

See tpc-org/pbs-settings for the file layout and the file content schemas
for each type.

### PBS file fetcher is NON-RECURSIVE

The filesystem backend in PBS reads `<configured-dir>/*.json` only —
files in subdirectories are ignored. This is why pbs-settings keeps
per-client subdirs in the repo (for organisation) but flattens them
at release time (its release tooling rebuilds the flat inventory).

If you add files manually on the EC2 host, put them flat in
`/var/www/pbs/config/stored_requests/` or `/var/www/pbs/config/stored_imps/`,
not in client subdirectories.

### `cache.vastxml` gotcha — banner placements

PBS supports server-side bid caching via Prebid Cache (a separate
service). The pbs.yaml in this deployment does NOT configure a cache
endpoint — we're caching client-side via Prebid.js.

**Do not include `ext.prebid.cache.vastxml` in a Stored Request used
for banner placements.** With no cache endpoint configured, PBS will
silently strip the banner `adm` (markup) from the response. Bid
metadata returns (cpm, size, creativeId) but the ad won't render.

This was discovered during sayhola Path C migration. If you ever add
a cache endpoint configuration to pbs.yaml, revisit this restriction.

## Build and test

Standard upstream commands work:

```bash
# Single test file
go test github.com/prebid/prebid-server/v4/exchange

# Full validation (warning: slow, occasionally flaky)
./validate.sh

# Race detection on TestRace.* only
./validate.sh --race 5

# Format
make format

# Coverage HTML
./scripts/coverage.sh --html
```

Adapter-specific tests live at `adapters/<bidder>/<bidder>_test.go` and
use the `adapterstest.RunJSONBidderTest` framework. Golden-file test
cases are JSON files in `adapters/<bidder>/<bidder>test/`.

## Working with this fork

To add a new TPC-specific PBS adapter:

1. Create `adapters/tpc<vendor>/` with the standard PBS adapter files:
   - `tpc<vendor>.go` — implements `adapters.Bidder` interface
   - `tpc<vendor>_test.go` — calls `adapterstest.RunJSONBidderTest`
   - `tpc<vendor>test/exemplary/*.json` — golden test cases
   - `static/bidder-info/tpc<vendor>.yaml` — bidder metadata
   - `static/bidder-params/tpc<vendor>.json` — JSON Schema for `imp.ext.bidder`
   - `openrtb_ext/imp_tpc<vendor>.go` — Go struct matching the schema

2. Register in two places:
   - `openrtb_ext/bidders.go` — add a `BidderTpcXxx` constant
   - `exchange/adapter_builders.go` — add the entry to `newAdapterBuilders()`

3. Update pbs.yaml in tpc-org/pbs-settings to enable the new adapter:
```yaml
   adapters:
     tpc<vendor>:
       disabled: false
```

4. Push pbs-settings to production to deploy the config change.
5. Push this repo to production to build the new adapter into the binary.

(Order: pbs-settings first to avoid a brief window where the binary
knows about an adapter the config hasn't enabled yet.)

## Key learnings preserved

Things we learned through painful experience that aren't obvious from
the code:

- **`docker rm` deletes the container but not the image**, so deploy.sh
  has to handle the case where the image exists but no container does.

- **`set -e` in deploy.sh** means any failure aborts. `docker stop ||
  true` is required because docker stop on a non-running container
  returns non-zero.

- **The `git` user's dubious ownership check** — modern Git refuses
  to operate on repos where the `.git` directory is owned by another
  user. Fix is `git config --global --add safe.directory <path>` as
  the affected user.

- **EC2 file permissions for git-checkout**. The `/var/www/pbs/src`
  directory must be `chown -R ec2-user:git` and `chmod -R 775` so
  that both the ec2-user (which builds) and the git user (which
  checks out from the bare repo) can write.

- **The Dockerfile's release stage uses port 8000, not 80**. The Nginx
  proxy in front of it terminates HTTPS and forwards plain HTTP to
  127.0.0.1:8000. ALB health checks go to the bare IP and hit Nginx's
  default_server block, which proxies /status to the container.
