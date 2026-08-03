# sparks-effect-routing-worker

Consumes routing jobs from a queue and computes chained isochrones against a
self-hosted Valhalla.

## Why it is a separate service

Valhalla runs ClusterIP-only on the home cluster with no ingress, so
`sparks-effect-api` — deployed on Railway — cannot reach it. The queue is the
only transport between them, which means the routing fan-out has to execute
inside the cluster. This worker is that execution.

The two repositories share **no Go code**. What they share is the Postgres
schema and one message body:

```
API                                    worker
 ├── resolves auth, ownership,          ├── decodes the message
 │   target slug, staleness             ├── computes the chained isochrone
 ├── inserts routing_jobs row           │   against Valhalla
 ├── publishes the message  ──queue──▶  └── updates the routing_jobs row
 └── answers 202; client polls
```

Everything to do with *who may ask for what* is resolved on the API side before
publishing. By the time a message exists it names one specific immutable
compiled graph and carries it inline, so this worker needs no database of its
own to read graphs from and no piece of resolution logic crosses the boundary.

The message contract is pinned by `internal/routing/testdata/message.golden.json`
— the same file, byte for byte, that the API asserts it produces. Nothing else
stops the two hand-maintained copies of the struct drifting apart.

## What it owns

- **The queue consumer** (`internal/worker`) — one durable classic queue with a
  dead-letter queue behind it, prefetch 1, acked only after the job row is
  written.
- **The Valhalla client** (`internal/valhalla`) — isochrone, matrix and status,
  plus the request limits the server enforces.
- **The chainer** (`internal/isochrone`) — access leg, transit ride, egress leg,
  unioned into one reachability surface. Moved here wholesale from the API.
- **The graph model** (`internal/transit`) — enough of the compiled-graph types
  to decode one and run Dijkstra over it, and no more.

## Failure handling

Getting this split wrong is the failure mode that leaves a user watching a
spinner forever, so it is explicit:

| Cause | Treatment |
|---|---|
| Unknown mode, malformed message, unsupported schema | **Permanent** — mark failed, ack |
| A request Valhalla rejects (400) | **Permanent** — mark failed, ack |
| Routing job row missing (compile job cascaded away) | **Permanent** — discard, ack |
| Valhalla unreachable, 5xx, timeout, rate limit | **Transient** — requeue, bounded |
| Database unreachable | **Transient** — requeue, bounded |
| Retries exhausted | Mark failed, then dead-letter |

Unrecognised errors are treated as transient. Retrying one a bounded number of
times costs seconds; abandoning one that would have succeeded throws work away.

Retries are counted in an `x-routing-attempts` header rather than in memory,
because a redelivered message may land on a different worker — or on this one
after a restart — and an in-memory counter would begin again at zero. That is
also why the retry path republishes rather than nacking with requeue: a nack
puts the original bytes back unchanged, so the counter would never advance.

## Running it

`VALHALLA_URL` decides what backs the routing calls. Unset, the worker serves
**synthetic** isochrones — circles sized from the budget — which makes the whole
publish → consume → Postgres loop runnable in seconds with no tileset. Every
synthetic feature carries `"synthetic": true`, and the fallback is logged
loudly at startup, because it is the right default locally and a serious problem
in production.

```sh
cp .env.example .env   # set AMQP_URL and DATABASE_URL at minimum
make run
```

## Development

```sh
make dev-workflow   # test, vet, lint, build — run this before pushing
make itest          # throwaway Postgres + RabbitMQ, full integration suite, teardown
```

Integration tests skip themselves unless `TEST_DATABASE_URL` and `TEST_AMQP_URL`
are set, so `make test` stays green with nothing running. Under CI a missing URL
is a hard failure instead, so a broken service fails the job rather than
silently skipping.

The throwaway containers listen on 5433 and 5673 — one above the API repo's —
so both repositories' test stacks can be up at once without either connecting to
the other's.

## Schema ownership

`routing_jobs` and `isochrone_cache` live in **sparks-effect-api's** migration
sequence. Both want real foreign keys into `users` and `jobs`, and a foreign key
cannot cross a schema owned by a different migration sequence.

This repository therefore runs no migrations, and should not gain any: a second
goose sequence against the same database would be two tools racing to own one
`schema_migrations` table. `internal/testsupport/schema.sql` is a test-only copy
of the columns this worker touches. It is a copy, and copies drift — if a test
passes locally but the worker fails deployed, that file disagreeing with
`00014_routing_jobs.sql` is the first place to look.

## Routing limits

The limits are configuration, defaulted to what the deployed container actually
enforces (see `kustomize-config`):

| Setting | Default | Where it comes from |
|---|---|---|
| `VALHALLA_MAX_ISO_CONTOUR_MINS` | 320 | Patched — `ISOCHRONE_MAX_TIME_MINUTES=320` applied to `service_limits.isochrone.max_time_contour` by an init container |
| `VALHALLA_MAX_MATRIX_PAIRS` | 2500 | Not patched — Valhalla's own `max_matrix_location_pairs`, enforced as sources × targets |
| `VALHALLA_MATRIX_BLINE_AUTO_KM` | 400 | Not patched — Valhalla's `auto.max_matrix_distance` |
| `VALHALLA_MATRIX_BLINE_OTHER_KM` | 200 | Not patched — Valhalla's `pedestrian`/`bicycle.max_matrix_distance` |
| `VALHALLA_MAX_CONCURRENCY` | 8 | Not a Valhalla limit — a bound on the egress fan-out |

The values inherited from the API were Stadia plan allowances, and two of them
were wrong for this deployment in opposite directions: the contour ceiling was
120 (nearly three times too strict) and the matrix cap was 600 (over four times
too strict). Both are now the real numbers.

`VALHALLA_MAX_CONCURRENCY` is a courtesy to the server rather than a limit it
imposes. Note that `valhalla.env` currently sets `server_threads=2`; a
deployment keeping that should lower this to match.
