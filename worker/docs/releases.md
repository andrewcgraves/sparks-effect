# Branching and releases

One trunk: `main`.

- Branch from `main`. Open the pull request into `main`. Merge on green CI.
- No direct pushes to `main`.
- Staging follows `main` automatically. Production runs a build that someone
  tagged — never a branch merge, and never a rebuild. This is the same model
  as `sparks-effect-api` and the website.

## Images

Every commit on `main` publishes `sha-<commit>` (immutable — this is what gets
promoted) and `staging` (moving). There is no `latest`, and no `main` tag.

`main` publishes only after test, vet, lint and build pass, so a `sha-` tag
existing is itself the proof that the commit went through the trunk green.

No `prd` branch. What production runs is the `:prd` tag, and each release
keeps its own immutable `:vX.Y.Z` tag.

## How a merge reaches staging

The `publish` job in `.github/workflows/ci.yml` builds once and pushes
`sha-<commit>` and `:staging`. The `deploy-staging` job then runs
`railway redeploy --service routing-worker-staging --yes` with the Railway
project token, because moving a tag does not by itself make Railway pull it.
Railway's own image auto-update on `:staging` is the backstop: it polls GHCR and
redeploys when the digest moves, but it is polling, so it can lag by hours. The
CI step is what makes a merge land on staging within a minute.

## Releasing

A release is a tag. Tag a commit that is already on `main`:

```sh
git fetch origin main
git tag -a v1.4.0 -m "v1.4.0" origin/main   # or an older SHA on main
git push origin v1.4.0
```

That fires the **Release** workflow (`.github/workflows/release.yml`), which
re-tags the image built from that commit as `:prd` and as `:v1.4.0`, then runs
`railway redeploy --service routing-worker --yes` so production pulls the new
`:prd`. Nothing rebuilds and the suite does not re-run — that commit passed on
its way into `main`.

The workflow refuses to promote:

- a tag that is not `vMAJOR.MINOR.PATCH` (an optional `-suffix` is allowed)
- a tag pointing at a commit that is not an ancestor of `main` — this is what
  stops a feature branch reaching production
- a commit with no published image — i.e. one that never went through `main`.
  A tag pushed straight after the merge is fine: the workflow waits up to 20
  minutes for `main`'s CI to publish the image before it refuses. If a
  release does fail on a missing image, re-run it for the same tag once
  `main`'s CI is green (Actions → **Release** → *Run workflow*)

### Rolling back

Re-promote an earlier tag: Actions → **Release** → *Run workflow*, pass the tag
(`v1.3.0`). It moves `:prd` back to that image and redeploys `routing-worker`.
Roll forward the same way, or with a new tag. Nothing is rebuilt, and staging
is untouched: it keeps following `main`.

### What production is running

```sh
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-routing-worker:prd
docker buildx imagetools inspect ghcr.io/andrewcgraves/sparks-effect-routing-worker:v1.4.0
```

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

GitHub side, on a repository environment named `production` (Settings →
Environments), so the credential is scoped to the jobs that deploy rather than
to every workflow run:

| name | kind | what it is |
| --- | --- | --- |
| `RAILWAY_TOKEN` | secret | Railway **project token** for the environment holding both worker services. Read by `release.yml` (promote) and by `ci.yml`'s `deploy-staging` job. |
| `LINEAR_ACCESS_KEY_PRODUCTION` | secret | Access key for the **Routing Worker production** Linear release pipeline. Lives on this environment so only a promotion can write a production release. |

And at the repository level (Settings → Secrets and variables → Actions), not
on the `production` environment — staging records every trunk publish:

| name | kind | what it is |
| --- | --- | --- |
| `LINEAR_ACCESS_KEY_STAGING` | secret | Access key for the **Worker Build Staging** Linear release pipeline. |

Without `RAILWAY_TOKEN` the release still re-tags `:prd` and the publish still
pushes `:staging`; each just leaves the redeploy to be clicked in Railway (or
to Railway's auto-update). `GITHUB_TOKEN` covers GHCR — nothing to set. Without
either Linear key the matching sync step is skipped rather than failed, so a
missing pipeline does not block a publish or a promotion.

Adding required reviewers to that `production` environment is what puts a human
approval in front of a promotion, if that is wanted later. Note that it would
also sit in front of `deploy-staging`, because the token that job needs lives
there; the image publish itself never waits.

## Linear

Linear Releases answer "which issues are on worker staging, and which made it
to production?" — not by reading a branch, but by scanning the commits this
repo already ships.

Two **continuous** pipelines, not one pipeline with stages. Staging auto-deploys
every commit on `main` while production lags on a tagged SHA, so the two
environments hold different commits at the same time. Linear's rule for that
shape is two pipelines:

| Pipeline | Created when | Version |
| --- | --- | --- |
| Worker Build Staging | `main` publishes `sha-<commit>` + `:staging` | short SHA |
| Routing Worker production | a `vMAJOR.MINOR.PATCH` tag promotes that image as `:prd` | the git tag; the release is named `Routing Worker vX.Y.Z` |

The action pulls `SPA-123` identifiers out of subjects and squash messages
(`SPA-249: … (#12)`) and attaches those issues to the new release. Staging
scans commits since the last **Linear** release in the staging pipeline
(every `main` publish). Production scans `<previous git tag>..<promoted tag>`:
the exclusive lower bound is the previous `vMAJOR.MINOR.PATCH` tag reachable
from the promoted commit, so a production release records every issue that
landed since the last tag went to production — including when the Linear
pipeline has no prior release of its own. The first production tag, with no
previous git tag to bound from, still uses Linear's automatic baseline.

An issue that merged to `main` shows up on Worker Build Staging immediately; it
only appears on Routing Worker production when a tag that contains it is
promoted.

Create both pipelines in Linear (Settings → Releases, team Sparks Effect),
generate a **pipeline** access key per pipeline (not a personal API key), and
paste them into the secrets in the tables above. The worker production release
that `kustomize-config`'s `linear-release.yml` used to record from the kustomize
pin ([SPA-267](https://linear.app/sparks-effect-personal/issue/SPA-267)) is
retired: the tag records it here now.

To override the production scan range (for example to sweep from `v0.1.0`),
re-run **Release** (*Run workflow*) with that tag and `base_ref` set. Linear
scans `<base_ref>..HEAD` exclusively. Leave `base_ref` empty to use the
previous git tag.

To re-record Linear for an already-promoted tag without moving `:prd`, re-run
**Release** with `linear_only` checked. A rollback (re-promoting an older tag
without `linear_only`) does not rewrite Linear history: the original production
release stays as the one that first shipped those issues.
