# CLAUDE.md

Use the Makefile for all build/test tasks (Go project).

- `make dev-workflow` — run before pushing: test, vet, lint, build (single verification step)
- `make build` — compile to `./bin/sparks-effect-routing-worker`
- `make run` — build and run
- `make test` — `go test ./... -race -cover` (integration tests skip themselves)
- `make itest` — throwaway Postgres + RabbitMQ, full integration suite, teardown
- `make vet` / `make lint` — static checks
- `make tidy` — `go mod tidy`
- `make clean` — remove build artifacts

The Postgres schema is owned by sparks-effect-api's migration sequence. This
repository runs no migrations; `internal/testsupport/schema.sql` is a test-only
copy.

## Branching

One trunk: `main`. Branch from it, PR into it. No `prd` branch here. See
`docs/releases.md`.
