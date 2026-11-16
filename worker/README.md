# sparks-effect-routing-worker

Consumes routing jobs from a queue and computes chained isochrones against a
self-hosted Valhalla.

## Why it is a separate service

Valhalla runs ClusterIP-only on the home cluster with no ingress, so
`sparks-effect-api` — deployed on Railway — cannot reach it. The queue is the
only transport between them, which means the routing fan-out has to execute
inside the cluster. This worker is that execution.

The two services share **no Go code**. What they share is one message body
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
`make check-contract` diffs them against the API's copies in `../api`, and CI
runs it on every push that touches the worker or the API's fixtures. It is a
local diff, so a change to the contract has to land on both sides in the same
pull request: a new field means a fixture to copy across and a struct to
update. The API's contract job reads these paths; a missing file here fails
that job.

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

The key is `(compile_job_id, station_slug, mode, contour_mins, departs_on, budget_mins)`.

Keying on the publication is what makes this correct with no invalidation
machinery at all. A published scenario is frozen and its stations cannot move,
and editing an authored service produces a *new* compile with a new id — so a
recompile rotates the whole cache generation on its own and the previous one
simply stops being read. Nothing is ever purged, and there is no invalidation
step for anyone to forget to fire. Rows disappear with their compile job, by
foreign key.

`contour_mins` is the contour actually asked of Valhalla, after clamping to the
server's ceiling, so for walk / bike / drive every budget that clamps to the same
contour shares one row; transit rows are split by budget as well (below).
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

**A row from another tileset is a miss (SPA-325).** A Valhalla tile rebuild
changes every polygon without changing any compile job, so nothing in the key
notices a map cycle. Each row instead carries the tileset timestamp Valhalla
reported when it was drawn (`tileset_at`), and a lookup serves a row only when
that stamp equals the one the worker holds now. Anything else, older or newer,
is a miss on the same path a corrupt row takes: counted as a miss, recomputed,
and written back with the current stamp. The stamp stays off the key on
purpose. Keying on it would mint a new row per rebuild; comparing on read
reuses the key, and the API's write guard (SPA-328) lets the stamped recompute
replace the stale row. A cycle therefore costs each row one recompute and needs
no purge. The stamp is refreshed from Valhalla `/status` at the start of each
job, and a failed read keeps the last known stamp, so a cycle under a running
worker is noticed without a restart.

The NULL case was decided deliberately:

- **A row with no stamp is a miss.** That is a synthetic circle, or a row
  written while `/status` had not answered since boot. The API lets a stamped
  row replace an unstamped one, so this costs one recompute per row, not one
  per request.
- **A worker with no stamp reads nothing.** In synthetic mode, or when
  `/status` has not answered since boot, the worker has nothing to compare a row
  against, so it skips the lookup and counts every key as a miss. It still
  writes, unstamped, and those rows lose to the first stamped recompute. The
  per-job `/status` warning says why the hit rate is zero.
- **Rolling back to an older tileset does not heal.** After
  `make rollback-tiles`, rows drawn from the newer tiles mismatch, but the
  recompute carries the older stamp, and the API never lets an older stamp
  replace a newer one. Those keys are recomputed on every request until the map
  moves forward past them. Every answer is correct, just uncached, and each
  chain that hits them logs `recomputing cache entries cut from another tileset`.

`budget_mins` is the chain's budget, and like `departs_on` it is NULL for every
mode but transit. Since SPA-274 the egress request's departure carries a time
of day — the chain's departure plus the journey to that station — and the
contour is the budget minus that same journey. The two are exactly
complementary: two chains land on the same contour precisely when their journey
times differ by the difference in their budgets, and then their polygons were
asked for that far apart in the timetable. Budget 90 riding from a 10-minute
access leg and budget 120 riding from a 40-minute one both reach a station with
79 minutes left, 30 minutes apart on the platform. Before SPA-326 they shared a
row; with six UI presets from 45 to 240 the worst such gap was 195 minutes, and
through the API it was unbounded.

With the budget in the key, and the chain always departing on the departure
clock, a transit row below the contour ceiling pins its egress departure to
the minute truncation of `remaining_mins` — under a minute apart, which no
timetable notices. Walk / bike / drive send Valhalla no departure at all, so
the budget stays out of their key rather than fragmenting it for nothing.

The price is hit rate. The collision was also what compressed the keyspace:
contour alone spans 1–240 from the UI, and contour × budget is roughly three
times as many reachable rows per station. The whole transit corpus also goes
cold when this ships — every existing row carries a NULL budget, which under
`NULLS NOT DISTINCT` no transit lookup matches. SPA-327 buys the hit rate back
by bucketing `contour_mins` to five minutes, which deliberately reintroduces a
bounded version of the same collision at bucket scale.

**Accepted risk, second one.** Above the contour ceiling the key is still
coarser than the request. `contour_mins` is clamped, so at the ceiling the
remaining time — and with it the departure — is no longer recoverable from the
key: budget 400 min arriving 10 minutes in and budget 400 min arriving 100
minutes in both key on contour 320 and budget 400, and share one row 90 minutes
of timetable apart. No UI preset reaches the ceiling (the largest is 240, the
ceiling 320), so it takes a budget sent to the API directly; bounding
`budget_mins` there (SPA-332) is what makes it reachable only in theory.
Closing it outright means putting the departure itself in the key — a
`departs_at` rather than a date — which is a migration in sparks-effect-api.
Until then the reuse is wrong only in the clamped regime, where the polygon is
a ceiling-sized answer to an over-ceiling ask in the first place.

The cache is an optimisation over work the worker can always do itself, so
nothing about it can fail a job: a miss, an entry that will not parse, or an
API that cannot be reached all mean the same thing — compute the polygon,
and log that the cache is not working.

**Synthetic and real polygons share one keyspace, but not one another's
rows.** Nothing in the key says which backend drew the shape, but a synthetic
worker has no tileset stamp, so it never reads the cache, and every row it
writes is unstamped, which a real worker treats as a miss and replaces
(SPA-325). Switching backends against a shared cache no longer needs the cache
cleared. A NULL `tileset_at` is still the tell that a row has never seen a
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

A per-station egress 400 or timeout drops that polygon; the job succeeds if
any other egress (or a cache hit) remains. If every egress miss fails with a
transient error, the job is still transient. A rate limit still fails the job.

Unrecognised errors are treated as transient. Retrying one a bounded number of
times costs seconds; abandoning one that would have succeeded throws work away.

Retries are counted in an `x-routing-attempts` header rather than in memory,
because a redelivered message may land on a different worker — or on this one
after a restart — and an in-memory counter would begin again at zero. That is
also why the retry path republishes rather than nacking with requeue: a nack
puts the original bytes back unchanged, so the counter would never advance.

## Branching and releases

One trunk: `main`. Branch from it, PR into it. Staging follows `main`: every
push that changes the worker publishes `sha-<commit>` and moves `:staging`, and
CI redeploys the `routing-worker-staging` Railway service in the same run.
Production is promoted by tag, the same way as the API and the website: push a
`worker/vX.Y.Z` tag on a commit that is already on `main`, and the **Worker
release** workflow re-tags the existing image of the worker at that commit as
`:prd` (nothing is rebuilt) and redeploys `routing-worker`.

To roll back, release an older tag: Actions → Worker release → Run workflow →
pass the tag. See [`docs/releases.md`](docs/releases.md).

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

### Shipping logs to Grafana Cloud

On Railway nothing tails the container's stderr into Loki the way Alloy
tailed the pod's, so with `OTEL_EXPORTER_OTLP_ENDPOINT` set the worker also
ships every log line it writes as an OTLP log record, to the same Grafana
Cloud gateway and with the same credentials the API uses:

```sh
OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-us-west-0.grafana.net/otlp
OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic <base64 of instance-id:token>"
OTEL_RESOURCE_ATTRIBUTES=service.namespace=production   # staging on routing-worker-staging
```

stderr stays the primary sink and Railway's log viewer keeps reading it; the
export is a copy, gated by the same `LOG_LEVEL`. The resource carries
`service.name=sparks-effect-routing-worker` and the `service.namespace` from
`OTEL_RESOURCE_ATTRIBUTES`, which Grafana folds into `job`, so the two worker
services stay apart in Loki exactly as the API's staging and production do:

```logql
{service_name="sparks-effect-routing-worker", service_namespace="production"} | trace_id="…"
```

With no endpoint set nothing is exported and boot logs one `info` line saying
so. `-check` never builds the exporter, so a config check still opens no
connection. Fatal lines are flushed before the process exits.

The worker also serves five Prometheus counters and two gauges on
`:9090/metrics` (override with `METRICS_ADDR`):

| Metric | Labels | What it counts |
|---|---|---|
| `routing_jobs_total` | `outcome=succeeded\|permanent\|transient` | Jobs settled by the handler |
| `isochrone_cache_total` | `result=hit\|miss` | Egress-polygon cache lookups |
| `routing_dead_letters_total` | — | Jobs moved to the dead-letter queue after retries run out |
| `valhalla_errors_total` | `class=bad_request\|rate_limit\|upstream` | Errors from the Valhalla client |
| `transit_access_legs_total` | `rode=true\|false` | Pairwise transit access legs that Valhalla successfully routed, labelled by whether they actually rode |
| `routing_job_in_progress_seconds` (gauge) | — | How long the job in hand has been running; 0 when idle |
| `routing_worker_start_time_seconds` (gauge) | — | Unix time the process booted. Railway has no restart counter, so the restart-loop alert is `changes(routing_worker_start_time_seconds[1h]) > 3` |

Job, cache, and Valhalla labels are the handler and client vocabulary already
in use (`Outcome.String()`, `ErrBadRequest` / `ErrRateLimit` / `ErrUpstream`).
`transit_access_legs_total` uses `rode=true|false`. Series are initialised at
zero so a cold cache is `hit=0, miss>0` on a dashboard, not an empty panel,
and a 100% `rode="false"` rate is a real series rather than a missing one.
The denominator is successfully routed access legs only: a bad-request drop
or a failed fan-out is not a `false`. An unreadable cache entry and a failed
Get count as misses; no cache configured is not a miss. A sustained
`rode="false"` ratio near 1.0 with `true+false > 0` means Valhalla is answering
200 and walking. `true=0,false=0` is the other no-service signature (no routed
legs at all).

Scraping is configured in `kustomize-config`: a `prometheus.io/scrape` /
`prometheus.io/port` annotation on the worker Deployment, and pod-metrics
discovery in `config.alloy`. Without those, `/metrics` is served but never
collected.

### Liveness

`GET /healthz` on the same listener answers 200 when the AMQP connection and
channel are open, either no job is in progress or the current one started
less than `WORKER_STUCK_AFTER` ago (a Go duration, default `10m`), and Valhalla
answers `GET /status` within 2 seconds. Otherwise it answers 503 with the reason
as plain text. With `VALHALLA_SYNTHETIC=true` there is no Valhalla to ask, so
that part always passes. The Deployment's liveness probe
points at it, so a consumer that is connected but stuck gets restarted, and the
message it had prefetched is redelivered because it was never acked.

The clock runs from delivery to settlement (ack, requeue, or dead-letter). An
idle worker is never stuck, however long the queue stays empty. The threshold
has to exceed the longest real chain (SPA-436 measures it), or the probe kills
healthy work. API reachability is deliberately not part of the check: an API
outage already surfaces as transient job failures, and restarting every worker
because the API is down would not bring it back.

Valhalla is checked because a worker that holds its queue but cannot route
fails every job it takes. The API is to ask `/healthz` over Railway's private
network whether routing works (SPA-551), which the 30-second check-in to the
API cannot tell it. The probe asks only when the broker and stuck-job checks have
already passed, so a broken worker costs Valhalla nothing. On Railway nothing
restarts the worker on a failing `/healthz`, since the service sets no
healthcheck path. If one is ever set, note that a Valhalla outage would then
fail every worker deploy, and restarting the worker would not bring Valhalla
back.

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
make check-contract  # diff the golden fixtures against ../api
```

`check-contract` is not part of `dev-workflow` because it reads the API's tree
and nothing else in that target does. CI runs it as a job of its own.

Integration tests skip themselves unless `TEST_AMQP_URL` is set, so `make test`
stays green with nothing running. Under CI a missing URL is a hard failure
instead, so a broken broker fails the job rather than silently skipping.

The throwaway broker listens on 5673 — one above the API's 5672 — so both
services' test stacks can be up at once without either connecting to the
other's. Job writes and the egress cache are exercised against an in-process
stand-in for the API's worker endpoints; there is no throwaway Postgres for
the worker.

## Schema ownership

`routing_jobs` and `isochrone_cache` live in **sparks-effect-api's** migration
sequence. Both want real foreign keys into `users` and `jobs`, and a foreign key
cannot cross a schema owned by a different migration sequence.

The worker therefore runs no migrations, connects to no database, and
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
| GET | `/api/internal/worker` | 204 (startup ping, then a heartbeat every 30 s) | — |
| POST | `/api/internal/routing-jobs/{id}/running` | 204 | missing or already terminal |
| POST | `/api/internal/routing-jobs/{id}/succeeded` | 204 | missing |
| POST | `/api/internal/routing-jobs/{id}/failed` | 204 | missing |
| POST | `/api/internal/isochrone-cache/lookup` | 200 `{entries}` | — |
| POST | `/api/internal/isochrone-cache` | 204 | — |

Authorization is `Bearer $WORKER_TOKEN`. A 404 on a job write is
`ErrJobNotFound` (permanent); anything else is retried.

The heartbeat is what lets the API's public `GET /api/routing/status` tell an
idle worker from a dead one (SPA-442): the API calls the worker gone after two
minutes without any authenticated request from it. A failed beat is logged at
warn and the next one tried. It never stops the worker.

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
| `VALHALLA_MAX_ACCESS_ROUTES` | 50 | Not a Valhalla limit — a bound on how many transit access `/route` calls one chain makes |

The values inherited from the API were Stadia plan allowances, and two of them
were wrong for this deployment in opposite directions: the contour ceiling was
120 (nearly three times too strict) and the matrix cap was 600 (over four times
too strict). Both are now the real numbers.

`VALHALLA_MAX_CONCURRENCY` is a courtesy to the server rather than a limit it
imposes. Note that `valhalla.env` currently sets `server_threads=2`; a
deployment keeping that should lower this to match. The same bound covers the
egress fan-out and, for transit mode, the pairwise `/route` access-leg
workaround.

`VALHALLA_MAX_ACCESS_ROUTES` bounds how *many* of those pairwise access calls a
transit chain makes; the 80 km transit reach bounds only how *far*. When more
nodes fall inside the reach than the cap, the chain keeps the nearest by
straight-line distance — the same nearest-N rule as the matrix pair cap — and
logs `chain: nodes truncated to access route cap` at info with the cap and how
many nodes were within reach. A station cut this way cannot be an access
station for that chain. Walk, bike and drive access goes through Matrix and is
bounded by `VALHALLA_MAX_MATRIX_PAIRS` instead; this setting does not touch it.

The default of 50 is chosen against the call budget, not a station count: the
seeded `ca-hsr` corridor has 15 stations in total, so it never reaches the cap,
and the cap only matters for a dense authored network. Fifty access legs is
seven waves at the default concurrency of 8 and 25 waves at staging's 2 —
bounded latency per click, and still far more access stations than a click
has useful nearby ones. These calls are never cached (the access leg starts
wherever the user clicked), so this is the half of a transit chain that a warm
cache does not shrink. It is not yet a `worker-env` literal in
`kustomize-config`, so the cluster runs the default until one is added.

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
2. Keeps the nearest `VALHALLA_MAX_ACCESS_ROUTES` survivors, then issues one
   `/route` `costing=multimodal` per station kept, at most
   `VALHALLA_MAX_CONCURRENCY` at a time, and reads `trip.summary.time` as access
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
fan-out logs and counts how many of its legs boarded anything, and every
reachable station carries the answer for its own access leg as
`access_rode_transit` (SPA-336). The station is kept either way: a walked leg is
still the fastest way there, and dropping it would shrink the surface. It is
only labelled, so the website can say "Walk" rather than "Transit". The field is
written for transit only, as an explicit `false` when the leg walked; walk, bike
and drive omit it, because they cannot disagree with their own mode. A station
reached by riding the scenario inherits the answer from the station its journey
started at. `make acceptance` asserts the maneuver reading against a real
server on a pair where the answer is known:

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
Service and runs `make acceptance` from the `worker/` directory of a checkout of this repository after
its in-pod checks; a failure there is a failed cycle (SPA-339).

On Railway, Valhalla has no public address and Railway's SSH refuses a bare
`ssh -N -L` tunnel, so the suite runs inside the Valhalla container instead,
against its own loopback. It reads no files, only `TEST_VALHALLA_URL` (and
`TEST_TRANSIT_DEPART_AT`), so a static test binary is all it needs:

```
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o acceptance.test ./internal/valhalla/
railway ssh --service valhalla -- 'cat > /tmp/acceptance.test && chmod +x /tmp/acceptance.test' < acceptance.test
railway ssh --service valhalla -- 'TEST_VALHALLA_URL=http://127.0.0.1:8002 /tmp/acceptance.test -test.run Acceptance -test.v; rm -f /tmp/acceptance.test'
```

The binary lands in the container's `/tmp`, not the tiles volume, and is
removed after the run.
`TEST_TRANSIT_DEPART_AT=YYYY-MM-DDTHH:MM` asks about a specific service date,
which is how "do the feeds on the volume still cover next week?" gets answered
before it turns into an undersized isochrone.

Isochrones expose no maneuvers, so a transit egress polygon that does not beat
a walk-speed circle of the same contour is not cached under a transit key
(SPA-338): first-write-wins would otherwise freeze a walked answer for the rest
of the service day. The computed polygon is still returned. The circle's radius
is Valhalla's pedestrian default (5.1 km/h), not the domain walk speed of 5.0.

Error 140 is a 400 and already maps to `ErrBadRequest` / permanent. It
should not appear if Matrix is skipped. Other transit-costing 400s (no
service near a location, no path) are the same bucket; the pairwise
access path treats them as an unrouted station rather than a failed job.
Egress is the same: a per-station 400 or timeout drops that polygon
rather than failing the job, as long as another egress (or a cache hit)
remains. If every egress miss fails transiently, the job is still
transient. A rate limit still fails the job.

### Where the access time goes

A transit access time is door-to-door wall clock, schedule wait included
([ADR-0004](../api/docs/adr/0004-transit-isochrone-claim-and-reported-time.md)),
so every reachable station with a transit access leg also reports
`access_walk_secs`, `access_wait_secs` and `access_ride_secs` beside
`access_secs`, summing to it exactly (SPA-348). `access_secs` is unchanged.
A walked leg reports its whole time as walk with explicit zeros; walk, bike and
drive carry none of the three, since the matrix that serves them has no
maneuvers. Like `access_rode_transit`, a station reached by riding the scenario
carries the split of the station its journey started at, because that is the
leg its `access_secs` is.

The split comes from each `/route` maneuver's `time` and, for a transit
maneuver, its stops' `departure_date_time` and `arrival_date_time`:

- **Walk** — the non-transit maneuvers.
- **Ride** — the timetabled ride: the alighting stop's arrival clock minus the
  boarding stop's departure clock, capped at the maneuver's `time`.
- **Wait** — taken *out of* the transit maneuver, never added beside it.
  Valhalla folds the platform wait into the maneuver's own `time` (SPA-335: a
  16.7 min maneuver that was a 9.0 min ride from 08:31 to 08:40 and a 7.7 min
  wait), so the wait is whatever the maneuver holds beyond the ride. Each
  vehicle on a leg with a transfer has its own.

A leg Valhalla walked end to end is all walk without any of this: there is
nothing to take out of it.

Valhalla's stop clocks are minute precision and carry their UTC offset
(`2026-09-02T08:31-07:00`), while the origin's echoed `date_time` is a bare
local clock; all of them are read as local wall clocks, the offset dropped. The
wait is cross-checked against the clock gap from reaching the platform — the
origin's `date_time` plus every maneuver before — to the boarding departure.
SPA-335's gap was 8.1 min against the 7.7 min wait, 24 s apart: the boarding
clock's minute rounding. Up to `ReconcileToleranceSecs` (60 s, one clock tick)
either way is accepted; more, or a transit maneuver without both clocks, and
the split is *unreconciled*. Separately, the maneuvers must sum to
`summary.time` within `SummaryDriftToleranceSecs` (2 s; SPA-335's maneuvers summed
exactly), so the walk cannot absorb a difference between them. The total
rounds to whole seconds and the walk carries that rounding residue, so the
three sum to `access_secs` exactly. An unreconciled split is withheld while the
station and its `access_secs` are kept: the total is still right, and a split
that does not sum to it would be worse than none. Each one is logged at Debug;
the job's `transit access legs routed` line counts them as `unreconciled` and
is raised to a single Warn, with the first one's error as
`unreconciled_sample`, whenever that count is not zero. `make acceptance`
checks the split reconciles on a real ridden route.
