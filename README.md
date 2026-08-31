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
stops the two hand-maintained copies of the struct drifting apart, so
`make check-contract` fetches the API's copy and diffs it, and CI runs it on
every push. That check can go red without anything here changing: the API
merging a new field is enough, and means this side has a fixture to copy in and
a struct to update.

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
- **The egress cache** (`internal/store`) — the polygons a chain would otherwise
  recompute, keyed on the publication.

## Caching egress polygons

A chain makes three kinds of routing call, and they are not equally cacheable.
The origin isochrone and the access matrix both start from wherever the user
clicked, so they are as particular as the click is. The egress isochrones start
from station positions, which the publication fixed — and they are the bulk of
the work, one call per station reached by riding transit against two for
everything else. A station reached by walking alone gets no egress call, and
nothing to cache: its own reach is already covered by the origin polygon.

The key is `(compile_job_id, station_slug, mode, contour_mins)`.

Keying on the publication is what makes this correct with no invalidation
machinery at all. A published scenario is frozen and its stations cannot move,
and editing an authored service produces a *new* compile with a new id — so a
recompile rotates the whole cache generation on its own and the previous one
simply stops being read. Nothing is ever purged, and there is no invalidation
step for anyone to forget to fire. Rows disappear with their compile job, by
foreign key.

`contour_mins` is the contour actually asked of Valhalla, after clamping to the
server's ceiling, so every budget that clamps to the same contour shares one row.
`mode` is the domain's vocabulary — walk / bike / drive — matching the column;
Valhalla's "costing" stays at the client boundary.

**Accepted risk.** A Valhalla tile rebuild changes every polygon without
changing any compile job, and nothing here detects that. Rebuilds are rare and
are already a deliberate operator ritual. To keep it diagnosable, each row
records the server's tileset timestamp as *data only* — never part of the key,
because keying on it would silently invalidate the entire cache on every rebuild.
The stamp is read once at startup, so a rebuild under a running worker keeps
stamping the old timestamp until the pod restarts.

The cache is an optimisation over work the worker can always do itself, so
nothing about it can fail a job: a miss, an entry that will not parse, or a
database that cannot be reached all mean the same thing — compute the polygon,
and log that the cache is not working.

**Synthetic and real polygons share one keyspace.** Nothing in the key says
which backend drew the shape, so a worker started with `VALHALLA_URL` unset
against a database another worker warmed will serve real polygons, and one
started *with* it against a database warmed synthetically will serve circles.
Locally that is what the throwaway database is for; anywhere shared, treat
switching `VALHALLA_URL` as needing the cache cleared. A NULL `tileset_at` is
the tell — synthetic rows have never seen a tile.

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

## Branching and releases

One trunk: `main`. Branch from it, PR into it. Production runs whichever
`sha-<commit>` image `kustomize-config` pins — see
[`docs/releases.md`](docs/releases.md).

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

## Observability

Every log line is a JSON object — level, timestamp, message, and structured
fields — written via the standard library's `log/slog`. `LOG_LEVEL=debug` (or
`VERBOSE=true`) additionally emits per-job debug detail: node counts, cache
hits, per-candidate routing decisions. Anything else — including the
default, unset — stays at info level: a job's arrival, the decisions that
shape its result (contour clamping, matrix truncation, cache hits/misses),
and its outcome, but not the blow-by-blow. This worker runs no migrations and
ships no metrics exporter of its own; these logs are what SPA-193 exists to
make queryable, ahead of the Grafana Alloy hookup that will ship them
somewhere queryable.

A `trace_id` field is attached to every log line written while a job is being
processed, so one isochrone can be followed across both services on one value.
It comes from the message body's `trace_id`, which the API mints for the request
that created the job. A delivery may also carry an `x-trace-id` header, which is
what a line logged *before* the body is decoded uses and what traces a message
injected by hand; the body's value supersedes it once there is one. A message
carrying neither simply logs without the field, and nothing about processing it
changes.

## Development

```sh
make dev-workflow    # test, vet, lint, build — run this before pushing
make itest           # throwaway Postgres + RabbitMQ, full integration suite, teardown
make check-contract  # diff the golden message fixture against sparks-effect-api
```

`check-contract` is not part of `dev-workflow` because it needs the network and
nothing else in that target does. CI runs it as a job of its own.

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
