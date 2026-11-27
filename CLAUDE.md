# Sparks Effect monorepo

Three services, each in its own directory with its own toolchain, Makefile,
docs and release cycle:

- `api/`: Go HTTP API. Start with `api/CLAUDE.md` and `api/CONTEXT.md`.
- `worker/`: Go routing worker. Start with `worker/CLAUDE.md` and `worker/CONTEXT.md`.
- `website/`: Vue and TypeScript site on Vercel. Start with `website/CLAUDE.md` and `website/CONTEXT.md`.

Read the service's own `CLAUDE.md` before working in it; its rules apply to
everything under that directory. Run commands from the service directory
(`make -C api test`), not from the root.

## Repo map

- `api/`, `worker/`, `website/`: the three services, each with its own
  `CLAUDE.md`.
- `.github/`: workflows, CODEOWNERS, the PR and issue templates, and the
  rulesets as JSON in `.github/rulesets/`.
- `scripts/`: repository scripts, such as the import from the old repos.
- `docs/`: the cutover from the old per-service repositories.

## Commands

- `make help` (or `make`): list the root targets.
- `make prepush`: run before every push. Lint, unit tests and build for every
  service, plus `make check-contract`. No Docker; a few minutes.
- `make ci`: `make prepush` plus the API and worker Docker integration tests.
  Run it when a change touches code the integration tests cover: the API's
  Postgres persistence and migrations, and the RabbitMQ queue on either side.
- `make doctor`: check that Go, Node and Docker match what the services ask for.

## Pull requests

Fill in every heading of `.github/pull_request_template.md`. A body passed with
`gh pr create --body` skips the template, so write its headings into the body.
Say what changed and how it was checked; leave out process notes.

## What agents never do

- Create or push tags. Tags release to production.
- Set repository variables or secrets, or change repository settings.
- Run `railway` or `vercel` commands.
- Edit or apply rulesets.
- Copy anything from `kustomize-config`.

## What is shared

- **Workflows** live only in the root `.github/workflows/`, one CI and one
  release workflow per service, each filtered to its service's paths.
- **The queue and worker-store contract** is duplicated between `api/` and
  `worker/` and pinned by golden fixtures. `make check-contract` at the root
  fails if the two copies differ. A change to either side changes both in the
  same commit.
- **The website's API types** in `website/src/api/` mirror the API by hand.
  When an API response changes shape, update the mirror in the same change.

## Deploys are per service, not atomic

One commit can change all three services, but they still deploy separately
and at different moments. Keep an API change backward compatible until the
worker and website that depend on it are live.

## Releases

Tags are namespaced per service: `api/v1.2.3`, `worker/v1.2.3`,
`website/v1.2.3`. A bare `v1.2.3` tag releases nothing. Each service's
`docs/releases.md` has the steps.

## Elsewhere

Deploy and monitoring configuration is in the separate, private
`kustomize-config` repository. Do not copy anything from it into this
repository, which is public.
