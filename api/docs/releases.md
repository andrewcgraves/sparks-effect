# Branching and releases

One trunk: `main`, shared with the worker and the website in this monorepo.

- Branch from `main`. Open the pull request into `main`. Merge on green CI.
- No direct pushes to `main`.
- Staging follows `main` automatically. Production runs a build that someone
  tagged — never a branch merge, and never a rebuild.

The API's pipeline is two workflows at the repository root:
`.github/workflows/api.yml` (CI, image publish, Linear staging) and
`.github/workflows/api-release.yml` (promotion to production).

## Which commits the API workflow runs for

`api.yml` is path-filtered. It runs on a pull request, or on a push to `main`,
that changes any of:

- `api/**`
- `worker/internal/routing/testdata/**` and
  `worker/internal/store/testdata/**` — the worker's golden fixtures, which
  the contract job diffs against the API's copies
- `.github/workflows/api.yml`

A commit that only touches the website or the rest of the worker does not run
it, and so has no API image of its own.

## Images

A push to `main` that runs the workflow publishes
`ghcr.io/andrewcgraves/sparks-effect-api` as `sha-<commit>` (immutable — this
is what gets promoted) and `staging` (moving). There is no `latest`. The
package name is the one the old `sparks-effect-api` repository published to;
it did not change with the move.

`<commit>` is the last commit on `main`, at or before the one that was pushed,
that touched the paths above: `git rev-list -1 --first-parent <sha> -- <paths>`.
That is nearly always the pushed commit itself. The release workflow runs the
same command to find the image for a tag, which is why the publish job names
the image this way rather than after whatever commit happened to be the tip.

`main` publishes only after test, vet, lint and build pass, so a `sha-` tag
existing is itself the proof that the commit went through the trunk green.

Running the **API** workflow by hand from `main` (Actions → **API** → *Run
workflow*) publishes the same way a push does. Use it when a commit needs an
image and its push did not produce one.

## How a merge reaches staging

Not through the image. The staging Railway service builds from the GitHub
repository source: it watches `andrewcgraves/sparks-effect`, branch `main`,
with root directory `api`, builds `api/Dockerfile` itself and waits for CI
before deploying. The `:staging` image tag is published on every run but no
Railway service consumes it.

So staging and production do not run the same bytes: staging runs Railway's
own build of the commit, production runs the image `api.yml` built from it.

## Releasing

A release is a tag, namespaced by service because the repository holds three
of them: `api/vMAJOR.MINOR.PATCH`. Tag a commit that is already on `main`:

```sh
git fetch origin main
git tag -a api/v1.4.0 -m "api/v1.4.0" origin/main   # or an older SHA on main
git push origin api/v1.4.0
```

That fires the **API release** workflow. It finds the last commit at or
before the tag that touched the API's paths (the list above), re-tags the image
built from that commit as `:prd` and as `:v1.4.0` — the image tag is the bare
version, without the `api/` prefix — then redeploys the production Railway
service so it pulls the new `:prd`. Nothing rebuilds and the suite does not
re-run — that commit passed on its way into `main`.

The tag does not have to sit on a commit that changed the API. Tagging the tip
of `main` after a run of website-only commits promotes the image of the most
recent API change, which is what the API at that tip is.

The bare `v*` tags of the old `sparks-effect-api` repository were not imported.
A bare `v1.4.0` pushed here starts nothing.

The workflow refuses to promote:

- a tag that is not `api/vMAJOR.MINOR.PATCH` (an optional `-suffix` is allowed)
- a tag pointing at a commit that is not an ancestor of `main` — this is what
  stops a feature branch reaching production
- a commit with no published image — i.e. one whose API change never went
  through `main` green. A tag pushed straight after the merge is fine: the
  workflow waits up to 20 minutes for `main`'s CI to publish the image before
  it refuses. If a release does fail on a missing image, re-run it for the
  same tag once the **API** workflow is green on `main` (Actions → **API
  release** → *Run workflow*). If the **API** workflow never ran for that
  commit, run it by hand from `main` first

### Rolling back

Re-promote an earlier tag: Actions → **API release** → *Run workflow*, pass the
tag (`api/v1.3.0`). It moves `:prd` back to that image and redeploys. Roll
forward the same way, or with a new tag.

Releases cut in the old repository have no `api/v*` tag here, but their images
are still in the registry under their bare version. To go back to one of
those, re-tag it by hand (`docker buildx imagetools create --tag
ghcr.io/andrewcgraves/sparks-effect-api:prd
ghcr.io/andrewcgraves/sparks-effect-api:vX.Y.Z`) and redeploy the production
service in Railway.

### What production is running

There is no `prd` branch. What production runs is the `:prd` tag, and each
release keeps its own immutable tag:

```sh
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-api:prd
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-api:v1.4.0
```

## Cutover gates

Two repository variables (Settings → Secrets and variables → Actions →
Variables) switch the deploying half of the pipeline on. Both are unset when
the repository is first pushed, so that push deploys nothing.

| variable | what it gates |
| --- | --- |
| `STAGING_DEPLOY_ENABLED` | The `publish` and `linear-staging` jobs of `api.yml`. Until it is `true` no image is pushed and nothing is written to Linear. |
| `PRODUCTION_RELEASE_ENABLED` | The `promote` job of `api-release.yml`. Until it is `true` a pushed `api/v*` tag starts the workflow and the job is skipped. |

They must be repository variables, not variables on the `production`
environment: a job-level `if:` is evaluated before the job's environment is
attached. The same two variables gate the worker's and the website's pipelines.

The check jobs (test, vet, lint, vulnerability scan, contract, build) are not
gated and never were.

Neither variable reaches Railway. The staging service builds from source, so
it starts deploying `main` as soon as it is pointed at this repository,
whatever `STAGING_DEPLOY_ENABLED` says.

A commit pushed while `STAGING_DEPLOY_ENABLED` was off has no image. After
turning it on, run the **API** workflow by hand from `main` once, or wait for
the next API change to merge, before cutting the first release.

## What has to be configured outside the repo

Railway:

- production service → the image `ghcr.io/andrewcgraves/sparks-effect-api:prd`.
  It must deploy the image, not a branch: pointed at a branch it rebuilds from
  source and production stops being the build CI tested
- staging service → the GitHub repository `andrewcgraves/sparks-effect`, branch
  `main`, root directory `api`, wait for CI on
- prd credentials (`DATABASE_URL`, AMQP URL, bootstrap admin password) live only
  in the production environment — the staging service and its build logs must
  not be able to read them
- both services' **health check path** → `/readyz` (Settings → Deploy →
  Healthcheck Path), so a deploy that cannot reach its database or broker never
  takes traffic. Not `/healthz`: that answers "ok" whenever the process is up,
  whatever state its dependencies are in, and stays the liveness check

GHCR: the `sparks-effect-api` package was created by the old repository, so
this repository's `GITHUB_TOKEN` cannot push to it until the package grants it
access (package settings → Manage Actions access → add
`andrewcgraves/sparks-effect` with the **Write** role).

GitHub side, on a repository environment named `production` (Settings →
Environments), so the credential is scoped to releases rather than to every
workflow run:

| name | kind | what it is |
| --- | --- | --- |
| `RAILWAY_TOKEN` | secret | Railway **project token**, shared with the worker's pipelines. |
| `API_LINEAR_ACCESS_KEY_PRODUCTION` | secret | Access key for the **API production** Linear release pipeline. Lives on this environment so only a promotion can write a production release. |

And at the repository level (Settings → Secrets and variables → Actions), not
on the `production` environment:

| name | kind | what it is |
| --- | --- | --- |
| `API_LINEAR_ACCESS_KEY_STAGING` | secret | Access key for the **API staging** Linear release pipeline. Repository level because staging records every trunk publish. |
| `API_RAILWAY_SERVICE` | variable | Name (or id) of the production API service. The Railway token covers more than one service, so this is what makes the redeploy hit the production API and nothing else. |
| `STAGING_DEPLOY_ENABLED`, `PRODUCTION_RELEASE_ENABLED` | variables | The cutover gates above. |

The Linear keys carry the `API_` prefix because the repository now holds three
pipelines' worth of them.

Without `RAILWAY_TOKEN` or `API_RAILWAY_SERVICE` the release still re-tags
`:prd`; it just leaves the redeploy to be clicked in Railway. `GITHUB_TOKEN`
covers GHCR once the package grants this repository access. Without either
Linear key the matching sync step is skipped rather than failed, so a missing
pipeline does not block a publish or a promotion.

Adding required reviewers to that `production` environment is what puts a human
approval in front of a promotion, if that is wanted later. The worker's
staging redeploy uses the same environment and would wait on the same approval.

## Linear

Linear Releases answer "which issues are on staging, and which made it to
production?" — not by reading a branch, but by scanning the commits `main`
already ships.

Two **continuous** pipelines, not one pipeline with stages. Staging auto-deploys
every API commit on `main` while production lags on a tagged SHA, so the two
environments hold different commits at the same time. Linear's rule for that
shape is two pipelines:

| Pipeline | Created when | Version |
| --- | --- | --- |
| API staging | `main` publishes `sha-<commit>` + `:staging` | short SHA |
| API production | an `api/vMAJOR.MINOR.PATCH` tag promotes that image as `:prd` | the bare version, `vX.Y.Z` |

The action pulls `SPA-123` identifiers out of subjects and squash messages
(`SPA-258: … (#69)`) and attaches those issues to the new release. Both
pipelines pass `include_paths: "api/**"`, so only commits that changed the API
are scanned; a worker or website commit in the same range is left to its own
pipeline.

Staging scans commits since the last **Linear** release in the staging
pipeline (every `main` publish). Production scans
`<previous api/v* tag>..<promoted tag>`: the exclusive lower bound is the
previous `api/vMAJOR.MINOR.PATCH` tag reachable from the promoted commit, so a
production release records every issue that landed since the last tag went to
production — including when the Linear pipeline has no prior release of its
own.

The first release cut from the monorepo has no previous `api/v*` tag, because
the old repository's tags were not imported. That is not an error. A tag push
falls back to Linear's automatic baseline, which is the commit of the
pipeline's last release — a commit from the old repository, which the import
kept in this history. To choose the range instead, run **API release** by hand for that tag
with `base_ref` set to the commit to scan from (a SHA works), and with
`linear_only` checked if the tag has already been promoted.

An issue that merged to `main` shows up on API staging immediately; it only
appears on API production when a tag that contains it is promoted.

Create both pipelines in Linear (Settings → Releases), generate an access key
per pipeline, and paste them into the secrets in the tables above.

To override the production scan range (for example to sweep from
`api/v0.1.0`), re-run **API release** (*Run workflow*) with that tag and
`base_ref` set. Linear scans `<base_ref>..HEAD` exclusively. Leave `base_ref`
empty to use the previous `api/v*` tag.

To re-record Linear for an already-promoted tag without moving `:prd`, re-run
**API release** with `linear_only` checked. Required reviewers on the GitHub
`production` environment may still fire because the Linear key lives there.
A rollback (re-promoting an older tag without `linear_only`) does not rewrite
Linear history: the original production release stays as the one that first
shipped those issues.
