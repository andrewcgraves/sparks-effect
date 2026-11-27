# CLAUDE.md

Use the Makefile for all build/test tasks (npm project).

- `make dev-workflow` — run before pushing: lint, test, build (non-interactive, no server)
- `make dev` — `npm run dev` (long-running Vite dev server with hot reload; avoid in automation)
- `make build` — `npm run build` (output in `dist/`)
- `make run` — build then `npm run preview` (long-running server; avoid in automation)
- `make test` — `npm run test`
- `make lint` — `npm run lint`
- `make clean` — remove `node_modules` and `dist`

Deps install automatically via `make install`; targets depend on it.

## Domain vocabulary

[`CONTEXT.md`](CONTEXT.md) defines this repository's own words — travel zone, the
route map's ridden / unridden / unfinished line states, and the time-remaining
graph's view / row / lane / through / fork. Shared domain vocabulary (scenario,
service, station, node, edge, dwell, boarding wait, mode vs costing, the error
codes, the job statuses) is defined once in
[the API's `CONTEXT.md`](../api/CONTEXT.md),
and chain vocabulary in
[the worker's](../worker/CONTEXT.md).
Use those words; do not mint synonyms. Visitor copy follows the Product
vocabulary table in `CONTEXT.md`: product words (line, network, route, travel
zone) in anything a person reads, domain words in identifiers, test ids, router
names, URLs and API fields.

Declaration-level comments were deliberately removed from this source in SPA-307
because they restated their declarations. Domain meaning belongs in `CONTEXT.md`;
rationale belongs in in-function comments and the README. Do not reintroduce
comments that only repeat a declaration.

## Issues

Issues live in Linear, team `Sparks Effect` (`SPA-` prefix). Commits and PR titles
must include the identifier in the subject, e.g. `SPA-84: add authoring API client, domain types, and job-polling helper`.

## Branching

One trunk: `main`, shared with the API (`../api`) and the worker (`../worker`).
Branch from it, PR into it. No `prd` branch here — production is promoted by
pushing a `website/vX.Y.Z` tag on a commit that is already on `main`, which
promotes the Vercel build of the website that commit already had. The workflows
are `.github/workflows/website.yml` and `website-release.yml` at the repository
root. See `docs/releases.md`.
