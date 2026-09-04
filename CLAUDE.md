# CLAUDE.md

Use the Makefile for all build/test tasks (Go project).

- `make dev-workflow` — run before pushing: test, vet, lint, build (single verification step)
- `make build` — compile to `./bin/sparks-effect-routing-worker`
- `make run` — build and run
- `make test` — `go test ./... -race -cover` (integration tests skip themselves)
- `make itest` — throwaway RabbitMQ, full integration suite, teardown
- `make acceptance TEST_VALHALLA_URL=…` — asks a real Valhalla whether it actually puts a rider on a train. Needs transit tiles, so it sits outside `test` and `dev-workflow`, and refuses rather than skipping silently
- `make check-contract` — diff the golden message fixture against sparks-effect-api's copy (needs network; CI runs it, `dev-workflow` does not)
- `make vet` / `make lint` — static checks
- `make tidy` — `go mod tidy`
- `make clean` — remove build artifacts

The Postgres schema is owned by sparks-effect-api's migration sequence. This
repository runs no migrations and holds no database connection; job writes
go through authenticated API endpoints.

## Branching

One trunk: `main`. Branch from it, PR into it. No `prd` branch here. See
`docs/releases.md`.
