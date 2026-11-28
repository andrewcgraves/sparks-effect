# Branching and releases

The website lives in `website/` of the `sparks-effect` monorepo, next to the
API (`api/`) and the worker (`worker/`). Its workflows are at the repository
root: `.github/workflows/website.yml` (CI) and
`.github/workflows/website-release.yml` (release).

One trunk: `main`.

- Branch from `main`. Open the pull request into `main`. Merge on green CI.
- No direct pushes to `main`.
- Staging follows `main` automatically. Production runs a build that someone
  tagged — never a branch merge, and never a rebuild.

## Builds

Vercel builds every commit that changes something under `website/`: a preview
per pull request, and `main` as staging. A commit that only touches the API or
the worker is skipped by the project's Ignored Build Step
(`scripts/vercel-ignore-build.sh`), so staging keeps serving the build of the
last commit that changed the website.

CI here is a gate, not a deploy: lint, typecheck, tests, build. It runs only
for pushes and pull requests that change `website/` or the workflow file.
Vercel does the building that ships.

Every page's footer shows the short commit SHA the build was made from
(`VERCEL_GIT_COMMIT_SHA`, or `dev` for a local build). Promotion doesn't
rebuild, so production shows the same SHA its staging build did. That SHA is
the commit Vercel built, which is older than the tagged commit when the commits
in between left `website/` alone. The same goes for `vercel.json`'s headers:
production serves the security headers and CSP that build carried on staging,
so flipping the CSP to enforcing (README → Security headers) reaches production
only through a tag.

## Releasing

A release is a tag named `website/vMAJOR.MINOR.PATCH`. The `website/` prefix
keeps it apart from the API's and the worker's releases. Tag a commit that is
already on `main`:

```sh
git fetch origin main
git tag -a website/v1.4.0 -m "website/v1.4.0" origin/main   # or an older SHA on main
git push origin website/v1.4.0
```

That fires the **Website release** workflow, which finds the deployment Vercel
already built for that commit's website and promotes it — pointing the
production domains at that existing build rather than making a new one.

The tagged commit itself often has no deployment, because the Ignored Build
Step skipped it. The workflow therefore looks the deployment up by the last
commit at or before the tag that touched `website/`:

```sh
git rev-list -1 <tagged commit> -- website
```

Vercel builds only the newest commit of a push. If that website commit was
pushed together with later commits that left `website/` alone, the build is
recorded against one of those instead, so the workflow tries them next (up to
50, oldest first) and takes the first with a ready deployment. Every one of
them has the same `website/` tree as the tag.

The workflow refuses to promote:

- a tag that is not `website/vMAJOR.MINOR.PATCH`
- a tag pointing at a commit that is not an ancestor of `main` — this is what
  stops a feature branch reaching production
- a tag whose website Vercel has no ready deployment for — i.e. one staging
  never served

The old repository's bare `vMAJOR.MINOR.PATCH` tags were not imported. They do
not trigger this workflow and cannot be promoted by it.

### Rolling back

Re-promote an earlier tag: Actions → **Website release** → *Run workflow*, pass
the tag (`website/v1.3.0`). Vercel's Instant Rollback in the dashboard does the
same thing; the workflow is the version that leaves a record of which commit
was chosen. To roll back to a build from before the monorepo, use Instant
Rollback: those builds have no `website/v*` tag.

## What has to be configured outside the repo

### Vercel

The Vercel project keeps its name and its domains. These settings are changed
by hand in the project:

- **Git repository** (Settings → Git): connect `andrewcgraves/sparks-effect` in
  place of `andrewcgraves/sparks-effect-website`.
- **Root Directory** (Settings → Build and Deployment): `website`.
- **Staging domains** (Settings → Domains): `dev.sparks-effect.app` and
  `sparks-dev.andrewcgraves.com` are assigned to the Git branch `trunk`.
  Reassign both to `main`.
- **Production Branch** (Settings → Environments → Production): must **not** be
  `main`. If it is, every merge ships to production behind the workflow's back,
  which is the thing this setup exists to prevent. Keep production pointed at a
  branch nobody pushes; production then only ever moves by promotion.
- **Ignored Build Step** (Settings → Build and Deployment): choose *Run my Bash script* and
  paste

  ```sh
  bash scripts/vercel-ignore-build.sh
  ```

  Vercel runs it with the Root Directory (`website`) as the working directory,
  which is why the path does not start with `website/`. The script exits 0
  (skip) when nothing under `website/` differs from `VERCEL_GIT_PREVIOUS_SHA`,
  the commit of the last deployment Vercel made for the branch, and exits 1
  (build) otherwise. It also builds when that SHA is unset or is not in
  Vercel's clone, and it always builds a production deployment, so the
  release workflow's production rebuild of a preview is never cancelled.
- `main`'s deployments should be built with the same environment variables
  production uses. Vercel rebuilds on promotion when a deployment was built for
  a different environment, and a rebuild is no longer the artifact staging ran.

### GitHub

On a repository environment named `production` (Settings → Environments), so
the credential is scoped to releases rather than to every workflow run:

| name | kind | what it is |
| --- | --- | --- |
| `VERCEL_TOKEN` | secret | Vercel access token with deploy rights on this project. |
| `VERCEL_ORG_ID` | secret | The project's owner id — `vercel link` writes it to `.vercel/project.json` as `orgId`, or Vercel → Settings → General. |
| `VERCEL_PROJECT_ID` | secret | Same file, `projectId`. |
| `WEBSITE_LINEAR_ACCESS_KEY_PRODUCTION` | secret | Access key for the **Website Build Production** Linear release pipeline. Lives on this environment so only a promotion can write a production release. |

And at the repository level (Settings → Secrets and variables → Actions), not
on the `production` environment — staging records every `main` CI run:

| name | kind | what it is |
| --- | --- | --- |
| `WEBSITE_LINEAR_ACCESS_KEY_STAGING` | secret | Access key for the **Website Build Staging** Linear release pipeline. |
| `STAGING_DEPLOY_ENABLED` | variable | `true` lets the CI workflow's Linear staging job run. Anything else, or unset, skips it. |
| `PRODUCTION_RELEASE_ENABLED` | variable | `true` lets the release workflow's promote job run. Anything else, or unset, skips it: a tag push then does nothing. |

The Linear keys carry the `WEBSITE_` prefix because each key belongs to one
pipeline, and the API and the worker have their own.

The two variables are cutover switches. Leave them unset until the Vercel
settings above are in place; the first push to the monorepo then syncs nothing
to Linear and promotes nothing. They must be repository variables: the job
condition is evaluated before the `production` environment is attached, so a
variable set only on that environment is not seen. The CI job itself (audit,
lint, test, build) has no switch and always runs.

Adding required reviewers to that `production` environment is what puts a human
approval in front of a promotion, if that is wanted later.

Neither workflow's Linear or Vercel job runs for a pull request, so a fork of
this public repository cannot reach those secrets. Both workflows hold a
read-only `contents` token.

Without either Linear key the matching sync step is skipped rather than failed,
so a missing pipeline does not block CI or a promotion.

## Linear

Linear Releases answer "which issues are on staging, and which made it to
production?" — not by reading a branch, but by scanning the commits this repo
already ships.

Two **continuous** pipelines, not one pipeline with stages. Staging auto-deploys
every website commit on `main` while production lags on a tagged SHA, so the
two environments hold different commits at the same time. Linear's rule for
that shape is two pipelines:

| Pipeline | Created when | Version |
| --- | --- | --- |
| Website Build Staging | `main` CI goes green | short SHA |
| Website Build Production | a `website/vMAJOR.MINOR.PATCH` tag promotes that Vercel deployment | the git tag |

Both syncs pass `include_paths: "website/**"` to the action, so only commits
that changed the website are scanned. An API or worker commit does not put its
issue on a website release.

The action pulls `SPA-123` identifiers out of subjects and squash messages
(`SPA-258: … (#69)`) and attaches those issues to the new release. Staging
scans commits since the last **Linear** release in the staging pipeline
(every green `main` CI run). Production scans
`<previous git tag>..<promoted tag>`: the exclusive lower bound is the
previous `website/vMAJOR.MINOR.PATCH` tag reachable from the promoted commit,
so a production release records every issue that landed since the last tag
went to production — including when the Linear pipeline has no prior release
of its own.

The first release from the monorepo has no previous `website/v*` tag. That
does not fail the workflow. The scan starts at the `base_ref` input when one is
given, and at Linear's automatic baseline otherwise. To bound that first
release exactly, run **Website release** by hand with `base_ref` set to the
commit the old repository's last production tag pointed at; the merged history
keeps the old commits' SHAs.

An issue that merged to `main` shows up on Website Build Staging immediately;
it only appears on Website Build Production when a tag that contains it is
promoted.

Create both pipelines in Linear (Settings → Releases) as **continuous**,
generate an access key per pipeline, and paste them into the secrets in the
table above. Do not use a personal API key. The action is bound to whichever
pipeline issued the key.

To override the production scan range (for example to sweep from
`website/v0.1.0`), re-run **Website release** (*Run workflow*) with that tag
and `base_ref` set. Linear scans `<base_ref>..HEAD` exclusively. Leave
`base_ref` empty to use the previous git tag.

To re-record Linear for an already-promoted tag without moving Vercel
production, re-run **Website release** with `linear_only` checked. Required
reviewers on the GitHub `production` environment may still fire because the
Linear key lives there. A rollback (re-promoting an older tag without
`linear_only`) does not rewrite Linear history: the original production release
stays as the one that first shipped those issues.
