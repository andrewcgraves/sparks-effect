# Branching and releases

One trunk: `main`, shared with the API and the website in this monorepo.

- Branch from `main`. Open the pull request into `main`. Merge on green CI.
- No direct pushes to `main`.
- Staging follows `main` automatically. Production runs a build that someone
  tagged — never a branch merge, and never a rebuild. This is the same model
  as the API and the website.

The worker's pipeline is two workflows at the repository root:
`.github/workflows/worker.yml` (CI, image publish, staging redeploy, Linear
staging) and `.github/workflows/worker-release.yml` (promotion to production).

## Which commits the Worker workflow runs for

`worker.yml` is path-filtered. It runs on a pull request, or on a push to
`main`, that changes any of:

- `worker/**`
- `api/internal/routing/testdata/**` and `api/internal/handler/testdata/**` —
  the API's golden fixtures, which the contract job diffs against the worker's
  copies
- `.github/workflows/worker.yml`

A commit that only touches the website or the rest of the API does not run it,
and so has no worker image of its own.

## Images

A push to `main` that runs the workflow publishes
`ghcr.io/andrewcgraves/sparks-effect-routing-worker` as `sha-<commit>`
(immutable — this is what gets promoted) and `staging` (moving). There is no
`latest`, and no `main` tag. The package name is the one the old
`sparks-effect-routing-worker` repository published to; it did not change with
the move.

`<commit>` is the last commit on `main`, at or before the one that was pushed,
that touched the paths above: `git rev-list -1 --first-parent <sha> -- <paths>`.
That is nearly always the pushed commit itself. The release workflow runs the
same command to find the image for a tag, which is why the publish job names
the image this way rather than after whatever commit happened to be the tip.

`main` publishes only after test, vet, lint and build pass, so a `sha-` tag
existing is itself the proof that the commit went through the trunk green.

No `prd` branch. What production runs is the `:prd` tag, and each release
keeps its own immutable `:vX.Y.Z` tag.

Running the **Worker** workflow by hand from `main` (Actions → **Worker** →
*Run workflow*) publishes and redeploys staging the same way a push does. Use
it when a commit needs an image and its push did not produce one.

## How a merge reaches staging

The `publish` job in `.github/workflows/worker.yml` builds once, with `worker/`
as the build context, and pushes `sha-<commit>` and `:staging`. The
`deploy-staging` job then runs
`railway redeploy --service routing-worker-staging --yes` with the Railway
project token, because moving a tag does not by itself make Railway pull it.
Railway's own image auto-update on `:staging` is the backstop: it polls GHCR and
redeploys when the digest moves, but it is polling, so it can lag by hours. The
CI step is what makes a merge land on staging within a minute.

Staging and production therefore run the same image: `:prd` is a re-tag of a
`sha-<commit>` that `:staging` pointed at first.

## Releasing

A release is a tag, namespaced by service because the repository holds three
of them: `worker/vMAJOR.MINOR.PATCH`. Tag a commit that is already on `main`:

```sh
git fetch origin main
git tag -a worker/v1.4.0 -m "worker/v1.4.0" origin/main   # or an older SHA on main
git push origin worker/v1.4.0
```

That fires the **Worker release** workflow
(`.github/workflows/worker-release.yml`). It finds the last commit at or before
the tag that touched the worker's paths (the list above), re-tags the image
built from that commit as `:prd` and as `:v1.4.0` — the image tag is the bare
version, without the `worker/` prefix — then runs
`railway redeploy --service routing-worker --yes` so production pulls the new
`:prd`. Nothing rebuilds and the suite does not re-run — that commit passed on
its way into `main`.

The tag does not have to sit on a commit that changed the worker. Tagging the
tip of `main` after a run of website-only commits promotes the image of the
most recent worker change, which is what the worker at that tip is.

The bare `v*` tags of the old `sparks-effect-routing-worker` repository were
not imported. A bare `v1.4.0` pushed here starts nothing.

The workflow refuses to promote:

- a tag that is not `worker/vMAJOR.MINOR.PATCH` (an optional `-suffix` is
  allowed)
- a tag pointing at a commit that is not an ancestor of `main` — this is what
  stops a feature branch reaching production
- a commit with no published image — i.e. one whose worker change never went
  through `main` green. A tag pushed straight after the merge is fine: the
  workflow waits up to 20 minutes for `main`'s CI to publish the image before
  it refuses. If a release does fail on a missing image, re-run it for the
  same tag once the **Worker** workflow is green on `main` (Actions → **Worker
  release** → *Run workflow*). If the **Worker** workflow never ran for that
  commit, run it by hand from `main` first

### Rolling back

Re-promote an earlier tag: Actions → **Worker release** → *Run workflow*, pass
the tag (`worker/v1.3.0`). It moves `:prd` back to that image and redeploys
`routing-worker`. Roll forward the same way, or with a new tag. Nothing is
rebuilt, and staging is untouched: it keeps following `main`.

Releases cut in the old repository have no `worker/v*` tag here, but their
images are still in the registry under their bare version. To go back to one
of those, re-tag it by hand (`docker buildx imagetools create --tag
ghcr.io/andrewcgraves/sparks-effect-routing-worker:prd
ghcr.io/andrewcgraves/sparks-effect-routing-worker:vX.Y.Z`) and redeploy
`routing-worker` in Railway.

### What production is running

```sh
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-routing-worker:prd
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-routing-worker:v1.4.0
```

## Cutover gates

Two repository variables (Settings → Secrets and variables → Actions →
Variables) switch the deploying half of the pipeline on. Both are unset when
the repository is first pushed, so that push deploys nothing.

| variable | what it gates |
| --- | --- |
| `STAGING_DEPLOY_ENABLED` | The `publish`, `deploy-staging` and `linear-staging` jobs of `worker.yml`. Until it is `true` no image is pushed, Railway is not redeployed and nothing is written to Linear. |
| `PRODUCTION_RELEASE_ENABLED` | The `promote` job of `worker-release.yml`. Until it is `true` a pushed `worker/v*` tag starts the workflow and the job is skipped. |

They must be repository variables, not variables on the `production`
environment: a job-level `if:` is evaluated before the job's environment is
attached. The same two variables gate the API's and the website's pipelines.

The check jobs (test, vet, lint, vulnerability scan, contract, build) are not
gated and never were.

A commit pushed while `STAGING_DEPLOY_ENABLED` was off has no image. After
turning it on, run the **Worker** workflow by hand from `main` once, or wait
for the next worker change to merge, before cutting the first release.

## What has to be configured outside the repo

Railway must deploy the **image**, not a branch. Both worker services are
declared in `kustomize-config`'s `.railway/railway.ts`, and that is where the
tag pins live:

- `routing-worker` (production) → `ghcr.io/andrewcgraves/sparks-effect-routing-worker:prd`
- `routing-worker-staging` → `ghcr.io/andrewcgraves/sparks-effect-routing-worker:staging`

Both services live in the one Railway environment the project token is scoped
to, so the service name on `railway redeploy` — not the token — is what keeps a
staging redeploy off production and vice versa. The GHCR package must be
public so Railway can pull without a registry credential.

GHCR: the `sparks-effect-routing-worker` package was created by the old
repository, so this repository's `GITHUB_TOKEN` cannot push to it until the
package grants it access (package settings → Manage Actions access → add
`andrewcgraves/sparks-effect` with the **Write** role).

GitHub side, on a repository environment named `production` (Settings →
Environments), so the credential is scoped to the jobs that deploy rather than
to every workflow run:

| name | kind | what it is |
| --- | --- | --- |
| `RAILWAY_TOKEN` | secret | Railway **project token** for the environment holding both worker services, shared with the API's release. Read by `worker-release.yml` (promote) and by `worker.yml`'s `deploy-staging` job. |
| `WORKER_LINEAR_ACCESS_KEY_PRODUCTION` | secret | Access key for the **Routing Worker production** Linear release pipeline. Lives on this environment so only a promotion can write a production release. |

And at the repository level (Settings → Secrets and variables → Actions), not
on the `production` environment:

| name | kind | what it is |
| --- | --- | --- |
| `WORKER_LINEAR_ACCESS_KEY_STAGING` | secret | Access key for the **Worker Build Staging** Linear release pipeline. Repository level because staging records every trunk publish. |
| `STAGING_DEPLOY_ENABLED`, `PRODUCTION_RELEASE_ENABLED` | variables | The cutover gates above. |

The Linear keys carry the `WORKER_` prefix because the repository now holds
three pipelines' worth of them.

Without `RAILWAY_TOKEN` the release still re-tags `:prd` and the publish still
pushes `:staging`; each just leaves the redeploy to be clicked in Railway (or
to Railway's auto-update). `GITHUB_TOKEN` covers GHCR once the package grants
this repository access. Without either Linear key the matching sync step is
skipped rather than failed, so a missing pipeline does not block a publish or
a promotion.

Adding required reviewers to that `production` environment is what puts a human
approval in front of a promotion, if that is wanted later. Note that it would
also sit in front of `deploy-staging`, because the token that job needs lives
there; the image publish itself never waits.

## Linear

Linear Releases answer "which issues are on worker staging, and which made it
to production?" — not by reading a branch, but by scanning the commits `main`
already ships.

Two **continuous** pipelines, not one pipeline with stages. Staging auto-deploys
every worker commit on `main` while production lags on a tagged SHA, so the two
environments hold different commits at the same time. Linear's rule for that
shape is two pipelines:

| Pipeline | Created when | Version |
| --- | --- | --- |
| Worker Build Staging | `main` publishes `sha-<commit>` + `:staging` | short SHA |
| Routing Worker production | a `worker/vMAJOR.MINOR.PATCH` tag promotes that image as `:prd` | the bare version, `vX.Y.Z`; the release is named `Routing Worker vX.Y.Z` |

The action pulls `SPA-123` identifiers out of subjects and squash messages
(`SPA-249: … (#12)`) and attaches those issues to the new release. Both
pipelines pass `include_paths: "worker/**"`, so only commits that changed the
worker are scanned; an API or website commit in the same range is left to its
own pipeline.

Staging scans commits since the last **Linear** release in the staging
pipeline (every `main` publish). Production scans
`<previous worker/v* tag>..<promoted tag>`: the exclusive lower bound is the
previous `worker/vMAJOR.MINOR.PATCH` tag reachable from the promoted commit, so
a production release records every issue that landed since the last tag went
to production — including when the Linear pipeline has no prior release of its
own.

The first release cut from the monorepo has no previous `worker/v*` tag,
because the old repository's tags were not imported. That is not an error. A
tag push falls back to Linear's automatic baseline, which is the commit of the
pipeline's last release — a commit from the old repository, which the import
kept in this history. To choose the range instead, run **Worker release** by
hand for that tag with `base_ref` set to the commit to scan from (a SHA
works), and with `linear_only` checked if the tag has already been promoted.

An issue that merged to `main` shows up on Worker Build Staging immediately; it
only appears on Routing Worker production when a tag that contains it is
promoted.

Create both pipelines in Linear (Settings → Releases, team Sparks Effect),
generate a **pipeline** access key per pipeline (not a personal API key), and
paste them into the secrets in the tables above. The worker production release
that `kustomize-config`'s `linear-release.yml` used to record from the kustomize
pin ([SPA-267](https://linear.app/sparks-effect-personal/issue/SPA-267)) is
retired: the tag records it here now.

To override the production scan range (for example to sweep from
`worker/v0.1.0`), re-run **Worker release** (*Run workflow*) with that tag and
`base_ref` set. Linear scans `<base_ref>..HEAD` exclusively. Leave `base_ref`
empty to use the previous `worker/v*` tag.

To re-record Linear for an already-promoted tag without moving `:prd`, re-run
**Worker release** with `linear_only` checked. A rollback (re-promoting an
older tag without `linear_only`) does not rewrite Linear history: the original
production release stays as the one that first shipped those issues.
