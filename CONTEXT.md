# CONTEXT.md — chain vocabulary

The words this repository uses for the thing it does: taking one origin, one
budget and one compiled graph, and turning them into a reachability surface.

Shared domain vocabulary — scenario, service, station, stop, node, edge, dwell,
boarding wait, mode vs costing, the error codes, the job statuses — is defined
once in
[**sparks-effect-api's `CONTEXT.md`**](https://github.com/andrewcgraves/sparks-effect-api/blob/main/CONTEXT.md).
Read that first; this file only adds the terms that exist here and nowhere else.

> Declaration-level doc comments were deliberately removed from the source in
> SPA-307. Domain meaning lives in these two files; the *why* behind a decision
> lives in in-function comments and the README. Do not reintroduce godoc that
> only repeats a name.

## The chain

**Chaining** is the whole computation, and `Chain` is its entry point. A chain
answers "where can a rider get from here, in this budget, given this graph?" by
composing three pieces of reachability:

```
        access leg              the ride              egress leg
origin ───────────────▶ station ─────────▶ station ───────────────▶ surface
       Valhalla, by mode      Dijkstra over        Valhalla, by mode
                             the compiled graph
```

| Term | Definition |
| --- | --- |
| **Access leg** | Getting from the origin to a station, routed by Valhalla in the requested mode. One Matrix call for walk/bike/drive; pairwise `/route` for transit, because Valhalla's Matrix rejects `multimodal`. A transit access leg may not ride anything — Valhalla walks the whole way when it finds no service — so each reachable station says whether its own access leg did, as `access_rode_transit`, and how its access time splits into walk, wait and ride (below) |
| **Egress leg** | Getting *away* from a station the rider reached, drawn as an isochrone sized by whatever budget is left. This is the bulk of the work — one call per station reached by riding transit |
| **Origin isochrone** | The surface reachable from the origin without boarding anything. A station reached by walking alone needs no egress call: the origin polygon already covers its own reach |
| **Journey** | The graph-side part: everything between stepping onto the first vehicle and stepping off the last, as one `transit.Reached` value. Access time in, ride and dwell and boarding wait accumulated, predecessor chain recorded |
| **Leg** | One hop of that journey — a single `Edge` traversed, with its service, seconds and dwell. `Legs` on a reachable station is the whole ridden path, oldest first |
| **Publication** | The act of the API publishing the queue message, and by extension the frozen inputs it carries. "The publication fixed the station positions" means: the graph in the message is immutable, so anything keyed on its compile job id can never go stale. Not to be confused with a **published** service in sparks-effect-api ([ADR-0005](https://github.com/andrewcgraves/sparks-effect-api/blob/main/docs/adr/0005-publishing-an-authored-service.md)): the same freezing one level up, pinning the compile job whose graph every message for that service then carries. This repository never sees that sense; here the word always means the queue message |

## Access time and its split

| Term | Definition |
| --- | --- |
| **Access time** | Door-to-door elapsed wall clock for the access leg, from leaving the origin to arriving at the boarding station, including the wait for the first scheduled departure. `access_secs`. It is what Valhalla's `summary.time` says for a depart-at request, and it is the figure the journey is seeded with ([ADR-0004](https://github.com/andrewcgraves/sparks-effect-api/blob/main/docs/adr/0004-transit-isochrone-claim-and-reported-time.md)) |
| **In-motion time** | Walking plus riding with every wait removed: `access_walk_secs + access_ride_secs`. A component of an access time, never a substitute for one. Not the API's **run time** — that is a compiled edge's time in motion with dwell excluded, one level down in the model; in-motion time is a whole access leg and keeps whatever dwell Valhalla's ride includes |
| **Access split** | The access time as `access_walk_secs`, `access_wait_secs` and `access_ride_secs`, which sum to `access_secs` exactly. Transit access legs only: the matrix that serves walk, bike and drive has no maneuvers to split, so those modes carry none. A leg Valhalla walked end to end is all walk, with an explicit zero wait and zero ride, beside `access_rode_transit: false` |
| **Access wait** | `access_wait_secs`: every wait on the access leg — for its first scheduled departure, and again at each transfer — the access leg's counterpart of the API's **boarding wait**, measured off Valhalla's timetable rather than charged by a policy. Named to sit beside `board_wait_secs`, and never overlapping it: `access_wait_secs` is inside `access_secs`, while `board_wait_secs` is the boarding wait for the authored service ridden on from the boarding station, which the graph search charges after the access time. Both are reported beside their totals, neither is subtracted from them |

Valhalla folds the platform wait **inside** the transit maneuver's own `time`; there
is no wait field. So the ride is the timetabled one — the alighting stop's arrival
clock minus the boarding stop's departure clock — and the wait is what the maneuver
holds beyond it, taken out of the maneuver rather than added beside it. That wait is
cross-checked against the clock gap from reaching the platform to the boarding
departure. Valhalla's stop clocks are minute precision, so the two may miss by up to
a minute (SPA-335 measured 24 s); past that the clocks and the seconds describe
different trips, and the split is **unreconciled**: withheld, the station and its
access time kept.

A station reached by riding the scenario has the access time of the station its
journey started at, so it carries that station's split too, as it does that
station's `access_rode_transit`.

## Reached vs reachable

Not synonyms, and the distinction matters when reading `chainer.go`:

- **Reached** — settled by the graph search. `map[string]transit.Reached` holds
  every node the Dijkstra pass assigned a total time to, *including nodes past
  the budget*. `tripProgress` reads exactly those overshooting entries to draw
  how far along a corridor the budget actually ran out.
- **Reachable** — reached **and** within budget. `ReachableStation` is the
  filtered, wire-facing form, and `reachable_stations` in the response metadata
  contains only these.

So every reachable station is reached; the reached set is deliberately larger.

A reachable station may have no egress polygon when that station's Valhalla
call failed. It stays in `reachable_stations` and is named in `dropped_egress`.

## Starter

- **Starter station** — of the stations the rider can get to *without* riding
  anything (`viaTransit` false), the one with the smallest access time, ties
  broken by slug. It is the station a rider would actually set off towards.
- **Starter walk** — the drawn route from the origin to the starter station, in
  the request's mode, returned as `starter_walk` so the map can show the first
  leg as a line rather than leaving it implied by a polygon. Named "walk"
  regardless of mode. Failing to route it is not a failed job — the chain simply
  returns without one.

## The departure clock

One job resolves **one departure**, in the handler before `Chain` runs, and
every Valhalla call the chain makes carries it. It defaults to 08:00 on the next
weekday — today, if today is one — resolved on `America/Los_Angeles`, because
every scenario served is in California or Nevada.

**Egress is the one leg that does not depart when the rider does.** It is stamped
at the chain's departure plus the whole journey to that station: access leg,
boarding wait and every hop ridden. That is when the rider is actually standing
on the platform.

- **Service date** (`departs_on`) — the GTFS calendar day a transit polygon was
  computed against, and part of the egress cache key. Empty for the modes with no
  timetable.
- **Rollover** (`reusable_until`) — the midnight that ends a transit job's
  service date, when the clock first chooses a different one
  (`DepartureRollsOver`). Reported on success with the job's tileset stamp, so
  the API can answer a repeat request from this job until then and no later
  (SPA-331). Not reported for the modes with no timetable.
- **Budget** (`budget_mins`) — the chain's budget, and part of a transit egress
  cache key: below the ceiling, budget and contour together fix when the rider is
  on the platform. Empty for the modes with no timetable, as the service date is.
- **Contour** / **clamping** — the minutes actually asked of Valhalla, after
  capping at `VALHALLA_MAX_ISO_CONTOUR_MINS`. At the ceiling the contour no
  longer says how much of the budget the journey spent, so two transit chains
  with one budget share a row whatever their departures, which is why the cache
  is coarser than the request above the ceiling.

## Synthetic

With `VALHALLA_SYNTHETIC=true` the worker draws **synthetic** isochrones —
circles sized from the budget — so the whole publish → consume → record loop
runs in seconds with no tileset. It is an explicit opt-in, never a fallback:
`VALHALLA_SYNTHETIC=true` and `VALHALLA_URL` are mutually exclusive, and a
worker with neither set refuses to start. Every synthetic feature carries
`"synthetic": true`, and a NULL `tileset_at` on a cache row is the tell that it
was drawn this way.
Synthetic and real polygons share one cache keyspace: nothing in the key says
which backend drew the shape. They do not share rows in practice. A synthetic
worker has no tileset stamp, so it reads nothing, and a real worker treats an
unstamped row as a miss (SPA-325).
