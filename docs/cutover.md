# Cutting over from the three old repositories

The code and workflows are in place. What remains are settings that live
outside the repository. Staging moves first; production moves only after
staging has run from this repository for a while. The old repositories stay
untouched until the last step, so every stage can be undone by pointing a
setting back.

Delete this file once the old repositories are archived.

## How nothing deploys by accident

Every job that publishes an image, redeploys Railway, promotes Vercel or syncs
Linear checks a repository variable first:

| Variable                     | Turns on                                               |
| ---------------------------- | ------------------------------------------------------ |
| `STAGING_DEPLOY_ENABLED`     | Image publish, worker staging redeploy, staging Linear |
| `PRODUCTION_RELEASE_ENABLED` | The three release workflows                            |

Both must be repository variables set to `true`. Until then only checks run.

One deploy is outside these gates: once the Railway API staging service
watches this repository, Railway builds it on every push to `main` that
touches `api/`.

## Stage 0: before the first push

- [ ] Merge sparks-effect-api PR 166 (Go 1.26.11), then bring it across with
      `scripts/import-from-old-repo.sh api main`. Until then the API
      vulnerability scan fails here as it does there.
- [ ] Decide on a licence. None of the three repositories had one.
- [ ] Push `main`.
- [ ] Recreate the branch ruleset on `main` (pull request required, no force
      push, no deletion). Do not make a per-service check required: a pull
      request that skips that workflow by path filter would wait forever.
- [ ] Add a tag ruleset for `api/v*`, `worker/v*` and `website/v*`, matching the
      old `production version tags` ruleset.

## Stage 1: staging

GitHub, repository `andrewcgraves/sparks-effect`:

- [ ] Repository secrets: `API_LINEAR_ACCESS_KEY_STAGING`,
      `WORKER_LINEAR_ACCESS_KEY_STAGING`, `WEBSITE_LINEAR_ACCESS_KEY_STAGING`.
- [ ] Environment `production` with secret `RAILWAY_TOKEN`. The worker's staging
      service lives in Railway's production environment, so staging needs this
      token too.
- [ ] On the GHCR packages `sparks-effect-api` and
      `sparks-effect-routing-worker`: Manage Actions access, add
      `andrewcgraves/sparks-effect` with the Write role.

Railway, project `sparks-effect`, environment `staging`, service
`sparks-effect-api` (dashboard only; this service is not in `railway.ts`):

- [ ] Grant the Railway GitHub app access to `andrewcgraves/sparks-effect`.
- [ ] Source repository `andrewcgraves/sparks-effect`, branch `main`, root
      directory `api`, watch paths `api/**`.

Vercel, project `sparks-effect-website`:

- [ ] Check first that the Production Branch is not `main`. If it is, every
      merge ships to production.
- [ ] Git repository `andrewcgraves/sparks-effect`, Root Directory `website`.
- [ ] Rebind `dev.sparks-effect.app` and `sparks-dev.andrewcgraves.com` from
      branch `trunk` to `main`.
- [ ] Ignored Build Step: `bash scripts/vercel-ignore-build.sh`.

Then:

- [ ] Pause staging deploys from the old repositories so two pipelines do not
      move the same `:staging` tag: disable the `CI` workflow's publish in
      sparks-effect-routing-worker (or disable the workflow) for the trial.
- [ ] Set `STAGING_DEPLOY_ENABLED=true`.
- [ ] Run the `API` and `Worker` workflows once by hand from `main`, so the
      current commit has an image.
- [ ] Confirm: a change under `api/` redeploys only API staging; a change under
      `worker/` redeploys only `routing-worker-staging`; a change under
      `website/` builds only on Vercel. Railway watch paths have been reported
      to redeploy every service, which is what this trial is for.

To undo: point Railway and Vercel back at the old repositories, set the
variable to `false`, re-enable the old workflow.

## Stage 2: production

- [ ] In environment `production`, add `API_LINEAR_ACCESS_KEY_PRODUCTION`,
      `WORKER_LINEAR_ACCESS_KEY_PRODUCTION`,
      `WEBSITE_LINEAR_ACCESS_KEY_PRODUCTION`, `VERCEL_TOKEN`, `VERCEL_ORG_ID`,
      `VERCEL_PROJECT_ID`. Limit the environment to the three tag patterns.
- [ ] Repository variable `API_RAILWAY_SERVICE` (the value of the old
      repository's `RAILWAY_SERVICE` variable).
- [ ] Set `PRODUCTION_RELEASE_ENABLED=true`.
- [ ] Release each service once with its new tag (`api/vX.Y.Z` and so on),
      passing `base_ref` on the first run because there is no earlier tag in
      the new scheme. Each service's `docs/releases.md` has the steps.
- [ ] Rollback to a version from before the move still goes through the old
      repository's release workflow, so keep that working until each service
      has two releases from here.

## Stage 3: retire the old repositories

- [ ] Carry across any branch still wanted with
      `scripts/import-from-old-repo.sh <service> <branch>`. Open pull requests
      do not transfer.
- [ ] Disable the workflows in the three old repositories, then archive them.
- [ ] In `kustomize-config`, update the documentation links that point at the
      old repositories. Image names did not change, so `railway.ts` and the
      compose file need no edit.
