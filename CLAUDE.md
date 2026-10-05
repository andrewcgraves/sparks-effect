# CLAUDE.md

Use the Makefile for all build/test tasks (Go project).

- `make dev-workflow` — run before pushing: test, vet, lint, build (single verification step)
- `make build` — compile to `./bin/sparks-effect-routing-worker`
- `make run` — build and run
- `make test` — `go test ./... -race -cover` (integration tests skip themselves)
- `make itest` — throwaway RabbitMQ, full integration suite, teardown
- `make acceptance TEST_VALHALLA_URL=…` — asks a real Valhalla whether it actually puts a rider on a train. Needs transit tiles, so it sits outside `test` and `dev-workflow`, and refuses rather than skipping silently
- `make check-contract` — diff the golden fixtures against sparks-effect-api's copies (needs network; CI runs it, `dev-workflow` does not)
- `make vet` / `make lint` — static checks
- `make vulncheck` — `govulncheck ./...`. Needs the network, so it is not in
  `dev-workflow`; CI runs it
- `make tidy` — `go mod tidy`
- `make clean` — remove build artifacts

The Postgres schema is owned by sparks-effect-api's migration sequence. This
repository runs no migrations and holds no database connection; job writes
go through authenticated API endpoints.

## Domain vocabulary

[`CONTEXT.md`](CONTEXT.md) defines this repository's own words — chaining, access
and egress legs, journey and leg, reached vs reachable, starter station and
starter walk, publication, the departure clock, synthetic. Shared domain
vocabulary (scenario, service, station, node, edge, dwell, boarding wait, mode vs
costing, the error codes, the job statuses) is defined once in
[sparks-effect-api's `CONTEXT.md`](https://github.com/andrewcgraves/sparks-effect-api/blob/main/CONTEXT.md),
which this file links to rather than restating. Use those words; do not mint
synonyms.

Declaration-level doc comments were deliberately removed from this source in
SPA-307 because they restated their declarations. Domain meaning belongs in
`CONTEXT.md`; rationale belongs in in-function comments and the README. Do not
reintroduce godoc that only repeats a name.

## Branching

One trunk: `main`. Branch from it, PR into it. No `prd` branch here. See
`docs/releases.md`.
