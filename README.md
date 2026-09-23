# sparks-effect-routing-worker

Consumes routing jobs from a queue and computes chained isochrones against a
self-hosted Valhalla.

## Why it is a separate service

Valhalla runs ClusterIP-only on the home cluster with no ingress, so
`sparks-effect-api` — deployed on Railway — cannot reach it. The queue is the
only transport between them, which means the routing fan-out has to execute
inside the cluster. This worker is that execution.

The two repositories share **no Go code**. What they share is one message body
and the authenticated HTTP contract this worker uses to write results back:

```
API                                    worker
 ├── resolves auth, ownership,          ├── decodes the message
 │   target slug, staleness             ├── computes the chained isochrone
 ├── inserts routing_jobs row           │   against Valhalla
 ├── publishes the message  ──queue──▶  └── records the result through
 └── answers 202; client polls              authenticated API endpoints
```

Everything to do with *who may ask for what* is resolved on the API side before
publishing. By the time a message exists it names one specific immutable
compiled graph and carries it inline, so this worker needs no database of its
own to read graphs from and no piece of resolution logic crosses the boundary.

The queue-message contract is pinned by `internal/routing/testdata/message.golden.json`
and the worker-store HTTP envelope by `internal/store/testdata/worker-store.golden.json`
— the same files, byte for byte, that the API asserts it produces. Nothing else
stops the two hand-maintained copies of the structs drifting apart, so
`make check-contract` fetches the API's copies and diffs them, and CI runs it on
every push. That check can go red without anything here changing: the API
merging a new field is enough, and means this side has a fixture to copy in and
a struct to update. The API's contract job curls these paths; a missing file
here 404s that job rather than reporting drift.

## What it owns

- **The queue consumer** (`internal/worker`) — one durable classic queue with a
  dead-letter queue behind it, prefetch 1, acked only after the job is
  recorded through the API.
- **The Valhalla client** (`internal/valhalla`) — isochrone, matrix, route and status,
  plus the request limits the server enforces.
- **The chainer** (`internal/isochrone`) — access leg, transit ride, egress leg,
  unioned into one reachability surface. Moved here wholesale from the API.
- **The graph model** (`internal/transit`) — enough of the compiled-graph types
  to decode one and run Dijkstra over it, and no more.
- **The result and cache client** (`internal/store`) — job transitions and
  egress-polygon reuse, over authenticated HTTP rather than a database
  connection. The SQL lives in sparks-effect-api.

## Caching egress polygons

A chain makes three kinds of routing call, and they are not equally cacheable.
The origin isochrone and the access times both start from wherever the user
clicked, so they are as particular as the click is. Access times are one Matrix
call for walk / bike / drive, and pairwise `/route` for transit — Valhalla's
Matrix endpoint hard-rejects multimodal costing (error 140), so the chainer
never asks it for that mode (SPA-246). The egress isochrones start from
station positions, which the publication fixed — and they are the bulk of
the work, one call per station reached by riding transit against two for
everything else. A station reached by walking alone gets no egress call, and
nothing to cache: its own reach is already covered by the origin polygon.

The key is `(compile_job_id, station_slug, mode, contour_mins, departs_on)`.

Keying on the publication is what makes this correct with no invalidation
machinery at all. A published scenario is frozen and its stations cannot move,
and editing an authored service produces a *new* compile with a new id — so a
recompile rotates the whole cache generation on its own and the previous one
simply stops being read. Nothing is ever purged, and there is no invalidation
step for anyone to forget to fire. Rows disappear with their compile job, by
foreign key.

`contour_mins` is the contour actually asked of Valhalla, after clamping to the
server's ceiling, so every budget that clamps to the same contour shares one row.
`mode` is the domain's vocabulary — walk / bike / drive / transit — matching
the column; Valhalla's "costing" stays at the client boundary (`transit` →
`multimodal`, never Valhalla's own `transit` costing, which is stop-to-stop
and useless for an origin that is a house). Adding `transit` needed no schema
change: the column is unconstrained text, and SPA-248 left it that way
because this worker is the only writer.

`departs_on` is the GTFS service day a transit polygon was computed against,
and is empty — NULL — for the modes that have no timetable. A transit polygon
is a function of that day's services, so without it the first request against a
compile job froze that day's answer for as long as the job lived, which for the
seeded corridor is indefinitely. Two service dates being two rows is what makes
a GTFS calendar boundary self-correcting instead of mixing pre-expiry and
post-expiry polygons into one response (SPA-269). The API's constraint treats
NULLs as *not* distinct, so walk / bike / drive still keep exactly one row per
station and contour. The date on the key is the date the egress request itself
was stamped with — see "the departure clock" below — never one recomputed
beside it.

**Accepted risk.** A Valhalla tile rebuild changes every polygon without
changing any compile job, and nothing here detects that. Rebuilds are rare and
are already a deliberate operator ritual. To keep it diagnosable, each row
records the server's tileset timestamp as *data only* — never part of the key,
because keying on it would silently invalidate the entire cache on every rebuild.
The stamp is refreshed from Valhalla `/status` at the start of each job. A failed
status read keeps the last known stamp, or NULL if there has not been one, so a
rebuild under a running worker does not require a restart to keep this diagnostic
truthful.

**Accepted risk, second one.** `departs_on` is a *date*, but since SPA-274 the
egress request's departure also carries a time of day — the chain's departure
plus the journey to that station. So the key is coarser than the request it
describes, and two chains can share a row whose polygons were computed at
different hours of the same day.

Reaching it takes a coincidence: the same graph, the same station, the same
clamped contour and the same date, but different journey durations — which
needs different origins and budgets that differ by exactly the difference in
journey time. Two cases are worth knowing about. Below the contour ceiling the
gap is bounded by the minute-truncation of `remaining_mins`, so at worst 59
seconds, which no timetable notices. Above the ceiling every budget clamps to
the same contour, so the gap is unbounded: budget 400 min arriving 10 minutes
in and budget 500 min arriving 100 minutes in both key on contour 320 and share
one row, 90 minutes of timetable apart.

It is accepted rather than fixed because closing it means putting the departure
itself in the key, and the column the API owns is a `date` — so the fix is a
migration in sparks-effect-api (a `departs_at`, or a minute-of-day beside the
date), not something this repository can do alone. Until then the reuse is
wrong only in the clamped regime, where the polygon is a ceiling-sized answer
to an over-ceiling ask in the first place.

The cache is an optimisation over work the worker can always do itself, so
nothing about it can fail a job: a miss, an entry that will not parse, or an
API that cannot be reached all mean the same thing — compute the polygon,
and log that the cache is not working.

**Synthetic and real polygons share one keyspace.** Nothing in the key says
which backend drew the shape, so a worker started with `VALHALLA_SYNTHETIC=true`
against a cache another worker warmed will serve real polygons, and one
started against Valhalla with a cache warmed synthetically will serve circles.
Locally that is what a throwaway API (or the in-process fake the tests use)
is for; anywhere shared, treat switching backends as needing the cache
cleared. A NULL `tileset_at` is the tell — synthetic rows have never seen a
tile.

## Failure handling

Getting this split wrong is the failure mode that leaves a user watching a
spinner forever, so it is explicit:

| Cause | Treatment |
|---|---|
| Unknown mode, malformed message, unsupported schema | **Permanent** — mark failed, ack |
| A request Valhalla rejects (400) | **Permanent** — mark failed, ack |
| Routing job row missing (compile job cascaded away) | **Permanent** — discard, ack |
| Valhalla unreachable, 5xx, timeout, rate limit | **Transient** — requeue, bounded |
| API unreachable | **Transient** — requeue, bounded |
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

`config.Validate()` is the same in every environment. It inspects the process
env (and `.env` via godotenv) and refuses to boot if a required variable is
missing or incoherent. It does not open a socket: a typo is caught before
AMQP, the API, or Valhalla are contacted.

Required everywhere — local `.env`, staging, production:

| Variable | Local / non-live | Live Valhalla |
|---|---|---|
| `AMQP_URL` | required | required |
| `API_URL` | required | required |
| `WORKER_TOKEN` | required | required |
| `VALHALLA_URL` | unset | required |
| `VALHALLA_SYNTHETIC` | `true` | unset |

`VALHALLA_URL` and `VALHALLA_SYNTHETIC=true` are mutually exclusive. A typo in
the URL is a startup failure, not a silent fallback to circles. Every synthetic
feature carries `"synthetic": true`, and the opt-in is logged loudly at startup.

To check a `.env` (or any dotenv) without a live cluster:

```sh
cp .env.example .env   # set AMQP_URL, API_URL, WORKER_TOKEN, and VALHALLA_URL
                       # (or VALHALLA_SYNTHETIC=true) at minimum
make check-config      # ./bin/sparks-effect-routing-worker -check
# or, to judge a file as the source of truth (overrides the process env):
# ./bin/sparks-effect-routing-worker -check -env-file path/to.env
make run
```

Unread worker-prefixed variables (`DATABASE_URL`, `SPARKS_*`, …) are warned
about, not fatal. Kubernetes service-link injects are skipped. Unit tests cover
the same rules with `t.Setenv` — they are how `make test` validates config
with no `.env` and no running services.

## Observability

Every log line is a JSON object — level, timestamp, message, and structured
fields — written via the standard library's `log/slog`. `LOG_LEVEL=debug` (or
`VERBOSE=true`) additionally emits per-job debug detail: node counts, cache
hits, per-candidate routing decisions. Anything else — including the
default, unset — stays at info level: a job's arrival, the decisions that
shape its result (contour clamping, matrix truncation, cache hits/misses),
and its outcome, but not the blow-by-blow.

The worker also serves four Prometheus counters on `:9090/metrics` (override
with `METRICS_ADDR`):

| Metric | Labels | What it counts |
|---|---|---|
| `routing_jobs_total` | `outcome=succeeded\|permanent\|transient` | Jobs settled by the handler |
| `isochrone_cache_total` | `result=hit\|miss` | Egress-polygon cache lookups |
| `routing_dead_letters_total` | — | Jobs moved to the dead-letter queue after retries run out |
| `valhalla_errors_total` | `class=bad_request\|rate_limit\|upstream` | Errors from the Valhalla client |

Label values are the same vocabulary the handler and client already use
(`Outcome.String()`, `ErrBadRequest` / `ErrRateLimit` / `ErrUpstream`).
Series are initialised at zero so a cold cache is `hit=0, miss>0` on a
dashboard, not an empty panel. An unreadable cache entry and a failed Get
count as misses; no cache configured is not a miss.

Scraping is configured in `kustomize-config`: a `prometheus.io/scrape` /
`prometheus.io/port` annotation on the worker Deployment, and pod-metrics
discovery in `config.alloy`. Without those, `/metrics` is served but never
collected.

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
make check-config    # Validate() against .env; no AMQP/API/Valhalla
make itest           # throwaway RabbitMQ, full integration suite, teardown
make check-contract  # diff the golden fixtures against sparks-effect-api
```

`check-contract` is not part of `dev-workflow` because it needs the network and
nothing else in that target does. CI runs it as a job of its own.

Integration tests skip themselves unless `TEST_AMQP_URL` is set, so `make test`
stays green with nothing running. Under CI a missing URL is a hard failure
instead, so a broken broker fails the job rather than silently skipping.

The throwaway broker listens on 5673 — one above the API repo's 5672 — so both
repositories' test stacks can be up at once without either connecting to the
other's. Job writes and the egress cache are exercised against an in-process
stand-in for the API's worker endpoints; there is no throwaway Postgres in
this repository.

## Schema ownership

`routing_jobs` and `isochrone_cache` live in **sparks-effect-api's** migration
sequence. Both want real foreign keys into `users` and `jobs`, and a foreign key
cannot cross a schema owned by a different migration sequence.

This repository therefore runs no migrations, connects to no database, and
should not gain either: a second goose sequence against the same database
would be two tools racing to own one `schema_migrations` table, and a
`DATABASE_URL` on this worker is how the database used to be reachable from
the private internet (SPA-273). Job transitions and cache writes go through
authenticated endpoints on the API (`/api/internal/...`), gated by a shared
`WORKER_TOKEN`. Deploy the API side of that contract before this worker, or
startup ping fails and the pod will not consume.

The paths this client speaks:

| Method | Path | Success | 404 |
|---|---|---|---|
| GET | `/api/internal/worker` | 204 (startup ping) | — |
| POST | `/api/internal/routing-jobs/{id}/running` | 204 | missing or already terminal |
| POST | `/api/internal/routing-jobs/{id}/succeeded` | 204 | missing |
| POST | `/api/internal/routing-jobs/{id}/failed` | 204 | missing |
| POST | `/api/internal/isochrone-cache/lookup` | 200 `{entries}` | — |
| POST | `/api/internal/isochrone-cache` | 204 | — |

Authorization is `Bearer $WORKER_TOKEN`. A 404 on a job write is
`ErrJobNotFound` (permanent); anything else is retried.

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
deployment keeping that should lower this to match. The same bound covers the
egress fan-out and, for transit mode, the pairwise `/route` access-leg
workaround.

## Transit costing

`transit` is an access/egress mode: walk plus scheduled local transit to and
from an authored station. The ride along the authored line stays
physics-compiled on the API; Valhalla never sees it.

Valhalla's `/sources_to_targets` (Matrix) does not support `costing=multimodal`
— it returns 400 / error 140 whether or not transit tiles are loaded. The
chainer therefore:

1. Pre-filters stations by haversine at 40 km/h (blended local-transit),
   capped at 80 km — not the 200 km pedestrian matrix b-line, which would
   be a target list pairwise `/route` cannot afford.
2. Issues one `/route` `costing=multimodal` per survivor, bounded by
   `VALHALLA_MAX_CONCURRENCY`, and reads `trip.summary.time` as access
   seconds. A 400 on one pair drops that station (the way a null Matrix
   cell does); a rate-limit or 5xx still fails the job.
3. Draws origin and egress isochrones with the same multimodal costing.
   Those polygons are visualisation only — a 30-minute multimodal
   isochrone can miss a stop `/route` reached in 13 minutes
   (valhalla/valhalla#5636) — and are never used as a station filter.
4. Always sends `date_time`. Without it the same pair waits for the next
   trip and inflates by a factor of four.
   `costing_options.pedestrian.transit_start_end_max_distance` is raised
   to 10 km (Valhalla's request default is 2415 m) so suburban walks to a
   station are not clipped. The docs-renamed key
   `multimodal_start_end_max_distance` is ignored by the costing engine
   and must not be sent (SPA-297).

### The departure clock

One chain resolves one departure, at the top of `Chain`, and every call it
makes carries it. It defaults to 08:00 on the next weekday — today, if today
is one — resolved on `America/Los_Angeles`, since every scenario served is in
California or Nevada. Resolving it on the UTC calendar, as it used to, rolled
the date over at about 17:00 Pacific: the same pin at the same budget drew a
different surface in the evening than at lunchtime, a Friday evening asked for
Monday, and an evening request the day before a feed's `end_date` asked for a
day that feed does not cover (SPA-274).

Egress is the one leg that does *not* depart when the rider does. It is
stamped at the chain's departure plus the whole journey to that station —
access leg, boarding wait, and every hop ridden — because that is when the
rider is actually standing on the platform. Compiled Local is 5 h 06 m
SF→Anaheim, so a rider leaving at 08:00 steps off in the early afternoon and
was previously handed a morning-peak reach.

### Did it actually ride anything?

Valhalla does not error on a multimodal request it can find no service for. It
walks the whole way and answers 200, with a plausible shape and a plausible
duration, so a tileset whose GTFS calendars have all expired is
indistinguishable from a corridor with genuinely poor service unless the
maneuvers are read. `RouteResponse.RodeTransit` reads them, the transit access
fan-out logs how many of its legs boarded anything, and `make acceptance`
asserts it against a real server on a pair where the answer is known:

```
make acceptance TEST_VALHALLA_URL=http://localhost:8002
```

A 200 with a polygon is exactly what the broken state returns, so a check
weaker than "a maneuver whose `travel_mode` is `transit`" proves nothing.
Riding *something* is not enough either: LA Metro and Muni arrive on free
Mobility Database rows, so a tileset built with no 511 token still boards
them. `TestAcceptanceSanJoseRouteBoardsVTA` asserts on the operator
(`RouteResponse.Operators`, from each transit maneuver's
`transit_info.operator_name`) for San Jose State → Santa Teresa, a Blue Line
pair that rides whenever VTA is in the graph. It is not SJSU → Diridon, the
reported pair: that one walks by three minutes at Valhalla's default walking
speed on a healthy tileset (SPA-335) and so cannot fail cleanly.

This suite is part of the post-cycle procedure, not CI — it needs a real
tileset. kustomize-config's `make verify-cycle` port-forwards the Valhalla
Service and runs `make acceptance` from a checkout of this repository after
its in-pod checks; a failure there is a failed cycle (SPA-339).
`TEST_TRANSIT_DEPART_AT=YYYY-MM-DDTHH:MM` asks about a specific service date,
which is how "do the feeds on the volume still cover next week?" gets answered
before it turns into an undersized isochrone.

Error 140 is a 400 and already maps to `ErrBadRequest` / permanent. It
should not appear if Matrix is skipped. Other transit-costing 400s (no
service near a location, no path) are the same bucket; the pairwise
access path treats them as an unrouted station rather than a failed job.
