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
| **Access leg** | Getting from the origin to a station, routed by Valhalla in the requested mode. One Matrix call for walk/bike/drive; pairwise `/route` for transit, because Valhalla's Matrix rejects `multimodal` |
| **Egress leg** | Getting *away* from a station the rider reached, drawn as an isochrone sized by whatever budget is left. This is the bulk of the work — one call per station reached by riding transit |
| **Origin isochrone** | The surface reachable from the origin without boarding anything. A station reached by walking alone needs no egress call: the origin polygon already covers its own reach |
| **Journey** | The graph-side part: everything between stepping onto the first vehicle and stepping off the last, as one `transit.Reached` value. Access time in, ride and dwell and boarding wait accumulated, predecessor chain recorded |
| **Leg** | One hop of that journey — a single `Edge` traversed, with its service, seconds and dwell. `Legs` on a reachable station is the whole ridden path, oldest first |
| **Publication** | The act of the API publishing the queue message, and by extension the frozen inputs it carries. "The publication fixed the station positions" means: the graph in the message is immutable, so anything keyed on its compile job id can never go stale. Not to be confused with a **published** service in sparks-effect-api ([ADR-0005](https://github.com/andrewcgraves/sparks-effect-api/blob/main/docs/adr/0005-publishing-an-authored-service.md)): the same freezing one level up, pinning the compile job whose graph every message for that service then carries. This repository never sees that sense; here the word always means the queue message |

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

One chain resolves **one departure**, at the top of `Chain`, and every Valhalla
call it makes carries it. It defaults to 08:00 on the next weekday — today, if
today is one — resolved on `America/Los_Angeles`, because every scenario served
is in California or Nevada.

**Egress is the one leg that does not depart when the rider does.** It is stamped
at the chain's departure plus the whole journey to that station: access leg,
boarding wait and every hop ridden. That is when the rider is actually standing
on the platform.

- **Service date** (`departs_on`) — the GTFS calendar day a transit polygon was
  computed against, and part of the egress cache key. Empty for the modes with no
  timetable.
- **Budget** (`budget_mins`) — the chain's budget, and part of a transit egress
  cache key: below the ceiling, budget and contour together fix when the rider is
  on the platform. Empty for the modes with no timetable, as the service date is.
- **Contour** / **clamping** — the minutes actually asked of Valhalla, after
  capping at `VALHALLA_MAX_ISO_CONTOUR_MINS`. At the ceiling the contour no
  longer says how much of the budget the journey spent, so two transit chains
  with one budget share a row whatever their departures, which is why the cache
  is coarser than the request above the ceiling.

## Synthetic

With `VALHALLA_URL` unset the worker draws **synthetic** isochrones — circles
sized from the budget — so the whole publish → consume → record loop runs in
seconds with no tileset. Every synthetic feature carries `"synthetic": true`, and
a NULL `tileset_at` on a cache row is the tell that it was drawn this way.
Synthetic and real polygons share one cache keyspace: nothing in the key says
which backend drew the shape.
